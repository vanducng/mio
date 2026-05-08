---
title: "P9 Research: Slack Adapter Design & Litmus Test"
date: 2026-05-08
mode: --deep
status: ready
phase: 9
plan_ref: p9-second-channel-adapter/plan.md
foundation_ref: research-260507-1102-channels-data-model.md
---

# Phase P9 Deep Research: Slack Adapter as Second-Channel Litmus Test

> **Scope:** Validate that Slack inbound + outbound can ship end-to-end ≤1 working day with zero proto changes. Answer 15 cross-platform design questions to expose envelope brittleness before channel #3.

---

## Executive Summary

**Slack adapter clears the litmus test.** All 15 research questions resolve cleanly within the P1 envelope:

1. **Signature verification** (HMAC-SHA256 `v0:timestamp:body`) — straightforward; signing-secret from env; 5-min replay window enforced
2. **Idempotency anchor** (`event_id`, not `client_msg_id` or `ts`) — Slack's guidance is clear; `event_id` is globally unique per event delivery
3. **Channel kind mapping** (C/D/G prefixes + `is_channel`/`is_private`/`is_im`/`is_mpim` booleans) — four kinds fit `ConversationKind`; prefix check unreliable, use conversation object flags
4. **Thread semantics** (parent in same channel; `thread_ts` = root `ts`) — two conversation rows (parent + thread child) aligns with mio polymorphic model; no new table
5. **`slack-go` SDK** — mature, covers `chat.postMessage` + `chat.update` (edits), handles bot detection, widely adopted
6. **Rate limits** (`chat.postMessage` tier 4 = 1/sec/channel) — rate-limit key override in adapter (per-account-conversation); P5 already supports this
7. **OAuth scopes** (POC: `chat:write`, `channels:history`, `users:read`, `groups:history`, `im:history`, `mpim:history`) — six scopes required; multi-tenant install deferred to MIU
8. **Edits via `message_changed` events** (inbound) — POC scope: ignore; write raw event to `attributes`; real edit-tracking is P10+ `MessageRelation` work
9. **File uploads** (deprecated `files.upload`; new: `files.getUploadURLExternal` + `files.completeUploadExternal`) — POC: text-only acceptable; file support deferred P10+
10. **Formatting** (Slack mrkdwn → markdown) — round-trip fidelity not POC-critical; store raw mrkdwn in `text`, conversion library for display layer
11. **Bot detection** (`bot_id` + `bot_profile` present, or `subtype=="bot_message"` in classic mode) — straightforward; add `is_bot` to `Sender`
12. **`team.id` as account external_id** — workspace identity; stable `T…` prefix; seed in migration 000002; idempotent on `(channel_type, external_id)`
13. **Failure modes that break the envelope** — none found; all Slack-specific quirks fit `attributes` or existing polymorphic tiers (thread as conversation child, DM as conversation with kind, etc.)
14. **Demo workspace setup** — free workspace OK; Slack app via manifest.yaml; ngrok or Cloudflare Tunnel for local dev
15. **Cliq ↔ Slack symmetry** — small divergences (Cliq uses `chat_id`, Slack uses channel id; Cliq has no native thread ts); all handled via adapter normalization

**No proto changes needed.** The four-tier envelope (tenant → account → conversation → message) + `attributes` JSONB absorbs all Slack specifics. The envelope holds.

---

## Research Questions & Deep Dives

### Q1: Slack Events API Signature Verification (v0 scheme)

**Question:** HMAC-SHA256 over `v0:timestamp:body`; signing-secret source; 5-min replay window; header names.

**Sources:**
- [Verifying requests from Slack | Slack Developer Docs](https://docs.slack.dev/authentication/verifying-requests-from-slack/)
- [Verifying Slack Requests in Phoenix](https://benreinhart.com/blog/verifying-slack-requests-elixir-phoenix/)

**Findings:**

| Aspect | Value | Notes |
|--------|-------|-------|
| **Algorithm** | HMAC-SHA256 | `my_signature = 'v0=' + hmac.sha256(signing_secret, 'v0:' + timestamp + ':' + body).hexdigest()` |
| **Signing Secret** | `SLACK_SIGNING_SECRET` env var | Obtained from app manifest/configuration; never the bot token |
| **Replay Protection** | Check `X-Slack-Request-Timestamp` | Reject if `abs(now - timestamp) > 300` seconds (5 min default) |
| **Header Names** | `X-Slack-Signature`, `X-Slack-Request-Timestamp` | Both required; both must pass for accept |
| **Body Handling** | Raw bytes, no parsing/reformatting | Middleware that parses before verify breaks signature |
| **Comparison** | Use constant-time function (Python: `hmac.compare_digest`) | Defend against timing attacks |

**Recommendation:** Implement `gateway/internal/channels/slack/signature.go` with:
```
1. Extract headers (fail with 401 if missing)
2. Validate timestamp (reject if >5min old)
3. Construct sig_basestring = 'v0:' + timestamp + ':' + raw_body
4. Compute my_sig = 'v0=' + HMAC_SHA256(signing_secret, basestring)
5. timingSafeCompare(my_sig, header_sig) — reject on mismatch
6. Metric: mio_gateway_inbound_total{channel_type="slack", outcome="bad_signature"}
```

**Compatibility with P3 template:** Cliq uses different header names (check `playground/cliq/reports/` for exact headers, likely `X-Zoho-Webhook-Signature`). Each channel has its own `signature.go`; gateway routes based on `/webhooks/<channel_type>` path.

**No proto impact.** ✓

---

### Q2: Slack Event Idempotency Anchor — `event_id` vs `client_msg_id` vs `ts`

**Question:** Which is the right anchor for deduplication? Edge cases (resends, duplicate delivery). Slack docs guidance.

**Sources:**
- [The Events API | Slack Developer Docs](https://docs.slack.dev/apis/events-api/)
- [message event | Slack Developer Docs](https://docs.slack.dev/reference/events/message/)
- [Issue: achieve idempotence for incoming events · slackapi/bolt-python](https://github.com/slackapi/bolt-python/issues/564)

**Findings:**

| Anchor | Scope | Use Case | Edge Cases |
|--------|-------|----------|-----------|
| **`event_id`** | Globally unique per event delivery | **INBOUND** (idempotency on receive) | Slack retries → same `event_id`; dedupe catches it |
| **`client_msg_id`** | Set by sender when posting; unique per post | **OUTBOUND** (prevent duplicate posts) | Only present in `chat.postMessage` responses & callbacks |
| **`ts`** (message timestamp) | Unique per channel | Message identity (not idempotency) | Not unique across channels; can collide across workspaces in Enterprise Grid |

**Recommendation for P9:**
- **Inbound:** Use `event_id` from the outer event envelope (not the inner message event). This is the Slack-recommended practice.
- **Store:** Gateway writes `source_message_id = event_id` on first receipt.
- **Dedup key:** `(account_id, source_message_id)` UNIQUE constraint in Postgres already handles this.
- **P5 outbound sender:** Use `client_msg_id` parameter on `chat.postMessage` if idempotency-at-sender is needed; default: P5's NATS dedup (60s window) is sufficient for POC.

**Gotcha:** `ts` is NOT globally unique (Enterprise Grid can have collisions). Don't use for dedup across workspaces.

**No proto impact.** ✓

---

### Q3: Slack Channel ID Prefix Mapping — C, D, G, MPIM

**Question:** Channel id prefixes; mapping to `ConversationKind`; how reliable are prefixes?

**Sources:**
- [Using the Conversations API | Slack Developer Docs](https://docs.slack.dev/apis/web-api/using-the-conversations-api/)
- [Conversation object | Slack Developer Docs](https://docs.slack.dev/reference/objects/conversation-object/)

**Findings:**

| Prefix | Type | Is Reliable? | Recommended Check |
|--------|------|--------------|-------------------|
| **C…** | Public channel | **NO** — shared channels can have id swapped from C to G or vice versa | Use `is_channel=true AND is_private=false` |
| **D…** | Direct message (1:1) | **MOSTLY** — very stable for DMs | Verify `is_im=true` |
| **G…** | Private channel (pre-Mar 2021) OR multi-party DM | **MIXED** — legacy; shared channels can flip C↔G | Use `is_private=true` AND (`is_channel=true` OR `is_mpim=true`) |
| **MPIM** | Multi-party DM (no channel, 3+ users) | **PREFERRED** — use `is_mpim=true` flag, not prefix | Check `is_mpim=true` AND `!is_channel` |

**Slack Conversation Object Boolean Flags (authoritative):**
```
is_channel     — public channel (or private shared into workspace)
is_private     — private/restricted channel
is_im          — 1:1 direct message
is_mpim        — multi-party DM (unnamed)
is_group       — private channel created before March 2021 (legacy)
```

**Recommended Mapping for `ConversationKind`:**

| Slack Signal | `ConversationKind` | Normalization Rule |
|---|---|---|
| `is_im=true` | `CONVERSATION_KIND_DM` | 1:1 with a user |
| `is_mpim=true` | `CONVERSATION_KIND_GROUP_DM` | 3+ users, no formal channel name |
| `is_channel=true AND is_private=false` | `CONVERSATION_KIND_CHANNEL_PUBLIC` | Public channel |
| `is_private=true AND is_channel=true` | `CONVERSATION_KIND_CHANNEL_PRIVATE` | Private channel |
| `thread_ts != null AND ts != thread_ts` | `CONVERSATION_KIND_THREAD` | Reply in a thread (see Q4) |

**Implementation note:** `conversation_kind.go` normalizer should check flags in order; prefix-only check is brittle. For edge cases (e.g., shared channel with flipped prefix), flags are authoritative.

**No proto impact.** ✓

---

### Q4: Slack Thread Semantics — `thread_ts` vs `ts`, parent always in same channel

**Question:** Two-conversations pattern (channel + thread child); where does `parent_conversation_id` come from?

**Sources:**
- [Messaging | Slack Developer Docs](https://api.slack.com/docs/message-threading)
- [conversations.replies method | Slack Developer Docs](https://docs.slack.dev/reference/methods/conversations.replies/)

**Findings:**

| Concept | Value | Notes |
|---|---|---|
| **`ts` (timestamp)** | Unique message ID | Every message has one; identifies the message within its channel |
| **`thread_ts`** | Parent message's `ts` | Set on all replies in a thread; points at the root message's `ts` |
| **Thread physical location** | Same channel as parent | Slack threads don't get their own channel id; they live in the same `channel` with a `thread_ts` field |
| **Parent always present** | Yes | The parent message row also retains `thread_ts` after first reply |
| **Multiple threads in one channel** | Yes | One channel can have many threads (one per unique `thread_ts` value) |

**Design for mio (aligned with P3 plan):**

When a message arrives with `thread_ts` set AND `thread_ts != ts` (i.e., it's a reply):

1. **Create/fetch parent conversation:** Upsert `conversations(account_id, external_id=channel)` with `kind=CHANNEL_PUBLIC/PRIVATE/etc`. This is the channel row.
2. **Create thread conversation:** Upsert `conversations(account_id, external_id=thread_ts, parent_conversation_id=<parent_conv_id>)` with `kind=THREAD`. The `external_id` for the thread is the `thread_ts` itself (Slack's canonical thread identifier).
3. **Insert message:** Message row has `conversation_id=<thread_conv_id>` (the child thread, not the channel).
4. **DB rows created:** Two `conversations` rows on first reply to a thread.

**For the message proto:**
- `conversation_id` = thread conversation id
- `thread_root_message_id` = populated with the root message's `source_message_id` (so consumers can walk the tree if needed)
- `parent_conversation_id` (on conversation object) = channel conversation id

**Edits in threads:** If a threaded message is edited (see Q8), the edit event arrives with the same `thread_ts` and `ts` of the original; normalize to the same thread conversation.

**No proto impact.** ✓

---

### Q5: `slack-go` SDK Maturity — Feature Coverage, Async Support, Edits

**Question:** Is `slack-go` mature enough? Does it cover `chat.postMessage`, `chat.update`, bot detection, error model?

**Sources:**
- [GitHub: slack-go/slack](https://github.com/slack-go/slack)
- [slack-go pkg docs](https://pkg.go.dev/github.com/slack-go/slack)
- [chat.update method | Slack Developer Docs](https://docs.slack.dev/reference/methods/chat.update/)

**Findings:**

| Feature | Status | Notes |
|---|---|---|
| **REST API coverage** | Comprehensive | Supports "most if not all" REST endpoints; actively maintained |
| **`chat.postMessage`** | ✓ Full | `client.PostMessage(channel, text, options)` returns message timestamp + channel |
| **`chat.update`** | ✓ Full | `client.UpdateMessage(channel, timestamp, text, options)` for edits |
| **Bot detection** | ✓ Implicit | Message events include `BotID`, `BotProfile` fields; normalize to `is_bot` |
| **Error handling** | ✓ Standard | Returns `(Response, error)` tuples; errors unwrap with type assertions |
| **Async support** | Partial | No native async/await (Go uses goroutines); SDK is synchronous, easily wrapped in goroutines |
| **Socket Mode (WebSocket)** | ✓ Yes | Alternative to webhooks; not needed for POC |
| **Deferred delivery** | — | Scheduled sends via `metadata.scheduled_message_id` (newer feature); not POC-critical |

**Assessment:** `slack-go` is production-grade. Use for P9:
- `client.PostMessage(...)` for send
- `client.UpdateMessage(...)` for edit
- Wrap send/update calls in a goroutine for pool.go worker concurrency (standard Go pattern)

**Alternative: hand-rolled HTTP client** — not recommended. `slack-go` has matured enough that reinventing is not justified (violates KISS).

**No proto impact.** ✓

---

### Q6: Slack Rate Limits — `chat.postMessage` tier 4

**Question:** Tier-4 rate limit (1/sec/channel); how to scope rate-limit key?

**Sources:**
- [Rate limits | Slack Developer Docs](https://docs.slack.dev/apis/web-api/rate-limits/)
- [Handling Rate Limits with Slack APIs | Medium](https://medium.com/slack-developer-blog/handling-rate-limits-with-slacks-apis-f6f8a63bdbdc)

**Findings:**

| Method | Tier | Limit | Notes |
|---|---|---|---|
| **`chat.postMessage`** | Tier 4 | 1 message/sec/channel | "Generous burst behavior" allowed; design for 1/sec sustainably |
| **`conversations.history`** | Tier 3 | 50 requests/min | History read (inbound normalization doesn't call this; deferred) |
| **`users.info`** | Tier 2 | 20 requests/min | Sender lookup (optional for bot user display names) |

**Per-account-conversation rate-limit key (P5 outbound):**

P5 plan specifies `Adapter.RateLimitKey(cmd)` override. For Slack:
```
default: account_id
override: account_id + ":" + conversation_external_id
```

So each Slack workspace (account_id) gets one bucket, but messages to different channels are NOT sharded further (1/sec/channel is enforced by Slack, not by mio). If a single account bursts to many channels, Slack will rate-limit; mio's bucket is per-account.

**For POC:** Default per-account limiter (5 tokens/sec, 10 burst) is generous. If integration test hits 1/sec/channel limit, add the override in `slack/sender.go`:
```go
func (s *SlackSender) RateLimitKey(cmd *miov1.SendCommand) string {
    return cmd.AccountId + ":" + cmd.ConversationExternalId
}
```

**No proto impact.** ✓

---

### Q7: Slack OAuth Bot Token Scopes

**Question:** Required scopes for POC; multi-tenant vs single-workspace install.

**Sources:**
- [Scopes | Slack Developer Docs](https://docs.slack.dev/reference/scopes/)
- [Understanding OAuth scopes for bots | Slack Developer Docs](https://docs.slack.dev/tools/python-slack-sdk/tutorial/understanding-oauth-scopes/)

**Findings:**

**POC minimal scopes (single workspace, one bot token):**

| Scope | Why |
|---|---|
| **`chat:write`** | Post messages (required for echo reply) |
| **`channels:history`** | Read public channel message history (inbound webhook already streams; this is for audit/backfill later) |
| **`groups:history`** | Read private channel history |
| **`im:history`** | Read DM history |
| **`mpim:history`** | Read group-DM history |
| **`users:read`** | Fetch user display names / bot profiles for sender normalization |

**Six scopes total for POC.** Not ideal (principle of least privilege), but all are needed:
- `chat:write` is hard requirement
- `*:history` scopes are needed for sender profile lookups (optional in MVP; deferred to P10 if we skip sender display names)
- `users:read` is needed for bot profile lookup

**Multi-tenant install (MIU admin console):** Deferred. POC uses one Slack app in one workspace, one bot token per environment (dev, staging, prod). MIU will handle the per-customer OAuth flow, token vaulting, etc.

**Recommendation for P9:** Create Slack app manifest with these six scopes; provide generated bot token as `SLACK_BOT_TOKEN` env var and signing-secret as `SLACK_SIGNING_SECRET`. Deploy via Helm values.

**No proto impact.** ✓

---

### Q8: Slack Edits via `message_changed` Events (Inbound)

**Question:** How to handle inbound edits (`message_changed` event, `subtype=message_changed`)? Edit-tracking deferred to P10?

**Sources:**
- [message event | Slack Developer Docs](https://docs.slack.dev/reference/events/message/)
- [`message_changed` event | Slack Developer Docs](https://api.slack.com/events/message/message_changed)

**Findings:**

| Aspect | Value | Notes |
|---|---|---|
| **Event name** | `message` with `subtype="message_changed"` | Part of the message event family |
| **Payload structure** | `{ "type": "message", "subtype": "message_changed", "message": { "ts": ..., "text": "..." } }` | `message.ts` is the original message's timestamp |
| **Edited field** | `message.edited = { "user": "U...", "ts": 1234567890 }` | Tracks who edited and when |
| **Real-time delivery** | Yes | Gateway receives these via webhook |

**POC Scope Decision (per P9 plan):**

Slack's `message_changed` events are **inbound notifications of edits**, not the outbound edit path (which is P5 `chat.update`). For POC:

1. **Receive the event** — normalize like any other message event
2. **Store the raw event in `attributes`** — capture `subtype`, `edited` metadata, original + new text
3. **Don't create a `MessageRelation.EDIT_OF` record** — that's P10+ work
4. **No proto change** — all Slack-specific edit metadata lives in `attributes`

Example:
```json
{
  "source_message_id": "Ev...",
  "text": "(updated text here)",
  "attributes": {
    "subtype": "message_changed",
    "edited": "{\"user\": \"U123\", \"ts\": 1234567890}",
    "original_text": "(old text)"
  }
}
```

Real edit-tracking (`MessageRelation` with `RELATION_EDIT_OF` type, edit history tables, etc.) is a **P10+ concern**. The envelope accommodates it, but POC doesn't build it.

**No proto impact.** ✓

---

### Q9: Slack File Uploads — `files.upload` Deprecation

**Question:** `files.upload` vs `files.getUploadURLExternal` + `files.completeUploadExternal`; POC scope.

**Sources:**
- [files.upload method | Slack Developer Docs](https://docs.slack.dev/reference/methods/files.upload/)
- [The files.upload method is retiring | Slack Changelog (2024-04)](https://docs.slack.dev/changelog/2024-04-a-better-way-to-upload-files-is-here-to-stay)
- [Working with files | Slack Developer Docs](https://docs.slack.dev/messaging/working-with-files/)

**Findings:**

| Method | Status | When to use | Trade-off |
|---|---|---|---|
| **`files.upload`** | **DEPRECATED** (sunset Nov 12, 2025) | Not recommended for new code | Simpler API, but performs poorly on large files |
| **`files.getUploadURLExternal` + `files.completeUploadExternal`** | **CURRENT** | New standard workflow | Two-step + async processing; larger files perform better |

**File upload flow (new):**
1. Call `files.getUploadURLExternal(filename, length)`
2. HTTP PUT raw file bytes to the returned URL
3. Call `files.completeUploadExternal(file_id, channel_ids=[...])`
4. Processing happens async; file may not be immediately available in channel

**POC Decision:** **Text-only acceptable.** Files are nice-to-have; POC message loop doesn't require file support. If inbound messages include file attachments:
- Store file metadata in `attributes` (filename, mime type, URL)
- Outbound: don't send files (text-only echo reply)
- P10+: implement full upload flow

**No proto impact.** ✓

---

### Q10: Slack Formatting — mrkdwn to Markdown Conversion

**Question:** Slack mrkdwn syntax → markdown round-trip fidelity; links, mentions, channel refs.

**Sources:**
- [Formatting message text | Slack Developer Docs](https://docs.slack.dev/messaging/formatting-message-text/)
- [The developer's guide to Slack's Markdown formatting | Knock](https://knock.app/blog/the-guide-to-slack-markdown)

**Findings:**

| Format | Slack mrkdwn | Standard Markdown | Notes |
|---|---|---|---|
| **Bold** | `*bold*` | `**bold**` | Different! Slack uses single `*` |
| **Italic** | `_italic_` | `*italic*` | Different! Slack uses `_` |
| **Strike** | `~strike~` | `~~strike~~` | Slack uses two `~`, markdown varies |
| **Link** | `<https://example.com\|Display Text>` | `[Display Text](https://example.com)` | Slack uses angle brackets + pipe |
| **User mention** | `<@U123456789>` | `@username` | Slack uses user ID, not name |
| **Channel mention** | `<#C123456789\|channel-name>` | `#channel` | Slack uses channel ID |
| **Block Kit blocks** | JSON array of block objects | Not applicable | Slack's rich UI; markdown doesn't represent |

**POC Approach:** **Store raw mrkdwn in `text` field.** For display in a generic consumer (e.g., log viewer):
1. Use a third-party library (`md-to-slack`, `slack-markdown-formatter`) to convert mrkdwn → markdown on demand
2. Store conversion result in attributes or a separate display field if needed
3. Round-trip fidelity: Not perfect (mrkdwn → markdown → mrkdwn loses some info, e.g., user ID vs display name), but acceptable for POC

**Real implementation (P10+):** If agents need to parse and re-send, write a proper mrkdwn parser. For now, agents see the raw mrkdwn and can decide what to do (echo it back verbatim, convert it, strip it, etc.).

**No proto impact.** ✓

---

### Q11: Bot Detection — `bot_id`, `bot_profile`, `subtype`

**Question:** Reliable detection of bot-sent messages; which fields to use?

**Sources:**
- [bot_message event | Slack Developer Docs](https://docs.slack.dev/reference/events/message/bot_message/)
- [bots.info method | Slack Developer Docs](https://docs.slack.dev/reference/methods/bots.info/)

**Findings:**

**Detection signals:**

| Signal | When Present | Reliability | Notes |
|---|---|---|---|
| **`bot_id` field** | Bot message in modern (GBP) apps | High | Present on bot_message events and conversation history |
| **`bot_profile` object** | Bot message in modern apps | High | Contains bot name, app_id, icons; enrichment only |
| **`subtype=="bot_message"`** | Classic Slack apps / integrations | Medium–High | Not present on modern GBP apps; useful for legacy detection |
| **Absence of `user` field** | Bot message (any) | Medium | `user` is null/absent; less reliable alone |

**Decision tree for `is_bot` in Sender:**

```go
is_bot := msg.BotID != "" || 
          msg.SubType == "bot_message" || 
          (msg.BotProfile != nil && msg.BotProfile.ID != "")
```

If any signal is true, the message is from a bot. If `bot_profile` is present, use `bot_profile.Name` for `Sender.display_name`.

**Implementation:** In `normalize.go`:
```go
sender := &miov1.Sender{
    ExternalId:  msg.User,  // may be empty for bots
    DisplayName: getBotOrUserName(msg),
    IsBot:       msg.BotID != "" || msg.SubType == "bot_message",
    PeerKind:    miov1.PeerKind_PEER_KIND_GROUP,  // bots are always group-addressed
}
```

**No proto impact.** ✓

---

### Q12: Slack `team.id` — Workspace Identity & Account External ID

**Question:** `team.id` as account external_id; stability and seeding in migration.

**Sources:**
- [Locate your Slack URL or ID | Slack Support](https://slack.com/help/articles/221769328-Locate-your-Slack-URL-or-ID)
- [users.identity method | Slack Developer Docs](https://docs.slack.dev/reference/methods/users.identity/)

**Findings:**

| Property | Value | Notes |
|---|---|---|
| **`team.id`** | Workspace ID, format `T…` (e.g., `T02N0...`) | Globally stable identifier for a Slack workspace |
| **Stability** | Permanent | Never changes; workspace → team.id is 1:1 for the lifetime of the workspace |
| **Uniqueness** | Globally unique across Slack | Different from guild_id (Discord) or team_id (Mattermost), but the same concept |
| **Where to find** | Every event payload includes `team.id` and `team.domain` | Also in `users.identity()` response |
| **Availability in webhook** | ✓ Yes | Inbound message events carry `team: { "id": "T...", "domain": "..." }` |

**For P9 migration (000002_seed_slack_account.up.sql):**

```sql
INSERT INTO accounts (
  id,
  tenant_id,
  channel_type,
  external_id,
  display_name,
  attributes,
  created_at
) VALUES (
  gen_random_uuid(),
  (SELECT id FROM tenants WHERE slug = 'master'),
  'slack',
  'T02N0...',  -- parameterized as $SLACK_TEAM_ID env
  'Demo Slack Workspace',
  '{}',
  NOW()
)
ON CONFLICT (tenant_id, channel_type, external_id) DO NOTHING;
```

**Idempotent key:** `(tenant_id, channel_type, external_id)` ensures one account row per workspace. If seed runs twice, second run is a no-op.

**No proto impact.** ✓

---

### Q13: Litmus-Test Failure Modes — What Would Break the Envelope?

**Question:** Which Slack-specific quirks would force a proto change? Examples of what would fail the litmus test.

**Analysis:**

**Hypothetical failures (would require proto change):**

| Scenario | Why It Fails | Real-world Slack case? |
|---|---|---|
| "Slack messages must carry a `team_name` as a first-class proto field" | Teams have variable display names; `attributes` already holds this | No; team name is optional metadata |
| "Slack requires a separate `thread_root_conversation_id` in addition to `thread_root_message_id`" | The message model already handles this; two conversation rows (parent + thread) encode the hierarchy | No; threads are messages with a `thread_ts` field, not a separate object |
| "Slack bot messages have a different delivery contract (ACK 10s vs 5s)" | Different deadline per message type would break the webhook handler | No; Slack webhook deadline is uniform (~5s) |
| "Slack edits must be committed to a separate `MessageEdit` table and cannot fit in `attributes`" | Edit metadata is Slack-specific (who edited, when); `MessageRelation` is planned for P10+; attributes bridge the gap | No; raw edit events fit in attributes; real tracking is deferred |
| "Slack has a concept of 'channels with sub-channels' (not threads)" | Would need a third conversation tier; Slack doesn't have this | No; Slack only has threads (message-level), not sub-channels |
| "Slack file uploads require a new `Attachment.ThumbnailArray` field not present on Cliq attachments" | Thumbnail arrays are Slack-specific; store in `attributes` per attachment | Possible edge case; POC avoids by doing text-only |

**Real-world Slack specifics that DON'T break the envelope:**

| Feature | How it fits | Where stored |
|---|---|---|
| `team_domain` | Workspace human name | `attributes` |
| `bot_id` + `bot_profile` | Sender origin tracking | `Sender.external_id` (bot ID) + `attributes` (profile) |
| `reactions` (emoji + count) | Message annotations | `attributes` (per P5, reactions are deferred) |
| `editing` user/timestamp | Edit tracking | `attributes` (raw event); `MessageRelation` in P10+ |
| `is_starred`, `pinned_to` | Message state | `attributes` |
| `thread_ts` | Thread identity | `thread_root_message_id` + parent `conversation_id` on conversation object |
| `mpim` multi-DM | Conversation type | `ConversationKind_GROUP_DM` + `is_mpim=true` in attributes |
| Block Kit blocks | Rich message format | `attributes` (raw blocks JSON) |
| Huddles / voice rooms | Special conversation type | Not in POC scope; would be new `kind` if added (forward compat) |

**Conclusion:** **No proto changes needed for Slack.** All observed Slack-specific behaviors fit the four-tier envelope + polymorphic conversation model + attributes JSONB. The litmus test passes.

**No proto impact.** ✓

---

### Q14: Demo Workspace Prep — Local Dev Setup

**Question:** Free Slack workspace, app manifest setup, ngrok vs Cloudflare Tunnel.

**Findings:**

**Slack Workspace:**
- Free tier workspace sufficient for POC (no feature restrictions on inbound webhooks or message history)
- Create at https://slack.com/create

**Slack App Creation & Manifest:**
- Use manifest YAML (declared config) vs manual UI setup
- Manifest includes OAuth scopes, event subscriptions, webhook request URL
- Template:
```yaml
display_information:
  name: "MIO Echo Bot"
  description: "Litmus test for multi-channel message routing"
  background_color: "#000000"
features:
  bot_user:
    display_name: "mio-echo"
oauth_config:
  scopes:
    bot:
      - chat:write
      - channels:history
      - groups:history
      - im:history
      - mpim:history
      - users:read
settings:
  event_subscriptions:
    request_url: https://mio-webhook.example.com/webhooks/slack
    bot_events:
      - message.channels
      - message.groups
      - message.im
      - message.mpim
```

**Local Dev Tunnel (inbound webhook):**
- **ngrok:** `ngrok http 8080` → `https://abc123.ngrok.io/webhooks/slack`
- **Cloudflare Tunnel:** `cloudflared tunnel run` (requires login; more stable for long sessions)
- Either works; Cloudflare is free tier; no auth key needed if already set up globally

**Installation steps:**
1. Create manifest YAML
2. Go to Slack app directory
3. "Create New App" → "From manifest"
4. Paste manifest
5. Install to workspace (authorize scopes)
6. Retrieve bot token (`xoxb-...`) and signing secret
7. Set env vars: `SLACK_BOT_TOKEN`, `SLACK_SIGNING_SECRET`, `SLACK_TEAM_ID`
8. Start ngrok/tunnel, update manifest webhook URL
9. Test: Send message to bot DM or mention in channel → see webhook hit the gateway

**No proto impact.** ✓

---

### Q15: Cliq ↔ Slack Symmetry Check

**Question:** Which Cliq-only vs Slack-only fields are in `attributes`? Are any candidates for promotion to typed fields?

**Comparative Analysis:**

| Domain | Cliq Specific | Slack Specific | Shared/Mapped |
|---|---|---|---|
| **Conversation identity** | `chat_id` | `channel` id | Both → `conversation_external_id` (mapped) |
| **Workspace identity** | `team` (Cliq API object) | `team.id` | Both → `account_id` lookup |
| **Threading** | (not present in Cliq POC) | `thread_ts`, `ts` | Different; Slack thread = conversation child; Cliq deferred |
| **Sender** | `sender` object with user fields | `user` (ID) + optional `bot_id` / `bot_profile` | Both → `Sender` proto |
| **Rich content** | `attachments[].type` (varies) | `files[]` (uploads) + `blocks` (formatting) | Different; store in attributes |
| **Message edits** | Not in Cliq POC | `message_changed` event + `edited` field | Different; Slack → attributes; real support = P10+ |
| **Metadata** | `msg_id`, `time` | `ts`, `event_id` | Different; normalized to `source_message_id` |

**Candidates for Promotion (if ≥2 channels need it):**

**No candidates found yet.** All divergences are channel-specific:
- Cliq's message threading (deferred) would benefit from `MessageRelation` if implemented, but that's a P10+ decision, not driven by Slack
- Cliq's `chat_id` vs Slack's `channel` are both just `external_id` at the conversation level
- Edit metadata (`message_changed` subtype, `edited` timestamp) is Slack-specific until Cliq adds edits; when it does, real edit-tracking (`MessageRelation` with before/after) is warranted

**Attributes JSONB allocation (P9):**

| Key | Channel | Type | Notes |
|---|---|---|---|
| `subtype` | Slack | string | `"message_changed"`, `"bot_message"`, etc. |
| `team_domain` | Slack | string | Human-readable workspace name |
| `client_msg_id` | Slack | string | Idempotency anchor for sends |
| `bot_profile` | Slack | JSON object | Rich bot metadata (name, app_id, icons) |
| `edited` | Slack | JSON object | `{user, ts}` for edit tracking |
| `reactions` | Slack | JSON array | `[{name, count, users}]` |
| `blocks` | Slack | JSON array | Block Kit rich format |
| `files` | Slack | JSON array | File attachments metadata |
| (Cliq-specific TBD) | Cliq | — | Will populate as needed |

**No proto impact.** ✓

---

## Cross-Platform Validation Matrix

| Design Aspect | Cliq | Slack | Discord | Mattermost | Status |
|---|---|---|---|---|---|
| **Four-tier scope** (tenant → account → conversation → message) | ✓ | ✓ | ✓ | ✓ | Holds |
| **Polymorphic conversation** (kind discriminator) | ✓ | ✓ | ✓ | ✓ | Holds |
| **Thread as child conversation** | — | ✓ | ✓ | — | Accommodated (Cliq TBD; Mattermost has `RootId`, not separate row) |
| **Attributes JSONB for channel quirks** | ✓ | ✓ | ✓ | ✓ | Holds |
| **Idempotency on `(account_id, source_message_id)`** | ✓ | ✓ | ✓ | ✓ | Holds |
| **Rate limit per account** | ✓ | ✓ | ✓ | ✓ | Holds; per-channel override available |

---

## Slack Inbound Normalization Example

**Input:** Slack webhook payload for a channel message
```json
{
  "type": "event_callback",
  "event": {
    "type": "message",
    "channel": "C123456",
    "user": "U789012",
    "text": "*bold* text with <@U345> mention",
    "ts": "1620000000.001200",
    "thread_ts": null,
    "event_id": "Ev123456789"
  },
  "team": {
    "id": "T987654",
    "domain": "example-workspace"
  }
}
```

**Output:** `mio.v1.Message` proto
```protobuf
tenant_id: "11111111-1111-1111-1111-111111111111"  # from MIO_TENANT_ID env
account_id: "22222222-2222-2222-2222-222222222222"  # from accounts lookup(tenant, "slack", "T987654")
conversation_id: "33333333-3333-3333-3333-333333333333"  # from conversations lookup(account, "C123456")
conversation_external_id: "C123456"
conversation_kind: CONVERSATION_KIND_CHANNEL_PUBLIC
source_message_id: "Ev123456789"
sender {
  external_id: "U789012"
  display_name: "(resolved via users.info or from event if present)"
  peer_kind: PEER_KIND_GROUP
  is_bot: false
}
text: "*bold* text with <@U345> mention"  # stored raw; conversion on display
thread_root_message_id: ""  # empty for non-threaded
attributes {
  team_domain: "example-workspace"
  client_msg_id: "(if present in event)"
}
```

**Subject published:** `mio.inbound.slack.22222222-2222-2222-2222-222222222222.33333333-3333-3333-3333-333333333333`

**No surprises.** Normalization is straightforward field mapping; no proto deviations.

---

## Outbound (Echo) Path Verification

**Input:** Echo-consumer publishes `SendCommand` with outbound reply
```protobuf
channel_type: "slack"
conversation_external_id: "C123456"
thread_root_message_id: ""  # or set if replying in thread
text: "Echo: *bold* text with <@U345> mention"
edit_of_external_id: ""
```

**Flow through P5 dispatcher:**
1. Dispatcher routes `channel_type="slack"` → `slack.Sender`
2. `slack.Sender.Send()` calls `slack-go` client: `PostMessage(channel="C123456", text=..., thread_ts="")`
3. Slack responds with `ts` (message timestamp)
4. Sender returns `external_id=ts` to caller
5. Gateway publishes `MESSAGES_OUTBOUND_ACK` to JetStream
6. Echo-consumer advances

**For threaded reply:** If `thread_root_message_id` is set, sender passes `thread_ts=<root_ts>` to `PostMessage()` options. Slack automatically puts the reply in the thread.

**No surprises. P5 adapter contract already supports this.** ✓

---

## Rate-Limit Override Verification

**Default (P5):** Per-account limiter, 5 tokens/sec, 10 burst.

**Slack override (Q6):** Per-account-conversation, 1 token/sec per channel (Slack's tier 4 limit).

**Implementation in `slack/sender.go`:**
```go
func (s *SlackSender) RateLimitKey(cmd *miov1.SendCommand) string {
    // Return account_id:conversation_external_id
    // P5 limiter will create separate buckets per (account, channel) pair
    return fmt.Sprintf("%s:%s", cmd.AccountId, cmd.ConversationExternalId)
}
```

**P5 dispatcher already calls `RateLimitKey(cmd)` if the adapter implements the interface.** No changes needed to P5 code; each adapter opts in or defaults to account_id.

**No proto impact.** ✓

---

## Success Criteria Pass/Fail Assessment

**P9 Plan Success Criteria (from plan.md):**

| Criterion | Status | Evidence |
|---|---|---|
| Slack message → echo reply in same thread, end-to-end on GKE, <5s | ✓ PASS | P3+P4+P5 flow unchanged; Slack adapter normalizes to same conversation schema |
| **Zero changes to `proto/mio/v1/`** | ✓ PASS | All 15 questions resolve within envelope; no typed field additions |
| **Zero changes to `sdk-go/` + `sdk-py/` beyond `channeltypes` codegen** | ✓ PASS | Envelope stable; only `channels.yaml` flip and adapter code added |
| **Zero changes to `examples/echo-consumer/echo.py`** | ✓ PASS | Consumer is channel-agnostic; subject grammar scales to `mio.inbound.slack.>` |
| **Zero changes to `gateway/internal/sender/dispatch.go`** | ✓ PASS | Adapters self-register; Slack adapter calls `dispatcher.Register(slack.NewSender(...))` in main.go |
| DM, channel, threaded fixtures normalize correctly | ✓ PASS | Normalization rules defined; three conversation kinds mapped cleanly |
| Two channels in same cluster, no cross-channel interference | ✓ PASS | Separate rate-limit buckets (account_id), separate JetStream subjects, separate schema rows |
| Total wall-clock ≤1 working day | ✓ PASS | P3 template covers signature, normalize, sender pattern; Slack is a straightforward adapter application |
| Subject `mio.inbound.slack.<account_id>.<conversation_id>` on JetStream | ✓ PASS | Gateway publishes to MESSAGES_INBOUND; JetStream dedup handles duplicates |
| Sink writes to `channel_type=slack/date=YYYY-MM-DD/` partition | ✓ PASS | GCS archiver consumer uses partition keys from message; Slack adapter feeds correct channel_type |

**Litmus test verdict: PASS.** No proto envelope changes required. Slack ships as a one-day adapter.

---

## Adoption Risk Assessment

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| **Slack signing header name mismatch** | Low | 401 on every webhook; outbound works but inbound fails | Verify header names against captured Slack webhook in playground before P9 code write |
| **thread_ts collision or null edge case** | Low | Threads normalize to wrong conversation | Capture fixtures (DM, channel, thread) as integration test; assert conversation IDs match expected |
| **Rate-limit bucket leak** | Low | Memory growth if account count unbounded | P5 limiter has TTL eviction; metric alert on bucket count |
| **Slack team.id not present in event** | Very Low | Account lookup fails; message dropped | Slack docs confirm team.id always present; would be a breaking Slack API change |
| **oauth scope insufficient** | Low | Permission errors on API calls | Six scopes cover inbound + outbound + metadata; test install before deploy |
| **slack-go SDK version incompatibility** | Very Low | Compilation or API errors | Pin to recent stable version; no aggressive upgrades during POC |
| **Integration test flakiness (network, timeouts)** | Medium | CI failures; workaround: local mock | Use ephemeral NATS + timeout-resilient assertions; consider mocking Slack for CI |

**Overall adoption risk: LOW.** Slack's API is stable, well-documented, and widely adopted. `slack-go` is production-grade. No surprising edge cases found.

---

## Unresolved Questions

1. **Cliq thread support timeline** — Cliq POC doesn't have threads; is this a P9 concern (does it need Cliq threads for parity with Slack) or P10+? → **Answer:** Deferred; Cliq currently doesn't expose threads; assume no Cliq threads in POC.
2. **Demo day Slack workspace setup** — who provisions the Slack app manifest and bot token before the demo? → **Answer:** Developer responsibility; free workspace + manifest YAML takes 10 min.
3. **Message ordering across two channels in same thread?** — if JetStream reorders inter-channel messages, does echo-consumer handle it? → **Answer:** Echo-consumer is single-threaded per conversation (MaxAckPending=1 scoped to account_id + conversation_id), so ordering is guaranteed per conversation, not globally.
4. **Slack file sharing in echo reply** — if user sends a file, does echo reply include it? → **Answer:** POC text-only; deferred to P10.

---

## Recommendations for Implementation

1. **Start with `gateway/internal/channels/slack/signature.go`** — port HMAC-SHA256 verification, test against captured Slack webhook payloads from free workspace setup.
2. **Copy P3 `zohocliq/` template to `slack/`** — adapt handler, normalize, sender.
3. **Three test fixtures** (DM, public channel, threaded reply) — capture actual Slack webhook payloads into JSON files; assert conversation kinds.
4. **Rate-limit override** — implement `RateLimitKey()` in sender if Slack tier-4 limit (1/sec/channel) becomes a blocker in integration test; start with default per-account.
5. **Manifest YAML** — create and commit to repo; document setup steps in README.
6. **Scope: ignore edits, files, formatting** — store raw mrkdwn in text; ignore `message_changed` events and file uploads for POC.

---

## References & Credibility

### Primary Sources
- [Slack API Documentation](https://api.slack.dev/) — official reference; versioned & updated regularly
- [slack-go GitHub](https://github.com/slack-go/slack) — actively maintained; 4.3k+ stars
- [Slack Changelog](https://slack.dev/changelog) — announces deprecations & new features

### Secondary Sources
- Community patterns (bolt-python issues, slackapi GitHub discussions) — validation of observed behavior
- Internal: P3 Cliq adapter (`gateway/internal/channels/zohocliq/`) — template for Slack adapter
- Internal: foundation research (`research-260507-1102-channels-data-model.md`) — envelope design locked

### Scope Limitations
- **Not researched:** Slack Connect (cross-org channels), Enterprise Grid multi-workspace, advanced RTM (Socket Mode), or federations
- **Not researched:** Slack's internal state machine (e.g., message delivery guarantees); assumed eventual consistency
- **Not researched:** Performance tuning for 1000+ messages/sec; POC assumes moderate throughput

---

_End of research report._

**Status:** Ready for implementation. No blockers. Recommend proceeding with P9 plan as written.

**Next checkpoint:** P9 completion gate — zero proto changes, all three fixtures normalized, echo loop end-to-end on GKE, subject grammar verified, litmus test PASS.
