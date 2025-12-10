package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/lunc/mesh/pkg/gossip"
	"github.com/lunc/mesh/pkg/registry"
)

type Config struct {
	Gossip          gossip.Config
	Meta            registry.BackendMeta
	MetricsInterval time.Duration
	VerifyListen    string
	OwnerKey        ed25519.PrivateKey
	Collector       CollectorConfig
}

type Agent struct {
	log             *zap.Logger
	service         *gossip.Service
	store           *registry.Store
	ownerKey        ed25519.PrivateKey
	meta            registry.BackendMeta
	verifyListen    string
	metricsInterval time.Duration
	collector       *Collector
	agentCtx        context.Context
	cancel          context.CancelFunc
	metaClock       uint64
}

func New(cfg Config, logger *zap.Logger) (*Agent, error) {
	if logger == nil {
		return nil, errors.New("agent: logger required")
	}
	if cfg.OwnerKey == nil || len(cfg.OwnerKey) != ed25519.PrivateKeySize {
		return nil, errors.New("agent: owner Ed25519 private key required")
	}
	if cfg.Gossip.IdentityKey == nil {
		return nil, errors.New("agent: gossip identity key required")
	}

	interval := cfg.MetricsInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}

	verifyListen := cfg.VerifyListen
	if verifyListen == "" {
		verifyListen = ":8081"
	}

	collector, err := NewCollector(cfg.Collector, logger)
	if err != nil {
		return nil, err
	}

	agentCtx, cancel := context.WithCancel(context.Background())

	store := registry.NewStore(interval * 3)
	service, err := gossip.NewService(agentCtx, cfg.Gossip, store, logger)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("agent: start gossip service: %w", err)
	}

	meta := cfg.Meta
	if meta.ID == "" {
		cancel()
		return nil, errors.New("agent: backend ID required")
	}
	if meta.ChainID == "" {
		cancel()
		return nil, errors.New("agent: chain ID required")
	}
	if meta.Host == "" {
		cancel()
		return nil, errors.New("agent: backend host required")
	}
	if meta.Caps == nil {
		meta.Caps = map[string]bool{}
	}
	if meta.AddedAt == 0 {
		meta.AddedAt = time.Now().Unix()
	}
	if meta.Clock == 0 {
		meta.Clock = 1
	}

	pubKey := cfg.OwnerKey.Public().(ed25519.PublicKey)
	meta.OwnerPK = append([]byte(nil), pubKey...)

	encodedMeta, err := registry.CanonicalBackendMeta(meta)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("agent: encode backend meta: %w", err)
	}
	meta.Sig = ed25519.Sign(cfg.OwnerKey, encodedMeta)

	a := &Agent{
		log:             logger,
		service:         service,
		store:           store,
		ownerKey:        cfg.OwnerKey,
		meta:            meta,
		verifyListen:    verifyListen,
		metricsInterval: interval,
		collector:       collector,
		agentCtx:        agentCtx,
		cancel:          cancel,
		metaClock:       meta.Clock,
	}

	return a, nil
}

func (a *Agent) Run(ctx context.Context) error {
	if err := a.publishMeta(ctx); err != nil {
		a.log.Warn("failed to publish backend meta", zap.Error(err))
	}

	g, gCtx := errgroup.WithContext(ctx)

	if a.verifyListen != "" {
		g.Go(func() error {
			return a.runVerifyServer(gCtx)
		})
	}

	g.Go(func() error {
		return a.runMetricsLoop(gCtx)
	})

	err := g.Wait()
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

func (a *Agent) Close(ctx context.Context) error {
	a.cancel()
	if a.service != nil {
		if err := a.service.Shutdown(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) runVerifyServer(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/.mesh/verify", a.verifyHandler)

	server := &http.Server{
		Addr:              a.verifyListen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		<-errCh
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (a *Agent) runMetricsLoop(ctx context.Context) error {
	ticker := time.NewTicker(a.metricsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			a.collectAndPublish(ctx)
		}
	}
}

func (a *Agent) collectAndPublish(ctx context.Context) {
	result, err := a.collector.Collect(ctx)
	if err != nil {
		a.log.Warn("metrics collection error", zap.Error(err))
	}

	metrics := registry.BackendMetrics{
		ID:         a.meta.ID,
		AtUnix:     time.Now().Unix(),
		Height:     result.Height,
		CatchingUp: result.CatchingUp,
		CPU:        result.CPU,
		P95ms:      result.P95ms,
		RPS:        result.RPS,
		FailRate:   result.FailRate,
		RTTms:      result.RTTms,
		Health:     result.Health,
	}

	encoded, err := registry.CanonicalBackendMetrics(metrics)
	if err != nil {
		a.log.Warn("failed to encode backend metrics", zap.Error(err))
		return
	}
	metrics.Sig = ed25519.Sign(a.ownerKey, encoded)

	if err := a.service.PublishBackendMetrics(ctx, metrics); err != nil {
		a.log.Warn("failed to publish backend metrics", zap.Error(err))
	}
}

func (a *Agent) publishMeta(ctx context.Context) error {
	clock := atomic.LoadUint64(&a.metaClock)
	if clock == 0 {
		clock = atomic.AddUint64(&a.metaClock, 1)
	}
	a.meta.Clock = clock
	return a.service.PublishBackendMeta(ctx, a.meta)
}

func (a *Agent) verifyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	challenge := r.URL.Query().Get("challenge")
	if challenge == "" {
		http.Error(w, "challenge parameter required", http.StatusBadRequest)
		return
	}

	message := buildChallengeMessage(a.meta.ID, challenge)
	sig := ed25519.Sign(a.ownerKey, message)

	response := map[string]string{
		"backend_id": string(a.meta.ID),
		"challenge":  challenge,
		"signature":  encodeBase64(sig),
		"owner_pk":   encodeBase64(a.meta.OwnerPK),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
	}
}

func buildChallengeMessage(id registry.BackendID, challenge string) []byte {
	return []byte(fmt.Sprintf("%s|%s", id, challenge))
}

func encodeBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}
