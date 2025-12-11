package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"go.uber.org/zap"

	"github.com/lunc/mesh/pkg/config"
	"github.com/lunc/mesh/pkg/gossip"
	"github.com/lunc/mesh/pkg/logging"
	"github.com/lunc/mesh/pkg/registry"
)

func main() {
	configPath := flag.String("config", os.Getenv("MESH_CONFIG"), "path to configuration file")
	flag.Parse()

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "meshd: configuration file required")
		os.Exit(1)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "meshd: load config: %v\n", err)
		os.Exit(1)
	}

	logger, err := logging.NewLogger()
	if err != nil {
		fmt.Fprintf(os.Stderr, "meshd: logger init: %v\n", err)
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

	service, err := gossip.NewService(ctx, gossip.Config{
		ListenAddrs: cfg.Mesh.ListenAddrs,
		Bootstrap:   cfg.Mesh.Bootstrap,
		DataDir:     cfg.Mesh.DataDir,
		IdentityKey: privKey,
	}, store, logger)
	if err != nil {
		logger.Fatal("failed to start gossip service", zap.Error(err))
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	logger.Info("meshd started", zap.Strings("listen", cfg.Mesh.ListenAddrs))

	<-sigCh
	logger.Info("shutdown signal received")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := service.Shutdown(shutdownCtx); err != nil {
		logger.Error("gossip shutdown", zap.Error(err))
	}
}

func parseIdentityKey(b64 string) (crypto.PrivKey, error) {
	if b64 == "" {
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
