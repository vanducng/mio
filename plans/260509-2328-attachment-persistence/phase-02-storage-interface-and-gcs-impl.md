---
phase: 2
title: "storage-interface-and-gcs-impl"
status: completed
priority: P1
effort: "4h"
depends_on: []
---

# Phase 2: Storage interface + GCS implementation

## Overview

Define the `Storage` interface that backends implement, and ship a GCS
implementation. This is the architectural seam — once frozen, swapping
backends (S3, R2, B2) is one new file, no callers touched.

## Files
- **Create:** `attachment-downloader/go.mod` (new module under repo root)
- **Create:** `attachment-downloader/go.sum`
- **Create:** `attachment-downloader/internal/storage/storage.go` (interface + types)
- **Create:** `attachment-downloader/internal/storage/storage_test.go` (interface contract tests)
- **Create:** `attachment-downloader/internal/storage/gcs/gcs.go` (GCS impl)
- **Create:** `attachment-downloader/internal/storage/gcs/gcs_test.go` (httptest-mock based)
- **Create:** `attachment-downloader/internal/storage/factory.go` (env-var → backend dispatch)
- **Modify:** `go.work` (add `./attachment-downloader`)

## Steps

### 2.1 Define the interface

`internal/storage/storage.go`:

```go
// Package storage defines the backend-agnostic interface for persisting
// attachment bytes. Implementations live under sub-packages (gcs, s3, ...).
package storage

import (
    "context"
    "io"
    "time"
)

// Object describes a stored object's metadata (returned by Stat / List).
type Object struct {
    Key         string    // backend-relative key (e.g. "mio/attachments/zoho_cliq/.../abc.png")
    Size        int64
    ContentType string
    SHA256Hex   string    // lowercase hex of sha256(bytes); empty if backend can't surface it
    ModifiedAt  time.Time
}

// PutOptions controls write behaviour. Defaults are fine for most callers.
type PutOptions struct {
    ContentType string
    SHA256Hex   string  // recorded as object metadata for integrity
    // IfNotExists: when true, write only if the key does not already exist.
    // Backend translates to GCS x-goog-if-generation-match=0 / S3 If-None-Match.
    IfNotExists bool
}

// SignOptions controls signed-URL issuance.
type SignOptions struct {
    TTL      time.Duration
    Method   string  // "GET" only for POC
    // ResponseContentDisposition lets us force download filenames cross-backend.
    ResponseContentDisposition string
}

// LifecycleRule is the minimal cross-backend abstraction:
// "expire objects older than N days under prefix P".
type LifecycleRule struct {
    Prefix    string
    AgeDays   int
}

// Storage is the contract every backend implements. Methods are blocking
// and context-respecting; callers control deadlines.
type Storage interface {
    // Put streams body to key. Honours opts.IfNotExists for race-free dedup.
    // Returns ErrAlreadyExists when IfNotExists=true and the key exists.
    Put(ctx context.Context, key string, body io.Reader, size int64, opts PutOptions) error

    // Get streams the bytes at key. Caller must Close the returned reader.
    // Returns ErrNotFound when the key does not exist.
    Get(ctx context.Context, key string) (io.ReadCloser, *Object, error)

    // Stat returns metadata without fetching bytes.
    Stat(ctx context.Context, key string) (*Object, error)

    // Delete removes a single object. Idempotent (ErrNotFound is not an error).
    Delete(ctx context.Context, key string) error

    // List enumerates objects under prefix. Used for GDPR sweeps.
    // For high cardinality prefixes, callers should iterate with the returned
    // cursor pattern (impl-specific). Keep it simple for POC: yield via channel.
    List(ctx context.Context, prefix string) (<-chan Object, <-chan error)

    // SignedURL issues a time-limited URL for direct GET by external consumers.
    // Backend may return an empty string + nil error when public-bucket mode
    // (e.g. bucket has uniform public read); callers must then use the
    // canonical url shape from CanonicalURL().
    SignedURL(ctx context.Context, key string, opts SignOptions) (string, error)

    // SetLifecycle sets the bucket-wide lifecycle rules (idempotent).
    // Backends without lifecycle (e.g. raw filesystem) return ErrUnsupported.
    SetLifecycle(ctx context.Context, rules []LifecycleRule) error

    // Backend returns the implementation name ("gcs", "s3", ...) for metric labels.
    Backend() string
}

// Sentinel errors. Backend impls wrap concrete errors with these.
var (
    ErrNotFound      = errors.New("storage: not found")
    ErrAlreadyExists = errors.New("storage: already exists")
    ErrUnsupported   = errors.New("storage: unsupported by backend")
)
```

### 2.2 Implement GCS

`internal/storage/gcs/gcs.go`:

- Wraps `cloud.google.com/go/storage`.
- `Put` uses `obj.NewWriter(ctx)` with `Metadata["sha256"]` set; honours `IfNotExists` via `obj.If(storage.Conditions{DoesNotExist: true}).NewWriter(ctx)`.
- Streaming write — never buffers full body in memory.
- `SignedURL` uses `bucket.SignedURL(key, &storage.SignedURLOptions{Method: "GET", Expires: time.Now().Add(opts.TTL), Scheme: V4})`.
- `SetLifecycle` reads existing rules, merges new ones by Prefix (idempotent), `bucket.Update(ctx, ...)`.
- Wrap GCS error `storage.ErrObjectNotExist` → `ErrNotFound`; HTTP 412 (precondition failed) when `IfNotExists` collides → `ErrAlreadyExists`.
- Constructor: `New(ctx, bucket string) (*Backend, error)`. Uses ADC (Workload Identity in cluster).

### 2.3 Factory

`internal/storage/factory.go`:

```go
// New returns a Storage backend chosen by env vars:
//   MIO_STORAGE_BACKEND = "gcs" | "s3"      (default "gcs")
//   MIO_STORAGE_BUCKET  = bucket name       (required)
//   MIO_STORAGE_PREFIX  = key prefix        (default "mio/attachments/")
//   MIO_S3_ENDPOINT     = override (R2/B2/MinIO); empty = AWS
//   MIO_S3_REGION       = region            (S3 only; default "us-east-1")
func New(ctx context.Context) (Storage, error) {
    switch os.Getenv("MIO_STORAGE_BACKEND") {
    case "", "gcs":
        return gcs.New(ctx, mustEnv("MIO_STORAGE_BUCKET"))
    case "s3":
        return nil, fmt.Errorf("storage: s3 backend not yet implemented (deferred)")
    default:
        return nil, fmt.Errorf("storage: unknown backend %q", v)
    }
}
```

### 2.4 Contract tests (table-driven)

`internal/storage/storage_test.go` runs the same test suite against any
backend that implements an internal helper `newTestBackend(t)` — for GCS
this points at `cloud.google.com/go/storage/fake` or skips if no creds.
Tests:

- `Put` then `Get` round-trip — bytes match, metadata SHA matches.
- `IfNotExists` collision — second Put returns `ErrAlreadyExists`.
- `Stat` on missing key returns `ErrNotFound`.
- `Delete` is idempotent.
- `SignedURL` returns a parseable HTTPS URL with expected `Expires` query param.
- `List` yields all objects under prefix and closes channels.

### 2.5 Wire into workspace

Append `./attachment-downloader` to `go.work`'s `use ()` block.
Run `go work sync` then `cd attachment-downloader && go build ./...`.

## Tests
- [ ] `cd attachment-downloader && go test ./...` passes
- [ ] `go vet ./...` clean
- [ ] `golangci-lint run ./...` clean
- [ ] `go work sync` produces no diff after changes settle

## Success Criteria
- [ ] `Storage` interface compiled, all 8 methods present
- [ ] GCS impl satisfies the interface (compile-time check via `var _ Storage = (*Backend)(nil)`)
- [ ] Factory dispatches on env var, returns clear error for unconfigured S3
- [ ] Contract tests pass against GCS impl with `cloud.google.com/go/storage/fake` or skipped on missing creds
- [ ] Adding S3 later requires zero changes to interface or factory dispatch table (only the case body)

## Risks
- **GCS V4 signing requires IAM credentials API access** — Workload Identity supports it via `iam.serviceAccounts.signBlob` permission; verify GSA has `roles/iam.serviceAccountTokenCreator` on itself
- **`fake` GCS test pkg may not implement signed URLs** — fall back to `nil error + empty URL` semantic for tests, real signing tested in phase-07 smoke
- **Interface drift if S3 needs methods GCS doesn't** — keep interface minimal (the 8 methods above); add adapter-private methods inside the impl package if needed
