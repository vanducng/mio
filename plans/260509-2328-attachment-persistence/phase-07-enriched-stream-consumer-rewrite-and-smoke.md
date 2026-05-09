---
phase: 7
title: "enriched-stream-consumer-rewrite-and-smoke"
status: completed
priority: P1
effort: "3h"
depends_on: [4, 5]
---

# Phase 7: Switch echo consumer to enriched stream + end-to-end smoke

## Overview

Point the echo consumer (and future MIU AI service) at
`MESSAGES_INBOUND_ENRICHED` instead of `MESSAGES_INBOUND` so they receive
attachment-rewritten Messages. Run an end-to-end smoke proving the full
loop: Cliq webhook → gateway → INBOUND → sidecar → ENRICHED → echo →
OUTBOUND → Cliq REST.

## Files
- **Modify:** `examples/echo-consumer/echo.py` — change `INBOUND_SUBJECT` from `mio.inbound.>` to `mio.inbound_enriched.>`, durable name to `ai-consumer-enriched`
- **Modify:** `examples/echo-consumer/tests/test_echo_handler.py` — assert it consumes from the enriched subject
- **Modify:** `sdk-py/mio/subjects.py` — add `inbound_enriched(channel_type, account_id, conversation_id)` helper
- **Modify:** `sdk-go/subjects.go` — same Go-side helper
- **Modify:** `docs/deployment.md` — append section "Attachment persistence flow" with new subject + retention notes
- **Modify:** `plans/260509-2328-attachment-persistence/plan.md` — set `status: completed` after smoke passes

## Steps

### 7.1 SDK subject helpers

`sdk-py/mio/subjects.py`:
```python
def inbound_enriched(channel_type: str, account_id: str, conversation_id: str) -> str:
    return f"mio.inbound_enriched.{channel_type}.{account_id}.{conversation_id}"
```

Same in `sdk-go/subjects.go`. Tests in both SDKs assert the format and that
`inbound_enriched("zoho_cliq", "*", "*")` builds a wildcarded sub.

### 7.2 Echo consumer cutover

`examples/echo-consumer/echo.py`:
- `INBOUND_SUBJECT = "mio.inbound_enriched.>"`
- `DURABLE = "ai-consumer-enriched"` (new durable so we don't conflict with the existing one during cutover; the original durable's seq stays available for rollback)
- Keep `handle()` unchanged — it already consumes the same Message proto; the only difference is `att.url` now points to GCS signed URLs instead of platform URLs.

### 7.3 Verify gateway is unchanged

`gateway/internal/channels/zohocliq/handler.go` keeps publishing to
`mio.inbound.>` — it does NOT publish to enriched. The sidecar owns the
fan-in/fan-out. This is the central seam: gateway = source of truth,
sidecar = enrichment, consumer = enriched only.

### 7.4 Smoke test plan

After full deploy (gateway + sidecar + echo all running):

1. **Text-only ping** (regression baseline)
   - Type `ping` in `ducdev`
   - Expect: echo replies `echo: ping` ≤5s
   - Verify: `MESSAGES_INBOUND_ENRICHED` count incremented; sidecar log shows "no attachments, fast-pass"
   - **Pass criterion:** existing P8 happy-path still works under the new flow

2. **Image attachment**
   - Send a small image (≤1 MB) in `ducdev`
   - Expect: echo replies `echo: ` (text empty per current echo behaviour) ≤5s after attachment is downloaded
   - Verify (kubectl):
     - `kubectl -n mio logs deploy/mio-attachment-downloader` shows "downloaded sha=... bytes=..."
     - `gsutil ls gs://ab-spectrum-backups-prod/mio/attachments/zoho_cliq/yyyy=*/...` lists the new object
     - `gsutil stat <object>` shows `Hash (md5)` matching, `Metadata: sha256=<hex>`, `Content-Length` matching
     - `MESSAGES_INBOUND_ENRICHED` last msg has `att.url` containing `googleapis.com` and `att.storage_key` populated
   - **Pass criterion:** byte-identical retrieval `gsutil cp` produces same SHA as posted image

3. **Dedup**
   - Send the same image again
   - Verify: only one object exists for that SHA; sidecar log shows `dedup_hit=true`; metric `mio_attachment_dedup_hits_total` incremented

4. **Expired URL** (forced)
   - Send an image, then **wait 15 minutes** before letting the sidecar process (achievable by `kubectl scale deploy/mio-attachment-downloader --replicas=0`, sending the image, waiting 15 min, then `--replicas=1`)
   - Expect: enriched message has `att.error_code = ATTACHMENT_ERROR_EXPIRED`
   - Verify: echo still receives + processes the message; no crash

5. **Large file** (≥10 MB)
   - Send a >10 MB image (Cliq UI may convert; if so, use a video/file >10 MB)
   - Expect: download streams, RSS stays bounded, object lands in GCS
   - Metric: `mio_attachment_download_duration_seconds_bucket` shows entry in 5–30s bucket

6. **Lifecycle**
   - `gcloud storage buckets describe gs://ab-spectrum-backups-prod --format='value(lifecycle)'` shows `condition.age=7` rule on `mio/attachments/`
   - **No 7d wait** — verifying rule presence is sufficient; actual expiry is GCS's responsibility

7. **GDPR delete dry-run**
   - `kubectl -n mio run cli --rm -it --image=ghcr.io/vanducng/mio/attachment-downloader:<sha> --command -- /mio-attachment-cli delete --account_id=00000000-0000-0000-0000-000000000002 --dry-run`
   - Expect: count equals number of stored attachments; no actual deletes (verify with second `gsutil ls`).

### 7.5 Update plan + finalize

After smokes pass:
- Set `phase-07` frontmatter `status: completed`
- Set `plan.md` `status: completed`
- Append to `docs/deployment.md`:
  - New stream `MESSAGES_INBOUND_ENRICHED` (subject pattern, retention 7d)
  - Where attachment bytes live (`gs://ab-spectrum-backups-prod/mio/attachments/`)
  - Signed URL TTL (1h default)
  - GDPR delete CLI usage

## Tests
- [ ] All 7 smoke cases pass
- [ ] `mio_attachment_downloaded_total{outcome="ok"}` ≥ 3 (one per non-expired smoke)
- [ ] `mio_attachment_dedup_hits_total` ≥ 1
- [ ] Echo consumer log shows it's reading `mio.inbound_enriched.zoho_cliq...` subjects
- [ ] No new errors in gateway/sidecar/echo logs in the 5 min window after smokes complete

## Success Criteria
- [ ] Image bytes round-trip GCS with byte-identical SHA verified by `gsutil cp` + `sha256sum`
- [ ] Plan-level criterion #1 (image retrievable ≥7d later): can't fully verify in phase, but lifecycle rule + sidecar persistence proves the mechanism. Note in deploy doc that operators should re-test ≥7d after first deploy.
- [ ] Plan-level criterion #3 (backend swap is one config + one impl file): document by reading the diff produced when phase-02's `factory.go` adds the S3 case (zero changes to caller code in worker.go / cmd / consumer); architectural-soundness check, no S3 deploy required
- [ ] Plan-level criterion #6 (lifecycle on prefix): verified
- [ ] Plan-level criterion #7 (GDPR CLI): dry-run case passes; real delete not executed in smoke

## Risks
- **Echo consumer durable cutover** — old durable `ai-consumer` still attached to MESSAGES_INBOUND with backlog; verify `flux reconcile` removes it OR document manual cleanup (`nats consumer rm MESSAGES_INBOUND ai-consumer`) before declaring done. Without cleanup, old durable accumulates pending messages forever.
- **Sidecar restarts during smoke** corrupt the consumer state — JS durable is on emptyDir; pod restart wipes ack state. Acceptable for POC (worst case: re-process the recent backlog). Document.
- **Cliq token expiry mid-smoke** — 1h Cliq access-token TTL; if smoke runs >50 min, refresh token via playground script and bump infra Secret before continuing
- **Consumer subject change is a breaking move** — if any other consumer is subscribed to `mio.inbound.>` for attachment metadata, this phase breaks them silently. Audit before merge: `grep -r "mio.inbound" --exclude-dir=plans .`
