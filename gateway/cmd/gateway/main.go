// Command gateway is the mio-gateway HTTP server.
// It receives channel webhooks, normalises payloads to mio.v1.Message,
// and publishes to MESSAGES_INBOUND via NATS JetStream.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	sdk "github.com/vanducng/mio/sdk-go"
	"github.com/vanducng/mio/gateway/internal/config"
	"github.com/vanducng/mio/gateway/internal/server"
	"github.com/vanducng/mio/gateway/internal/store"
)

// version is injected at build time via -ldflags="-X main.version=<ver>".
var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("gateway: fatal", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	logger.Info("gateway starting", "version", version)

	// ── 1. Config ──────────────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// ── 2. Postgres pool ───────────────────────────────────────────────────
	ctx := context.Background()
	pg, err := store.NewPool(ctx, cfg.PostgresDSN, int32(cfg.PgxMaxConns))
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pg.Close()
	logger.Info("postgres: pool ready", "max_conns", cfg.PgxMaxConns)

	// ── 3. Migrations ──────────────────────────────────────────────────────
	if cfg.MigrateOnStart {
		logger.Info("postgres: running migrations")
		if err := store.MigrateUp(cfg.PostgresDSN); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		logger.Info("postgres: migrations complete")
	}

	// ── 4. NATS connection ─────────────────────────────────────────────────
	natsURL := cfg.NatsURLs[0]
	if len(cfg.NatsURLs) > 1 {
		// nats.Connect accepts comma-joined URLs for cluster failover.
		natsURL = ""
		for i, u := range cfg.NatsURLs {
			if i > 0 {
				natsURL += ","
			}
			natsURL += u
		}
	}
	nc, err := nats.Connect(natsURL,
		nats.Name("mio-gateway/"+version),
		nats.MaxReconnects(-1), // reconnect forever
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return fmt.Errorf("nats: connect: %w", err)
	}
	defer nc.Drain() //nolint:errcheck
	logger.Info("nats: connected", "url", natsURL)

	// ── 5. JetStream stream provisioning (gateway-authoritative) ──────────
	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("nats: jetstream: %w", err)
	}
	natsReplicas := 1 // 1 locally; P7 bumps to 3 in cluster
	if err := store.EnsureStreams(ctx, js, natsReplicas); err != nil {
		return fmt.Errorf("jetstream: ensure streams: %w", err)
	}
	logger.Info("jetstream: streams provisioned",
		"inbound", store.StreamInbound,
		"outbound", store.StreamOutbound)

	// ── 6. SDK client ──────────────────────────────────────────────────────
	sdkReg := prometheus.NewRegistry()
	sdkClient, err := sdk.New(natsURL,
		sdk.WithName("mio-gateway/sdk/"+version),
		sdk.WithMetricsRegistry(sdkReg),
	)
	if err != nil {
		return fmt.Errorf("sdk: %w", err)
	}
	defer sdkClient.Close()

	// ── 7. HTTP server ─────────────────────────────────────────────────────
	serverCfg := server.Config{
		TenantID:          cfg.TenantID,
		AccountID:         cfg.AccountID,
		CliqWebhookSecret: []byte(cfg.CliqWebhookSecret),
		Logger:            logger,
	}
	handler := server.New(pg, nc, sdkClient, serverCfg, prometheus.DefaultRegisterer)

	addr := fmt.Sprintf(":%d", cfg.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// ── 8. Graceful shutdown ───────────────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	go func() {
		logger.Info("gateway: listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("gateway: server error", "err", err)
			os.Exit(1)
		}
	}()

	<-sigCh
	logger.Info("gateway: shutting down",
		"grace_sec", cfg.GracefulShutdownSec)

	shutCtx, cancel := context.WithTimeout(context.Background(),
		time.Duration(cfg.GracefulShutdownSec)*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	logger.Info("gateway: shutdown complete")
	return nil
}
