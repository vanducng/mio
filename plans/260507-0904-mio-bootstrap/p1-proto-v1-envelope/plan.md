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

1. Write the five `.proto` files; `package mio.v1;` everywhere.
2. Write `proto/channels.yaml` with `zoho_cliq` only (others added at adapter time).
3. `buf.yaml` updates: `breaking.use: [FILE]`, `lint.use: [DEFAULT]`.
4. `buf lint` clean → `buf breaking --against '.git#branch=main'` passes (first run trivially).
5. `buf generate` produces `proto/gen/go/mio/v1/` and `proto/gen/py/mio/v1/`.
6. `tools/proto-roundtrip/main.go`: connect to local NATS, publish a populated `Message{}` (with non-zero tenant_id, account_id, conversation_id, conversation_kind=DM, sender etc.), subscribe ephemeral, decode, assert field-by-field equality. Print `OK`.
7. Add `make proto-roundtrip` target.
8. Commit. PR title: `feat(proto): add mio.v1 envelope`.

## Success Criteria

- [ ] `buf lint` clean
- [ ] `buf breaking --against '.git#branch=main'` passes
- [ ] `buf generate` outputs into `proto/gen/{go,py}` without errors
- [ ] `make proto-roundtrip` exits 0 and prints `OK`
- [ ] Generated Go and Python types both decode the same wire bytes
- [ ] Round-trip test exercises **all** of: tenant_id, account_id, channel_type, conversation_id, conversation_external_id, conversation_kind, parent_conversation_id, source_message_id, thread_root_message_id, attributes (≥1 entry)
- [ ] `proto/channels.yaml` parses and `zoho_cliq` is `status: active`

## Risks

- **Field name churn** — wire format is by number, but generated code uses names. Pick clear names first try.
- **`channel_type` typos** — registry YAML + CI lint guards against drift across SDK/adapter.
- **`attributes` land-grab** — without discipline, channel-specific data leaks into core. Code-review rule: any `attributes[...]` read by ≥2 callers gets promoted to a typed proto field with backfill.
- **`conversation_kind` drift between adapters** — generate constants in both Go and Python from one proto file (already covered by `buf generate`).
- **Timestamp library mismatch** — pin `google.golang.org/protobuf` and `protobuf` (Python) versions; document in `tools/proto-roundtrip/go.mod`.

## Out (deferred — not foundation-blocking)

- `MessageRelation` (edit/reaction/reply linkage) — defer until P5 settles outbound edit semantics. Reserve field 17 in `Message` and field 15 in `SendCommand` for it.
- Cross-channel identity merge (`user_id` resolution) — happens in MIU, not here.
- Compaction `is_summary` flag — reserve field 18 in `Message`.
- Federation / event-sourcing — explicitly out per architecture doc §11.

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
