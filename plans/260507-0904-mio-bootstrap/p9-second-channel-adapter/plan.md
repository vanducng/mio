---
phase: 9
title: "Second channel adapter"
status: pending
priority: P1
effort: "1d (litmus test)"
depends_on: [8]
---

# P9 — Second channel adapter (Slack)

## Overview

Add Slack as the second channel. **Litmus test for the abstraction** — if
this takes more than a working day, the proto envelope is wrong and must
be fixed (in a new `mio.v2` package) before any third channel is
considered.

The bar: zero edits under `proto/mio/v1/`, zero edits to `dispatch.go`,
zero edits to `examples/echo-consumer/echo.py`. Adding a channel is
**YAML + adapter package + init() registration**:

1. Flip `proto/channels.yaml` `slack` entry from `status: planned` to
   `status: active`. Run `make proto-gen`.
2. Create `gateway/internal/channels/slack/` mirroring the `zohocliq/`
   shape: `signature.go`, `normalize.go`, `sender.go`,
   `conversation_kind.go`, `init.go`.
3. Add ONE blank import (`_ "…/channels/slack"`) to `main.go`. The
   adapter's `init()` block self-registers via the P5 contract — no
   dispatcher edits.
4. Done.

If step 2 needs a new `Message` field that isn't already covered by the
four-tier scope + `attributes` map, **stop**. The envelope is wrong.

## Goal & Outcome

**Goal:** Slack inbound + outbound shipped end-to-end on GKE in ≤1
working day, with no `proto/mio/v1/*.proto` changes.

**Outcome:** The same echo-consumer (no code change) handles messages
from both Cliq and Slack. Subjects scope cleanly:
`mio.inbound.slack.>` works just as `mio.inbound.zoho_cliq.>` does. Same
Postgres schema (`accounts`, `conversations`, `messages`) handles both.

## Pick

**Slack** for the second slot:
- Best-documented signing scheme (HMAC-SHA256 with timestamp anti-replay).
- `slack-go/slack` SDK is mature (4.3k+ stars, actively maintained).
- Edits are well-supported (`chat.update`).
- `ConversationKind` maps cleanly using **conversation-object boolean
  flags** (DM, GROUP_DM, CHANNEL_PUBLIC, CHANNEL_PRIVATE, THREAD).
- Most demo value (people recognize it).

Telegram is the alternative if a Slack workspace isn't available.

## Files

- **Create:**
  - `gateway/internal/channels/slack/signature.go` — HMAC-SHA256 `v0`
    verification + URL-verification challenge handling
  - `gateway/internal/channels/slack/handler.go` — webhook entry; mirrors
    `zohocliq/handler.go`
  - `gateway/internal/channels/slack/normalize.go` — Slack
    `event_callback` → `mio.v1.Message`
  - `gateway/internal/channels/slack/conversation_kind.go` — boolean-flag
    discriminator for `ConversationKind`
  - `gateway/internal/channels/slack/sender.go` — implements
    `sender.Adapter` (P5); `chat.postMessage` for new, `chat.update` for
    `edit_of_external_id`; per-channel `RateLimitKey` override
  - `gateway/internal/channels/slack/init.go` — `init()` block calls
    `sender.RegisterAdapter(NewSender(...))` per P5 self-registration
    contract (instance form, lives in `sender/registry.go`); **the only
    file `main.go` needs to know about (blank import)**
  - `gateway/internal/channels/slack/manifest.example.yaml` — sample
    Slack app manifest listing the six required scopes + bot events
  - `gateway/integration_test/slack_inbound_test.go`
  - `gateway/integration_test/slack_outbound_test.go`
  - `gateway/integration_test/fixtures/slack-message-dm.json`
  - `gateway/integration_test/fixtures/slack-message-channel.json`
  - `gateway/integration_test/fixtures/slack-message-thread.json`
  - `gateway/integration_test/fixtures/slack-url-verification.json`
  - `gateway/migrations/000002_seed_slack_account.up.sql` — seed one
    Slack `accounts` row for the demo workspace (idempotent on
    `(tenant_id, channel_type, external_id)`)
- **Modify:**
  - `proto/channels.yaml` — flip `slack` from `status: planned` to
    `status: active`
  - `gateway/internal/server/server.go` — register `/webhooks/slack`
    route
  - `gateway/cmd/gateway/main.go` — add ONE blank import
    `_ "…/gateway/internal/channels/slack"`. **No `sender.RegisterAdapter`
    call here** (the adapter's `init()` does it).
  - `deploy/charts/mio-gateway/values.yaml` — `slack.enabled: true`,
    secret refs (`SLACK_SIGNING_SECRET`, `SLACK_BOT_TOKEN`,
    `SLACK_TEAM_ID`)
- **DO NOT MODIFY:**
  - `proto/mio/v1/*.proto` — if you need to, the litmus test failed; stop
    and audit
  - `sdk-go/`, `sdk-py/` (beyond `channeltypes` codegen) — same SDK
    serves both channels
  - `examples/echo-consumer/echo.py` — must work unchanged
  - `gateway/internal/sender/dispatch.go` and `gateway/internal/sender/registry.go` —
    adapters self-register via `init()` calling `sender.RegisterAdapter`;
    no enum branch
  - `gateway/migrations/000001_init.up.sql` — schema is channel-agnostic

## Slack → mio.v1.Message normalize

| Slack field | mio.v1.Message field | Notes |
|---|---|---|
| (resolved at gateway) | `tenant_id` | from `MIO_TENANT_ID` env |
| `team.id` (`T…`) → lookup `accounts(tenant_id, channel_type='slack', external_id=team.id)` | `account_id` | account row seeded in migration 000002 |
| literal `"slack"` | `channel_type` | matches `proto/channels.yaml` registry |
| (resolved/upserted in `conversations`) | `conversation_id` | upsert by `(account_id, external_id=channel)`; threads upsert by `(account_id, external_id=thread_ts)` |
| `event.channel` | `conversation_external_id` | Slack channel id |
| **Conversation-object boolean flags** (see below) | `conversation_kind` | `is_im` / `is_mpim` / `is_channel`+`is_private` / `thread_ts != ts` |
| (set on thread row only) | `parent_conversation_id` | thread row → channel row; channel row's parent is empty |
| `event.event_id` (`Ev…`) | `source_message_id` | **globally unique per event delivery; the idempotency anchor** |
| `event.thread_ts` (if set) | `thread_root_message_id` | Slack ties threads to the root `ts`, not a separate channel |
| `event.user` → `Sender.external_id`; `bot_profile`/`users.info` → `display_name`; `bot_id`/`subtype=bot_message`/`bot_profile` → `is_bot` | `sender` | `peer_kind=DIRECT` for DMs, `GROUP` otherwise |
| `event.text` (raw Slack mrkdwn; conversion is display-layer) | `text` | |
| `event.files[]` | `attachments` | POC: text-only; capture metadata in `attributes` if present, do not upload |
| Slack-specific quirks (`team_domain`, `client_msg_id`, `subtype`, `edited`, `bot_profile`, `blocks`, `reactions`) | `attributes` | JSONB |

### `ConversationKind` mapping (Slack) — **boolean flags, NOT prefixes**

> **Do NOT use channel-id prefix (`C…`/`D…`/`G…`).** The `G…` prefix is
> ambiguous (legacy private channels AND modern MPDM). Shared channels
> can flip C↔G. Use the conversation-object boolean flags returned in
> the event payload's `channel` object (or via `conversations.info`).

| Slack flags | `ConversationKind` |
|---|---|
| `is_im: true` | `CONVERSATION_KIND_DM` |
| `is_mpim: true` | `CONVERSATION_KIND_GROUP_DM` |
| `is_channel: true && is_private: false` | `CONVERSATION_KIND_CHANNEL_PUBLIC` |
| `is_channel: true && is_private: true` (or `is_group: true`) | `CONVERSATION_KIND_CHANNEL_PRIVATE` |
| `thread_ts && thread_ts != ts` | `CONVERSATION_KIND_THREAD` |

**Threads as child conversations.** When a message arrives with
`thread_ts != ts`, the gateway creates **two** `conversations` rows on
first sight: one for the parent channel (kind = CHANNEL_PUBLIC/PRIVATE),
one for the thread itself (kind = THREAD,
`external_id = thread_ts`,
`parent_conversation_id = <channel_conv_id>`). The message row's
`conversation_id` points at the **thread** row. This matches the
polymorphic-conversation rule from the foundation.

## Slack outbound (echo path)

Echo-consumer (P4) runs unchanged. Outbound dispatch (P5) lands in
`slack.Sender` via `init()`-time self-registration:

| `SendCommand` field | Slack behavior |
|---|---|
| `cmd.channel_type == "slack"` | dispatcher routes to `slack.Sender` |
| `cmd.conversation_external_id` | Slack `channel` argument |
| `cmd.thread_root_message_id` | Slack `thread_ts` argument (reply-in-thread) |
| `cmd.text` | Slack `text` |
| `cmd.attachments` | POC: text-only; future: `files.getUploadURLExternal` + `files.completeUploadExternal` |
| `cmd.edit_of_external_id` set | `chat.update` with that `ts` |
| `cmd.edit_of_message_id` set, external missing | resolve via P5 `outbound_state` |

**Rate-limit override (Tier-4).** `chat.postMessage` is tier-4 = 1/sec
per channel. The adapter overrides P5's default `RateLimitKey`:

```go
func (s *SlackSender) RateLimitKey(cmd *miov1.SendCommand) string {
    return cmd.AccountId + ":" + cmd.ConversationExternalId
}
```

P5 already supports per-adapter `RateLimitKey` overrides — no P5
changes. Default per-account limiter (5 tokens/sec, 10 burst) is the
fallback if the adapter doesn't override.

## Steps

1. **Flip channel registry.** Edit `proto/channels.yaml`: `slack` →
   `status: active`. Run `make proto-gen` to regenerate
   `sdk-go/channeltypes.go` + `sdk-py/mio/channeltypes.py`. **No
   `.proto` files touched.**
2. **Copy template.** Copy `gateway/internal/channels/zohocliq/` to
   `slack/` as a starting shape; rename and adapt.
3. **Signature verification (`signature.go`).** Implement Slack's `v0`
   scheme:
   1. Read raw body (do not parse before verify).
   2. Read headers `X-Slack-Signature` and `X-Slack-Request-Timestamp`;
      reject 401 if either missing.
   3. Reject if `|now - timestamp| > 5min` (anti-replay).
   4. Compute `expected = "v0=" + hex(HMAC_SHA256(SLACK_SIGNING_SECRET,
      "v0:" + timestamp + ":" + body))`.
   5. Constant-time compare against `X-Slack-Signature`.
   6. **URL-verification handshake**: if body parses to
      `type=url_verification`, respond with `{"challenge": <challenge>}`
      after signature passes. Slack uses this to validate the webhook
      URL on app install/update.
   7. Metric: `mio_gateway_inbound_total{channel_type="slack",
      outcome="bad_signature"}`.
4. **SDK choice.** Use `slack-go/slack` for `chat.postMessage`,
   `chat.update`, and bot detection. Do not hand-roll the HTTP client
   (KISS — SDK is production-grade).
5. **Bot detection.** In `normalize.go`:
   ```go
   isBot := msg.BotID != "" ||
            msg.SubType == "bot_message" ||
            (msg.BotProfile != nil && msg.BotProfile.ID != "")
   ```
   Modern (GBP) apps set `bot_id` + `bot_profile`; legacy apps set
   `subtype=bot_message`. Use whichever is present.
6. **ConversationKind discriminator (`conversation_kind.go`).** Branch
   on the conversation-object boolean flags per the table above.
   **Never** branch on the channel-id prefix.
7. **Threads.** When `thread_ts != "" && thread_ts != ts`:
   - Upsert channel conversation:
     `(account_id, external_id=channel)` with channel-kind.
   - Upsert thread conversation:
     `(account_id, external_id=thread_ts,
       parent_conversation_id=<channel_conv_id>)` with
     `kind=CONVERSATION_KIND_THREAD`.
   - Message row's `conversation_id` = **thread** conversation id.
   - Message's `thread_root_message_id` = the root message's
     `source_message_id` (i.e., the `event_id` of the root event, if
     known; else carry the raw `thread_ts` in `attributes` until P10
     backfill).
8. **Idempotency.** `source_message_id = event.event_id` (the OUTER
   event envelope's `event_id`, not `client_msg_id`, not `ts`).
   - `client_msg_id` is only set by web/mobile clients on send.
   - `ts` is NOT globally unique (Enterprise Grid collisions).
   - `event_id` is the Slack-recommended dedup anchor.
   - Postgres UNIQUE `(account_id, source_message_id)` already enforces
     at-most-once delivery; resends drop on conflict.
9. **Inbound `message_changed` (subtype).** POC scope: store the raw
   event under `attributes` (`subtype`, `edited`, original/new text). Do
   NOT mutate the prior message row. Real edit-tracking
   (`MessageRelation` with `RELATION_EDIT_OF`) is a P10+ concern.
10. **Files.** POC: text-only. Capture `event.files[]` metadata in
    `attributes` if present; do not upload. Document
    `files.getUploadURLExternal` + `files.completeUploadExternal` (the
    new two-step API) for P10+. Old `files.upload` is sunset Nov 2025.
11. **mrkdwn formatting.** Store raw mrkdwn in `text`. Conversion to
    standard markdown is a display-layer concern (consumer's choice).
    Do not attempt round-trip in the gateway.
12. **Sender (`sender.go`).**
    - `chat.postMessage` for new (set `thread_ts` if reply).
    - `chat.update` for `edit_of_external_id`.
    - Override `RateLimitKey` per (account, conversation) for tier-4.
13. **Self-registration (`init.go`).**
    ```go
    func init() {
        sender.RegisterAdapter(NewSender( /* config from env */ ))
    }
    ```
    Matches P5's locked API in `sender/registry.go` (instance form, not
    string-keyed factory). `main.go` adds **one** blank import
    `_ "…/gateway/internal/channels/slack"`. **Zero edits to
    `dispatch.go`** — the registry holds the adapter list; dispatch reads
    it via `sender.RegisteredAdapters()`.
14. **Webhook route.** In `server.go`, register
    `POST /webhooks/slack` → `slack.Handler`. Each channel owns its own
    route (Slack uses different headers from Cliq).
15. **Migration.** `000002_seed_slack_account.up.sql` — seed one
    `accounts` row for the demo workspace, parameterized by
    `$SLACK_TEAM_ID`:
    ```sql
    INSERT INTO accounts (id, tenant_id, channel_type, external_id,
                          display_name, attributes, created_at)
    VALUES (gen_random_uuid(),
            (SELECT id FROM tenants WHERE slug = 'master'),
            'slack', :slack_team_id, 'Demo Slack Workspace', '{}', NOW())
    ON CONFLICT (tenant_id, channel_type, external_id) DO NOTHING;
    ```
16. **Slack app manifest.** Ship
    `gateway/internal/channels/slack/manifest.example.yaml` with the six
    required scopes (`chat:write`, `channels:history`, `groups:history`,
    `im:history`, `mpim:history`, `users:read`) and bot events
    (`message.channels`, `message.groups`, `message.im`,
    `message.mpim`). Operator copies, fills in webhook URL, installs to
    workspace, retrieves bot token + signing secret.
17. **Integration tests.**
    - DM fixture → `conversation_kind=DM`, no `parent_conversation_id`.
    - Public-channel fixture → `CHANNEL_PUBLIC`.
    - Threaded-reply fixture → **two** `conversations` rows (channel +
      thread), thread row has `parent_conversation_id` set,
      `thread_root_message_id` matches Slack `thread_ts`.
    - URL-verification fixture → 200 `{"challenge": ...}` echo.
    - **Resend test**: post the same fixture twice, assert one
      `messages` row (idempotency on `event_id`).
    - **Rate-limit test**: assert `RateLimitKey` returns
      `account:conversation_external_id` for tier-4 path.
18. **Deploy & demo.** Helm upgrade Slack-enabled gateway; install
    Slack app in workspace using manifest; test echo loop end-to-end on
    GKE.

## Success Criteria

- [ ] **Total wall-clock from "start P9" to "demo working" ≤1 working
      day** (litmus test PASS).
- [ ] **Zero changes to `proto/mio/v1/`** (failure here = envelope wrong,
      stop).
- [ ] **Zero changes to `sdk-go/` and `sdk-py/`** beyond `channeltypes`
      codegen.
- [ ] **Zero changes to `examples/echo-consumer/echo.py`** — same code
      handles both channels.
- [ ] **Zero adapter-specific edits to
      `gateway/internal/sender/dispatch.go`** — adapter self-registers
      via `init()`; `main.go` gets ONE blank import only.
- [ ] **`ConversationKind` detection uses conversation-object boolean
      flags (`is_im`, `is_mpim`, `is_channel`, `is_private`)** —
      verified by integration test; channel-id prefix logic forbidden.
- [ ] **`event.event_id` is the idempotency key** — verified by resend
      test (post same fixture twice, observe one `messages` row;
      duplicate dropped on `(account_id, source_message_id)` UNIQUE).
- [ ] **Threaded fixture creates two `conversations` rows** (channel +
      thread); thread row has `parent_conversation_id` set;
      `thread_root_message_id` matches Slack `thread_ts`.
- [ ] **Rate-limit override** `account_id:conversation_external_id`
      works for tier-4 (`chat.postMessage`); P5 default per-account
      limiter is the fallback.
- [ ] DM, channel, and threaded-reply fixtures all normalize to the
      right `ConversationKind`.
- [ ] URL-verification challenge handshake returns 200 with the
      challenge echoed.
- [ ] Slack message → echo reply in same thread, end-to-end on GKE,
      within 5s.
- [ ] Two channels in same cluster; no cross-channel interference; Cliq
      loop still works post-deploy.
- [ ] Subject `mio.inbound.slack.<account_id>.<conversation_id>` (and
      `…<message_id>` on outbound) accepted by JetStream and observed
      by `gcs-archiver` consumer.
- [ ] Sink writes to `channel_type=slack/date=YYYY-MM-DD/` partition
      (P6 contract auto-inherited; metric label `channel_type="slack"`,
      no underscore — single-word slug).
- [ ] Schema-version envelope check passes via SDK Verify (P2 contract).

## Failure mode

If P9 takes >1 day, **stop and audit the proto envelope**:

- What field did Slack need that Cliq didn't have, and where did you
  put it (typed field vs `attributes`)?
- What assumption about `ConversationKind` broke?
- Did you have to add a per-channel branch in `dispatch.go` or in
  `normalize.go` that hints at a missing typed field?
- Did `attributes` accumulate ≥2 channel-specific keys with the same
  meaning across channels (e.g., both Slack and Cliq use
  `attributes["edited"]`) — that's a promotion candidate for a new
  typed field.

Don't bolt on a workaround. Don't add channel-specific extensions to
the envelope. Fix the envelope (which means a new `mio.v2` package —
**never mutate `v1`**) before considering a third channel. The cost of
fixing it now is one revision; the cost of fixing it after channel #3
is a goclaw migration.

## Risks

- **Thread-root backfill (master-deferred risk).** For Slack threads
  where the parent message arrived before the bot was added,
  `thread_root_message_id` cannot be resolved from the inbound event
  alone. POC carries raw `thread_ts` in `attributes`; the proper backfill
  mechanism (lookup via `conversations.replies` + populate
  `thread_root_message_id` for orphaned threads) is **deferred to P10**
  alongside the `MessageRelation` table. Tracked here per master.md
  Progress Log (2026-05-08 11:30 deferred-risk row).
- **Channel-id prefix discrimination is ambiguous** (`G…` = legacy
  private channel OR modern MPDM; shared channels can flip C↔G).
  *Mitigation:* this phase mandates **conversation-object boolean
  flags** as the only `ConversationKind` discriminator. Prefix logic is
  forbidden by success criteria and called out in code review.
- **`event_id` vs `client_msg_id` confusion.** Slack docs explicitly
  recommend `event_id` for inbound dedup; `client_msg_id` is only set by
  web/mobile clients and is for outbound idempotency at the sender side.
  `ts` is not globally unique (Enterprise Grid collisions).
  *Mitigation:* success criteria requires resend test verifying
  `event_id` is the anchor.
- **MPDM event subtype edge cases.** Some payloads expose `is_mpim`
  only on `conversations.info`, not the inline `channel` object.
  *Mitigation:* if the inline event lacks `is_mpim`, fall back to
  `conversations.info`; cache the result per channel for the test
  duration.
- **Slack tier-4 rate limit** (`chat.postMessage` = 1/sec/channel).
  *Mitigation:* per-channel `RateLimitKey` override; default
  per-account limiter is the safety net.
- **OAuth scope drift.** Six scopes today; Slack may add new
  requirements. *Mitigation:* manifest YAML committed to repo; CI
  doesn't validate (operator concern).
- **Multi-tenant Slack install.** POC uses one Slack app + one
  workspace; full multi-tenant install is an MIU admin-console concern.
- **Demo-day workspace availability.** Pre-stage a free Slack
  workspace before the demo; document setup in
  `manifest.example.yaml` header comment.
- **`message_changed` subtype.** Inbound edits arrive as
  `subtype=message_changed`; POC writes raw event to `attributes` and
  does NOT mutate prior `messages` rows. Real edit tracking is P10+
  (`MessageRelation`).
- **`files.upload` sunset Nov 2025.** POC is text-only; future file
  support uses `files.getUploadURLExternal` +
  `files.completeUploadExternal`. Documented but unimplemented.
- **slack-go SDK version drift.** Pin to a recent stable version; no
  aggressive upgrades during POC.

## Cross-phase consistency contract

- **ConversationKind detection: conversation-object boolean flags, NOT
  channel-id prefixes.** This phase owns the contract.
- **Adapter self-registration via `init()` block** (P5 contract): no
  `dispatch.go` edits.
- **Stream/consumer provisioning**: gateway startup is authoritative;
  Slack adapter does not invent new streams or consumers.
- **Metric labels**: `channel_type="slack"` (no underscore — "slack"
  is a single word; consistent with `zoho_cliq` which uses underscore
  for multi-word slug).
- **Subject grammar**:
  `mio.<dir>.slack.<account_id>.<conversation_id>[.<message_id>]`.
- **Idempotency address**: `(account_id, source_message_id)` where
  `source_message_id = event.event_id`.
- **Schema-version**: enforced on publish via SDK Verify (P2 contract).
  No proto changes (litmus rule).
- **Filename scheme (sink-gcs)**: partition
  `channel_type=slack/date=…` automatically inherited from P6 contract.

## Research backing

[`plans/reports/research-260508-1056-p9-slack-adapter-second-channel.md`](../../reports/research-260508-1056-p9-slack-adapter-second-channel.md)

**Litmus test PASSES** per research (15 questions, all resolve cleanly
with the existing envelope). Critical findings integrated above:

- **`ConversationKind` mapping uses boolean flags, not prefixes.** The
  `G…` prefix is ambiguous; shared channels can flip C↔G; flags are
  authoritative.
- **Idempotency anchor: `event.event_id`** (globally unique per event
  delivery). Not `client_msg_id` (only set by web/mobile clients), not
  `ts` (changes on edit-replay; Enterprise Grid collisions).
- **Threads as child conversations**: two `conversations` rows on first
  sight (parent channel + thread), `parent_conversation_id` links them.
- **`slack-go/slack` SDK** is production-grade for `chat.postMessage` +
  `chat.update` + bot-message detection.
- **Tier-4 rate limit override**:
  `RateLimitKey = account_id + ":" + conversation_external_id`. P5
  already supports per-adapter override.
- **POC scope**: text-only; ignore `message_changed` (write raw to
  `attributes`); skip `files.upload` flow; store raw mrkdwn (display-
  layer concern).
- **OAuth scopes** (six): `chat:write`, `channels:history`,
  `groups:history`, `im:history`, `mpim:history`, `users:read`. Manifest
  YAML shipped in adapter package.
- **No proto changes needed.** All Slack-specific quirks fit in
  `attributes` JSONB. No promotion candidates yet (need ≥2 channels
  using the same key with the same semantics).
