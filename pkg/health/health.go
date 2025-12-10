package health

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// ProbeConfig defines health check parameters for a backend.
type ProbeConfig struct {
	Type     string        // "http" or "tcp"
	Path     string        // HTTP path
	Interval time.Duration // check frequency
	Timeout  time.Duration // per-check timeout
	Failures uint          // consecutive failures before marking unhealthy
}

// Status represents the health status of a backend.
type Status struct {
	Healthy        bool
	LastCheck      time.Time
	ConsecFailures uint
	LastError      string
	TotalChecks    uint64
	TotalFailures  uint64
}

// Prober periodically checks backend health and updates the status.
type Prober struct {
	cfg       ProbeConfig
	target    string // "host:port" for TCP, "https://host:port" for HTTP
	tlsConfig *tls.Config

	mu     sync.RWMutex
	status Status

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewProber creates a prober for the given target with optional TLS config.
func NewProber(cfg ProbeConfig, target string, tlsConfig *tls.Config) *Prober {
	return &Prober{
		cfg:       cfg,
		target:    target,
		tlsConfig: tlsConfig,
		status:    Status{Healthy: true},
	}
}

// Start begins periodic health checks in the background.
func (p *Prober) Start(ctx context.Context) {
	childCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.wg.Add(1)
	go p.loop(childCtx)
}

// Stop halts health checks and waits for completion.
func (p *Prober) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}

// GetStatus returns a copy of the current health status.
func (p *Prober) GetStatus() Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.status
}

func (p *Prober) loop(ctx context.Context) {
	defer p.wg.Done()
	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()

	// Initial check
	p.check(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.check(ctx)
		}
	}
}

func (p *Prober) check(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	var err error
	switch p.cfg.Type {
	case "http":
		err = p.checkHTTP(ctx)
	case "tcp":
		err = p.checkTCP(ctx)
	default:
		err = fmt.Errorf("unknown probe type: %s", p.cfg.Type)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.status.LastCheck = time.Now()
	p.status.TotalChecks++

	if err != nil {
		p.status.ConsecFailures++
		p.status.TotalFailures++
		p.status.LastError = err.Error()
		if p.status.ConsecFailures >= p.cfg.Failures {
			p.status.Healthy = false
		}
	} else {
		p.status.ConsecFailures = 0
		p.status.LastError = ""
		p.status.Healthy = true
	}
}

func (p *Prober) checkHTTP(ctx context.Context) error {
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: p.tlsConfig},
		Timeout:   p.cfg.Timeout,
	}
	url := p.target
	if p.cfg.Path != "" {
		url += p.cfg.Path
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (p *Prober) checkTCP(ctx context.Context) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", p.target)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}
