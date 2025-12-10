package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/miekg/dns"
	"go.uber.org/zap"

	"github.com/lunc/mesh/pkg/config"
	"github.com/lunc/mesh/pkg/dnsplugin"
	"github.com/lunc/mesh/pkg/gossip"
	"github.com/lunc/mesh/pkg/logging"
	"github.com/lunc/mesh/pkg/registry"
)

func main() {
	configPath := flag.String("config", os.Getenv("MESH_CONFIG"), "path to configuration file")
	flag.Parse()

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "meshdns: configuration file required")
		os.Exit(1)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "meshdns: load config: %v\n", err)
		os.Exit(1)
	}

	logger, err := logging.NewLogger()
	if err != nil {
		fmt.Fprintf(os.Stderr, "meshdns: logger init: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	zone := strings.TrimSpace(cfg.DNS.Zone)
	if zone == "" {
		logger.Fatal("dns zone must be configured")
	}

	identityKeyB64 := firstNonEmpty(cfg.Mesh.GossipKey)
	if identityKeyB64 == "" {
		logger.Fatal("gossip identity key required")
	}

	identityKey, err := parseIdentityKey(identityKeyB64)
	if err != nil {
		logger.Fatal("invalid gossip identity key", zap.Error(err))
	}

	listenAddrs := cfg.Mesh.ListenAddrs
	if len(listenAddrs) == 0 {
		listenAddrs = []string{"/ip4/0.0.0.0/tcp/0"}
	}

	store := registry.NewStore(30 * time.Second)

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gossipCfg := gossip.Config{
		ListenAddrs:   listenAddrs,
		Bootstrap:     cfg.Mesh.Bootstrap,
		DataDir:       cfg.Mesh.DataDir,
		IdentityKey:   identityKey,
		SyncInterval:  cfg.Mesh.AntiEntropyEvery,
		PruneInterval: cfg.Mesh.AntiEntropyEvery * 3,
	}

	gossipLog := logger.Named("gossip")
	gossipSvc, err := gossip.NewService(rootCtx, gossipCfg, store, gossipLog)
	if err != nil {
		logger.Fatal("failed to start gossip", zap.Error(err))
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := gossipSvc.Shutdown(shutdownCtx); err != nil {
			logger.Warn("gossip shutdown", zap.Error(err))
		}
	}()

	zoneView, err := dnsplugin.NewRegistryZoneView(dnsplugin.RegistryZoneViewConfig{
		Store:      store,
		Zone:       zone,
		NSLabels:   cfg.DNS.NSLabels,
		TTLNSA:     uint32(cfg.DNS.TTLNSA),
		TTLService: uint32(cfg.DNS.TTLService),
		Logger:     logger.Named("zone"),
	})
	if err != nil {
		logger.Fatal("zone view", zap.Error(err))
	}

	chains := make([]registry.ChainID, 0, len(cfg.DNS.Chains))
	for _, chain := range cfg.DNS.Chains {
		chain = strings.TrimSpace(chain)
		if chain == "" {
			continue
		}
		chains = append(chains, registry.ChainID(chain))
	}

	handlerCfg := dnsplugin.Config{
		Zone:         zone,
		Chains:       chains,
		NSLabels:     cfg.DNS.NSLabels,
		TTLService:   uint32(cfg.DNS.TTLService),
		TTLNSA:       uint32(cfg.DNS.TTLNSA),
		MaxAnswers:   cfg.DNS.MaxAnswers,
		EnableECS:    cfg.DNS.EnableECS,
		RRLRate:      cfg.DNS.RRLQPS,
		RRLBurst:     cfg.DNS.RRLBurst,
		RRLWindow:    cfg.DNS.RRLWindow,
		Logger:       logger.Named("dns"),
		ZoneProvider: zoneView,
	}

	handler, err := dnsplugin.NewHandler(handlerCfg)
	if err != nil {
		logger.Fatal("handler", zap.Error(err))
	}

	queryLog := logger.Named("query")
	dnsHandler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		ctx, cancelReq := context.WithTimeout(rootCtx, 2*time.Second)
		defer cancelReq()
		if _, err := handler.ServeDNS(ctx, w, r); err != nil && !errors.Is(err, context.Canceled) {
			queryLog.Debug("serve dns", zap.Error(err))
		}
	})

	udpServer := &dns.Server{Addr: cfg.DNS.Listen, Net: "udp", Handler: dnsHandler}
	tcpServer := &dns.Server{Addr: cfg.DNS.Listen, Net: "tcp", Handler: dnsHandler}

	errCh := make(chan error, 2)
	go func() { errCh <- udpServer.ListenAndServe() }()
	go func() { errCh <- tcpServer.ListenAndServe() }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	logger.Info("meshdns started",
		zap.String("listen", cfg.DNS.Listen),
		zap.String("zone", zone),
		zap.Int("chains", len(chains)),
		zap.Bool("ecs", cfg.DNS.EnableECS),
		zap.Float64("rrl_qps", cfg.DNS.RRLQPS),
	)

	var serveErr error
	select {
	case serveErr = <-errCh:
	case sig := <-sigCh:
		logger.Info("shutdown signal", zap.String("signal", sig.String()))
	}

	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := udpServer.ShutdownContext(shutdownCtx); err != nil {
		logger.Warn("udp shutdown", zap.Error(err))
	}
	if err := tcpServer.ShutdownContext(shutdownCtx); err != nil {
		logger.Warn("tcp shutdown", zap.Error(err))
	}

	if serveErr != nil {
		logger.Error("dns server stopped", zap.Error(serveErr))
	}
}

func parseIdentityKey(b64 string) (crypto.PrivKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("decode identity key: %w", err)
	}
	key, err := crypto.UnmarshalEd25519PrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("unmarshal identity key: %w", err)
	}
	return key, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
