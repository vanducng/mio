---
phase: 6
title: "mio-sink-gcs"
status: pending
priority: P2
effort: "1d"
depends_on: [2]
---

# P6 — Sink-gcs

## Overview

Long-tail consumer that writes raw inbound payloads to GCS, partitioned by
`channel_type` and date. Doubles as analytics substrate (BigQuery external
tables read directly). Locally uses MinIO; in cluster uses GCS via
Workload Identity.

Parallelizable with P3–P5; only depends on P1+P2.

**Foundation alignment**: partition slug uses the registry value
(`zoho_cliq`, underscore — matches `proto/channels.yaml` and the subject
grammar). The earlier hyphenated form (`zoho-cliq`) is removed.

## Goal & Outcome

**Goal:** `MESSAGES_INBOUND` is fully archived to `gs://mio-messages/channel_type=<slug>/date=YYYY-MM-DD/`. Replay-able from cold storage.

**Outcome:** A day's worth of messages appears in the partitioned bucket; a BigQuery external table over the bucket returns counts matching gateway metrics.

## Files

- **Create:**
  - `sink-gcs/cmd/sink/main.go`
  - `sink-gcs/internal/writer/writer.go` — interface (`Write(rec []byte) error`, `Flush() error`, `Close() error`)
  - `sink-gcs/internal/writer/gcs.go` — hand-coded GCS backend (Google Cloud SDK, resumable upload + atomic copy-rename)
  - `sink-gcs/internal/writer/minio.go` — hand-coded MinIO backend (`minio-go` SDK), shares `Writer` interface
  - `sink-gcs/internal/partition/partition.go` — `PartitionPath(channelType string, ts time.Time) string`
  - `sink-gcs/internal/filename/filename.go` — `Filename(consumerID string, seqStart, seqEnd uint64) string` — offset-based
  - `sink-gcs/internal/encode/ndjson.go` — proto → JSON (protojson) one record per line
  - `sink-gcs/sql/external_table.sql` — pinned BQ external table DDL (autodetect + Hive partitioning)
  - `sink-gcs/README.md` — documents slug-drift rule, multi-pod safety, dedup-in-BQ pattern
  - `sink-gcs/Dockerfile` — same multi-stage distroless pattern as P3 `gateway/Dockerfile` (builder `golang:1.23-alpine` with `--mount=type=cache` for `/root/.cache/go-build` + `/go/pkg/mod`, runtime `gcr.io/distroless/static-debian12:nonroot`, `CGO_ENABLED=0` static binary, `EXPOSE 8080` for `/healthz`, build context is repo root). Sink is Go (per `sink-gcs/cmd/sink/main.go`).
  - `sink-gcs/integration_test/sink_test.go`
  - `sink-gcs/internal/filename/testdata/golden_filenames.txt` — fixture for offset-based filename
- **Modify:**
  - `deploy/docker-compose.yml` — add `sink-gcs` pointing at MinIO
  - `Makefile` — `sink-up`, `sink-build`

## Partition path

```
gs://mio-messages/channel_type=<slug>/date=YYYY-MM-DD/<consumer-id>-<seq-start>-<seq-end>.ndjson
```

- `channel_type=<slug>` — value matches `proto/channels.yaml` (e.g. `zoho_cliq`, underscore). Hive-style for BQ external table partition discovery.
- `date=YYYY-MM-DD` — UTC; a message's `received_at` decides its partition (not wall clock at write time).
- Extension `.ndjson` (not `.json`) — newline-delimited; BQ understands it natively.

`PartitionPath("zoho_cliq", t)` is the only place that builds the directory path —
test with golden strings.

## Filename Convention (offset-based)

```
<consumer-id>-<seq-start>-<seq-end>.ndjson
```

- `<consumer-id>` — durable consumer name (`gcs-archiver`).
- `<seq-start>` — JetStream stream sequence of the **first** record in the file.
- `<seq-end>`   — JetStream stream sequence of the **last** record in the file.

Example: `gcs-archiver-1000-1063.ndjson` (records 1000–1063 from the stream).

**Why this scheme:**
- Two pods consuming the same durable cannot be assigned overlapping sequence ranges by JetStream — collision-impossible by construction.
- Pod restart safe: sequences come from JetStream state, not from per-pod counters that reset.
- Replay-friendly: you can locate any record by stream sequence with one `ls`.

The previous `<hostname>-<unixmillis>-<seq>` scheme is removed. Two pods booting in the same millisecond on the same host would collide and silently overwrite — unacceptable risk for an archival sink.

`Filename(consumerID, seqStart, seqEnd)` is the **only** place that builds filenames; covered by golden fixture.

### Multi-pod safety

Offset-based naming is **mandatory before P7 multi-replica deployment**. P6 may run a single replica during local development, but the codepath must already emit offset-based names so P7 can scale the Deployment to N replicas without changing sink-gcs code.

A concurrent two-pod integration test (see Success Criteria) is the gate: it must produce non-overlapping `<seq-start,seq-end>` ranges across all output files.

## Record format

Wire bytes are protobuf, but on-disk format is **NDJSON of `mio.v1.Message`
JSON-marshaled** via `protojson` (uses field names, not numbers; BQ
schema-autodetect-friendly). Bytes-on-the-wire would be more compact but
BQ schema autodiscovery prefers JSON for the POC. Revisit at the
**upgrade-to-Parquet trigger: archive ≥ $1500/mo or ≥ 50 GB/day**.

```json
{"id":"…","schema_version":1,"tenant_id":"…","account_id":"…","channel_type":"zoho_cliq","conversation_id":"…","conversation_external_id":"chat_…","conversation_kind":"CONVERSATION_KIND_DM","source_message_id":"…","sender":{…},"text":"hello","received_at":"2026-05-07T11:00:00Z","attributes":{…}}
```

### `protojson` defaults

```go
opts := protojson.MarshalOptions{
    EmitUnpopulated: false,  // skip zero values; smaller NDJSON
    UseEnumNumbers:  false,  // emit enum string names → SQL-friendly
    AllowPartial:    false,  // strict
}
```

**Proto rule (P1 idiom, restated for P6):** never assign meaning to enum value `0` — always use `*_UNSPECIFIED = 0`. With `EmitUnpopulated=false`, an enum field set to its zero value is omitted from the NDJSON; if `0` carried meaning, the row would silently lose information. P1's `UNSPECIFIED=0` convention covers this; P6 only consumes the constraint.

## Steps

1. **Backend abstraction.** Define `Writer` interface in `internal/writer/writer.go`. Hand-code `gcs.go` (Google Cloud SDK) and `minio.go` (`minio-go` SDK) against it. A `New(ctx, backend, bucket, path)` factory selects by env var (`SINK_BACKEND=gcs|minio`). Go CDK (`gocloud.dev/blob`) is future-proof but unnecessary today — YAGNI.
2. **Consumer bootstrap.** `cmd/sink/main.go` connects to NATS and idempotently ensures the durable consumer:
   ```
   durable_name:    gcs-archiver
   deliver_policy:  all
   ack_policy:      explicit
   ack_wait:        60s
   max_ack_pending: 64           # batching > strict ordering for archival
   max_deliver:     -1           # never give up; archival never drops
   replay_policy:   instant
   filter_subject:  mio.inbound.>
   ```
   `MESSAGES_INBOUND` stream is owned by gateway startup (P3); sink-gcs only creates the consumer.
3. **Encode.** Pull-subscribe in batches of 64. For each message, decode to `mio.v1.Message`, then encode to NDJSON line with `protojson` defaults (`EmitUnpopulated=false`, enum strings).
4. **Partition.** Derive directory from `msg.channel_type` + `msg.received_at` (UTC date). The `channel_type` slug used for the partition is **whatever is on the wire** — never rewritten (slug-drift rule, see §Risks).
5. **Buffered write to `.inflight`.** Open one buffered writer per `(partition, file)`. Track `seqStart` (JetStream sequence of first record) on open; update `seqEnd` on every append. Filename is finalised at flush time as `<consumer-id>-<seqStart>-<seqEnd>.ndjson`.
6. **Flush triggers.** Whichever fires first:
   - **size:** buffered ≥ 16 MB
   - **time:** writer age ≥ 1 min
   - **shutdown:** SIGTERM → flush all writers, then ack, then exit
7. **Atomic rename.** GCS has no native rename; use copy-then-delete:
   1. Close `.inflight` resumable upload.
   2. `dst.CopierFrom(src).Run(ctx)` — atomic at the GCS API.
   3. Delete `.inflight`.
   4. **Then** ack the JetStream messages (`seqStart..seqEnd`).
   If the process dies between (2) and (4), JetStream redelivers and the next pod re-emits an identical final object (same offset range → same name → idempotent overwrite). Never ack before final object exists.
8. **Orphaned `.inflight` cleanup.** Bucket lifecycle rule (deployed in P7) deletes objects matching `*.inflight` older than **24 h**. Documented in P7 setup script; not a runtime concern for sink-gcs.
9. **Bucket lifecycle (deferred to P7 setup script).** Standard → Nearline @ 30 d → Coldline @ 90 d. ≈60 % cost reduction vs Standard-only; all tiers are instant-retrieval. Codified as Terraform in P7.
10. **Auth.** Locally, MinIO with static creds in `.env`. In cluster (P7), Google SDK auto-detects credentials in order: `GOOGLE_APPLICATION_CREDENTIALS` → Workload Identity (KSA → GSA via `iam.gke.io/gcp-service-account`) → ADC. No code changes needed; environment determines auth source.
11. **BigQuery external table DDL.** Pinned in `sink-gcs/sql/external_table.sql`:
    ```sql
    CREATE OR REPLACE EXTERNAL TABLE `${PROJECT_ID}.${DATASET}.messages`
    OPTIONS (
      format = 'NEWLINE_DELIMITED_JSON',
      uris = ['gs://${BUCKET}/channel_type=*/date=*/*.ndjson'],
      hive_partition_uri_prefix = 'gs://${BUCKET}/',
      require_hive_partition_filter = false,
      autodetect = true
    );
    ```
    Partition columns `channel_type` (STRING) and `date` (DATE) are auto-discovered from the path.
12. **Dedup post-hoc in BQ.** No sink-side dedup — at-least-once on the consume side is accepted. Standard query view:
    ```sql
    SELECT * FROM `…messages`
    QUALIFY ROW_NUMBER() OVER (PARTITION BY id ORDER BY received_at DESC) = 1;
    ```
13. **Slug-drift rule (documented in `sink-gcs/README.md`).** Partition path uses the `channel_type` value as it arrives on the wire. The sink **never UPDATEs in place**, never rewrites paths, never normalises slugs. If `proto/channels.yaml` deprecates `zalo_oa` → `zalo_oauth`, old partitions stay under `channel_type=zalo_oa/`; new messages land under `channel_type=zalo_oauth/`. Queries spanning the rename must `WHERE channel_type IN ('zalo_oa','zalo_oauth')`.
14. **Integration tests.**
    - Publish 1000 messages locally → exactly 1000 records across all output files; each line round-trips `protojson.Unmarshal` → `proto.Equal` with the published message.
    - Run **two sink-gcs replicas** against the same durable; assert `seqStart..seqEnd` ranges across all files are pairwise non-overlapping.
    - Restart mid-flush → no record loss; the redelivered range produces an identical-name final object.

## Success Criteria

- [ ] Local MinIO has `channel_type=zoho_cliq/date=YYYY-MM-DD/` objects after the echo loop runs
- [ ] Every output filename matches `<consumer-id>-<seq-start>-<seq-end>.ndjson` (regex-checked in tests)
- [ ] Each object is valid NDJSON; every line round-trips through `protojson.Unmarshal` into a `mio.v1.Message` equal to the publish-time message
- [ ] **Concurrent two-pod test**: two sink-gcs replicas against the same durable produce pairwise non-overlapping `<seq-start..seq-end>` ranges across all output files
- [ ] Restart sink-gcs mid-flush → no data loss; redelivered ranges produce identical-name final objects (idempotent)
- [ ] `mio_sink_gcs_inflight_files` returns to 0 after the flush window once the stream goes idle
- [ ] After post-hoc dedup, BigQuery `SELECT COUNT(*)` over the external table matches `mio_gateway_inbound_total{outcome="published"}`
- [ ] `mio_sink_gcs_bytes_written_total{channel_type}` increases monotonically
- [ ] No `.inflight` objects older than 24 h in the bucket (lifecycle rule active in P7)

## Risks

- **Filename collision on multi-pod** — *now mitigated* by offset-based naming (`<consumer-id>-<seq-start>-<seq-end>`); JetStream sequence ranges are pairwise disjoint by construction, so no two pods can produce the same filename. Multi-pod integration test guards regression.
- **Flush-rename window data loss** — copy-then-delete is non-atomic across the two API calls. Mitigated by **deferring ack until after the final object exists**: a crash between copy and ack causes redelivery, and the redelivered range produces an identical-name object (idempotent overwrite of an immutable GCS object is safe — same bytes, same name).
- **At-least-once duplicates** — JetStream pull is at-least-once; the sink does **no** runtime dedup. Dedup is post-hoc in BigQuery via `ROW_NUMBER() OVER (PARTITION BY id ORDER BY received_at DESC)`. Validated by the gateway-count vs BQ-count success criterion.
- **`max_deliver=-1` backlog** — archival never gives up; if writes wedge, messages pile up. Pair with alert `mio_sink_gcs_inflight_files > 10 for 30m` and `nats_jetstream_consumer_pending{consumer="gcs-archiver"} > 1000 for 5m`.
- **Slug drift on rename** — if a channel slug is deprecated (e.g. `zalo_oa` → `zalo_oauth`), old partitions stay under the old slug forever. Rule: **never UPDATE-in-place; partition uses what's on the wire**. Documented in `sink-gcs/README.md`. Cross-rename queries must `WHERE channel_type IN (…)`.
- **BQ schema autodiscovery breaks if proto reuses an enum string** — defence is the proto rule "never reassign enum names": once `CONVERSATION_KIND_DM` exists with a meaning, it cannot be repurposed. Adding new enum values is safe; renaming/repurposing is not. Enforced in P1's proto review checklist.
- **Orphaned `.inflight` files** — process death between resumable-upload close and final copy can leave `.inflight` objects. Bucket lifecycle rule deletes `*.inflight` older than 24 h (deployed in P7).
- **Workload Identity setup latency** — IAM bindings take 2–7 min to propagate; rehearse with a static service-account JSON before swapping to WI in P7. Same `GOOGLE_APPLICATION_CREDENTIALS` codepath in the SDK works for both.
- **`received_at` clock skew** — a gateway clock drifting forward lands a message in tomorrow's partition. Acceptable for archival; analytics queries should not assume `date=` is wall-clock-tight.
- **JSON vs binary proto on disk** — NDJSON wins for analytics today; upgrade trigger is **archive ≥ $1500/mo or ≥ 50 GB/day**, at which point convert to Parquet with explicit schema.

## Research backing

[`plans/reports/research-260508-1056-p6-sink-gcs-archival-bigquery.md`](../../reports/research-260508-1056-p6-sink-gcs-archival-bigquery.md)

Findings are integrated into the sections above. Headline contract owned by this phase: **filename scheme is offset-based** (`<consumer-id>-<seq-start>-<seq-end>.ndjson`); the previous `<hostname>-<unixmillis>-<seq>` form is removed because it can collide on same-host millisecond-aligned pod restarts. All other validated picks (NDJSON for POC with Parquet upgrade trigger, `protojson` defaults, `max_deliver=-1` with backlog alert, post-hoc BQ dedup via `ROW_NUMBER`, lifecycle Standard→Nearline@30d→Coldline@90d, hand-coded `gcs.go`+`minio.go` over a shared `Writer` interface, slug-drift rule) are reflected in §Files, §Steps, §Risks.
