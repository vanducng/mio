---
phase: 4
title: "wire-cliq-fetch-and-publish"
status: completed
priority: P1
effort: "4h"
depends_on: [3]
---

# Phase 4: Wire Cliq attachment fetch + storage write + enriched republish

## Overview

Replace phase-03's stub `processMessage` with the real flow: fetch
attachment bytes from the platform (Cliq adapter), stream to storage,
rewrite `Attachment` fields, publish the enriched Message to a downstream
stream that AI consumers will read.

## Files
- **Modify:** `attachment-downloader/internal/worker/worker.go` (real `processMessage`)
- **Create:** `attachment-downloader/internal/fetcher/fetcher.go` (channel-agnostic interface)
- **Create:** `attachment-downloader/internal/fetcher/zohocliq/cliq.go` (Cliq impl)
- **Create:** `attachment-downloader/internal/fetcher/zohocliq/cliq_test.go`
- **Create:** `attachment-downloader/internal/keygen/keygen.go` (deterministic object-key builder)
- **Create:** `attachment-downloader/internal/keygen/keygen_test.go`
- **Create:** `attachment-downloader/internal/publisher/publisher.go` (enriched-stream writer)
- **Modify:** `attachment-downloader/internal/config/config.go` (add `MIO_ENRICHED_STREAM`, `MIO_CLIQ_BOT_TOKEN`)

## Steps

### 4.1 Fetcher interface

`internal/fetcher/fetcher.go`:

```go
// Fetcher streams attachment bytes from a channel platform.
// Implementations live under sub-packages (zohocliq, slack, ...).
type Fetcher interface {
    // ChannelType returns the registry slug ("zoho_cliq").
    ChannelType() string

    // Fetch streams bytes for the attachment to dst. Honours ctx deadline.
    // Returns FetchError with .Code populated when the platform-side state
    // is the cause (expired, forbidden, not found, too large).
    Fetch(ctx context.Context, att *miov1.Attachment, dst io.Writer) (FetchResult, error)
}

type FetchResult struct {
    Bytes       int64
    SHA256Hex   string  // computed during streaming
    ContentType string  // from response Content-Type
}

// FetchError wraps platform errors with a bounded ErrorCode the worker maps
// to Attachment.error_code.
type FetchError struct {
    Code miov1.Attachment_ErrorCode
    Msg  string
}
```

Registry: `var fetchers = map[string]Fetcher{}` populated via `init()` in
each adapter package, blank-imported in `cmd/attachment-downloader/main.go`.

### 4.2 Cliq fetcher

`internal/fetcher/zohocliq/cliq.go`:

- `ChannelType()` returns `"zoho_cliq"`.
- `Fetch`:
  - URL is `att.GetUrl()`. Reject if empty or scheme not https.
  - GET with `Authorization: Zoho-oauthtoken <token>` (token from env `MIO_CLIQ_BOT_TOKEN` or refresh token mint — start with static, mirror gateway's choice).
  - Set `User-Agent: mio-attachment-downloader/<version>`.
  - 60s deadline (overridable via ctx).
  - Stream `resp.Body` through a `sha256.New()` `io.MultiWriter` while copying to `dst`.
  - HTTP status mapping:
    - 200/206 → success
    - 400 with body containing `attachment_access_time_expired` → `FetchError{Code: ATTACHMENT_ERROR_EXPIRED}`
    - 401/403 → `FetchError{Code: ATTACHMENT_ERROR_FORBIDDEN}`
    - 404 → `FetchError{Code: ATTACHMENT_ERROR_NOT_FOUND}`
    - 413 / `Content-Length > MIO_DOWNLOAD_MAX_BYTES` → `FetchError{Code: ATTACHMENT_ERROR_TOO_LARGE}`
    - 5xx / network → return raw error → worker Naks (retryable)
  - Cap bytes via `io.LimitReader(resp.Body, MIO_DOWNLOAD_MAX_BYTES+1)` to detect oversize after-the-fact.

### 4.3 Object-key generator

`internal/keygen/keygen.go`:

```go
// Build returns the canonical content-addressable key:
//   {prefix}/{channel_type}/yyyy=YYYY/mm=MM/dd=DD/{sha256[:2]}/{sha256}{ext}
// where ext is derived from ContentType (image/jpeg → .jpg) or filename.
// Date partitioning is by inbound received_at, not "now" — preserves
// chronological cleanup semantics.
func Build(prefix, channelType, sha256hex, contentType, filename string, receivedAt time.Time) string
```

Pure function, table-tested. Path partitioning enables both prefix-delete
(GDPR by date) and content-hash dedup (same image stored once globally).

### 4.4 processMessage real flow

`internal/worker/worker.go` — `processMessage`:

```
for i, att := range msg.Attachments:
  if att.GetStorageKey() != "":          // already enriched
      continue
  fetcher := registry[msg.GetChannelType()]
  if fetcher == nil:
      mark att with ATTACHMENT_ERROR_FORBIDDEN, structured-log "no fetcher for channel"
      continue

  // Stream into a tee: hash + size + storage write happens in dedup.EnsurePersisted
  // which calls Fetcher.Fetch under the hood.
  pr, pw := io.Pipe()
  errCh := make(chan error, 1)
  resCh := make(chan fetcher.FetchResult, 1)
  go func() {
      defer pw.Close()
      r, e := fetcher.Fetch(ctx, att, pw)
      resCh <- r; errCh <- e
  }()

  // Provisional key (channel + date partition) — sha256 is filled below
  // *after* fetch completes. We compute key from the fetched sha, not
  // pre-computed (we have no sha until we've read the bytes).

  // Buffered approach for POC: io.Copy into a tempfile or limited memory
  // buffer (≤ MIO_DOWNLOAD_MAX_BYTES) so we can compute sha first, then
  // Put with the final key. For >5 MB, switch to streaming-multipart with
  // a deferred-rename-on-finish (S3 put-then-rename semantics; GCS is fine
  // with single-shot streaming).
  bufferedBody, sha, ct, size := readToTemp(pr, MIO_DOWNLOAD_MAX_BYTES)

  key := keygen.Build(prefix, msg.GetChannelType(), sha, ct, att.GetFilename(), msg.GetReceivedAt().AsTime())

  if existing, _ := storage.Stat(ctx, key); existing != nil:
      // dedup hit
      metrics.DedupHits.Inc()
  else:
      err = storage.Put(ctx, key, bufferedBody, size, PutOptions{
          ContentType: ct, SHA256Hex: sha, IfNotExists: true,
      })
      if errors.Is(err, ErrAlreadyExists):
          // race resolved cleanly
          metrics.DedupHits.Inc()
      else if err != nil:
          mark att with ATTACHMENT_ERROR_STORAGE; return retryable err

  signed, err := storage.SignedURL(ctx, key, SignOptions{TTL: 1*time.Hour})
  att.StorageKey = key
  att.ContentSha256 = sha
  att.Bytes = size
  att.Mime = ct
  att.Url = signed                 // overwrite platform URL
  att.ErrorCode = ATTACHMENT_OK
```

After the loop, regardless of per-attachment outcome, **publish to the
enriched stream** (so AI consumer sees every Message even with partial
failures):

```
publisher.Publish(ctx, msg)  // proto-marshal + nc.PublishMsg with
                             // Nats-Msg-Id = "enr:<msg.id>"
```

### 4.5 Enriched stream + publisher

`internal/publisher/publisher.go`:

- Subject: `mio.inbound_enriched.{channel_type}.{account_id}.{conversation_id}` (mirrors inbound shape).
- Stream: `MESSAGES_INBOUND_ENRICHED`, `Retention: Limits`, `MaxAge: 7d`, `Replicas: 1` for POC. Same storage class as inbound.
- Bootstrap: sidecar's `main.go` calls `nc.JetStream().CreateOrUpdateStream(...)` on startup (idempotent), mirroring gateway's `AddOrUpdateStream`.
- Publish with `Nats-Msg-Id: "enr:<msg.id>"` so re-deliveries dedup at the stream's `DuplicateWindow` (2m).

### 4.6 Worker integration

After phase-04, the worker's loop on success: ack inbound msg only **after**
publish to enriched stream succeeds. This ordering matters: if publish
fails, Nak the inbound to redrive. Storage write is idempotent so retry is
safe.

## Tests
- [ ] Unit: Cliq fetcher 200 path → FetchResult with sha + size matches
- [ ] Unit: Cliq fetcher 400 expired → FetchError{ATTACHMENT_ERROR_EXPIRED}
- [ ] Unit: Cliq fetcher 401 → ATTACHMENT_ERROR_FORBIDDEN
- [ ] Unit: Cliq fetcher 5xx → returns plain error (worker Naks)
- [ ] Unit: keygen idempotent + path-shaped correctly + unicode filename safe
- [ ] Integration (testcontainers/embedded NATS + GCS fake):
  - Publish a real-shape `Message` with one attachment to MESSAGES_INBOUND
  - Sidecar processes it; assert object exists at expected key + enriched stream has 1 msg with rewritten URL
  - Republish same Message → second object NOT created (dedup), enriched stream still emits

## Success Criteria
- [ ] End-to-end inside the dev compose: send a webhook with an image, sidecar logs "downloaded", object lands in GCS fake, enriched stream has the rewritten URL
- [ ] `att.ErrorCode = ATTACHMENT_ERROR_EXPIRED` is emitted (not crashed) when the URL is already dead — verified with a deliberately expired test URL
- [ ] No memory blowup on a 25 MB file — RSS stays under ~80 MB

## Risks
- **Buffered-to-tempfile vs pure-streaming** — for POC, ≤25 MB cap means in-memory buffer is fine; `tempfile-fallback for >5 MB` is a P10 enhancement
- **Cliq token rotation** — phase-04 reads `CLIQ_BOT_TOKEN` from env; relies on the gateway's existing rotation pattern. Refresh-token flow inside sidecar is out of scope here
- **Race on enriched-stream creation** — if sidecar and a future second-replica try `AddOrUpdateStream` concurrently, NATS handles it; ours is single-replica POC anyway
- **Forgot to set ATTACHMENT_OK on success** — code path coverage required; integration test asserts this
