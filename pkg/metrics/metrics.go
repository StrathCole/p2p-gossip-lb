package metrics

import (
	"fmt"
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds Prometheus collectors for mesh observability.
type Metrics struct {
	registry *prometheus.Registry

	// Edge proxy metrics
	RequestsTotal       *prometheus.CounterVec
	RequestDuration     *prometheus.HistogramVec
	CacheHits           *prometheus.CounterVec
	BackendFailures     *prometheus.CounterVec
	ActiveConnections   *prometheus.GaugeVec
	RateLimitedRequests prometheus.Counter

	// Backend agent metrics
	NodeAdvertsPublished      prometheus.Counter
	MetricsCollectionDuration prometheus.Histogram

	// Registry metrics
	RegistrySize       prometheus.Gauge
	GossipMessagesSent prometheus.Counter
	GossipMessagesRecv prometheus.Counter
}

// NewMetrics creates a new metrics registry with all collectors.
func NewMetrics(namespace string) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		registry: reg,
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "requests_total",
			Help:      "Total HTTP requests by status code",
		}, []string{"code", "method"}),
		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "request_duration_seconds",
			Help:      "HTTP request latency",
			Buckets:   prometheus.DefBuckets,
		}, []string{"method"}),
		CacheHits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "cache_hits_total",
			Help:      "Cache hits and misses",
		}, []string{"status"}),
		BackendFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "backend_failures_total",
			Help:      "Backend request failures",
		}, []string{"backend"}),
		ActiveConnections: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "active_connections",
			Help:      "Active client connections",
		}, []string{"type"}),
		RateLimitedRequests: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "rate_limited_requests_total",
			Help:      "Requests rejected by rate limiter",
		}),
		NodeAdvertsPublished: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "node_adverts_published_total",
			Help:      "Node advertisements published to gossip",
		}),
		MetricsCollectionDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "metrics_collection_duration_seconds",
			Help:      "Time to collect backend metrics",
		}),
		RegistrySize: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "registry_entries",
			Help:      "Number of entries in CRDT registry",
		}),
		GossipMessagesSent: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "gossip_messages_sent_total",
			Help:      "Gossip messages sent",
		}),
		GossipMessagesRecv: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "gossip_messages_received_total",
			Help:      "Gossip messages received",
		}),
	}

	// Register all collectors
	reg.MustRegister(
		m.RequestsTotal,
		m.RequestDuration,
		m.CacheHits,
		m.BackendFailures,
		m.ActiveConnections,
		m.RateLimitedRequests,
		m.NodeAdvertsPublished,
		m.MetricsCollectionDuration,
		m.RegistrySize,
		m.GossipMessagesSent,
		m.GossipMessagesRecv,
	)
	return m
}

// Handler returns an HTTP handler for Prometheus scraping.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// ListenAndServe starts an HTTP server on addr for metrics exposition.
func (m *Metrics) ListenAndServe(addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	return http.ListenAndServe(addr, mux)
}

// PushGatewayConfig holds push gateway connection details.
type PushGatewayConfig struct {
	URL      string
	Job      string
	Instance string
}

// Pusher periodically pushes metrics to a Prometheus push gateway.
type Pusher struct {
	cfg PushGatewayConfig
	m   *Metrics

	mu     sync.Mutex
	cancel chan struct{}
	wg     sync.WaitGroup
}

// NewPusher creates a pusher for the given metrics and push gateway.
func NewPusher(cfg PushGatewayConfig, m *Metrics) *Pusher {
	return &Pusher{cfg: cfg, m: m}
}

// Start begins pushing metrics at the configured interval (not implemented here - stub).
func (p *Pusher) Start() {
	// In production, use prometheus/common/push or similar library
	// For now, just a placeholder
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancel = make(chan struct{})
}

// Stop halts metric pushing.
func (p *Pusher) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		close(p.cancel)
		p.wg.Wait()
	}
}

// Push sends current metrics to push gateway (stub).
func (p *Pusher) Push() error {
	// Stub - would use push.New(...).Gatherer(p.m.registry).Push()
	return fmt.Errorf("push not implemented")
}
