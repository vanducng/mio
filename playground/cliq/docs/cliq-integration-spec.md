# Zoho Cliq Integration — Technical Spec

_Source: validated against working PoC at `playground/cliq/`. Date: 2026-05-03._

Single source of truth for `mio-gateway` (Go) Cliq integration. Captures: capture flows, identity model, OAuth, endpoints, payload schemas, signature scheme, limitations, and Go gateway design.

---

## 1. Scope & Use Cases

Two product use cases drive this:

1. **Conversational bot** — user `@mentions` a bot or DMs it; bot acknowledges, runs server-side logic (LLM, lookup, action), replies in the channel (quote-threaded) or via DM.
2. **Chat summarization** — bot ingests every message + attachment in channels it's added to; gateway persists, indexes, optionally feeds an LLM summarizer; a triggered summary is posted back to the channel by the bot.

Both flows share the same capture and reply infrastructure.

---

## 2. High-Level Architecture

```
                     #channel events       webhook events             reply / react / fetch
   Cliq users ───────────────────▶  Cliq cloud  ───────────▶  mio-gateway  ───────▶  Cliq REST
        ▲                                │ Deluge             (Go service)             │
        │                                │ (Participation Handler)                     │
        │                                ▼                                             │
        │                         POST /cliq                                           │
        │                       (HMAC-SHA256 signed)                                   │
        │                                                                              │
        └──────────────────────────────────────────────────────────────────────────────┘
                              bot-attributed channel posts (replies, summaries)
```

**Components:**
- **Cliq Bot** (`tobytime`) — declared in Cliq Bot dashboard. Runs Deluge code (Participation Handler) on Cliq's servers. Forwards every channel event to the gateway via HMAC-signed POST.
- **Public ingress** — Cloudflare Tunnel → `https://mio.vanducng.dev/cliq`. Protects the gateway behind a managed edge.
- **Gateway (Go)** — verifies HMAC, parses event, persists, dispatches business logic, posts replies via Cliq REST.
- **OAuth credentials** — server-based OAuth client; refresh token never expires, access token TTL = 1 hour. Two refresh tokens recommended (one per environment) to isolate prod from playground.

---

## 3. Identity Model — When the Bot Speaks vs When the User Does

This is the load-bearing constraint of Cliq's API. Get this wrong and your UX is wrong.

| Action | Sender shown as | Mechanism |
|---|---|---|
| Channel post (`/api/v2/channelsbyname/{name}/message?bot_unique_name=tobytime`) | **Bot** (TobyTime) | Cliq's only documented identity-switch param |
| Channel post **without** `bot_unique_name` | OAuth user | The default; bot identity is opt-in |
| Quote-reply (`reply_to: <msg_id>` in body) | Same rules as channel post | The `bot_unique_name` flag still applies |
| Reaction (`/chats/{cid}/messages/{mid}/reactions`) | **OAuth user** — always | No bot-identity option exists in Cliq |
| DM via `/buddies/{email}/message` | **OAuth user** — always | Bots cannot DM as themselves |
| Bot incoming webhook → bot subscriber | Bot | Only works for users subscribed to the bot |
| Edits / deletes | OAuth user | No bot-identity switch |

**Practical rules for mio-gateway:**
- Use `?bot_unique_name=tobytime` on **every** outbound channel post.
- Don't expose reactions in user-facing flows where authorship visibility matters; if needed, post `👀` as a quote-reply text instead.
- DMs to bot land in the **Bot Message Handler** (push); to send a reply, the handler returns a response Map — that reply IS attributed to the bot. The REST `/buddies/.../message` path is the wrong one for bot-to-user DM.

---

## 4. Authentication

### 4.1 OAuth client setup (one-time per environment)

api-console.zoho.com → **Add Client → Server-based Application**:

- Client name: `mio-gateway-{env}`
- Homepage URL: any controlled URL
- Authorized redirect URI: `http://localhost:8765` (any URI you can capture the code from once)

Output: `client_id`, `client_secret`. Both secret. Store in env, never in code.

### 4.2 Token flow

```
1. (one-time) Browser GET https://accounts.zoho.com/oauth/v2/auth?...
   → user consent → 302 to redirect_uri with ?code=...
2. (one-time) POST https://accounts.zoho.com/oauth/v2/token
     grant_type=authorization_code, code, client_id, client_secret, redirect_uri
   → { access_token, refresh_token (offline), expires_in: 3600 }
3. (every <60min) POST https://accounts.zoho.com/oauth/v2/token
     grant_type=refresh_token, client_id, client_secret, refresh_token
   → { access_token, expires_in: 3600 }   # refresh_token does NOT change
```

Refresh tokens do NOT expire under offline mode. Up to ~20 active refresh tokens per client per user are allowed before Zoho revokes the oldest.

### 4.3 Auth header (CRITICAL)

```
Authorization: Zoho-oauthtoken <access_token>
```
**Not** `Bearer`. Cliq specifically uses `Zoho-oauthtoken`.

### 4.4 Two-token strategy (recommended)

Keep prod and dev refresh tokens distinct so a re-auth in dev never lands on a live system:

```env
ZOHO_CLIQ_REFRESH_TOKEN              # prod
ZOHO_CLIQ_REFRESH_TOKEN_PLAYGROUND   # dev / staging — broader scopes for experimentation
```
Gateway picks the playground token if set. Re-auth always writes only to the playground variable.

---

## 5. OAuth Scopes — Authoritative Mapping

| Capability | Scope | Verified |
|---|---|---|
| Post channel message | `ZohoCliq.Webhooks.CREATE` (also `ZohoCliq.Messages.CREATE`) | ✅ |
| Edit own channel posts | `ZohoCliq.Messages.UPDATE` | docs only |
| Delete own channel posts | `ZohoCliq.Messages.DELETE` | docs only |
| List chats / channels | `ZohoCliq.Chats.READ` / `ZohoCliq.Channels.READ` | ✅ |
| Read single message | `ZohoCliq.Messages.READ` | ✅ |
| Read messages in a chat | `ZohoCliq.OrganizationMessages.READ` | ✅ |
| Add reaction | `ZohoCliq.messageactions.CREATE` (lowercase!) | ✅ |
| Get reactions | `ZohoCliq.messageactions.READ` | ✅ |
| Remove reaction | `ZohoCliq.messageactions.DELETE` | ✅ |
| Channel admin (add/remove members) | `ZohoCliq.Channels.UPDATE` (or `.ALL`) | docs only |
| Org-wide message export (ADMIN) | `ZohoCliq.OrganizationChats.READ` | docs only |
| Download attachment | `ZohoCliq.Attachments.READ` | not yet tested |

**Gotchas confirmed by failed scopes:**
- `ZohoCliq.Reactions.UPDATE` — **does not exist**. Reactions live under `messageactions`.
- `ZohoCliq.Messages.UPDATE` is for editing message text, **not** for reactions.
- Lowercase / mixed-case is real (`messageactions`, `messages.CREATE` aliasing `Messages.CREATE`). Treat scope strings as opaque — copy verbatim.

**Source:** validated against `phil-fetchski/n8n-nodes-zoho-cliq/nodes/ZohoCliq/v1/helpers/scopeRegistry.ts` (production-validated community node).

### 5.1 Recommended scope CSV for `mio-gateway`

Minimal:
```
ZohoCliq.Webhooks.CREATE,
ZohoCliq.Channels.ALL,
ZohoCliq.Messages.CREATE,
ZohoCliq.Messages.READ,
ZohoCliq.Chats.READ,
ZohoCliq.Channels.READ,
ZohoCliq.OrganizationMessages.READ,
ZohoCliq.messageactions.CREATE,
ZohoCliq.messageactions.READ,
ZohoCliq.messageactions.DELETE,
ZohoCliq.Attachments.READ
```

Add when needed: `ZohoCliq.Messages.UPDATE`, `ZohoCliq.Messages.DELETE` for edit/delete; `ZohoCliq.OrganizationChats.READ` for admin-only org backfill.

---

## 6. Message Capture — Bot Participation Handler

The only push mechanism that captures **every** channel message (not just `@mentions`).

### 6.1 Cliq side (one-time bot config)

1. Bot Edit page → enable **"Allow users to add this bot to any channel"**.
2. Permissions → **"Listen to messages"** (and "Send messages" if you also reply, which you do).
3. Edit Handlers → **Participation Handler** → paste Deluge code (see §6.3).
4. Add bot to channel via channel members menu. Bot must be a participant; max 10 bots per channel.

### 6.2 Trigger events

| `operation` | When |
|---|---|
| `bot_added` / `bot_removed` | Channel membership change (data may be empty) |
| `message_sent` | Any new message (text, attachment, link) |
| `message_edited` | User edits a message |
| `message_deleted` | User deletes a message |
| `thread_auto_followed` / `thread_added` / `thread_removed` | Thread membership |
| `thread_closed` / `thread_reopened` | Thread state — the message body for `thread_closed` is **not** delivered (documented quirk) |

### 6.3 Deluge handler (paste verbatim into Participation Handler editor)

```js
// WEBHOOK_URL + WEBHOOK_SECRET hardcoded — Cliq's `bot` object only exposes
// name/image/description, custom variable storage is not supported.
WEBHOOK_URL    = "https://mio.vanducng.dev/cliq";
WEBHOOK_SECRET = "<32-byte hex shared with gateway>";

payload = Map();
payload.put("operation", operation);
payload.put("data", data);
payload.put("user", user);
payload.put("chat", chat);

bodyJson = payload.toString();
signature = "sha256=" + zoho.encryption.hmacsha256(WEBHOOK_SECRET, bodyJson);

headers = Map();
headers.put("Content-Type", "application/json");
headers.put("X-Webhook-Signature", signature);

resp = invokeurl
[
    url: WEBHOOK_URL
    type: POST
    parameters: bodyJson
    headers: headers
];

response = Map();        // empty = no reply rendered in chat
return response;
```

### 6.4 Signature scheme (HMAC verification on gateway)

- Algorithm: HMAC-SHA256 over the raw request body bytes.
- Secret: shared 32-byte secret, generated once.
- **`zoho.encryption.hmacsha256` returns base64.** Gateway must accept both base64 and hex (the latter for `openssl`-style test posts).
- Header value format: `sha256=<base64-or-hex>`.
- Gateway should strip the optional `sha256=` prefix and compare both encodings with `hmac.Equal` (constant-time).

---

## 7. Reply / Bot Speaks Back — Channel Post

```
POST https://cliq.zoho.com/api/v2/channelsbyname/{channel_unique_name}/message
     ?bot_unique_name={bot_unique_name}
Headers:
  Authorization: Zoho-oauthtoken <access_token>
  Content-Type: application/json
Body:
  {
    "text": "echo: ...",
    "reply_to": "<message.id>"   // optional — renders as quote-thread
  }
Response: HTTP 204 (no body) on success
```

Quote-reply uses `reply_to` with the **exact `message.id` string** received in the webhook (e.g. `"1777805235487 162898249840"` — note the literal space).

### 7.1 Channel ID mapping

Each channel has THREE distinct IDs. Don't conflate:

```
channel_id          P1974166000016769023        (legacy / channels endpoint)
chat_id             CT_1608777491574236080_637446511
                                                (use for /chats/{id}/messages, reactions)
channel_unique_name "ducdev"                    (use for channelsbyname/.../message)
```

All three are present in every Participation Handler `chat` payload.

---

## 8. Bot Mention Handler & Bot Message Handler

These run **alongside** the Participation Handler. Use cases:

- **Bot Message Handler** — a user DMs the bot (1:1 chat). Trigger event for "talk to the bot" UX. Reply via the handler return Map (renders as the bot identity in the DM).
- **Bot Mention Handler** — `@bot` in any chat or channel. Use when you want a different (synchronous) response shape than the Participation Handler.

For the gateway, **prefer the Participation Handler** as the single ingestion path and detect mentions in the payload (see §11.4). One handler, one transport. Reserve Mention/Message Handlers for cases that need synchronous in-chat responses.

---

## 9. REST API Reference (gateway-relevant subset)

Base: `https://cliq.zoho.com/api/v2`

| Verb | Path | Scope | Purpose |
|---|---|---|---|
| POST | `/channelsbyname/{name}/message?bot_unique_name=...` | `Webhooks.CREATE` | Post / quote-reply as bot |
| POST | `/buddies/{email}/message` | `Messages.CREATE` | DM as OAuth user (NOT as bot) |
| GET | `/chats` | `Chats.READ` | List chats user is in |
| GET | `/chats/{chat_id}/messages` | `OrganizationMessages.READ` | Backfill chat history |
| GET | `/messages/{message_id}` | `Messages.READ` | Single message lookup |
| GET | `/channels` | `Channels.READ` | List channels |
| POST | `/chats/{chat_id}/messages/{msg_id}/reactions` | `messageactions.CREATE` | Add reaction (as user) |
| GET | `/chats/{chat_id}/messages/{msg_id}/reactions` | `messageactions.READ` | List reactions |
| DELETE | `/chats/{chat_id}/messages/{msg_id}/reactions` | `messageactions.DELETE` | Remove reaction |
| GET | `/maintenanceapi/v2/chats/{chat_id}/messages` | `OrganizationMessages.READ` (admin) | Org-wide export incl. private DMs |

**`message_id` URL-encoding:** the raw id contains a literal space (`"1777805235487 162898249840"`); URL-encode to `%20` once. Don't double-encode (`%2520`) — Cliq matches loosely but it's wrong.

---

## 10. Pagination

`GET` listing endpoints (`/chats`, `/channels`, `/chats/{id}/messages`) return:
- `data: [...]` — page items, **newest-first** for messages.
- `next_token: "..."` — opaque cursor; pass as `?next_token=...` to fetch next page.

Iterate until `next_token` is absent / null. Do not assume a fixed page size.

---

## 11. Payload Schemas (real samples from the PoC capture)

### 11.1 Outer envelope (Participation Handler → gateway)

```json
{
  "operation": "message_sent",
  "data": {
    "time": 1777804540189,
    "message": { ... }
  },
  "user":  { ... },
  "chat":  { ... }
}
```

### 11.2 `message` — text type

```json
{
  "id": "1777805235487 150012629296",
  "type": "text",
  "text": "hey {@b-1974166000016769005} , what is the current time?",
  "content": { ... },
  "mentions": [
    { "name": "TobyTime", "dname": "@TobyTime", "id": "b-1974166000016769005", "type": "bot" }
  ]
}
```

### 11.3 `message` — attachment type

```json
{
  "id": "1777804795069 141422252876",
  "type": "attachment",
  "comment": "test with file attached",
  "file": {
    "name": "doc.md",
    "type": "text/plain",
    "url":  "https://cliq.zoho.com/company/637446511/v2/attachments/<token>?type=transient"
  }
}
```

The `url` is **not public** — requires `Authorization: Zoho-oauthtoken` to download.

### 11.4 `mentions` array — bot detection

Match by **id**, never by display name (names can collide):

```go
const botID = "b-1974166000016769005"   // from Cliq bot edit URL
for _, m := range message.Mentions {
    if m.Type == "bot" && m.ID == botID { /* this is OUR bot */ }
}
```

The inline placeholder in `text` (`{@b-...}`) corresponds to the same id; strip it for clean display: `regexp.MustCompile(\`\{@[^}]+\}\s*\`)`.

### 11.5 `user` — sender

```json
{
  "id": "903112229",
  "zoho_user_id": "903112229",
  "first_name": "Duc", "last_name": "Nguyen",
  "email": "duc.nguyen@abspectrum.org",
  "country": "us", "language": "en", "timezone": "Asia/Ho_Chi_Minh",
  "organization_id": "637446511",
  "admin": true
}
```

### 11.6 `chat` — channel

```json
{
  "id":                  "CT_1608777491574236080_637446511",
  "type":                "channel",
  "chat_type":           "channel",
  "title":               "#DucDev",
  "channel_unique_name": "ducdev",
  "channel_id":          "P1974166000016769023",
  "owner":               "903112229"
}
```

For DMs (`chat_type: "chat"`), `channel_unique_name` is absent; address replies via `/api/v2/buddies/{email}/message` (sends as OAuth user) or via the Bot Message Handler return Map (sends as bot).

---

## 12. Attachments — Download Flow

The Participation Handler sends only metadata. To fetch bytes:

```
GET <message.file.url>
Authorization: Zoho-oauthtoken <access_token>
Accept: */*
```
The URL is signed with `?type=transient`; assume it's short-lived. Download immediately, persist locally.

Response is a `200` with the file body and a `Content-Type` matching `message.file.type`. Stream the response directly to disk / object store; do not buffer in memory (1GB max attachment per Cliq).

---

## 13. Rate Limits & Error Handling

Zoho doesn't publish numbers. Empirical:
- Most v2 endpoints throttle around **20 req/min/user**.
- Exceeding triggers `HTTP 400 "You have exceeded the URL throttle limits"`.
- Channel post historically allowed up to 50 req/min/user with a 10-minute lockout on overshoot.

**Gateway must:**
- Token-bucket all REST calls per OAuth user, ~15 req/min budget, with ~5 req burst.
- Exponential backoff with jitter on 400 throttle, 429, and 5xx.
- Persist `next_token` cursors so polling resumes after backoff.
- Treat refresh-token revocation as PagerDuty-class — alarm on first 401 from token endpoint.

---

## 14. Limitations (Hard Walls)

These are Cliq architectural limits, not bugs to fix:

1. **Bots cannot DM as themselves.** `/buddies/{email}/message` always sends as OAuth user. Workaround: deliver via Bot Message Handler return Map (only works after the user has DM'd the bot first).
2. **Bots cannot react.** Reactions on `/chats/{cid}/messages/{mid}/reactions` always show as the OAuth user. No `bot_unique_name` parameter exists. Confirmed by REST docs, Deluge tasks, Cliq response types, and the n8n community node.
3. **Bots cannot edit/delete others' messages.** Only own (bot's) posts via `Messages.UPDATE`/`DELETE`.
4. **Bot can't read private DMs between users.** Only org admin via `/maintenanceapi/v2/...` with `OrganizationChats.READ`.
5. **Bots can't enumerate themselves via REST without `ZohoCliq.Bots.READ`.** Hardcode bot id from the bot's edit URL (`/integrations/bots/b-...`).
6. **Channel outgoing webhooks are deprecated.** Use Bot Participation Handler instead.
7. **Free Cliq plan caps searchable history at 10,000 messages** — older messages disappear from chat-message endpoints. Confirm paid tier before relying on long-range backfill.
8. **Up to 10 bots per channel.** Plan integration consolidation early.

---

## 15. mio-gateway Design (Go)

### 15.1 Responsibilities

```
[Cliq] ──HMAC POST──▶ [Cliq Inbound Handler] ──▶ [Event Bus] ──▶ [Domain Workers]
                                                                       │
                              ┌────────────────────────────────────────┤
                              ▼                                        ▼
                       [Persistence]                            [Cliq REST Client]
                       (Postgres)                               (token mgr, rate limit)
```

### 15.2 Packages

```
internal/
  cliq/
    inbound/        // HTTP handler /cliq, HMAC verify, decode envelope
    domain/         // typed structs: ChannelEvent, Message, User, Chat, Mention
    rest/           // REST client: token cache, rate limiter, retry/backoff
    rest/channel.go // PostMessage(reply_to, text, bot_unique_name)
    rest/messages.go// GetMessages, GetReactions, AddReaction, RemoveReaction
    rest/files.go   // DownloadAttachment(url) -> io.ReadCloser
    handler/        // dispatch: on message_sent → fan out to subscribers
    deluge/         // helper to render the Participation Handler text
  bot/
    mention/        // @bot detection, command parsing, intent routing
    summarize/      // chat-summarization workflow (LLM client + prompt)
    converse/       // multi-turn DM/conversation manager
  store/
    messages.go     // append-and-query store; idempotent on message.id
    attachments.go  // local + GCS-backed blob store
  config/
    config.go       // env load: client_id, secret, refresh tokens, webhook secret
```

### 15.3 Idempotency

Cliq does NOT publish delivery semantics. Treat as **at-least-once**.
- Dedupe key: `message.id` for `message_sent` / `message_edited` (an edit increments a side-channel; track a separate `event_id` if needed).
- Maintain a Redis or Postgres `event_seen_at` table TTL'd to 24h.
- All REST writes (post, react) should be safe to retry; Cliq tolerates duplicate channel posts but you'll get duplicate UX — dedupe upstream.

### 15.4 Token manager

- Cache access token with `expiresAt = now + min(expires_in - 60s, 50min)`.
- Single-flight refresh under a `sync.Mutex`.
- On 401: force one refresh attempt; if still 401, alarm and fail open (don't burn the refresh token by retrying in a loop).

### 15.5 Rate limiter

- One `golang.org/x/time/rate.Limiter` per OAuth principal (we have one).
- Defaults: `rate.Limit(15.0/60)` = 0.25/sec, burst 5.
- Wrap every REST call with `limiter.Wait(ctx)`.

### 15.6 Webhook ingress (HTTP handler)

```go
func (h *CliqInbound) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
    if !h.verifySignature(body, r.Header.Get("X-Webhook-Signature")) {
        http.Error(w, "bad signature", http.StatusUnauthorized); return
    }
    var ev domain.Envelope
    if err := json.Unmarshal(body, &ev); err != nil {
        http.Error(w, "bad json", http.StatusBadRequest); return
    }
    h.bus.Publish(r.Context(), ev) // async; ack Cliq fast
    w.WriteHeader(http.StatusOK)
}
```

`verifySignature` accepts both hex and base64 (Deluge sends base64).

### 15.7 Reply path

```go
type Reply struct {
    Channel string  // channel_unique_name
    Text    string
    QuoteID string  // optional message.id
}

func (c *Client) Reply(ctx context.Context, r Reply) error {
    return c.postChannel(ctx, r.Channel, map[string]any{
        "text":     r.Text,
        "reply_to": r.QuoteID,   // omitempty if empty
    })
}
```

`postChannel` always appends `?bot_unique_name=<env>` to the URL — no opt-out.

### 15.8 Persistence schema (Postgres minimum)

```sql
create table cliq_message (
  id              text primary key,           -- Cliq message.id (with space)
  org_id          text not null,
  chat_id         text not null,
  chat_type       text not null,              -- "channel" | "chat"
  channel_unique  text,
  user_id         text not null,
  text            text,
  msg_type        text not null,              -- "text" | "attachment" | ...
  attachment_url  text,                       -- raw URL (auth required to fetch)
  attachment_name text,
  raw             jsonb not null,             -- full event for re-parse
  received_at     timestamptz not null default now()
);
create index on cliq_message (chat_id, received_at desc);
create index on cliq_message (user_id, received_at desc);

create table cliq_event_seen (
  event_key  text primary key,                -- message.id or "<id>:edit:<rev>"
  seen_at    timestamptz not null default now()
);
```

---

## 16. Security

- **WEBHOOK_SECRET**: 32 bytes, generated once with `crypto/rand`, stored in env. Rotate by re-saving in Deluge handler + env atomically.
- **OAuth secrets**: never commit. Two refresh tokens (prod + playground) — never share across environments.
- **TLS**: edge-terminated at Cloudflare; backend is HTTP within the docker network. Don't expose the receiver port directly.
- **Egress allowlist**: Cliq Deluge invokeurl source ranges aren't documented. Verify by source IP only as a sanity check, not as auth. Trust the HMAC.
- **Replay protection**: include `data.time` (epoch ms) in the verified body; reject events older than 5 minutes to limit replay window. (Optional — Cliq doesn't sign timestamps separately.)
- **Refresh-token storage**: prefer a secrets manager (Vault, GCP Secret Manager). For now, env files chmod 600.

---

## 17. Operational Playbook

| Failure | Symptom | Action |
|---|---|---|
| Cloudflared tunnel dies | All POSTs from Cliq fail; gateway is silent | `docker compose logs cloudflared`; restart; check tunnel `info` for active conns |
| Refresh-token revoked | All REST calls 401 | Re-auth in playground; rotate the new refresh token into env via secrets manager |
| HMAC mismatch | `WARNING sig mismatch` in logs | Confirm Deluge code's secret matches env; remember Deluge emits **base64** |
| Throttle 400 | `You have exceeded the URL throttle limits` | Token-bucket aggressively; Cliq has a multi-minute lockout on repeat overshoot |
| Bot removed from channel | Silent gap in capture | Watch for `bot_removed` op; alarm; add a daily REST poll reconciliation |
| Channel `chat_id` not found | 404 on `/chats/{id}/messages` | The chat may be archived; the OAuth user may have been removed from the channel |
| Free-tier history loss | Backfill stops returning >10k-old messages | Confirm org plan; treat live capture as authoritative |

### Smoke tests (run in CI nightly):

1. `POST /api/v2/channelsbyname/__healthcheck/message` → 200/204 (need a real channel; use a private one).
2. `GET /api/v2/users/me` → 200.
3. End-to-end: gateway internal `POST /cliq` with a known signed payload → expect persisted record + 200.

---

## 18. Comparison: Capture Strategies

| Strategy | Coverage | Latency | Complexity | Use when |
|---|---|---|---|---|
| Bot Participation Handler (PUSH) | every msg in channels bot is in | seconds | low (Deluge + ingress) | **Default** for live capture |
| REST polling `/chats/{id}/messages` | chats OAuth user is in | minutes | medium (rate limits, cursor) | Backfill / archive |
| Bot Mention Handler | only `@bot` mentions | seconds | low | Synchronous in-chat reply path |
| Bot Message Handler | DMs to bot | seconds | low | Conversational bot DM |
| Maintenance API (admin) | every chat in org | minutes | high (admin creds, scope) | Compliance / e-discovery |

mio-gateway uses #1 for capture, #2 for backfill, #3+#4 if it ever needs different reply UX.

---

## 19. Open Questions

1. Cliq's webhook delivery semantics — exactly-once, at-least-once, ordered? Treat as at-least-once until Zoho documents otherwise.
2. Does Cliq sign Deluge → gateway requests with anything beyond what we set in the handler? No. The `X-Webhook-Signature` in our setup is our own.
3. Are attachment URLs valid only inside Cliq's network range, or globally with the OAuth header? Empirical: globally with header. Verify before relying.
4. What's the per-org daily message ceiling for Cliq REST writes? Not published. Plan as if 50–100k/day is safe; alarm beyond that.
5. Is `ZohoCliq.OrganizationMessages.READ` granted to non-admin OAuth users without restriction? Yes for chats user belongs to; admin path is separate (`OrganizationChats.READ`).
6. How does Cliq react to multi-bot setups in one channel (10-bot ceiling)? Order of handler execution undocumented. Avoid relying on order.

---

## References

- [Cliq REST API v2](https://www.zoho.com/cliq/help/restapi/v2/)
- [Bot Handlers overview](https://www.zoho.com/cliq/help/platform/bothandlers.html)
- [Bot Participation Handler](https://www.zoho.com/cliq/help/platform/bot-participation-handler.html)
- [Bot Message Handler](https://www.zoho.com/cliq/help/platform/bot-messagehandler.html)
- [Bot Mentions Handler](https://www.zoho.com/cliq/help/platform/bot-mentionshandler.html)
- [Cliq Responses (handler return types)](https://www.zoho.com/cliq/help/platform/cliq-responses.html)
- [Cliq Limitations (admin)](https://help.zoho.com/portal/en/kb/zoho-cliq/admin-guides/manage-organization/articles/cliq-limitations)
- [Zoho OAuth scopes reference](https://www.zoho.com/accounts/protocol/oauth/scope.html)
- [n8n-nodes-zoho-cliq scope registry](https://github.com/phil-fetchski/n8n-nodes-zoho-cliq/blob/main/nodes/ZohoCliq/v1/helpers/scopeRegistry.ts)
- Reports in `playground/cliq/plans/reports/` — five prior research reports with cited sources.
