package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"go.uber.org/zap"

	"github.com/lunc/mesh/pkg/agent"
	"github.com/lunc/mesh/pkg/config"
	"github.com/lunc/mesh/pkg/gossip"
	"github.com/lunc/mesh/pkg/logging"
	"github.com/lunc/mesh/pkg/registry"
)

func main() {
	configPath := flag.String("config", os.Getenv("MESH_CONFIG"), "path to configuration file")
	flag.Parse()

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "meshagent: configuration file required")
		os.Exit(1)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "meshagent: load config: %v\n", err)
		os.Exit(1)
	}

	logger, err := logging.NewLogger()
	if err != nil {
		fmt.Fprintf(os.Stderr, "meshagent: logger init: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	identityKeyB64 := firstNonEmpty(cfg.Backend.IdentityKey, cfg.Mesh.GossipKey)
	if identityKeyB64 == "" {
		logger.Fatal("missing gossip identity key in configuration")
	}

	identityKey, err := parseIdentityKey(identityKeyB64)
	if err != nil {
		logger.Fatal("invalid gossip identity key", zap.Error(err))
	}

	ownerKey, err := parseOwnerKey(cfg.Backend.OwnerKey)
	if err != nil {
		logger.Fatal("invalid backend owner key", zap.Error(err))
	}

	ips, err := parseIPList(cfg.Backend.IPs)
	if err != nil {
		logger.Fatal("invalid backend IPs", zap.Error(err))
	}

	rpcEndpoint := firstNonEmpty(cfg.Backend.RPCEndpoint, cfg.Backend.Metadata["rpc_endpoint"], cfg.Backend.Host)
	rpcEndpoint = sanitizeEndpoint(rpcEndpoint, "http")
	rpcEndpoint = strings.TrimSuffix(rpcEndpoint, "/")
	rpcEndpoint = strings.TrimSuffix(rpcEndpoint, "/status")

	lcdEndpoint := firstNonEmpty(cfg.Backend.LCDEndpoint, cfg.Backend.Metadata["lcd_endpoint"])
	if lcdEndpoint == "" && cfg.Backend.Host != "" {
		lcdEndpoint = sanitizeEndpoint(cfg.Backend.Host, "http") + "/cosmos/base/tendermint/v1beta1/node_info"
	} else if lcdEndpoint != "" {
		lcdEndpoint = sanitizeEndpoint(lcdEndpoint, "http")
	}

	promEndpoint := firstNonEmpty(cfg.Backend.PromEndpoint, cfg.Backend.Metadata["prom_endpoint"])
	if promEndpoint != "" {
		promEndpoint = sanitizeEndpoint(promEndpoint, "http")
	}

	if cfg.Backend.Caps == nil {
		cfg.Backend.Caps = map[string]bool{}
	}

	meta := registry.BackendMeta{
		ID:       registry.BackendID(cfg.Backend.ID),
		Host:     cfg.Backend.Host,
		IPs:      ips,
		Country:  cfg.Backend.Country,
		Region:   cfg.Backend.Region,
		ChainID:  registry.ChainID(cfg.Backend.ChainID),
		Caps:     cfg.Backend.Caps,
		Archival: cfg.Backend.Archival,
		AddedAt:  time.Now().Unix(),
		Clock:    1,
	}

	listenAddrs := cfg.Mesh.ListenAddrs
	if len(listenAddrs) == 0 {
		listenAddrs = []string{"/ip4/0.0.0.0/tcp/0"}
	}

	gossipCfg := gossip.Config{
		ListenAddrs:   listenAddrs,
		Bootstrap:     cfg.Mesh.Bootstrap,
		DataDir:       cfg.Mesh.DataDir,
		IdentityKey:   identityKey,
		SyncInterval:  cfg.Mesh.AntiEntropyEvery,
		PruneInterval: cfg.Mesh.AntiEntropyEvery * 3,
	}

	collectorCfg := agent.CollectorConfig{
		ChainID:      meta.ChainID,
		RPCEndpoint:  rpcEndpoint,
		LCDEndpoint:  lcdEndpoint,
		PromEndpoint: promEndpoint,
		Timeout:      cfg.Probe.Timeout,
	}

	ag, err := agent.New(agent.Config{
		Gossip:          gossipCfg,
		Meta:            meta,
		MetricsInterval: cfg.Probe.Interval,
		VerifyListen:    cfg.Backend.VerifyListen,
		OwnerKey:        ownerKey,
		Collector:       collectorCfg,
	}, logger)
	if err != nil {
		logger.Fatal("failed to start mesh agent", zap.Error(err))
	}
	defer ag.Close(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		logger.Info("shutdown signal received")
		cancel()
	}()

	logger.Info("meshagent started",
		zap.String("backend_id", string(meta.ID)),
		zap.String("chain_id", string(meta.ChainID)),
		zap.String("verify_listen", firstNonEmpty(cfg.Backend.VerifyListen, ":8081")),
	)

	if err := ag.Run(ctx); err != nil {
		logger.Error("meshagent run error", zap.Error(err))
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := ag.Close(shutdownCtx); err != nil {
		logger.Error("meshagent shutdown error", zap.Error(err))
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

func parseOwnerKey(b64 string) (ed25519.PrivateKey, error) {
	if strings.TrimSpace(b64) == "" {
		return nil, errors.New("owner key missing")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("decode owner key: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("owner key length %d, expected %d", len(raw), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(raw), nil
}

func parseIPList(items []string) ([]net.IP, error) {
	if len(items) == 0 {
		return nil, nil
	}
	ips := make([]net.IP, 0, len(items))
	for _, item := range items {
		ip := net.ParseIP(strings.TrimSpace(item))
		if ip == nil {
			return nil, fmt.Errorf("invalid IP address %q", item)
		}
		ips = append(ips, ip)
	}
	return ips, nil
}

func sanitizeEndpoint(endpoint, defaultScheme string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return endpoint
	}
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		return endpoint
	}
	if strings.Contains(endpoint, "://") {
		return endpoint
	}
	return defaultScheme + "://" + endpoint
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
