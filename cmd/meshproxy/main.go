package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"go.uber.org/zap"

	"github.com/lunc/mesh/pkg/config"
	"github.com/lunc/mesh/pkg/gossip"
	"github.com/lunc/mesh/pkg/logging"
	"github.com/lunc/mesh/pkg/proxy"
	"github.com/lunc/mesh/pkg/registry"
	"github.com/lunc/mesh/pkg/selector"
)

func main() {
	configPath := flag.String("config", os.Getenv("MESH_CONFIG"), "path to configuration file")
	flag.Parse()

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "meshproxy: configuration file required")
		os.Exit(1)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "meshproxy: load config: %v\n", err)
		os.Exit(1)
	}

	logger, err := logging.NewLogger()
	if err != nil {
		fmt.Fprintf(os.Stderr, "meshproxy: logger init: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	privKey, err := parseIdentityKey(cfg.Mesh.GossipKey)
	if err != nil {
		logger.Fatal("invalid identity key", zap.Error(err))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := registry.NewStore(10 * time.Second)

	gossipSvc, err := gossip.NewService(ctx, gossip.Config{
		ListenAddrs:  cfg.Mesh.ListenAddrs,
		Bootstrap:    cfg.Mesh.Bootstrap,
		DataDir:      cfg.Mesh.DataDir,
		IdentityKey:  privKey,
		SyncInterval: cfg.Mesh.AntiEntropyEvery,
	}, store, logger)
	if err != nil {
		logger.Fatal("failed to start gossip service", zap.Error(err))
	}

	proxyCfg, err := buildProxyConfig(cfg, logger, store)
	if err != nil {
		logger.Fatal("invalid proxy configuration", zap.Error(err))
	}

	handler, err := proxy.New(proxyCfg)
	if err != nil {
		logger.Fatal("failed to construct proxy", zap.Error(err))
	}

	listenHTTP := cfg.Edge.ListenHTTP
	if listenHTTP == "" {
		listenHTTP = ":8080"
	}

	srv := &http.Server{
		Addr:              listenHTTP,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("meshproxy listening", zap.String("addr", listenHTTP))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	advertiser := newEdgeAdvertiser(cfg.Edge, privKey, gossipSvc, logger)
	go advertiser.Run(ctx)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sigCh:
		logger.Info("shutdown signal received")
	case err := <-serverErr:
		if err != nil {
			logger.Error("http server error", zap.Error(err))
		}
	}

	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown", zap.Error(err))
	}
	if err := gossipSvc.Shutdown(shutdownCtx); err != nil {
		logger.Warn("gossip shutdown", zap.Error(err))
	}

	logger.Info("meshproxy exited")
}

func parseIdentityKey(b64 string) (crypto.PrivKey, error) {
	if strings.TrimSpace(b64) == "" {
		return nil, fmt.Errorf("missing identity key")
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decode identity key: %w", err)
	}
	key, err := crypto.UnmarshalPrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("unmarshal identity key: %w", err)
	}
	return key, nil
}

func buildProxyConfig(cfg config.Config, logger *zap.Logger, store *registry.Store) (proxy.Config, error) {
	chains := make([]registry.ChainID, 0, len(cfg.Edge.Chains))
	for _, chain := range cfg.Edge.Chains {
		chains = append(chains, registry.ChainID(chain))
	}

	ttl, infinite, err := parseCacheTTL(cfg.Cache.TTLHeighted)
	if err != nil {
		return proxy.Config{}, err
	}

	backendScheme := cfg.Edge.BackendScheme
	if backendScheme == "" {
		backendScheme = "https"
	}

	return proxy.Config{
		Store:             store,
		Chains:            chains,
		Selector:          buildSelectorConfig(cfg),
		CacheEnabled:      cfg.Cache.Enabled,
		CacheMaxBytes:     int64(cfg.Cache.MaxBytes),
		CacheMaxItemBytes: int64(cfg.Cache.MaxItemBytes),
		CacheTTLHeighted:  ttl,
		CacheTTLInfinite:  infinite,
		NegativeCacheTTL:  5 * time.Second,
		RateLimit: proxy.RateLimitConfig{
			PerIPRPS: cfg.Rate.PerIPRPS,
			Burst:    cfg.Rate.Burst,
			Window:   10 * time.Minute,
		},
		EdgeCountry:   cfg.Edge.Country,
		EdgeRegion:    cfg.Edge.Region,
		BackendScheme: backendScheme,
		Logger:        logger,
	}, nil
}

func buildSelectorConfig(cfg config.Config) selector.Config {
	sel := selector.DefaultConfig()
	if cfg.Selector.DeltaHeight > 0 {
		sel.HeightDelta = cfg.Selector.DeltaHeight
	}
	if cfg.Selector.SoftmaxT > 0 {
		sel.SoftmaxTemperature = cfg.Selector.SoftmaxT
	}
	return sel
}

func parseCacheTTL(raw string) (time.Duration, bool, error) {
	value := strings.TrimSpace(strings.ToLower(raw))
	if value == "" || value == "infinite" || value == "forever" || value == "none" {
		return 0, true, nil
	}
	ttl, err := time.ParseDuration(raw)
	if err != nil {
		return 0, false, fmt.Errorf("cache ttl: %w", err)
	}
	return ttl, false, nil
}

type edgeAdvertiser struct {
	cfg      config.EdgeConfig
	key      crypto.PrivKey
	service  *gossip.Service
	logger   *zap.Logger
	sequence atomic.Uint64
}

func newEdgeAdvertiser(cfg config.EdgeConfig, key crypto.PrivKey, service *gossip.Service, logger *zap.Logger) *edgeAdvertiser {
	adv := &edgeAdvertiser{
		cfg:     cfg,
		key:     key,
		service: service,
		logger:  logger,
	}
	if cfg.ID == "" {
		adv.logger.Warn("edge id missing; node advert publishing disabled")
	}
	return adv
}

func (a *edgeAdvertiser) Run(ctx context.Context) {
	if a.cfg.ID == "" {
		return
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	a.publish(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.publish(ctx)
		}
	}
}

func (a *edgeAdvertiser) publish(ctx context.Context) {
	ips := make([]net.IP, 0, len(a.cfg.AdvertiseIPs))
	for _, raw := range a.cfg.AdvertiseIPs {
		ip := net.ParseIP(strings.TrimSpace(raw))
		if ip == nil {
			a.logger.Warn("skipping invalid advertise ip", zap.String("ip", raw))
			continue
		}
		ips = append(ips, ip)
	}
	if len(ips) == 0 {
		a.logger.Warn("no advertise IPs configured; node advert skipped")
		return
	}

	advert := registry.NodeAdvert{
		ID:        registry.NodeID(a.cfg.ID),
		NSServing: a.cfg.NSServing,
		IPs:       ips,
		Country:   a.cfg.Country,
		Region:    a.cfg.Region,
		AtUnix:    time.Now().Unix(),
		Clock:     a.sequence.Add(1),
	}

	pubKey, err := a.key.GetPublic().Raw()
	if err != nil {
		a.logger.Warn("extract node advert public key", zap.Error(err))
		return
	}
	advert.PubKey = append([]byte(nil), pubKey...)

	payload, err := registry.CanonicalNodeAdvert(advert)
	if err != nil {
		a.logger.Warn("encode node advert", zap.Error(err))
		return
	}
	sig, err := a.key.Sign(payload)
	if err != nil {
		a.logger.Warn("sign node advert", zap.Error(err))
		return
	}
	advert.Sig = sig

	if err := a.service.PublishNodeAdvert(ctx, advert); err != nil {
		a.logger.Warn("publish node advert", zap.Error(err))
	}
}
