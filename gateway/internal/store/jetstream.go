// Package store — JetStream stream provisioning.
//
// Gateway startup is the SINGLE SOURCE OF TRUTH for stream provisioning.
// EnsureStreams is called once at boot before the HTTP server starts.
// Failure causes the gateway to exit(1) — never serve without streams.
//
// The P7 mio-jetstream-bootstrap Job is VERIFICATION-ONLY: it asserts
// streams exist with locked config and exits non-zero otherwise. It
// NEVER creates or mutates streams. This prevents two-writer races.
//
// Single-replica constraint: AddOrUpdateStream is idempotent on identical
// config, but two replicas booting with disagreeing config could ping-pong.
// POC runs single replica (maxSurge:0, maxUnavailable:1) until P7 resolves.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

const (
	StreamInbound  = "MESSAGES_INBOUND"
	StreamOutbound = "MESSAGES_OUTBOUND"
)

// EnsureStreams provisions MESSAGES_INBOUND and MESSAGES_OUTBOUND with
// the locked configuration. Idempotent — safe to call on every boot.
func EnsureStreams(ctx context.Context, js jetstream.JetStream, natsReplicas int) error {
	if err := ensureInbound(ctx, js, natsReplicas); err != nil {
		return fmt.Errorf("store: ensure stream MESSAGES_INBOUND: %w", err)
	}
	if err := ensureOutbound(ctx, js, natsReplicas); err != nil {
		return fmt.Errorf("store: ensure stream MESSAGES_OUTBOUND: %w", err)
	}
	return nil
}

func ensureInbound(ctx context.Context, js jetstream.JetStream, replicas int) error {
	cfg := jetstream.StreamConfig{
		Name:        StreamInbound,
		Subjects:    []string{"mio.inbound.>"},
		Retention:   jetstream.LimitsPolicy,
		MaxAge:      7 * 24 * time.Hour, // 7 days; GCS sink archives beyond this
		Storage:     jetstream.FileStorage,
		Replicas:    replicas,
		Duplicates:  2 * time.Minute, // NATS-side dedup window for Nats-Msg-Id
		Description: "Inbound messages from all channel adapters (gateway-authoritative)",
	}
	_, err := js.CreateOrUpdateStream(ctx, cfg)
	return err
}

func ensureOutbound(ctx context.Context, js jetstream.JetStream, replicas int) error {
	cfg := jetstream.StreamConfig{
		Name:        StreamOutbound,
		Subjects:    []string{"mio.outbound.>"},
		Retention:   jetstream.WorkQueuePolicy, // consumed once by sender pool
		MaxAge:      24 * time.Hour,
		Storage:     jetstream.FileStorage,
		Replicas:    replicas,
		Duplicates:  60 * time.Second,
		Description: "Outbound send commands (gateway-authoritative)",
	}
	_, err := js.CreateOrUpdateStream(ctx, cfg)
	return err
}
