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
  - `sink-gcs/internal/writer/gcs.go` — buffered append writer, flush on size or time
  - `sink-gcs/internal/writer/minio.go` — same `Writer` interface for local
  - `sink-gcs/internal/writer/writer.go` — interface (`Write(rec []byte) error`, `Flush() error`, `Close() error`)
  - `sink-gcs/internal/partition/partition.go` — `PartitionPath(channelType string, ts time.Time) string`
  - `sink-gcs/internal/encode/ndjson.go` — proto → JSON (protojson) one record per line
  - `sink-gcs/Dockerfile`
  - `sink-gcs/integration_test/sink_test.go`
- **Modify:**
  - `deploy/docker-compose.yml` — add `sink-gcs` pointing at MinIO
  - `Makefile` — `sink-up`, `sink-build`

## Partition path

```
gs://mio-messages/channel_type=zoho_cliq/date=2026-05-07/<hostname>-<unixmillis>-<seq>.ndjson
```

- `channel_type=<slug>` — value matches `proto/channels.yaml`. Hive-style for BQ external table partition discovery.
- `date=YYYY-MM-DD` — UTC; a message `received_at` decides its partition (not wall clock at write time).
- Filename includes pod hostname + start unix ms + per-pod sequence so concurrent writers don't collide.
- Extension `.ndjson` (not `.json`) — newline-delimited; BQ understands it natively.

`PartitionPath("zoho_cliq", t)` is the only place that builds the path —
test with golden strings.

## Consumer config

Durable consumer name: `gcs-archiver` on stream `MESSAGES_INBOUND`.

```
durable_name:    gcs-archiver
deliver_policy:  all
ack_policy:      explicit
ack_wait:        60s
max_ack_pending: 64                # batching > strict ordering for archival
max_deliver:     -1                # never give up; archival is best-effort durability
replay_policy:   instant
filter_subject:  mio.inbound.>
```

`max_deliver=-1` is deliberate: archival can't drop. If a record can't be
written, we keep nakking + alert; manual intervention drains the backlog.

## Record format

Wire bytes are protobuf, but on-disk format is **NDJSON of `mio.v1.Message`
JSON-marshaled** via `protojson` (uses field names, not numbers; BQ
schema-autodetect-friendly). Bytes-on-the-wire would be more compact but
BQ schema autodiscovery prefers JSON for the POC. Revisit if archive cost
becomes meaningful.

```json
{"id":"…","schema_version":1,"tenant_id":"…","account_id":"…","channel_type":"zoho_cliq","conversation_id":"…","conversation_external_id":"chat_…","conversation_kind":"CONVERSATION_KIND_DM","source_message_id":"…","sender":{…},"text":"hello","received_at":"2026-05-07T11:00:00Z","attributes":{…}}
```

Enums emitted as their string names (protojson default) — easier to query
in SQL than integer codes.

## Steps

1. `cmd/sink/main.go` — connect to NATS, ensure `gcs-archiver` consumer exists (idempotent), spawn writer.
2. Pull-subscribe in batches of 64; per-message: derive partition path from `msg.channel_type` + `msg.received_at` → append protojson-encoded line to the matching open writer.
3. Flush on size (16MB) or time (1min) — whichever first; rename `.inflight` → final on flush. Object key encodes the byte boundary so retries don't double-write a record (records before last flush are final).
4. Workload Identity: in-cluster, mount via `iam.gke.io/gcp-service-account`; locally, use MinIO with static creds in `.env`.
5. Bucket lifecycle (one-time, in P7 setup script): Standard → Nearline @ 30d → Coldline @ 90d.
6. Integration test: publish 1000 messages locally → assert MinIO ends up with one or more `channel_type=zoho_cliq/date=YYYY-MM-DD/` objects whose total NDJSON record count equals 1000 and round-trip proto.Equal each.
7. BigQuery external table DDL (in repo as `sink-gcs/sql/external_table.sql`) — pinned to the partitioned layout for re-application.

## Success Criteria

- [ ] Local MinIO has `channel_type=zoho_cliq/date=YYYY-MM-DD/` objects after the echo loop runs
- [ ] Each object is valid NDJSON; every line round-trips through `protojson.Unmarshal` into a `mio.v1.Message` equal to the publish-time message
- [ ] Restart sink-gcs mid-run → no data loss (continue from JS consumer position) and no in-object duplicates within a single flush window
- [ ] BigQuery external table over MinIO/GCS returns `count(*)` matching `mio_gateway_inbound_total{outcome="published"}`
- [ ] `mio_sink_gcs_bytes_written_total{channel_type}` increases monotonically
- [ ] `mio_sink_gcs_inflight_files` returns to 0 within `flush_interval` after stream goes idle

## Risks

- **At-most-once vs at-least-once** — JS pull is at-least-once; sink-gcs design must tolerate duplicates within a flush window. NDJSON dedup is by `message.id` post-hoc in BQ.
- **Long-running file handles** — flush-on-size+time prevents stuck files; explicit flush on shutdown signal (`SIGTERM` → flush all writers → ack outstanding → exit).
- **Workload Identity setup** — the GKE-side IAM dance is fiddly; rehearse locally first using a service account JSON, then swap to WI in P7.
- **JSON vs binary proto on disk** — JSON wins for analytics today; revisit if archive cost becomes meaningful.
- **Slug drift** — if `proto/channels.yaml` renames a channel via `deprecated_aliases`, partition paths could split. Rule: never UPDATE-in-place; the alias maps the *new* name to the old slug for *publishing*, but the partition path uses what's on the wire. Document in `sink-gcs/README.md`.
- **`received_at` clock skew** — if a gateway clock drifts forward, a message lands in tomorrow's partition. Acceptable; BQ external table handles partition discovery; queries should use BQ ingestion time as a sanity check.
