package dnsplugin

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/lunc/mesh/pkg/registry"
)

type ZoneView interface {
	EdgeRecords(ctx context.Context, chain registry.ChainID, max int, ecsIP net.IP) ([]dns.RR, error)
	NSRecords(ctx context.Context) ([]dns.RR, error)
	ZoneSerial() uint32
}

type Config struct {
	Zone         string
	Chains       []registry.ChainID
	NSLabels     []string
	TTLService   uint32
	TTLNSA       uint32
	MaxAnswers   int
	EnableECS    bool
	RRLRate      float64
	RRLBurst     int
	RRLWindow    time.Duration
	Logger       *zap.Logger
	ZoneProvider ZoneView
}

type Handler struct {
	cfg       Config
	log       *zap.Logger
	rateLimit *requestLimiter
}

func NewHandler(cfg Config) (*Handler, error) {
	if cfg.Zone == "" {
		return nil, errors.New("dnsplugin: zone required")
	}
	if cfg.ZoneProvider == nil {
		return nil, errors.New("dnsplugin: zone provider required")
	}
	if cfg.MaxAnswers <= 0 {
		cfg.MaxAnswers = 4
	}
	if cfg.TTLService == 0 {
		cfg.TTLService = 20
	}
	if cfg.TTLNSA == 0 {
		cfg.TTLNSA = 30
	}
	if cfg.RRLRate < 0 {
		return nil, errors.New("dnsplugin: rrl rate cannot be negative")
	}
	if cfg.RRLWindow < 0 {
		return nil, errors.New("dnsplugin: rrl window cannot be negative")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	var limiter *requestLimiter
	if cfg.RRLRate > 0 {
		burst := cfg.RRLBurst
		if burst <= 0 {
			burst = int(cfg.RRLRate * 2)
			if burst < 1 {
				burst = 1
			}
		}
		window := cfg.RRLWindow
		if window == 0 {
			window = 5 * time.Minute
		}
		limiter = newRequestLimiter(rate.Limit(cfg.RRLRate), burst, window)
	}
	return &Handler{cfg: cfg, log: logger, rateLimit: limiter}, nil
}

func (h *Handler) Name() string { return "mesh" }

func (h *Handler) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	if len(r.Question) == 0 {
		return dns.RcodeFormatError, errors.New("dnsplugin: empty question")
	}

	q := r.Question[0]
	name := strings.ToLower(dns.Fqdn(q.Name))

	msg := new(dns.Msg)
	msg.SetReply(r)
	msg.Authoritative = true
	msg.RecursionAvailable = false

	if h.rateLimit != nil {
		ip := remoteIP(w.RemoteAddr())
		if !h.rateLimit.Allow(ip) {
			msg.Rcode = dns.RcodeRefused
			_ = w.WriteMsg(msg)
			h.log.Debug("query rate limited", zap.String("client", ip))
			return dns.RcodeRefused, nil
		}
	}

	switch q.Qtype {
	case dns.TypeSOA:
		msg.Answer = append(msg.Answer, h.soaRecord())
		_ = w.WriteMsg(msg)
		return dns.RcodeSuccess, nil
	case dns.TypeNS:
		records, err := h.cfg.ZoneProvider.NSRecords(ctx)
		if err != nil {
			h.log.Warn("ns records", zap.Error(err))
			return dns.RcodeServerFailure, err
		}
		msg.Answer = append(msg.Answer, records...)
		_ = w.WriteMsg(msg)
		return dns.RcodeSuccess, nil
	case dns.TypeA, dns.TypeAAAA, dns.TypeHTTPS:
		rr, err := h.answerService(ctx, name, q.Qtype, r)
		if err != nil {
			if errors.Is(err, errNoChainMatch) {
				msg.Rcode = dns.RcodeNameError
				_ = w.WriteMsg(msg)
				return dns.RcodeNameError, nil
			}
			h.log.Warn("service answer", zap.String("name", name), zap.Error(err))
			return dns.RcodeServerFailure, err
		}
		msg.Answer = append(msg.Answer, rr...)
		_ = w.WriteMsg(msg)
		return dns.RcodeSuccess, nil
	default:
		msg.Rcode = dns.RcodeNotImplemented
		_ = w.WriteMsg(msg)
		return dns.RcodeNotImplemented, nil
	}
}

func (h *Handler) soaRecord() dns.RR {
	serial := h.cfg.ZoneProvider.ZoneSerial()
	nsLabel := "ns1." + h.cfg.Zone
	if len(h.cfg.NSLabels) > 0 {
		nsLabel = h.cfg.NSLabels[0]
	}
	soa := &dns.SOA{
		Hdr:     dns.RR_Header{Name: dns.Fqdn(h.cfg.Zone), Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: h.cfg.TTLNSA},
		Ns:      dns.Fqdn(nsLabel),
		Mbox:    dns.Fqdn("hostmaster." + h.cfg.Zone),
		Serial:  serial,
		Refresh: 600,
		Retry:   120,
		Expire:  604800,
		Minttl:  60,
	}
	return soa
}

var errNoChainMatch = errors.New("dnsplugin: no chain match")

func (h *Handler) answerService(ctx context.Context, name string, qtype uint16, msg *dns.Msg) ([]dns.RR, error) {
	chainID, prefix, ok := h.matchServiceName(name)
	if !ok {
		return nil, errNoChainMatch
	}

	if !h.allowedService(prefix) {
		return nil, errNoChainMatch
	}

	var ecsIP net.IP
	if h.cfg.EnableECS {
		ecsIP = extractECS(msg)
	}

	records, err := h.cfg.ZoneProvider.EdgeRecords(ctx, chainID, h.cfg.MaxAnswers, ecsIP)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, errNoChainMatch
	}

	decorateRecords(records, h.cfg.TTLService, name)

	switch qtype {
	case dns.TypeHTTPS:
		svc := h.buildHTTPSRecord(name, records)
		if svc == nil {
			return nil, errNoChainMatch
		}
		msg.Extra = append(msg.Extra, records...)
		return []dns.RR{svc}, nil
	default:
		filtered := filterByType(records, qtype)
		if len(filtered) == 0 {
			filtered = records
		}
		return filtered, nil
	}
}

func (h *Handler) matchServiceName(name string) (registry.ChainID, string, bool) {
	zone := dns.Fqdn(h.cfg.Zone)
	if !strings.HasSuffix(name, zone) {
		return "", "", false
	}
	label := strings.TrimSuffix(name, zone)
	parts := strings.Split(strings.TrimSuffix(label, "."), ".")
	if len(parts) < 2 {
		return "", "", false
	}
	chainPart := parts[len(parts)-2]
	chain := registry.ChainID(chainPart)
	for _, c := range h.cfg.Chains {
		if c == chain {
			return chain, parts[0], true
		}
	}
	return "", "", false
}

func (h *Handler) allowedService(prefix string) bool {
	switch strings.ToLower(prefix) {
	case "rpc", "lcd", "ws":
		return true
	default:
		return false
	}
}

func filterByType(records []dns.RR, t uint16) []dns.RR {
	if len(records) == 0 {
		return nil
	}
	out := make([]dns.RR, 0, len(records))
	for _, rr := range records {
		if rr.Header().Rrtype == t {
			out = append(out, rr)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func decorateRecords(records []dns.RR, ttl uint32, name string) {
	for _, rr := range records {
		header := rr.Header()
		header.Ttl = ttl
		header.Name = name
	}
}

func (h *Handler) buildHTTPSRecord(name string, hints []dns.RR) dns.RR {
	svc := &dns.HTTPS{
		SVCB: dns.SVCB{
			Hdr:      dns.RR_Header{Name: name, Rrtype: dns.TypeHTTPS, Class: dns.ClassINET, Ttl: h.cfg.TTLService},
			Priority: 1,
			Target:   ".",
		},
	}

	alpn := &dns.SVCBAlpn{Alpn: []string{"h2", "http/1.1"}}
	svc.Value = append(svc.Value, alpn)

	var v4Hints []net.IP
	var v6Hints []net.IP
	for _, rr := range hints {
		switch rec := rr.(type) {
		case *dns.A:
			v4Hints = append(v4Hints, append(net.IP(nil), rec.A...))
		case *dns.AAAA:
			v6Hints = append(v6Hints, append(net.IP(nil), rec.AAAA...))
		}
	}
	if len(v4Hints) > 0 {
		svc.Value = append(svc.Value, &dns.SVCBIPv4Hint{Hint: v4Hints})
	}
	if len(v6Hints) > 0 {
		svc.Value = append(svc.Value, &dns.SVCBIPv6Hint{Hint: v6Hints})
	}
	return svc
}

func extractECS(msg *dns.Msg) net.IP {
	for _, extra := range msg.Extra {
		if opt, ok := extra.(*dns.OPT); ok {
			for _, option := range opt.Option {
				if ecs, ok := option.(*dns.EDNS0_SUBNET); ok {
					if len(ecs.Address) == 0 {
						continue
					}
					return append(net.IP(nil), ecs.Address...)
				}
			}
		}
	}
	return nil
}

func remoteIP(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	switch v := addr.(type) {
	case *net.UDPAddr:
		return v.IP.String()
	case *net.TCPAddr:
		return v.IP.String()
	default:
		host, _, err := net.SplitHostPort(addr.String())
		if err != nil {
			return addr.String()
		}
		return host
	}
}

type requestLimiter struct {
	mu            sync.Mutex
	entries       map[string]*limiterEntry
	limit         rate.Limit
	burst         int
	ttl           time.Duration
	lastCleanup   time.Time
	cleanupPeriod time.Duration
}

type limiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newRequestLimiter(limit rate.Limit, burst int, ttl time.Duration) *requestLimiter {
	cleanup := ttl / 2
	if cleanup <= 0 {
		cleanup = ttl
	}
	if cleanup <= 0 {
		cleanup = time.Minute
	}
	return &requestLimiter{
		entries:       make(map[string]*limiterEntry),
		limit:         limit,
		burst:         burst,
		ttl:           ttl,
		cleanupPeriod: cleanup,
	}
}

func (r *requestLimiter) Allow(ip string) bool {
	if ip == "" {
		return true
	}

	now := time.Now()

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cleanupPeriod > 0 && (r.lastCleanup.IsZero() || now.Sub(r.lastCleanup) >= r.cleanupPeriod) {
		cutoff := now.Add(-r.ttl)
		for key, entry := range r.entries {
			if entry.lastSeen.Before(cutoff) {
				delete(r.entries, key)
			}
		}
		r.lastCleanup = now
	}

	entry := r.entries[ip]
	if entry == nil {
		entry = &limiterEntry{limiter: rate.NewLimiter(r.limit, r.burst)}
		r.entries[ip] = entry
	}
	entry.lastSeen = now
	return entry.limiter.Allow()
}
