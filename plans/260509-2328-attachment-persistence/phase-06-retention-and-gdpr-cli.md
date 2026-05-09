---
phase: 6
title: "retention-and-gdpr-cli"
status: completed
priority: P2
effort: "3h"
depends_on: [2]
---

# Phase 6: Retention alignment + GDPR delete CLI

## Overview

Apply lifecycle rules to the storage prefix matching JetStream `Maximum
Age` (7d for inbound enriched), and ship a CLI that deletes all objects
for a given `account_id` (or `tenant_id`) across the bucket — used for
right-to-erasure flows. Decoupled from the sidecar so it can run as a Job
or from a developer laptop.

## Files
- **Create:** `attachment-downloader/cmd/mio-attachment-cli/main.go`
- **Create:** `attachment-downloader/internal/lifecycle/lifecycle.go`
- **Create:** `attachment-downloader/internal/lifecycle/lifecycle_test.go`
- **Create:** `attachment-downloader/internal/gdpr/gdpr.go` (delete-by-account)
- **Create:** `attachment-downloader/internal/gdpr/gdpr_test.go`
- **Create:** `docs/runbooks/attachment-gdpr-delete.md`
- **Create:** `deploy/charts/mio-attachment-downloader/templates/lifecycle-job.yaml` (idempotent post-install Job)

## Steps

### 6.1 Lifecycle rules

`internal/lifecycle/lifecycle.go`:

```go
// Rules for mio attachment storage. Aligned with JetStream Max Age.
// Inbound enriched stream is 7d; we mirror that on bytes so cleanup is
// JetStream-driven (NATS evicts the message → bytes age out shortly after).
//
// We do NOT add a separate outbound 1d rule yet — outbound has no attachments
// today (the gateway sender ignores SendCommand.attachments for Cliq).
func DefaultRules(prefix string) []storage.LifecycleRule {
    return []storage.LifecycleRule{
        {Prefix: prefix, AgeDays: 7},
    }
}
```

The sidecar's `main.go` calls `storage.SetLifecycle(ctx, lifecycle.DefaultRules(...))` once on startup. Idempotent — backend impl reads existing rules and only writes diff.

### 6.2 GDPR delete

`internal/gdpr/gdpr.go`:

```go
// DeleteByAccount removes every attachment whose storage_key partition
// includes account_id. Implementation: List the prefix, parse keys, filter,
// Delete in bounded-concurrency.
//
// Rationale for path-based filter (rather than metadata): GCS/S3 List is
// O(prefix size); filtering by metadata would require a Get per object.
// The keygen layout puts {channel_type} in the path but NOT account_id —
// that's a deliberate choice because content-hash dedup means a single
// blob may belong to multiple accounts. We therefore need a separate
// account→key index. For POC we accept full-prefix scan with O(N) filter
// against an in-memory account_id allowlist; scale to a side-table later.
//
// For 100k attachments/day × 7 days = 700k keys: List paginates at 1k/batch,
// so ~700 LIST requests per sweep. Sub-5-minute typical.
func DeleteByAccount(ctx context.Context, s storage.Storage, prefix, accountID string, dryRun bool) (deleted int, err error)
```

POC strategy: emit `account_id` into the **object metadata** (custom
header) on Put. List + Stat-skim to filter. Yes, this is N Gets — but
volumes are small for POC. Document the side-table design as a P10
optimisation if the operation runs >5 minutes.

Alternative considered: include `account_id` in the key path
(`mio/attachments/{channel_type}/{account_id}/...`). Rejected because
content-hash dedup breaks across accounts (same image stored once, but
which account "owns" it?). Object metadata avoids the dedup loss.

### 6.3 CLI surface

`cmd/mio-attachment-cli/main.go`:

```
mio-attachment-cli list   --prefix=mio/attachments/zoho_cliq/
mio-attachment-cli stat   <key>
mio-attachment-cli delete --account_id=<uuid> [--dry-run] [--concurrency=10]
mio-attachment-cli signed-url <key> [--ttl=1h]
mio-attachment-cli set-lifecycle [--age-days=7] [--prefix=mio/attachments/]
```

Reuses the same `storage.New(ctx)` factory; runs locally with ADC creds
(developer's `gcloud auth application-default login`) or in-cluster as a
Job using the `mio-attachments` GSA.

### 6.4 Lifecycle Job (chart)

`templates/lifecycle-job.yaml` — Helm post-install hook:

- Image: same as sidecar (CLI is in the same binary tree).
- Command: `["/mio-attachment-cli", "set-lifecycle", "--age-days=7", "--prefix=mio/attachments/"]`.
- `helm.sh/hook: post-install,post-upgrade`.
- Idempotent — re-running produces no diff if rules already match.

### 6.5 Runbook

`docs/runbooks/attachment-gdpr-delete.md`:

- When triggered (legal request, customer offboard).
- Pre-flight: `--dry-run` to count.
- Execute: kubectl run a one-off pod, or local `gcloud auth application-default login` + run.
- Audit: Stackdriver / Cloud Logging captures all `storage.objects.delete` calls per WI identity.

## Tests
- [ ] `set-lifecycle` against GCS fake → `bucket.Get(ctx).Attrs.Lifecycle` matches expected
- [ ] `set-lifecycle` re-run → no API write (compare-then-write semantics)
- [ ] `DeleteByAccount` on a synthetic 100-object prefix with mixed account_ids → only matching subset deleted
- [ ] `--dry-run` outputs count, no Delete calls
- [ ] CLI exit codes: 0 on success, 1 on auth error, 2 on partial failure

## Success Criteria
- [ ] `gcloud storage buckets describe gs://ab-spectrum-backups-prod --format='value(lifecycle)'` shows the new 7-day rule on `mio/attachments/`
- [ ] `mio-attachment-cli delete --account_id=<test-uuid> --dry-run` reports the right count against a real bucket fixture
- [ ] Lifecycle Job ran successfully on first chart install (visible in `kubectl -n mio get jobs`)

## Risks
- **GDPR sweep is N Gets** — for 100k+ objects it's slow. Document P10 side-table fix.
- **Lifecycle rules clobbering** — `SetLifecycle` impl must merge by Prefix, not replace whole bucket lifecycle. The bucket has CNPG-backup rules on `cnpg/` prefix that must survive.
- **Dry-run misses race**: between dry-run and execute, new attachments could land for the account. Acceptable for POC; production GDPR flows freeze the account first.
- **Audit trail** — emit a structured log line per Delete with `account_id`, `key`, `caller_identity`; cluster log retention preserves it ≥30d.
