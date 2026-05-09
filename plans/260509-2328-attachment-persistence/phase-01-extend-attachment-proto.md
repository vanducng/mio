---
phase: 1
title: "extend-attachment-proto"
status: completed
priority: P1
effort: "30m"
depends_on: []
---

# Phase 1: Extend Attachment proto for storage metadata

## Overview

Add additive fields to `mio.v1.Attachment` so the sidecar can record where
bytes ended up (`storage_key`) and surface fetch failures (`error_code`)
without bumping schema_version. Keeps existing producers/consumers working.

## Files
- **Modify:** `proto/mio/v1/attachment.proto`

## Steps

1. Edit `proto/mio/v1/attachment.proto`. Append to the `Attachment` message:
   ```proto
   // --- storage rewrite (set by attachment-downloader sidecar) ---
   // Stable object-storage key (e.g. "mio/attachments/zoho_cliq/yyyy=2026/.../{sha256}.png").
   // Empty until the sidecar persists bytes. Independent of `url` so consumers
   // that hold long-lived references can re-mint signed URLs from the key.
   string storage_key = 6;

   // SHA-256 of the persisted bytes (lowercase hex). Empty if not yet persisted.
   // Used for cross-message dedup checks and integrity verification.
   string content_sha256 = 7;

   // Bounded-set error code when persistence fails. Empty on success.
   // The downstream Message still flows; AI consumer should soft-handle missing bytes.
   ErrorCode error_code = 8;

   enum ErrorCode {
     ERROR_UNSPECIFIED      = 0;
     ATTACHMENT_OK          = 1; // bytes persisted; storage_key + content_sha256 set
     ATTACHMENT_ERROR_EXPIRED       = 2; // platform URL TTL elapsed before download
     ATTACHMENT_ERROR_FORBIDDEN     = 3; // 401/403 from platform
     ATTACHMENT_ERROR_NOT_FOUND     = 4; // 404 from platform
     ATTACHMENT_ERROR_TOO_LARGE     = 5; // exceeded sidecar size cap
     ATTACHMENT_ERROR_STORAGE       = 6; // backend write failed
     ATTACHMENT_ERROR_TIMEOUT       = 7; // download exceeded deadline
   }
   ```

2. Renumber check: `Attachment` proto currently uses fields `url=2, mime=3, bytes=4, filename=5` and a `Kind` enum at field 1 (verify exact numbers before editing). Use the next available field numbers; do not re-use reserved numbers.

3. Run `buf lint proto` and `buf breaking proto --against '.git#branch=main'` — both must pass. Additive fields are non-breaking.

4. Run `buf generate proto` and verify both `proto/gen/go/mio/v1/attachment.pb.go` and `proto/gen/py/mio/v1/attachment_pb2.py` regenerate cleanly.

## Tests
- [ ] `buf breaking` reports zero issues (proves additive)
- [ ] `cd gateway && go build ./...` succeeds (consumes new proto)
- [ ] `uv run pytest sdk-py/tests/` passes (Python regen consumed)

## Success Criteria
- [ ] New fields appear on the generated Go struct: `Attachment.StorageKey`, `Attachment.ContentSha256`, `Attachment.ErrorCode`
- [ ] No callers of `Attachment` need changes (additive)
- [ ] CI `test-proto` step passes on PR

## Risks
- **Field-number collision** — reserved numbers exist for future fields. Read attachment.proto first; if `reserved 6;` exists, pick 9+.
- **Python regen mismatch** — `buf.gen.yaml` must produce Python; if not, add the `protocolbuffers/python` plugin entry.
