---
title: "Attachment persistence with pluggable object storage"
status: completed
goal: "Persist all inbound attachments off-platform via a Storage interface (GCS first, S3-compatible next) so consumers always retrieve bytes regardless of platform URL TTLs."
created: 2026-05-09
mode: default
---

# Attachment persistence (mio P9)

## Goal

Ship a sidecar worker that downloads inbound attachment bytes within the
platform-side TTL, writes them to pluggable object storage, and surfaces a
stable URL on the enriched Message — so AI consumers never depend on
short-lived platform URLs (Cliq's are ~12 min).

## Approach

Separate Go binary `attachment-downloader` (lives in its own module, mirrors
`sink-gcs/`). Subscribes to a JetStream filtered consumer over
`MESSAGES_INBOUND` for messages where `attachments[].url` is populated and
the URL host is not the storage backend (i.e. not yet rewritten). For each
attachment: download with the channel adapter's auth scheme → SHA-256 the
bytes → check storage for an existing object at the content-addressable key
→ write if absent (multipart for >5 MB) → republish an enriched Message to
`MESSAGES_INBOUND_ENRICHED` with `Attachment.url` rewritten to a signed URL
and `Attachment.storage_key` set.

Storage abstraction is a Go interface (`Put / Get / Delete / SignedURL /
SetLifecycle / List`). GCS implementation lives at
`internal/storage/gcs/`; S3-compatible at `internal/storage/s3/` (deferred,
single-file drop-in using AWS SDK v2 + endpoint override for R2/B2/MinIO).
Backend selection is a single env var `MIO_STORAGE_BACKEND={gcs|s3}`.

## Success Criteria

- [ ] Image sent in Cliq channel → byte-identical bytes retrievable from
      object storage ≥7 days later
- [ ] Duplicate image sent twice → single object stored (content-hash dedup)
- [ ] Storage backend swap (GCS→S3) needs only `MIO_STORAGE_BACKEND` env
      change + one new impl file; gateway / consumer / sidecar core unchanged
- [ ] Sidecar restart loses no work: at-least-once delivery via JetStream
      redelivery; idempotent writes (HEAD-before-PUT)
- [ ] Consumer receives enriched Message with signed URL valid ≥1h
- [ ] Lifecycle rule on `mio/attachments/` prefix expires objects at JetStream
      Max Age (7d) — verified via `gsutil ls -L`
- [ ] GDPR delete: `mio-attachment-cli delete --account_id=<uuid>` removes
      all bytes for that tenant within 24h
- [ ] Expired-URL case: Cliq URL expires before download → Attachment marked
      `error_code=ATTACHMENT_ERROR_EXPIRED`, downstream message still flows

## Out of Scope

- **S3-compatible impl (AWS/R2/B2)** — interface designed for it; deferred to a P9.5 follow-up
- **Per-platform adapter beyond Cliq** — Slack/Discord/Teams/Telegram/WA/FB/Pancake; design accommodates, wiring deferred
- **AI service code changes** — it already reads `Attachment.url`; we only rewrite the URL
- **Auto-bucket-lifecycle Terraform** — `gsutil` runbook acceptable for POC; IaC deferred
- **Sidecar HA / horizontal scaling** — single replica acceptable until throughput demands it (the JetStream consumer is durable + ack-gated, so single-replica is correct-but-slow not unsafe)

## Phases

| # | Phase | Status | Depends on | Effort |
|---|---|---|---|---|
| 1 | [extend-attachment-proto](phase-01-extend-attachment-proto.md) | completed | — | 30m |
| 2 | [storage-interface-and-gcs-impl](phase-02-storage-interface-and-gcs-impl.md) | completed | — | 4h |
| 3 | [scaffold-sidecar-binary](phase-03-scaffold-sidecar-binary.md) | completed | 1, 2 | 4h |
| 4 | [wire-cliq-fetch-and-publish](phase-04-wire-cliq-fetch-and-publish.md) | completed | 3 | 4h |
| 5 | [helm-chart-and-flux-deploy](phase-05-helm-chart-and-flux-deploy.md) | completed | 4 | 2h |
| 6 | [retention-and-gdpr-cli](phase-06-retention-and-gdpr-cli.md) | completed | 2 | 3h |
| 7 | [enriched-stream-consumer-rewrite-and-smoke](phase-07-enriched-stream-consumer-rewrite-and-smoke.md) | completed | 4, 5 | 3h |

Total ≈ 2.5 days. Phases 1+2 parallel. Phase 6 parallel with 4+5. Phase 7 is the e2e gate.

## Constraints

- Go 1.25.0, multi-module repo with `go.work` (add new `./attachment-downloader/` module)
- Storage backend selectable at runtime via `MIO_STORAGE_BACKEND={gcs|s3}` env
- Workload Identity for GCS (no static creds in cluster). GSA `mio-attachments@dp-prod-7e26.iam.gserviceaccount.com` with `roles/storage.objectAdmin` scoped via IAM Condition to `objects/mio/attachments/`
- Object key shape: `mio/attachments/{channel_type}/{yyyy=YYYY/mm=MM/dd=DD}/{sha256[:2]}/{sha256}.{ext}` — partitioned for prefix delete + dedup-friendly
- Reuse `gs://ab-spectrum-backups-prod` with new prefix `mio/attachments/`; lifecycle rule scoped by prefix only (does not affect existing CNPG backups)
- New proto fields are additive — `schema_version` stays 1 (keeps backwards compat with running gateway/echo)
- JetStream R=1 emptyDir POC — sidecar must replay-safely from any seq

## Risks

- **Cliq URL expires mid-download for slow attachments** → mark `ATTACHMENT_ERROR_EXPIRED`; structured log with original URL hash for forensics; downstream still gets the Message
- **Sidecar lag exceeds platform TTL** under burst → JetStream durable consumer state lets us see lag in metrics; alert on `attachment_download_lag_seconds > 300`
- **Storage backend outage** → sidecar Naks message → JS redelivers up to MaxDeliver (default 5); after exhaustion, message lands on a poison-pill DLQ stream (defer: log loud and Term for POC)
- **Multi-replica dedup race** (two pods download same SHA simultaneously) → HEAD-before-PUT + If-None-Match conditional write absorbs the race; both succeed idempotently
- **Large file (>10 MB) blowing memory** → streaming download → streaming multipart upload; never `io.ReadAll(resp.Body)`
- **Proto field addition forgets schema_version** → CI guard (`buf breaking`) catches; phase-01 explicitly stays additive

## References

- Research: [`plans/reports/research-260509-2252-multi-channel-attachment-persistence.md`](../reports/research-260509-2252-multi-channel-attachment-persistence.md)
- P8 deploy report: [`plans/reports/cook-260509-2125-p8-poc-deploy-gke.md`](../reports/cook-260509-2125-p8-poc-deploy-gke.md)
- Existing GCS sink (pattern reference): `sink-gcs/`
- Cliq adapter: `gateway/internal/channels/zohocliq/`
- Attachment proto: `proto/mio/v1/attachment.proto`
