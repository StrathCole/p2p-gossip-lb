package dnsplugin

import (
	"context"
	"fmt"
	"hash/fnv"
	"net"
	"sort"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"go.uber.org/zap"

	"github.com/lunc/mesh/pkg/registry"
)

// RegistryZoneView implements ZoneView backed by the shared registry store.
type RegistryZoneView struct {
	store       *registry.Store
	zone        string
	nsLabels    []string
	ttlNSA      uint32
	ttlService  uint32
	log         *zap.Logger
	serialCache atomic.Uint32
}

// RegistryZoneViewConfig describes the required inputs for creating the zone view.
type RegistryZoneViewConfig struct {
	Store      *registry.Store
	Zone       string
	NSLabels   []string
	TTLNSA     uint32
	TTLService uint32
	Logger     *zap.Logger
}

// NewRegistryZoneView builds a zone view for the DNS plugin.
func NewRegistryZoneView(cfg RegistryZoneViewConfig) (*RegistryZoneView, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("dnsplugin: registry store required")
	}
	if cfg.Zone == "" {
		return nil, fmt.Errorf("dnsplugin: zone required")
	}
	labels := cfg.NSLabels
	if len(labels) == 0 {
		labels = []string{"ns1." + cfg.Zone, "ns2." + cfg.Zone}
	}
	ttlNS := cfg.TTLNSA
	if ttlNS == 0 {
		ttlNS = 30
	}
	ttlSvc := cfg.TTLService
	if ttlSvc == 0 {
		ttlSvc = 20
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	return &RegistryZoneView{
		store:      cfg.Store,
		zone:       dns.Fqdn(cfg.Zone),
		nsLabels:   labels,
		ttlNSA:     ttlNS,
		ttlService: ttlSvc,
		log:        logger,
	}, nil
}

// EdgeRecords selects up to max records representing mesh edges suitable for the query.
func (v *RegistryZoneView) EdgeRecords(ctx context.Context, chain registry.ChainID, max int, ecsIP net.IP) ([]dns.RR, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, _ := v.store.NodeSnapshot()
	v.log.Info("EdgeRecords called",
		zap.String("chain", string(chain)),
		zap.Int("node_entries", len(entries)),
	)
	type edge struct {
		rr   dns.RR
		hash uint32
	}
	edges := make([]edge, 0, len(entries))
	seen := make(map[string]struct{})
	for id, entry := range entries {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		v.log.Debug("checking node entry",
			zap.String("id", string(id)),
			zap.Bool("tombstone", entry.Tombstone),
			zap.Bool("ns_serving", entry.Value.NSServing),
			zap.Int("ips", len(entry.Value.IPs)),
		)
		if entry.Tombstone {
			continue
		}
		advert := entry.Value
		if !advert.NSServing {
			continue
		}
		if len(advert.IPs) == 0 {
			continue
		}
		for _, ip := range advert.IPs {
			rr := toRR(ip, v.zone, v.ttlService)
			if rr == nil {
				continue
			}
			key := rr.String()
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			score := hashEdge(chain, advert.ID, ip, ecsIP)
			edges = append(edges, edge{rr: rr, hash: score})
		}
	}
	if len(edges) == 0 {
		return nil, nil
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].hash < edges[j].hash })
	if max > 0 && len(edges) > max {
		edges = edges[:max]
	}
	records := make([]dns.RR, len(edges))
	for i := range edges {
		records[i] = edges[i].rr
	}
	return records, nil
}

// NSRecords returns the authoritative NS records for the zone.
func (v *RegistryZoneView) NSRecords(ctx context.Context) ([]dns.RR, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	records := make([]dns.RR, 0, len(v.nsLabels))
	for _, label := range v.nsLabels {
		fqdn := dns.Fqdn(label)
		records = append(records, &dns.NS{Hdr: dns.RR_Header{Name: v.zone, Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: v.ttlNSA}, Ns: fqdn})
	}
	return records, nil
}

// ZoneSerial derives a zone serial based on the registry version vector.
func (v *RegistryZoneView) ZoneSerial() uint32 {
	snapshot := v.store.NodeVersionVector()
	var sum uint64
	for _, value := range snapshot {
		sum += value
	}
	if sum == 0 {
		now := uint32(time.Now().Unix())
		v.serialCache.Store(now)
		return now
	}
	serial := uint32(sum % uint64(mathMaxUint32))
	prev := v.serialCache.Load()
	if serial <= prev {
		if prev == mathMaxUint32 {
			serial = prev
		} else {
			serial = prev + 1
		}
	}
	v.serialCache.Store(serial)
	return serial
}

const mathMaxUint32 = ^uint32(0)

func hashEdge(chain registry.ChainID, id registry.NodeID, ip net.IP, ecs net.IP) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(chain))
	_, _ = h.Write([]byte(id))
	if ip != nil {
		_, _ = h.Write([]byte(ip))
	}
	if ecs != nil {
		_, _ = h.Write([]byte(ecs))
	}
	return h.Sum32()
}

func toRR(ip net.IP, zone string, ttl uint32) dns.RR {
	if ip == nil {
		return nil
	}
	header := dns.RR_Header{Name: zone, Class: dns.ClassINET, Ttl: ttl}
	if v4 := ip.To4(); v4 != nil {
		header.Rrtype = dns.TypeA
		return &dns.A{Hdr: header, A: v4}
	}
	if v6 := ip.To16(); v6 != nil {
		header.Rrtype = dns.TypeAAAA
		return &dns.AAAA{Hdr: header, AAAA: v6}
	}
	return nil
}
