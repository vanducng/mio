# Research: Zoho Cliq Message Capture (Group Chats & DMs)

_Date: 2026-05-03 · Mode: --deep · Queries: 10_

## TL;DR
- **Recommendation (group chats):** **Bot Participation Handler** — only mechanism that legitimately captures *every* message in a channel (not just mentions) without admin privileges. Add the `tobytime` bot to each target channel with "Listen to messages" permission. Push events arrive at your Deluge or external HTTPS handler in near-real time.
- **Recommendation (DMs):** **Bot Message Handler** — only captures messages users send *to the bot directly*. There is no API path to capture user-to-user DMs unless you are an org admin and use the **Maintenance API** (`/maintenanceapi/v2/...`), which is a bulk-export tool, not a stream.
- **Runner-up:** **REST polling** (`GET /api/v2/chats/{chat_id}/messages` with `ZohoCliq.OrganizationMessages.READ`) — wins when you only need historical backfill or per-chat lookups, or when handler hosting is a problem. Loses on latency, rate limits (~20 req/min/user), and only works for chats the OAuth user already participates in.
- **Avoid:** Channel **Outgoing Webhooks** — officially deprecated; Zoho explicitly redirects you to the Participation Handler. Don't build new integrations on it.
- **Hard wall:** No public API can read user-to-user 1:1 DMs the bot is not part of. Only org-admin maintenance export covers that, and it is admin-credentialed only.

## The Question
How do we extend the existing Cliq integration to **capture incoming messages** from group/channel chats and direct messages, given the credentials in `services/sci/.env` (server-based OAuth app + `tobytime` bot)?

The decision: pick the right capture mechanism (push vs. pull, bot handler vs. REST), what OAuth scopes to add, and which endpoints to wire up — then know its limits before committing.

## Evaluation Criteria
- **Coverage:** can it see *every* message, or just messages that mention/target the bot?
- **Latency:** real-time push vs. polling delay
- **Auth & scope:** what OAuth scopes; admin vs. user; works in a server worker
- **Ops burden:** Deluge-hosted vs. external HTTPS handler; signature verification
- **Rate limits & cost:** Cliq enforces undocumented but real throttles
- **Privacy boundary:** does it cross into user-to-user DMs (almost always blocked)
- **Lock-in & deprecation risk:** is the API still supported

## Options Considered
- **A. Bot Participation Handler** — push events for every channel a bot is added to (channel adds, message sent/edited/deleted, threads).
- **B. Bot Message Handler** — push events for messages users send *to the bot* (DM-with-bot). Plus the Mentions Handler for `@bot` in channels.
- **C. REST polling — `/api/v2/chats/{chat_id}/messages`** — pull message history per chat using OAuth user credentials.
- **D. Maintenance API — `/maintenanceapi/v2/chats/{chat_id}/messages`** — admin-only bulk export of *all* org chats including private DMs.
- **E. Channel Outgoing Webhook** — the old way (deprecated).

## Comparison Matrix

| Criterion | A. Participation Handler | B. Message + Mentions Handler | C. REST polling (`/chats/{id}/messages`) | D. Maintenance API | E. Channel Outgoing Webhook |
|---|---|---|---|---|---|
| Captures all channel messages | ✅ Yes (after add + "Listen to messages") | ❌ Only `@bot` mentions or DMs to bot | ✅ Yes for chats the OAuth user belongs to | ✅ Yes, org-wide | ⚠️ Yes but deprecated |
| Captures user↔user DMs | ❌ No | ❌ No (only DMs to the bot) | ❌ No (only chats OAuth user is in) | ✅ Admin only | ❌ No |
| Captures DMs to bot | ❌ N/A | ✅ Yes (Message Handler) | ⚠️ Possible if you know chat_id | ✅ Yes | ❌ No |
| Real-time push | ✅ Yes | ✅ Yes | ❌ Pull only | ❌ Bulk only | ✅ Yes |
| OAuth scope | Bot platform (no extra) | Bot platform (no extra) | `ZohoCliq.OrganizationMessages.READ`, `ZohoCliq.Chats.READ` | `ZohoCliq.OrganizationChats.READ`, `ZohoCliq.OrganizationMessages.READ` (admin) | n/a |
| Hosting | Deluge in Cliq, or external HTTPS via Catalyst/your server | Same | Your worker | Your worker | External HTTPS |
| Rate limit risk | Low (push) | Low | High (≤20 req/min/user, undocumented per-day) | Bulk job, admin throttles | Low |
| Max bots / channel | 10 | n/a | n/a | n/a | n/a |
| Deprecation risk | Active, recommended replacement | Active | Active | Active (admin tool) | **Deprecated** |
| Setup friction | Toggle bot perms + add bot per channel | Already in place for `tobytime` | OAuth scope add + token refresh | Org admin acct + scope | Don't bother |
| Lock-in | Cliq bot platform | Cliq bot platform | Lowest (plain REST) | Highest (admin contract) | Dead-end |

## Per-Option Deep Dive

### A. Bot Participation Handler (recommended for group chats)

The participation handler fires when the bot is added to a channel and on every subsequent message-level event. It is Cliq's official replacement for the deprecated Outgoing Channel Webhook.

- **Trigger events** ([docs](https://www.zoho.com/cliq/help/platform/bot-participation-handler.html)):
  - Channel: bot added / removed
  - Message: `sent`, `edited`, `deleted`
  - Thread: `auto-followed`, `manually added/removed`, `closed`, `reopened`
- **Payload attributes:** `operation` (string), `data` (map — message body, ts, ids), `user` (sender), `chat` (channel), `environment`, `access`. Includes "text, links, and attachments posted by a channel participant" — no `@bot` required.
- **Configuration (one-time per bot):**
  1. Bot edit page → enable **"Allow users to add this bot to any channel"**
  2. Permissions → check **"Listen to messages"** (and "Send messages" if you also reply)
  3. Edit the **Participation Handler** code (Deluge) or wire to an external HTTPS endpoint via Catalyst
  4. Add the bot to each target channel via channel participants menu
- **Strengths:** push delivery, captures full message body without `@mention`, replaces the deprecated outgoing webhook.
- **Weaknesses:**
  - Must be added per channel — no org-wide auto-join.
  - Hard cap **10 bots per channel**.
  - "Personal bots" need extra config to be addable to channels.
  - The `thread_closed` operation skips message return — note for completeness logic.
  - Adding a bot to a channel requires the user has the channel's "add bot" permission set.
- **Real-world users:** Zoho's own first-party integrations (e.g., Zoho Projects, Sprints) use the Participation Handler pattern.
- **Recent CVEs / advisories:** none found.

### B. Bot Message Handler + Mentions Handler

Two separate handlers; only Message Handler covers DMs-to-bot.

- **Message Handler** ([docs](https://www.zoho.com/cliq/help/platform/bot-messagehandler.html)) — fires when a user sends a message to the bot in a 1:1 chat.
  - Payload: `message`, `attachments`, `mentions`, `links`, `user`, `chat`, `location`.
  - Response: `Map` with `text` and optional `suggestions` (≤10 buttons).
- **Mentions Handler** ([docs](https://www.zoho.com/cliq/help/platform/bot-mentionshandler.html)) — fires when bot is `@mentioned` in any chat or channel (including channels where the bot is *not* a participant via Participation Handler).
  - Payload: `message`, `mentions`, `user`, `chat`, optional `location`.
- **Strengths:** Already enabled in your `tobytime` bot architecture; trivial to extend; works in DM with bot.
- **Weaknesses:**
  - Will **not** see messages that don't tag the bot (in channels) and obviously not user-to-user DMs.
  - No documented payload schema — fields exposed via Deluge runtime; build a defensive parser.
- **Best for:** capturing user *intent* aimed at the bot ("@tobytime export this thread").

### C. REST polling — `/api/v2/chats/{chat_id}/messages`

Standard OAuth pull. Confirmed endpoints from the v2 reference:

```
GET https://cliq.zoho.com/api/v2/chats                          # list chats user is in
GET https://cliq.zoho.com/api/v2/chats/{chat_id}/messages       # message history
GET https://cliq.zoho.com/api/v2/chats/{chat_id}/members
GET https://cliq.zoho.com/api/v2/channels                       # list channels
GET https://cliq.zoho.com/api/v2/channels/{channel_id}/members
GET https://cliq.zoho.com/api/v2/messages/{message_id}
GET https://cliq.zoho.com/api/v2/messages/{message_id}/reactions
```
- **Auth:** existing `Zoho-oauthtoken` flow you already wire for sending. Add scopes:
  - `ZohoCliq.Chats.READ`
  - `ZohoCliq.Channels.READ`
  - `ZohoCliq.Messages.READ`
  - `ZohoCliq.OrganizationMessages.READ` (required for `chats/{id}/messages` per Zoho docs — yes, "Organization" is in the name even on the user-scoped endpoint)
- **Pagination:** `next_token` cursor. Filters: `joined`, `pinned` on the listing endpoints.
- **Strengths:** simplest mental model; no hosting required; works in your existing Hatchet worker.
- **Weaknesses:**
  - **Latency** — depends on poll cadence; minutes, not seconds.
  - **Rate limit** — community evidence: ~20 req/min/user on most v2 endpoints; some chat endpoints lower (≤10/min). Zoho's docs say "no limit"; reality returns HTTP 400 *"You have exceeded the URL throttle limits."*. Plan a token bucket and exponential backoff.
  - **Visibility** — only returns chats the OAuth token's user is in. The bot isn't an OAuth principal; you must impersonate a real human user.
  - **Free plan caveat** — only the **last 10,000 messages** are accessible to free orgs; paid orgs are unlimited.

### D. Maintenance API — `/maintenanceapi/v2/...`

```
GET /maintenanceapi/v2/chats                       # CSV export of all chats
GET /maintenanceapi/v2/chats/{chat_id}/messages    # JSON full transcript
GET /maintenanceapi/v2/channels                    # CSV export of all channels
```
- **Auth:** OAuth user must be **organization admin**. Scope: `ZohoCliq.OrganizationChats.READ` and/or `ZohoCliq.OrganizationMessages.READ`.
- **Coverage:** *all* org conversations including private user-to-user DMs and group chats the admin is not in.
- **Use case:** compliance archiving, e-discovery, one-shot backfill.
- **Strengths:** the only legal way to read user↔user DMs.
- **Weaknesses:**
  - Admin credential — never put in a service worker without strong audit/justification.
  - Bulk-only; not for streaming.
  - High blast radius if leaked.
- **Real-world users:** Zoho positions this for `Admin Console → Data Export` workflows.

### E. Channel Outgoing Webhook (deprecated)

Quote from Zoho's own docs: *"Outgoing Webhooks for channels have been deprecated. We recommend using the Bot Participation Handler for the same functionality."* Treat as dead-end. Don't ship anything new on it.

## Failure Modes

| Option | Mode | Symptom | Mitigation | Recovery cost |
|---|---|---|---|---|
| A. Participation | Bot removed by channel admin | Silent gap in capture; no events | Re-add hook on `bot_removed`; alert; daily reconciliation via REST `chats/{id}/messages` | Low — re-add bot |
| A. Participation | 10-bot ceiling per channel hit | New bot fails to attach | Audit existing bots; merge integrations into one | Medium — political |
| A. Participation | `thread_closed` skips message | Last message in closed thread missing | Polling backfill on thread close events | Low |
| B. Message/Mentions | User DMs bot at high rate | Handler executions throttled | Queue async; respond with deferred ack | Low |
| C. Polling | Throttle 400 "URL throttle" | Capture stalls | Exponential backoff + token-bucket; cache `next_token`; multiple OAuth users for fan-out | Medium |
| C. Polling | OAuth user removed from a chat | History stops returning | Detect 404/empty; have admin/Participation Handler as fallback | Medium |
| C. Polling | Free-plan 10k message ceiling | Older history gone | Confirm paid plan; otherwise ingest fast and store locally | High if not detected |
| D. Maintenance | Admin token revoked | Whole pipeline dead | Two-admin policy; rotate refresh tokens; alert on 401 | High |
| D. Maintenance | Bulk job rate-limited | Long export windows | Chunk by chat_id; resume tokens; off-peak schedule | Medium |
| All | Refresh token revoked by user | All auth dies at next refresh | Monitor refresh failures; on-call alert; UI to re-link | High |
| All | Org region mismatch (`zoho.com` vs `zoho.eu`/`.in`) | 401/404 on otherwise correct calls | Pin the API domain per OAuth response (`api_domain`); never hardcode `cliq.zoho.com` if you may go multi-region | Medium |

## Migration Paths

- **From "send-only" (today) → A. Participation Handler:** modify `tobytime` bot, enable channel-add + listen, write a Deluge handler or external HTTPS endpoint that POSTs to your worker (HMAC-verify with `webhook token`). Add the bot to existing channels manually or by script via `POST /api/v2/channels/{channel_id}/participants`. Reverse migration: just remove handler code; data flow stops.
- **From outgoing channel webhooks → A.** straight upgrade Zoho explicitly recommends.
- **Add C. polling for backfill alongside A.:** complementary, not exclusive. Use polling for one-shot historical import, Participation Handler for live tail.
- **Lock-in:** all options are 100% Cliq-specific. There is no portable abstraction; if you leave Cliq you re-write capture for the new platform regardless of choice.

## Operational War Stories

- **"Get all messages from channel" community thread (Zoho forum):** historically users could not list channel messages — only `last_message`. The `chats/{id}/messages` endpoint is the resolution; older blog posts and SO answers still cite the old gap. ([thread](https://help.zoho.com/portal/en/community/topic/get-all-messages-from-channel-in-cliq-api))
- **Rate-limit thread:** Zoho support claims "no limit" yet returns `400 You have exceeded the URL throttle limits` in production. Treat any "no limit" claim as marketing; assume ~20 req/min/user budget. ([thread](https://help.zoho.com/portal/en/community/topic/zoho-cliq-api-rate-limit))
- **TrustSwiftly integration guide:** uses bot incoming webhook for *delivery into* Cliq, not capture — common confusion vector. Don't conflate "incoming webhook" (external→Cliq) with "outgoing/participation" (Cliq→you).

## Performance Under Realistic Load

- **Push handlers (A, B):** events delivered within seconds of the user action. No public throughput SLO; community reports tens of events/sec sustained per bot without backpressure issues.
- **Polling (C):** at 20 req/min/user across all v2 endpoints, a single OAuth user can poll ~10 chats every 30s if you spend half the budget on listing and half on history. For dozens of channels, fan out across multiple OAuth principals or accept multi-minute lag.
- **Maintenance (D):** seconds-to-minutes per chat for a full transcript export; bulk runs are admin-throttled — pace to off-peak.
- **Independent benchmarks:** none published. The above is community-derived and from Zoho's own throttling-error pattern.

## Decision Reversibility

- **A → off:** disable handler code; bot stays in channels but no longer ingests. Hours of work to revert.
- **B → off:** remove handler. Trivial.
- **C → off:** stop scheduler. Trivial.
- **D → off:** remove admin scope from token; revoke. Trivial mechanically; political-cost depends on who holds the admin account.

## Recommendation

For SCI's likely use case (capture team conversations and DMs-with-bot for processing/analysis):

1. **Primary:** Use **Bot Participation Handler** to ingest channel messages. Enable "Listen to messages" on the `tobytime` bot, host the handler as Deluge code that forwards to a Hatchet webhook, OR expose an HTTPS endpoint on the SCI backend and use Zoho Catalyst / a thin Deluge bridge. Verify each event with the bot's webhook token.
2. **Secondary:** Keep **Bot Message Handler** (likely already wired) for DMs-to-bot — that's the only legitimate DM capture without admin rights.
3. **Backfill:** Add scope `ZohoCliq.OrganizationMessages.READ` + `ZohoCliq.Chats.READ` and use REST polling for one-shot historical import per chat. Build a token bucket (≤15 req/min/user, leaving headroom).
4. **Compliance archive (only if required):** Maintenance API with a separate admin OAuth client, isolated credentials, audit logging.
5. **Do not** build on Channel Outgoing Webhook.

Runner-up (REST polling) wins if: handler hosting is unavailable, or you only need on-demand snapshots, or the user explicitly rejects deploying bot code changes.

## Implementation Notes

### OAuth scopes to add (server-based app)

```
ZohoCliq.Channels.ALL
ZohoCliq.Messages.CREATE
ZohoCliq.Webhooks.CREATE
ZohoCliq.Chats.READ                  # NEW — list chats
ZohoCliq.Channels.READ               # NEW — list channels
ZohoCliq.Messages.READ               # NEW — single message + reactions
ZohoCliq.OrganizationMessages.READ   # NEW — list/export chat messages
# Admin-only:
# ZohoCliq.OrganizationChats.READ
```
Re-run the auth flow in `setup-zoho-cliq-oauth.sh` after editing the scope set on the api-console; the existing refresh token will not gain new scopes.

### Participation Handler — minimal Deluge skeleton

```js
// Triggered on operation: bot_added / message_sent / message_edited / message_deleted / thread_*
response = Map();
if (operation == "message_sent") {
    payload = Map();
    payload.put("chat_id", chat.get("id"));
    payload.put("message_id", data.get("message", Map()).get("id"));
    payload.put("text", data.get("message", Map()).get("text"));
    payload.put("user_id", user.get("id"));
    payload.put("ts", data.get("time"));
    // POST to SCI ingest with HMAC signature
    headers = Map();
    headers.put("Content-Type", "application/json");
    headers.put("X-Cliq-Signature", zoho.encryption.hmacsha256("<shared_secret>", payload.toString()));
    postUrl = "https://sci.internal/api/v1/cliq/ingest";
    resp = invokeurl [ url: postUrl  type: POST  parameters: payload.toString()  headers: headers ];
}
return response;
```

### REST polling — read history

```bash
ACCESS_TOKEN=$(curl -s -X POST https://accounts.zoho.com/oauth/v2/token \
  -d "grant_type=refresh_token" \
  -d "client_id=$ZOHO_CLIQ_CLIENT_ID" \
  -d "client_secret=$ZOHO_CLIQ_CLIENT_SECRET" \
  -d "refresh_token=$ZOHO_CLIQ_REFRESH_TOKEN" | jq -r .access_token)

# List chats the OAuth user is in
curl -s "https://cliq.zoho.com/api/v2/chats?joined=true" \
  -H "Authorization: Zoho-oauthtoken $ACCESS_TOKEN"

# Pull message history
curl -s "https://cliq.zoho.com/api/v2/chats/CT_1608777491535075383_637446511/messages" \
  -H "Authorization: Zoho-oauthtoken $ACCESS_TOKEN"
```

### Pitfalls to encode now

- `Authorization: Zoho-oauthtoken ...` — **not** `Bearer`.
- Use the `api_domain` returned by the token endpoint to pick the regional host instead of hardcoding `cliq.zoho.com`.
- Channel `chat_id` (`CT_*`) ≠ `channel_id` (`O*`). The messages endpoint takes `chat_id`. Resolve via `GET /api/v2/channels` first if you only have channel name.
- Free org? You will silently lose messages older than the last 10k. Verify plan before committing to historical capture.
- 10 bots per channel hard ceiling — audit before adding `tobytime` to popular channels.
- Bots cannot impersonate users for REST reads; the polling path uses a real user's OAuth token. Treat that user's account hygiene as production infra.

## References

- [Cliq REST API v2](https://www.zoho.com/cliq/help/restapi/v2/)
- [Bot Handlers overview](https://www.zoho.com/cliq/help/platform/bothandlers.html)
- [Bot Participation Handler](https://www.zoho.com/cliq/help/platform/bot-participation-handler.html)
- [Bot Message Handler](https://www.zoho.com/cliq/help/platform/bot-messagehandler.html)
- [Bot Mentions Handler](https://www.zoho.com/cliq/help/platform/bot-mentionshandler.html)
- [Bot Incoming Webhook Handler](https://www.zoho.com/cliq/help/platform/bot-incomingwebhookhandler.html)
- [Channel Webhooks (deprecated)](https://www.zoho.com/cliq/help/platform/channel-webhooks.html)
- [Webhook Tokens](https://www.zoho.com/cliq/help/platform/webhook-tokens.html)
- [Cliq specifications & limits](https://help.zoho.com/portal/en/kb/zoho-cliq/admin-guides/manage-organization/articles/cliq-limitations)
- [Zoho OAuth scopes reference](https://www.zoho.com/accounts/protocol/oauth/scope.html)
- [Postman: Zoho Cliq REST APIs v2](https://www.postman.com/cliqgeeks/zoho-cliq/collection/8s2nyyh/zoho-cliq-rest-apis-v2)
- [Community: API rate limits](https://help.zoho.com/portal/en/community/topic/zoho-cliq-api-rate-limit)
- [Community: get all messages from channel](https://help.zoho.com/portal/en/community/topic/get-all-messages-from-channel-in-cliq-api)
- [Admin: data export](https://zoho.com/cliq/help/admin/data-export.html)

## Open Questions

1. **Exact rate-limit numbers** — Zoho documents none; community reports ~20 req/min/user. Need empirical measurement against the SCI worker before committing SLOs.
2. **Participation Handler delivery semantics** — at-least-once? Idempotency keys? No public guarantee; design your ingest to dedupe by `message_id`.
3. **Webhook token verification format** — Zoho exposes a `webhook_token` per bot but does not publicly document the signature algorithm; confirm via support or by inspecting incoming requests.
4. **Edited/deleted message ordering** — does Participation Handler deliver edits in order? Need empirical test for chat reconstruction correctness.
5. **Free vs. paid plan** of the AB Spectrum Cliq org — confirm before relying on historical depth past 10k messages.
6. **Multi-region** — `services/sci/.env` does not pin region; confirm org is `.com` (US) and not `.eu`/`.in` to avoid 401/404 surprises.
7. **Catalyst vs. external HTTPS** for the handler runtime — what's the team preference; Catalyst keeps it inside Zoho but adds another platform.
