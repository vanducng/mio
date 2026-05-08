---
phase: 1
title: "Proto v1 envelope"
status: pending
priority: P1
effort: "1h"
depends_on: [0]
---

# P1 — Proto v1 envelope

## Overview

Get the canonical envelope right before any SDK or gateway code lands.
Protobuf via `buf`, one package `mio.v1`. Every later phase imports these.

**Foundation-correctness first**: the five locked-in design choices from
`plans/reports/research-260507-1102-channels-data-model.md` (TL;DR in
master.md → "Design strategy") are baked into this proto. Anything that
would force a goclaw-style 30-table retrofit later is a hard "no" here.

## Goal & Outcome

**Goal:** Stable `mio.v1` schema for `Message`, `SendCommand`, `Sender`,
`Attachment` plus the `ConversationKind` and `PeerKind` enums. Generated
code lands in `proto/gen/{go,py}`.

**Outcome:** A tiny round-trip test (publish a `Message` via NATS, decode
on consume) passes both Go and Python sides.

## Files

- **Create:**
  - `proto/mio/v1/enums.proto` — `ConversationKind`, `PeerKind`
  - `proto/mio/v1/attachment.proto` — `Attachment` + `Attachment.Kind`
  - `proto/mio/v1/sender.proto` — `Sender` (embedded in Message)
  - `proto/mio/v1/message.proto` — inbound envelope
  - `proto/mio/v1/send_command.proto` — outbound envelope
  - `proto/channels.yaml` — `channel_type` registry (single source of truth, read by SDK + CI)
  - `proto/buf.lock` (auto by `buf dep update`)
  - `tools/proto-roundtrip/main.go` (smoke test, throwaway location)
  - `tools/proto-roundtrip/go.mod` (pins `google.golang.org/protobuf` to a stable v1.x)
  - `tools/proto-roundtrip/requirements.txt` (pins `protobuf>=4.27.0,<5.0.0` for the Python half)
  - `CONTRIBUTING.md` — short doc with the `attributes` promotion rule + `channel_type` registry rule (research Q4, Q3)

Not creating `channel.proto` or `user.proto` as separate messages — channel
identity is flat on Message (`tenant_id`, `account_id`, `channel_type`);
sender identity is `Sender` embedded. Per-channel-install rows live in the
DB `accounts` table (P3), not in the wire envelope.

## Schema (binding choices — locked)

### `enums.proto`

```proto
enum ConversationKind {
  CONVERSATION_KIND_UNSPECIFIED = 0;
  CONVERSATION_KIND_DM = 1;               // 1:1 direct
  CONVERSATION_KIND_GROUP_DM = 2;         // multi-party DM, no channel
  CONVERSATION_KIND_CHANNEL_PUBLIC = 3;
  CONVERSATION_KIND_CHANNEL_PRIVATE = 4;
  CONVERSATION_KIND_THREAD = 5;           // sub-conversation under another
  CONVERSATION_KIND_FORUM_POST = 6;       // thread-in-forum (Discord-style)
  CONVERSATION_KIND_BROADCAST = 7;        // one-to-many (Telegram channel, news)
}

enum PeerKind {
  PEER_KIND_UNSPECIFIED = 0;
  PEER_KIND_DIRECT = 1;
  PEER_KIND_GROUP = 2;
}
```

### `sender.proto`

```proto
message Sender {
  string external_id   = 1;   // platform user id (cliq sender_id, slack user, ...)
  string display_name  = 2;
  PeerKind peer_kind   = 3;   // 'direct' | 'group' — kept for fast policy routing
  bool is_bot          = 4;
  // user_id (canonical mio user) is NOT on the wire — derived later by MIU
  // when contact-merge runs. Avoids forcing gateway to do a lookup.
}
```

### `attachment.proto`

```proto
message Attachment {
  enum Kind { KIND_UNSPECIFIED=0; IMAGE=1; FILE=2; AUDIO=3; VIDEO=4; LINK=5; }
  Kind kind        = 1;
  string url       = 2;
  string mime      = 3;
  int64 bytes      = 4;
  string filename  = 5;
}
```

### `message.proto`

```proto
message Message {
  // identity
  string id                       = 1;   // mio UUID v7
  int32 schema_version            = 2;   // = 1; SDK rejects mismatch

  // four-tier scope (foundation)
  string tenant_id                = 3;
  string account_id               = 4;   // mio UUID for the channel install
  string channel_type             = 5;   // string from proto/channels.yaml registry

  // where (the conversation)
  string conversation_id          = 6;   // mio UUID
  string conversation_external_id = 7;   // platform-side opaque id (cliq chat_id, slack channel, tg chat_id)
  ConversationKind conversation_kind = 8;
  string parent_conversation_id   = 9;   // empty unless this is a thread/forum-post

  // idempotency + threading
  string source_message_id        = 10;  // platform message id; (account_id, source_message_id) is unique
  string thread_root_message_id   = 11;  // empty if not in a Matrix-style thread

  // payload
  Sender sender                   = 12;
  string text                     = 13;
  repeated Attachment attachments = 14;

  // timing
  google.protobuf.Timestamp received_at = 15;

  // escape hatch — channel-specific data; promote to typed field at ≥2 consumers
  map<string, string> attributes  = 16;
}
```

### `send_command.proto`

```proto
message SendCommand {
  // identity (also the NATS Nats-Msg-Id for dedup)
  string id                       = 1;   // ULID
  int32 schema_version            = 2;

  // four-tier scope (mirrors Message)
  string tenant_id                = 3;
  string account_id               = 4;
  string channel_type             = 5;

  // where to send
  string conversation_id          = 6;
  string conversation_external_id = 7;   // denormalized so adapter avoids DB lookup at send time
  string parent_conversation_id   = 8;   // for "reply into thread" if the platform models that as a separate conversation
  string thread_root_message_id   = 9;   // for "reply in thread" (Slack-style)

  // payload
  string text                     = 10;
  repeated Attachment attachments = 11;

  // edit support — both IDs because adapter shouldn't lookup
  string edit_of_message_id       = 12;  // mio UUID; empty unless this is an edit
  string edit_of_external_id      = 13;  // platform message id

  // escape hatch
  map<string, string> attributes  = 14;
}
```

### `proto/channels.yaml` (registry)

```yaml
# Single source of truth for channel_type strings.
# CI rejects PRs that introduce a channel_type not listed here.
# Renames go via deprecated_aliases — never UPDATE-in-place (goclaw migration 58 lesson).
channel_types:
  - name: zoho_cliq
    status: active
  - name: slack
    status: planned
  - name: telegram
    status: planned
  - name: discord
    status: planned
deprecated_aliases: {}   # e.g. zalo_oauth: zalo_oa
```

### Field-number policy

- 1–15 reserved for hot fields (single-byte tag).
- Numbers above 15 ok for less-frequent fields.
- **Never reuse a field number.** Removing a field = `reserved 7;` line.

## Steps

1. **Write the five `.proto` files** with `package mio.v1;` everywhere.
   - [ ] `enums.proto`: every enum's `0` value is `_UNSPECIFIED` (research Q3 — proto3 open-enum default-zero rule).
   - [ ] `message.proto`: `import "google/protobuf/timestamp.proto";` for `received_at` (research Q7 — no Unix int64).
   - [ ] `message.proto`: emit explicit `reserved 17;` (MessageRelation, P5) and `reserved 18;` (is_summary, P6).
   - [ ] `send_command.proto`: emit explicit `reserved 15;` (MessageRelation mirror).
   - [ ] Comment block on `attributes` field in both Message and SendCommand pointing at the promotion rule in `CONTRIBUTING.md`.
2. **Write `proto/channels.yaml`** with `zoho_cliq` (status: active) only. Other platforms get added at adapter time. `deprecated_aliases: {}` placeholder for future renames (no UPDATE-in-place — research Q9 / goclaw migration 58).
3. **Configure `buf.yaml`** (matches the P0 scaffold; restated here for clarity):
   - [ ] `lint.use: [STANDARD]` — v2 canonical name (the v1 `DEFAULT` alias still works but STANDARD is what `buf config ls-lint-rules` reports).
   - [ ] `breaking.use: [WIRE_JSON]` — research Q10 confirms: catches field renames that would break the JSON-encoded GCS sink (P6) and BigQuery external tables (P8). NOT `WIRE` (binary-only) and NOT `FILE` (too strict for normal proto evolution).
4. **Validate the protos**:
   - [ ] `buf lint` clean.
   - [ ] `buf breaking --against '.git#branch=main'` passes (trivial on first run; confirms WIRE_JSON ruleset is wired correctly for future PRs).
   - [ ] Manual grep: every `enum` block has a `*_UNSPECIFIED = 0` first value.
5. **Generate code**: `buf generate` produces `proto/gen/go/mio/v1/` and `proto/gen/py/mio/v1/`. Both must build/import without errors.
6. **Pin protobuf library versions** (research Q7 — prevents Go/Python JSON-serialization skew that bit goclaw):
   - [ ] `tools/proto-roundtrip/go.mod` pins `google.golang.org/protobuf` to a single stable v1.x.
   - [ ] `tools/proto-roundtrip/requirements.txt` pins `protobuf>=4.27.0,<5.0.0`.
7. **Write `tools/proto-roundtrip/main.go`**: connect to local NATS, publish a populated `Message{}` (non-zero `tenant_id`, `account_id`, `channel_type="zoho_cliq"`, `conversation_id`, `conversation_external_id`, `conversation_kind=DM`, `parent_conversation_id`, `source_message_id`, `thread_root_message_id`, `sender`, `received_at`, `attributes` with ≥1 entry), subscribe ephemeral, decode in **both Go and Python**, assert field-by-field equality. Round-trip must also exercise each `reserved` field gap (parse a hand-crafted message that sets unknown field 17 → ensure decoder doesn't blow up). Print `OK`.
8. **Add subject-token validation helper** in the round-trip tool: regex `^[a-zA-Z0-9_-]+$` on every token of `mio.<dir>.<channel_type>.<account_id>.<conversation_id>` before publish (research Q8). Reject dots in any token. This helper is the seed for the SDK's publish-time validator in P2.
9. **Write `CONTRIBUTING.md`** with two short sections (research Q4, Q3):
   - [ ] **`attributes` promotion rule**: any `attributes[key]` read by ≥2 consumers OR written by ≥2 channels gets promoted to a typed proto field with backfill. Code review enforces. Use named constants (`const AttrSlackTS = "slack_ts"`), not string literals.
   - [ ] **`channel_type` registry rule**: new entries go into `proto/channels.yaml` only; renames go via `deprecated_aliases`, never UPDATE-in-place.
10. **Add `make proto-roundtrip` target** (runs both Go and Python halves; either failing fails the build).
11. **Commit**. PR title: `feat(proto): add mio.v1 envelope`.

## Success Criteria

- [ ] `buf lint` clean
- [ ] `buf breaking --against '.git#branch=main'` runs the **WIRE_JSON** ruleset clean (verify with `buf config ls-breaking-rules` listing WIRE_JSON rules, not just WIRE)
- [ ] `buf generate` outputs into `proto/gen/{go,py}` without errors
- [ ] `make proto-roundtrip` exits 0 and prints `OK` for both Go and Python halves
- [ ] Generated Go and Python types both decode the same wire bytes
- [ ] Round-trip test exercises **all** of: tenant_id, account_id, channel_type, conversation_id, conversation_external_id, conversation_kind, parent_conversation_id, source_message_id, thread_root_message_id, attributes (≥1 entry), `received_at` as `google.protobuf.Timestamp`
- [ ] Round-trip test exercises **every reserved-field gap** — sends a message with unknown field 17 and 18 set; both decoders preserve/ignore without erroring (research Q9)
- [ ] Every enum in `enums.proto` and `attachment.proto` has `*_UNSPECIFIED = 0` as the first value (grep-verified)
- [ ] `proto/channels.yaml` parses and `zoho_cliq` is `status: active`; `deprecated_aliases` key present (even if empty)
- [ ] `CONTRIBUTING.md` documents the `attributes` promotion rule and the `channel_type` registry/alias rule
- [ ] Subject-token validator rejects any token with a dot (unit-tested in the round-trip tool)
- [ ] Protobuf library versions are pinned in `tools/proto-roundtrip/go.mod` and `requirements.txt` (no `latest`, no unbounded ranges)

## Risks

- **Field name churn** — wire format is by number, but generated code uses names AND the GCS-sink JSON keys are by name. Mitigation: `WIRE_JSON` ruleset on `buf breaking` blocks rename PRs (research Q10).
- **`channel_type` typos** — registry YAML + CI lint guards against drift across SDK/adapter. Renames flow through `deprecated_aliases` (research Q3 / goclaw migration 58).
- **`attributes` land-grab** — without discipline, channel-specific data leaks into core. Mitigation: promotion rule in `CONTRIBUTING.md` (≥2 consumers/channels → typed field), constants-not-literals convention, periodic audit at P5 (research Q4 / Risk 1 in research report).
- **`conversation_kind` drift between adapters** — generate constants in both Go and Python from one proto file (already covered by `buf generate`). Adapter rejects unknown platform conversation types (returns 4xx) instead of silently mapping to UNSPECIFIED (research Risk 4).
- **Timestamp library skew between Go and Python** — pin `google.golang.org/protobuf` (single stable v1.x) and `protobuf>=4.27.0,<5.0.0` (Python). Round-trip test catches JSON-serialization divergence early (research Q7 / Risk 2).
- **Subject-token poisoning from user-supplied IDs** — workspace names or external IDs containing dots would split NATS subject tokens. Mitigation: validation regex at SDK publish-time and at gateway intake; reject rather than sanitize (research Q8 / Risk 3). Account-lookup cache is P3's concern, not P1's.
- **Idempotency-key shape regrets** — `(channel_type, source_message_id)` looks tempting; it collides for tenants running two workspaces of the same platform. P1 locks the proto in the right shape (`account_id` on Message field 4, `source_message_id` on field 10) so P3's DB schema can drop the unique index without retrofit (research Q6 / goclaw migration 27).
- **Reserved-field reclamation** — a future contributor could "free up" field 17 or 18 not knowing they're earmarked. Mitigation: explicit `reserved 17; reserved 18;` lines in `message.proto` (and `reserved 15;` in `send_command.proto`) plus a comment block linking to P5/P6 (research Q9 / goclaw migration 59).

## Out (deferred — not foundation-blocking)

- `MessageRelation` (edit/reaction/reply linkage) — defer until P5 settles outbound edit semantics. Reserve field 17 in `Message` and field 15 in `SendCommand` for it.
- Cross-channel identity merge (`user_id` resolution) — happens in MIU, not here.
- Compaction `is_summary` flag — reserve field 18 in `Message`.
- Federation / event-sourcing — explicitly out per architecture doc §11.

## Research backing

[`plans/reports/research-260508-1056-p1-proto-envelope-design.md`](../../reports/research-260508-1056-p1-proto-envelope-design.md)

Validated against industry conventions (Slack/Discord/Mattermost/Sendbird/Matrix surveys + Google protobuf style guide + Buf rules + goclaw migration scars). Notable:

- `buf breaking` rule: use **`WIRE_JSON`** (not just `WIRE`). Catches field renames that would break the JSON-encoded archive in GCS sink (P6) and BigQuery external tables (P8). NDJSON consumers care about field names.
- Enum `UNSPECIFIED=0` idiom is standard; Go-generated enums must reject 0 on validation paths.
- `attributes map<string,string>` is the right escape hatch (vs `Any`/`Struct`/JSON-`bytes`/`oneof`). Promotion rule (≥2 channels read same key → typed field) goes into `CONTRIBUTING.md`.
- Idempotency `(account_id, source_message_id)` confirmed against goclaw migration 27 scar (which used `(channel_type, source_id)` and broke on dual-workspace tenants).
- `Sender` embedded (no `user_id` resolution on the wire) confirmed; cross-channel identity merge is a MIU concern.
- 7 `ConversationKind` values cover all 7 surveyed platforms; `BROADCAST` validated against Telegram channels.

Reserved field numbers (write `reserved 17;` etc. into the proto so they
can't be reclaimed by accident):

```proto
message Message {
  // ... fields 1–16 ...
  reserved 17;  // reserved for MessageRelation (P5 outbound edit semantics)
  reserved 18;  // reserved for is_summary (compaction flag, future)
}

message SendCommand {
  // ... fields 1–14 ...
  reserved 15;  // reserved for MessageRelation
}
```
