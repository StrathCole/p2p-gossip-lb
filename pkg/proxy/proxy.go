package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/lunc/mesh/pkg/cache"
	"github.com/lunc/mesh/pkg/registry"
	"github.com/lunc/mesh/pkg/selector"
)

// RateLimitConfig describes the token bucket parameters for client rate limiting.
type RateLimitConfig struct {
	PerIPRPS int
	Burst    int
	Window   time.Duration
}

// Config describes the inputs required to construct a Proxy service.
type Config struct {
	Store             *registry.Store
	Chains            []registry.ChainID
	Selector          selector.Config
	CacheEnabled      bool
	CacheMaxBytes     int64
	CacheMaxItemBytes int64
	CacheTTLHeighted  time.Duration
	CacheTTLInfinite  bool
	NegativeCacheTTL  time.Duration
	RateLimit         RateLimitConfig
	EdgeCountry       string
	EdgeRegion        string
	BackendScheme     string
	Logger            *zap.Logger
	Transport         http.RoundTripper
}

// Proxy implements the edge proxy logic for RPC/LCD/WebSocket traffic.
type Proxy struct {
	store            *registry.Store
	selectorCfg      selector.Config
	allowedChains    map[registry.ChainID]struct{}
	cacheEnabled     bool
	cache            *cache.ResponseCache
	cacheMaxItem     int64
	cacheTTL         time.Duration
	cacheInfinite    bool
	negativeCacheTTL time.Duration
	rateLimiter      *clientRateLimiter
	logger           *zap.Logger
	transport        http.RoundTripper
	backendScheme    string
	edgeCountry      string
	edgeRegion       string
}

// New constructs a Proxy using the provided configuration.
func New(cfg Config) (*Proxy, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("proxy: registry store required")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	allowed := make(map[registry.ChainID]struct{}, len(cfg.Chains))
	for _, chain := range cfg.Chains {
		allowed[chain] = struct{}{}
	}

	var limiter *clientRateLimiter
	if cfg.RateLimit.PerIPRPS > 0 {
		window := cfg.RateLimit.Window
		if window <= 0 {
			window = 10 * time.Minute
		}
		burst := cfg.RateLimit.Burst
		if burst <= 0 {
			burst = cfg.RateLimit.PerIPRPS * 2
		}
		limiter = newClientRateLimiter(cfg.RateLimit.PerIPRPS, burst, window)
	}

	var respCache *cache.ResponseCache
	if cfg.CacheEnabled {
		capacity := 1024
		if cfg.CacheMaxBytes > 0 && cfg.CacheMaxItemBytes > 0 {
			est := int(cfg.CacheMaxBytes / cfg.CacheMaxItemBytes)
			if est > 0 {
				capacity = est
			}
		}
		store, err := cache.NewResponseCache(capacity, cfg.CacheMaxBytes)
		if err != nil {
			return nil, err
		}
		respCache = store
	}

	transport := cfg.Transport
	if transport == nil {
		transport = defaultTransport()
	}

	scheme := cfg.BackendScheme
	if scheme == "" {
		scheme = "https"
	}

	return &Proxy{
		store:            cfg.Store,
		selectorCfg:      cfg.Selector,
		allowedChains:    allowed,
		cacheEnabled:     cfg.CacheEnabled,
		cache:            respCache,
		cacheMaxItem:     cfg.CacheMaxItemBytes,
		cacheTTL:         cfg.CacheTTLHeighted,
		cacheInfinite:    cfg.CacheTTLInfinite,
		negativeCacheTTL: cfg.NegativeCacheTTL,
		rateLimiter:      limiter,
		logger:           logger,
		transport:        transport,
		backendScheme:    scheme,
		edgeCountry:      cfg.EdgeCountry,
		edgeRegion:       cfg.EdgeRegion,
	}, nil
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	chain, service, err := extractChain(r)
	if err != nil {
		http.Error(w, "meshproxy: unable to determine chain", http.StatusBadRequest)
		return
	}
	if len(p.allowedChains) > 0 {
		if _, ok := p.allowedChains[chain]; !ok {
			http.Error(w, "meshproxy: chain not permitted", http.StatusForbidden)
			return
		}
	}

	clientIP := clientIPFromRequest(r)
	if p.rateLimiter != nil && clientIP != "" {
		if !p.rateLimiter.Allow(clientIP) {
			http.Error(w, "meshproxy: rate limit exceeded", http.StatusTooManyRequests)
			return
		}
	}

	needWS := isWebsocketRequest(r) || strings.EqualFold(service, "ws")
	reqCtx := buildRequestContext(r, chain, needWS)
	p.applyGeoHints(&reqCtx)

	if needWS {
		p.handleWebsocket(w, r, chain, reqCtx)
		return
	}

	p.handleHTTP(w, r, chain, reqCtx)
}

func (p *Proxy) handleHTTP(w http.ResponseWriter, r *http.Request, chain registry.ChainID, reqCtx selector.RequestContext) {
	pool, env := p.buildCandidates(chain)
	if len(pool) == 0 {
		http.Error(w, "meshproxy: no healthy backends", http.StatusServiceUnavailable)
		return
	}

	cacheKey, cacheable := p.shouldCacheRequest(r, reqCtx)
	if cacheable && p.cacheEnabled {
		if entry, ok := p.cache.Lookup(cacheKey, time.Now()); ok {
			p.writeCachedResponse(w, entry)
			return
		}
	}

	bodyCopy, err := readRequestBody(r)
	if err != nil {
		p.logger.Warn("failed to read request body", zap.Error(err))
		http.Error(w, "meshproxy: invalid request body", http.StatusBadRequest)
		return
	}

	tried := make(map[registry.BackendID]struct{})
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		candidate, err := p.pickCandidate(reqCtx, env, pool, tried)
		if err != nil {
			lastErr = err
			break
		}
		resp, payload, err := p.forwardRequest(r.Context(), r, bodyCopy, candidate)
		if err != nil {
			lastErr = err
			tried[candidate.Meta.ID] = struct{}{}
			continue
		}
		p.writeUpstreamResponse(w, r, candidate, resp, payload, cacheable, cacheKey)
		return
	}

	if lastErr != nil {
		p.logger.Warn("request failed", zap.Error(lastErr))
	}
	http.Error(w, "meshproxy: upstream unavailable", http.StatusBadGateway)
}

func (p *Proxy) handleWebsocket(w http.ResponseWriter, r *http.Request, chain registry.ChainID, reqCtx selector.RequestContext) {
	pool, env := p.buildCandidates(chain)
	if len(pool) == 0 {
		http.Error(w, "meshproxy: no healthy backends", http.StatusServiceUnavailable)
		return
	}

	tried := make(map[registry.BackendID]struct{})
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		candidate, err := p.pickCandidate(reqCtx, env, pool, tried)
		if err != nil {
			lastErr = err
			break
		}
		if err := p.proxyWebsocket(w, r, candidate); err != nil {
			lastErr = err
			tried[candidate.Meta.ID] = struct{}{}
			continue
		}
		return
	}

	if lastErr != nil {
		p.logger.Warn("websocket proxy failed", zap.Error(lastErr))
	}
	http.Error(w, "meshproxy: websocket upstream unavailable", http.StatusBadGateway)
}

func (p *Proxy) pickCandidate(ctx selector.RequestContext, env selector.Environment, pool []selector.Candidate, tried map[registry.BackendID]struct{}) (selector.Candidate, error) {
	filtered := make([]selector.Candidate, 0, len(pool))
	for _, cand := range pool {
		if _, seen := tried[cand.Meta.ID]; seen {
			continue
		}
		filtered = append(filtered, cand)
	}
	if len(filtered) == 0 {
		return selector.Candidate{}, selector.ErrNoBackend
	}
	return selector.Pick(ctx, env, filtered, p.selectorCfg)
}

func (p *Proxy) forwardRequest(ctx context.Context, original *http.Request, body []byte, candidate selector.Candidate) (*http.Response, []byte, error) {
	outbound, err := buildOutboundRequest(ctx, original, body, candidate.Meta.Host, p.backendScheme)
	if err != nil {
		return nil, nil, err
	}

	resp, err := p.transport.RoundTrip(outbound)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(payload))
	return resp, payload, nil
}

func (p *Proxy) writeUpstreamResponse(w http.ResponseWriter, req *http.Request, backend selector.Candidate, resp *http.Response, body []byte, cacheable bool, cacheKey string) {
	header := cloneHeader(resp.Header)
	sanitizeHopHeaders(header)
	header.Del("Set-Cookie")
	for key, values := range header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("X-Mesh-Backend", string(backend.Meta.ID))
	if cacheable && p.cacheEnabled {
		w.Header().Set("X-Mesh-Cache", "miss")
	}

	w.WriteHeader(resp.StatusCode)
	if len(body) > 0 {
		if _, err := w.Write(body); err != nil {
			p.logger.Warn("failed to write response", zap.Error(err))
		}
	}

	if !p.cacheEnabled {
		return
	}

	if cacheable && resp.StatusCode == http.StatusOK {
		if p.cacheMaxItem > 0 && int64(len(body)) > p.cacheMaxItem {
			return
		}
		tl := p.cacheTTL
		if p.cacheInfinite {
			tl = 0
		}
		entry := cache.BuildEntry(resp, body, tl)
		if entry == nil {
			return
		}
		entry.Header = header
		p.cache.Store(cacheKey, entry)
		return
	}

	if p.negativeCacheTTL > 0 && resp.StatusCode == http.StatusNotFound && shouldNegativeCache(req) {
		entry := cache.BuildEntry(resp, body, p.negativeCacheTTL)
		if entry == nil {
			return
		}
		entry.Header = header
		p.cache.Store(negativeCacheKey(req), entry)
	}
}

func (p *Proxy) shouldCacheRequest(r *http.Request, ctx selector.RequestContext) (string, bool) {
	if !p.cacheEnabled {
		return "", false
	}
	if r.Method != http.MethodGet {
		return "", false
	}
	if ctx.Height == nil {
		return "", false
	}
	return cacheKey(r, "height"), true
}

func (p *Proxy) writeCachedResponse(w http.ResponseWriter, entry *cache.Entry) {
	header := cloneHeader(entry.Header)
	sanitizeHopHeaders(header)
	for key, values := range header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("X-Mesh-Cache", "hit")
	w.Header().Set("Content-Length", strconv.Itoa(len(entry.Body)))
	w.WriteHeader(entry.Status)
	if len(entry.Body) > 0 {
		if _, err := w.Write(entry.Body); err != nil {
			p.logger.Warn("failed to write cached response", zap.Error(err))
		}
	}
}

func (p *Proxy) buildCandidates(chain registry.ChainID) ([]selector.Candidate, selector.Environment) {
	snapshot, _ := p.store.BackendSnapshot()
	metrics := p.store.LatestMetrics()

	candidates := make([]selector.Candidate, 0, len(snapshot))
	var bestHeight int64
	for _, entry := range snapshot {
		if entry.Tombstone {
			continue
		}
		meta := entry.Value
		if meta.ChainID != chain {
			continue
		}
		metric, ok := metrics[meta.ID]
		if !ok {
			continue
		}
		if metric.Height > bestHeight {
			bestHeight = metric.Height
		}
		cand := selector.Candidate{
			Meta:       meta,
			Metrics:    metric,
			GeoPenalty: p.geoPenalty(meta),
		}
		candidates = append(candidates, cand)
	}

	return candidates, selector.Environment{BestHeight: bestHeight}
}

func (p *Proxy) geoPenalty(meta registry.BackendMeta) float64 {
	if p.edgeCountry == "" || meta.Country == "" {
		return 0
	}
	country := strings.ToLower(meta.Country)
	region := strings.ToLower(meta.Region)
	edgeCountry := strings.ToLower(p.edgeCountry)
	edgeRegion := strings.ToLower(p.edgeRegion)
	if country == edgeCountry {
		if edgeRegion != "" && region == edgeRegion {
			return 0
		}
		return 15
	}
	return 30
}

func (p *Proxy) applyGeoHints(ctx *selector.RequestContext) {
	// Reserved for future use; currently noop but kept for extensibility.
}
