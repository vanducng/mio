---
phase: 3
title: "scaffold-sidecar-binary"
status: completed
priority: P1
effort: "4h"
depends_on: [1, 2]
---

# Phase 3: Scaffold the attachment-downloader binary + JetStream wiring

## Overview

Stand up the binary skeleton: NATS connect, JetStream durable consumer on
`MESSAGES_INBOUND`, message-loop with ack/nak/term semantics, signal-aware
graceful shutdown, Prometheus metrics, structured logs. No real download
logic yet — Step C smoke is "binary boots, attaches a consumer, ack-passes
messages without modification."

## Files
- **Create:** `attachment-downloader/cmd/attachment-downloader/main.go`
- **Create:** `attachment-downloader/internal/config/config.go`
- **Create:** `attachment-downloader/internal/worker/worker.go` (consume loop)
- **Create:** `attachment-downloader/internal/worker/worker_test.go`
- **Create:** `attachment-downloader/internal/metrics/metrics.go`
- **Create:** `attachment-downloader/internal/dedup/dedup.go` (HEAD-before-PUT helper)
- **Create:** `attachment-downloader/internal/dedup/dedup_test.go`
- **Create:** `attachment-downloader/Dockerfile` (multi-stage, distroless or scratch)

## Steps

### 3.1 Config

`internal/config/config.go` — env-driven, mirrors `gateway/internal/config/config.go`:

| Env | Required | Default | Purpose |
|---|---|---|---|
| `MIO_NATS_URLS` | y | `nats://localhost:4222` | NATS endpoint |
| `MIO_TENANT_ID` | y | — | UUID |
| `MIO_ACCOUNT_ID` | y | — | UUID |
| `MIO_STORAGE_BACKEND` | n | `gcs` | dispatch key for factory |
| `MIO_STORAGE_BUCKET` | y | — | bucket name |
| `MIO_STORAGE_PREFIX` | n | `mio/attachments/` | key prefix |
| `MIO_DOWNLOAD_TIMEOUT_SECONDS` | n | `60` | per-attachment download deadline |
| `MIO_DOWNLOAD_MAX_BYTES` | n | `26214400` (25 MB) | reject larger up front |
| `MIO_SIGNED_URL_TTL_SECONDS` | n | `3600` | 1h default |
| `MIO_DURABLE_NAME` | n | `attachment-downloader` | JS durable name |
| `MIO_METRICS_PORT` | n | `9090` | Prom scrape |
| `MIO_LOG_LEVEL` | n | `info` | slog level |

Validate required fields up front; fail-fast.

### 3.2 Worker loop

`internal/worker/worker.go`:

- Create JetStream durable pull consumer named `attachment-downloader` on `MESSAGES_INBOUND`, `DeliverPolicy: All`, `MaxDeliver: 5`, `AckWait: 90s` (covers 60s download + headroom), `MaxAckPending: 8` (parallel inflight cap).
- `Fetch(8)` loop with 5s timeout (matches sdk-py pattern).
- For each msg:
  1. Unmarshal `*miov1.Message`.
  2. If `len(msg.Attachments) == 0` → ack (nothing to do).
  3. If every attachment has `StorageKey != ""` → ack (already enriched; fast path for replay).
  4. Otherwise dispatch to `processMessage(ctx, msg)` (stub returning nil for now; phase-04 fills it).
  5. On nil error → `Ack`. On retryable error → `Nak(jitter())`. On terminal → `Term` + log + metric.
- Concurrency: at most `MaxAckPending` workers (sized to download bandwidth, not CPU).

### 3.3 Dedup helper

`internal/dedup/dedup.go`:

```go
// EnsurePersisted is HEAD-before-PUT: returns existing object key if already
// stored, otherwise calls writeFn to stream bytes and returns the new key.
// Backend's IfNotExists makes the PUT race-free across concurrent callers.
func EnsurePersisted(
    ctx context.Context,
    s storage.Storage,
    key string,
    writeFn func(io.Writer) (sha256hex string, size int64, err error),
) (sha256hex string, alreadyExisted bool, err error)
```

- `Stat(key)` → if found and metadata sha256 matches expected (caller passes
  pre-computed when known), return existing.
- Else stream: pipe writer → `Put(ctx, key, pipe.Reader, ...)` running in
  goroutine; caller's `writeFn` copies bytes through pipe with hashing.
- On `ErrAlreadyExists` from `IfNotExists` → fetch the existing object's
  sha256 from metadata for downstream.

### 3.4 Metrics

`internal/metrics/metrics.go`:

- `mio_attachment_downloaded_total{channel_type, outcome}` — counter (success/expired/forbidden/timeout/storage_error)
- `mio_attachment_bytes_total{channel_type}` — counter (bytes persisted)
- `mio_attachment_dedup_hits_total{channel_type}` — counter (HEAD said exists)
- `mio_attachment_download_duration_seconds{channel_type, outcome}` — histogram, buckets [0.1, 0.5, 1, 2, 5, 10, 30, 60]
- `mio_attachment_storage_duration_seconds{backend, op}` — histogram for Put/Get/SignedURL
- `mio_attachment_inflight` — gauge

Expose on `MIO_METRICS_PORT` via `promhttp.Handler()`.

### 3.5 Main

`cmd/attachment-downloader/main.go`:

1. `signal.NotifyContext(ctx, SIGINT, SIGTERM)` — graceful shutdown.
2. Load config; init logger; init storage via factory.
3. Connect NATS; create JS context; ensure durable consumer exists (idempotent `CreateOrUpdate`).
4. Start metrics server (separate goroutine).
5. Run worker.Loop until ctx cancels; wait for inflight Acks (max 30s).
6. `nc.Drain()`; exit.

### 3.6 Dockerfile

Multi-stage:
- builder: `golang:1.25-alpine`, `go build -trimpath -ldflags="-s -w" -o /out/attachment-downloader ./cmd/attachment-downloader`
- runtime: `gcr.io/distroless/static-debian12:nonroot`, `COPY --from=builder /out/attachment-downloader /attachment-downloader`
- `USER 65532`, `ENTRYPOINT ["/attachment-downloader"]`

Build context = repo root (for cross-module imports via `go.work`). Dockerfile uses `GOWORK=off` so it pins via the module's `replace` directives like sink-gcs does. Mirror the working sink-gcs Dockerfile pattern for COPYs.

## Tests
- [ ] `go build ./...` clean
- [ ] `go test ./...` clean (worker_test mocks JetStream via `nats-server` testserver pkg)
- [ ] Unit test: stub Storage; publish a Message with one attachment; assert `processMessage` is called once and returns success
- [ ] Unit test: replay an already-enriched Message → fast-path ack, no Storage calls
- [ ] dedup_test.go: HEAD-found short-circuits Put; HEAD-miss + IfNotExists race resolves to one stored object
- [ ] `docker build -f attachment-downloader/Dockerfile .` from repo root succeeds; image <40 MB

## Success Criteria
- [ ] Binary runs locally: `MIO_STORAGE_BUCKET=test ... ./attachment-downloader` connects to local NATS, creates durable consumer, ack-passes messages, exposes /metrics
- [ ] `kubectl apply` of a one-off Pod manifest runs the binary against in-cluster NATS without crashing (smoke deploy; full chart in phase-05)
- [ ] No real attachment fetch yet — the stub in `processMessage` returns nil and acks; phase-04 fills it

## Risks
- **JetStream consumer name collision** if a previous `attachment-downloader` durable exists with mismatched filter — use `MIO_DURABLE_NAME` to override per env
- **Drainage on SIGTERM** with inflight downloads — give pod 60s `terminationGracePeriodSeconds`; worker waits up to 30s for Acks before NATS drain
- **Multipart upload threshold** — defer to phase-04; phase-03 uses single-shot Put which the GCS impl already streams
