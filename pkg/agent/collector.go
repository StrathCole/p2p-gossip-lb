package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	io_prom "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"go.uber.org/zap"

	"github.com/lunc/mesh/pkg/registry"
)

type CollectorConfig struct {
	ChainID      registry.ChainID
	RPCEndpoint  string
	LCDEndpoint  string
	PromEndpoint string
	Timeout      time.Duration
}

type ProbeResult struct {
	Height     int64
	CatchingUp bool
	CPU        float32
	P95ms      float32
	RPS        float32
	FailRate   float32
	RTTms      float32
	Health     string
}

type Collector struct {
	cfg            CollectorConfig
	client         *http.Client
	log            *zap.Logger
	mu             sync.Mutex
	lastPromSample promSample
}

type promSample struct {
	valid      bool
	cpuSeconds float64
	requests   float64
	errors     float64
	timestamp  time.Time
	p95Seconds float64
}

func NewCollector(cfg CollectorConfig, logger *zap.Logger) (*Collector, error) {
	if cfg.RPCEndpoint == "" {
		return nil, errors.New("collector: RPC endpoint required")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	cfg.Timeout = timeout

	client := &http.Client{Timeout: timeout}

	if logger == nil {
		logger = zap.NewNop()
	}

	return &Collector{
		cfg:    cfg,
		client: client,
		log:    logger,
	}, nil
}

func (c *Collector) Collect(ctx context.Context) (ProbeResult, error) {
	result := ProbeResult{Health: "ok"}

	statusCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	height, catchingUp, rtt, err := c.fetchStatus(statusCtx)
	if err != nil {
		result.Health = "down"
		return result, fmt.Errorf("collector: status fetch: %w", err)
	}
	result.Height = height
	result.CatchingUp = catchingUp
	result.RTTms = rtt

	if c.cfg.LCDEndpoint != "" {
		lcdCtx, lcdCancel := context.WithTimeout(ctx, c.cfg.Timeout)
		if err := c.fetchNodeInfo(lcdCtx); err != nil {
			c.log.Debug("collector LCD probe failed", zap.Error(err))
			if result.Health == "ok" {
				result.Health = "degraded"
			}
		}
		lcdCancel()
	}

	if c.cfg.PromEndpoint != "" {
		promCtx, promCancel := context.WithTimeout(ctx, c.cfg.Timeout)
		stats, err := c.scrapeProm(promCtx)
		if err != nil {
			c.log.Debug("collector Prometheus scrape failed", zap.Error(err))
			if result.Health == "ok" {
				result.Health = "degraded"
			}
		} else {
			result.CPU = float32(stats.cpuPercent)
			result.RPS = float32(stats.rps)
			result.FailRate = float32(stats.failRate)
			result.P95ms = float32(stats.p95Seconds * 1000)
		}
		promCancel()
	}

	if result.CatchingUp && result.Health == "ok" {
		result.Health = "degraded"
	}

	return result, nil
}

func (c *Collector) fetchStatus(ctx context.Context) (int64, bool, float32, error) {
	start := time.Now()
	resp, err := c.doGet(ctx, c.cfg.RPCEndpoint+"/status")
	if err != nil {
		return 0, false, 0, err
	}
	defer resp.Body.Close()

	duration := time.Since(start)
	rtt := float32(duration) / float32(time.Millisecond)

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, false, rtt, err
	}

	var payload struct {
		Result struct {
			SyncInfo struct {
				LatestBlockHeight string `json:"latest_block_height"`
				CatchingUp        bool   `json:"catching_up"`
			} `json:"sync_info"`
		} `json:"result"`
	}

	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, false, rtt, err
	}

	height, err := strconv.ParseInt(strings.TrimSpace(payload.Result.SyncInfo.LatestBlockHeight), 10, 64)
	if err != nil {
		return 0, false, rtt, fmt.Errorf("parse latest_block_height: %w", err)
	}

	return height, payload.Result.SyncInfo.CatchingUp, rtt, nil
}

func (c *Collector) fetchNodeInfo(ctx context.Context) error {
	resp, err := c.doGet(ctx, c.cfg.LCDEndpoint)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}

	var payload struct {
		DefaultNodeInfo struct {
			Network string `json:"network"`
		} `json:"default_node_info"`
	}

	if err := json.Unmarshal(body, &payload); err != nil {
		return err
	}

	if c.cfg.ChainID != "" && payload.DefaultNodeInfo.Network != string(c.cfg.ChainID) {
		return fmt.Errorf("lcd chain id mismatch: expected %s got %s", c.cfg.ChainID, payload.DefaultNodeInfo.Network)
	}

	return nil
}

func (c *Collector) scrapeProm(ctx context.Context) (promStats, error) {
	resp, err := c.doGet(ctx, c.cfg.PromEndpoint)
	if err != nil {
		return promStats{}, err
	}
	defer resp.Body.Close()

	parser := expfmt.TextParser{}
	metrics, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		return promStats{}, err
	}

	sample := promSample{valid: true, timestamp: time.Now()}
	sample.cpuSeconds = findCounter(metrics, []string{"process_cpu_seconds_total", "process_cpu_time_seconds"})
	sample.requests = findCounter(metrics, []string{"tendermint_rpc_requests_total", "rpc_requests_total"})
	sample.errors = findCounter(metrics, []string{"tendermint_rpc_failures_total", "rpc_errors_total"})
	sample.p95Seconds = findLatencyP95(metrics)

	c.mu.Lock()
	prev := c.lastPromSample
	c.lastPromSample = sample
	c.mu.Unlock()

	stats := promStats{}
	if !prev.valid {
		return stats, nil
	}

	deltaT := sample.timestamp.Sub(prev.timestamp).Seconds()
	if deltaT <= 0 {
		return stats, nil
	}

	deltaCPU := sample.cpuSeconds - prev.cpuSeconds
	if deltaCPU >= 0 {
		stats.cpuPercent = math.Min(100, (deltaCPU/deltaT)*100)
	}

	deltaRequests := sample.requests - prev.requests
	if deltaRequests >= 0 {
		stats.rps = deltaRequests / deltaT
	}

	deltaErrors := sample.errors - prev.errors
	if deltaRequests > 0 && deltaErrors >= 0 {
		rate := deltaErrors / deltaRequests
		if rate < 0 {
			rate = 0
		}
		if rate > 1 {
			rate = 1
		}
		stats.failRate = rate
	}

	if sample.p95Seconds > 0 {
		stats.p95Seconds = sample.p95Seconds
	}

	return stats, nil
}

func (c *Collector) doGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/plain")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

type promStats struct {
	cpuPercent float64
	rps        float64
	failRate   float64
	p95Seconds float64
}

func findCounter(metrics map[string]*io_prom.MetricFamily, names []string) float64 {
	for _, name := range names {
		if mf, ok := metrics[name]; ok {
			var sum float64
			for _, metric := range mf.Metric {
				if counter := metric.GetCounter(); counter != nil {
					sum += counter.GetValue()
				}
			}
			if sum > 0 {
				return sum
			}
		}
	}
	return 0
}

func findLatencyP95(metrics map[string]*io_prom.MetricFamily) float64 {
	if value, ok := summaryQuantile(metrics, []string{"rpc_request_duration_seconds", "tendermint_rpc_request_duration_seconds"}, 0.95); ok {
		return value
	}
	if value, ok := histogramPercentile(metrics, []string{"rpc_request_duration_seconds_bucket", "tendermint_rpc_request_duration_seconds_bucket"}, 0.95); ok {
		return value
	}
	return 0
}

func summaryQuantile(metrics map[string]*io_prom.MetricFamily, names []string, quantile float64) (float64, bool) {
	for _, name := range names {
		mf, ok := metrics[name]
		if !ok {
			continue
		}
		for _, metric := range mf.Metric {
			if summary := metric.GetSummary(); summary != nil {
				for _, q := range summary.Quantile {
					if q.GetQuantile() == quantile {
						return q.GetValue(), true
					}
				}
			}
		}
	}
	return 0, false
}

func histogramPercentile(metrics map[string]*io_prom.MetricFamily, names []string, quantile float64) (float64, bool) {
	for _, name := range names {
		mf, ok := metrics[name]
		if !ok {
			continue
		}
		for _, metric := range mf.Metric {
			hist := metric.GetHistogram()
			if hist == nil {
				continue
			}
			total := hist.GetSampleCount()
			if total == 0 {
				continue
			}
			target := uint64(math.Ceil(float64(total) * quantile))
			var accum uint64
			for _, bucket := range hist.Bucket {
				accum += bucket.GetCumulativeCount()
				if accum >= target {
					return bucket.GetUpperBound(), true
				}
			}
		}
	}
	return 0, false
}
