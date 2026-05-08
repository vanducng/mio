---
phase: 9
title: "Second channel adapter"
status: pending
priority: P1
effort: "1d (litmus test)"
depends_on: [8]
---

# P9 — Second channel adapter

## Overview

Add Slack as the second channel. **Litmus test for the abstraction** —
if this takes more than a working day, the proto envelope is wrong and
must be fixed before any third channel is considered.

The bar: zero edits under `proto/mio/v1/`. Adding a channel is **YAML +
adapter**:

1. Add an entry to `proto/channels.yaml` (`status: planned` → `active`).
2. Create `gateway/internal/channels/slack/` with `signature.go`,
   `normalize.go`, `sender.go` — same shape as the `zohocliq/` package.
3. Register the sender in `main.go`.
4. Done.

If step 2 needs a new `Message` field that isn't already covered by the
four-tier scope + `attributes` map, **stop**. The envelope is wrong.

## Goal & Outcome

**Goal:** Slack inbound + outbound shipped end-to-end on GKE in ≤1 working day, with no `proto/mio/v1/*.proto` changes.

**Outcome:** The same echo-consumer (no code change) handles messages from both Cliq and Slack. Subjects scope cleanly: `mio.inbound.slack.>` works just as `mio.inbound.zoho_cliq.>` does. Same Postgres schema (`accounts`, `conversations`, `messages`) handles both.

## Pick

Recommend **Slack** for the second slot:
- Best-documented signing scheme (HMAC-SHA256 with timestamp anti-replay).
- `slack-go` SDK is mature.
- Edits are well-supported (`chat.update`).
- `ConversationKind` maps cleanly: DM, GROUP_DM (mpdm), CHANNEL_PUBLIC (`#public`), CHANNEL_PRIVATE (`#private`), THREAD (via `thread_ts`).
- Most demo value (people recognize it).

Telegram is the alternative if a Slack workspace isn't available.

## Files

- **Create:**
  - `gateway/internal/channels/slack/handler.go` (mirrors `zohocliq/handler.go`)
  - `gateway/internal/channels/slack/signature.go`
  - `gateway/internal/channels/slack/normalize.go` — Slack `event_callback` → `mio.v1.Message`
  - `gateway/internal/channels/slack/sender.go` — implements `sender.Adapter` (P5); `chat.postMessage` for new, `chat.update` for `edit_of_external_id`
  - `gateway/internal/channels/slack/conversation_kind.go` — Slack channel-flavor → `ConversationKind`
  - `gateway/integration_test/slack_inbound_test.go`
  - `gateway/integration_test/slack_outbound_test.go`
  - `gateway/integration_test/fixtures/slack-message-dm.json`
  - `gateway/integration_test/fixtures/slack-message-channel.json`
  - `gateway/integration_test/fixtures/slack-message-thread.json`
- **Modify:**
  - `proto/channels.yaml` — flip `slack` from `status: planned` to `status: active`
  - `gateway/internal/server/server.go` — register `/webhooks/slack` route
  - `gateway/cmd/gateway/main.go` — `dispatcher.Register(slack.NewSender(...))`
  - `gateway/migrations/000002_seed_slack_account.up.sql` — seed one Slack `accounts` row for the demo workspace (idempotent on `(tenant_id, channel_type, external_id)`)
  - `deploy/charts/mio-gateway/values.yaml` — `slack.enabled: true`, secret refs (`SLACK_SIGNING_SECRET`, `SLACK_BOT_TOKEN`)
- **DO NOT MODIFY:**
  - `proto/mio/v1/*.proto` — if you need to, the litmus test failed; stop and audit
  - `sdk-go/`, `sdk-py/` — same SDK serves both channels
  - `examples/echo-consumer/echo.py` — must work unchanged
  - `gateway/internal/sender/dispatch.go` — adapters self-register, no enum branch
  - `gateway/migrations/000001_init.up.sql` — schema is channel-agnostic

## Slack → mio.v1.Message normalize

| Slack field | mio.v1.Message field | Notes |
|---|---|---|
| (resolved at gateway) | `tenant_id` | from `MIO_TENANT_ID` env |
| `team.id` (Slack workspace) → lookup `accounts(tenant_id, channel_type='slack', external_id=team.id)` | `account_id` | account row seeded in migration 000002 for POC |
| literal `"slack"` | `channel_type` | matches `proto/channels.yaml` registry |
| (resolved/upserted in `conversations`) | `conversation_id` | upsert by `(account_id, external_id=channel)` |
| `event.channel` | `conversation_external_id` | Slack channel id (`C…`, `D…`, `G…`) |
| Slack channel-flavor (see below) | `conversation_kind` | DM/GROUP_DM/CHANNEL_PUBLIC/CHANNEL_PRIVATE/THREAD |
| (none — Slack threads share a channel id) | `parent_conversation_id` | empty; `thread_ts` lives in `thread_root_message_id` |
| `event.event_id` | `source_message_id` | `Ev…` — globally unique per Slack event delivery |
| `event.thread_ts` (if set) | `thread_root_message_id` | Slack ties threads to a root `ts`, not a separate channel |
| `event.user` → `Sender.external_id`; `bot_profile`/`users.info` → display_name; bot? → `is_bot` | `sender` | `peer_kind=DIRECT` for DMs, `GROUP` otherwise |
| `event.text` (run through Slack formatting → markdown if needed) | `text` |  |
| `event.files[]` | `attachments` | map kind: `image` → IMAGE, others → FILE; carry `mime`, `filename`, `bytes`, `url_private` |
| anything Slack-specific that doesn't fit | `attributes` | `team_domain`, `client_msg_id`, `subtype`, etc. |

### `ConversationKind` mapping (Slack)

| Slack signal | `ConversationKind` |
|---|---|
| Channel id starts with `D` | `CONVERSATION_KIND_DM` |
| `mpim` event subtype, channel id starts with `G` | `CONVERSATION_KIND_GROUP_DM` |
| Public channel (`is_channel: true`, `is_private: false`) | `CONVERSATION_KIND_CHANNEL_PUBLIC` |
| Private channel (`is_private: true`) | `CONVERSATION_KIND_CHANNEL_PRIVATE` |
| `thread_ts` set and != `ts` | `CONVERSATION_KIND_THREAD` (set the thread's own `conversation_id`) |

If a thread, the gateway creates **two** `conversations` rows the first
time it sees the thread: one for the parent channel, one for the thread
itself. `parent_conversation_id` on the thread row points at the channel.
This matches the polymorphic-conversation rule from the foundation.

## Slack → mio.v1.SendCommand outbound (echo path)

Echo-consumer (P4) runs unchanged. Outbound dispatch (P5) lands in
`slack.Sender`:

| Field | Behavior |
|---|---|
| `cmd.channel_type == "slack"` | dispatcher routes to `slack.Sender` |
| `cmd.conversation_external_id` | Slack `channel` argument |
| `cmd.thread_root_message_id` | Slack `thread_ts` argument (for reply-in-thread) |
| `cmd.text` | Slack `text` |
| `cmd.attachments` | Slack `blocks` / `files` (file upload via `files.upload` only if needed; POC: text-only) |
| `cmd.edit_of_external_id` set | `chat.update` with that `ts` |
| `cmd.edit_of_message_id` set, external missing | resolve via P5 `outbound_state` |

Rate-limit key: per-account default works (one bucket per Slack
workspace). For Slack tier-4 (`chat.postMessage` = 1/sec/channel), the
adapter overrides `RateLimitKey(cmd) = account_id + ":" + conversation_external_id`.

## Steps

1. Flip `proto/channels.yaml` slack entry to `status: active`. Run `make proto-gen` to refresh `sdk-go/channeltypes.go` + `sdk-py/mio/channeltypes.py`. **No `.proto` files touched.**
2. Copy `gateway/internal/channels/zohocliq/` to `slack/` as a template; rename + adapt.
3. Slack signature verification per their docs (`v0` scheme, HMAC-SHA256 over `v0:timestamp:body`, reject if timestamp > 5 min old).
4. Implement normalize per the table above; capture three fixtures: DM, public-channel message, threaded reply.
5. Slack sender: `chat.postMessage` for new, `chat.update` for edits. Use a per-account-conversation rate-limit override (above).
6. Migration 000002: seed one `accounts` row for the demo Slack workspace (parameterized by `SLACK_TEAM_ID` env).
7. Register route in `server.go`; register sender in `main.go`.
8. Integration tests:
   - DM fixture → assert `conversation_kind=DM`, no `parent_conversation_id`.
   - Channel fixture → assert `CHANNEL_PUBLIC`.
   - Threaded fixture → assert two `conversations` rows (channel + thread), thread row has `parent_conversation_id` set, `thread_root_message_id` matches Slack `thread_ts`.
9. Deploy Slack-enabled gateway via Helm upgrade; install Slack app in a workspace; test echo loop end-to-end.

## Success Criteria

- [ ] Slack message → echo reply in same thread, end-to-end on GKE, within 5s
- [ ] **Zero changes to `proto/mio/v1/`** (failure here = envelope wrong, stop)
- [ ] **Zero changes to `sdk-go/` and `sdk-py/`** beyond `channeltypes` codegen
- [ ] **Zero changes to `examples/echo-consumer/echo.py`** — same code handles both channels
- [ ] **Zero changes to `gateway/internal/sender/dispatch.go`** — adapter self-registers
- [ ] DM, channel, and threaded-reply fixtures all normalize to the right `ConversationKind`
- [ ] Two channels in same cluster; no cross-channel interference; Cliq loop still works post-deploy
- [ ] Total wall-clock from "start P9" to "demo working" ≤1 working day
- [ ] Subject `mio.inbound.slack.<account_id>.<conversation_id>` accepted by JetStream and observed by `gcs-archiver` consumer
- [ ] Sink writes to `channel_type=slack/date=YYYY-MM-DD/` partition

## Failure mode

If P9 takes >1 day, **stop and audit the proto envelope**:

- What field did Slack need that Cliq didn't have, and where did you put it (typed field vs `attributes`)?
- What assumption about `ConversationKind` broke?
- Did you have to add a per-channel branch in dispatch or normalize that hints at a missing typed field?
- Did `attributes` accumulate ≥2 channel-specific keys with the same meaning across channels (e.g. both Slack and Cliq use `attributes["edited"]`) — that's a promotion candidate for a new typed field.

Don't bolt on a workaround. Don't add channel-specific extensions to the
envelope. Fix the envelope (which means a new `mio.v2` package — never
mutate `v1`) before considering a third channel. The cost of fixing it
now is one revision; the cost of fixing it after channel #3 is a goclaw
migration.

## Risks

- **Slack rate limits** — tier 4 (`chat.postMessage` = 1/sec/channel); rate-limit bucket per `(account_id, conversation_external_id)`, not per account.
- **Slack thread semantics** — `thread_ts` vs `ts` parity; getting normalize right requires fixtures for DM, channel, threaded reply (all three captured in step 4).
- **Slack OAuth scoping for the install** — POC can use a single Slack app in a single workspace; full multi-tenant install is a MIU admin-console concern.
- **Demo-day Slack workspace availability** — pre-stage one if needed.
- **`event.event_id` retention** — Slack's `event_id` is the right idempotency anchor (not `client_msg_id`, not `ts` — both have edge cases). Verify against Slack docs.
- **Slack edits arriving as `message_changed` event** — these are inbound updates, not the outbound edit path. POC scope: ignore `subtype=message_changed` (write to `attributes` only). Real edit-tracking is a P10+ MessageRelation concern.
