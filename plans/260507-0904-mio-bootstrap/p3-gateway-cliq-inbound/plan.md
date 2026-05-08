---
phase: 3
title: "mio-gateway + Zoho Cliq inbound"
status: pending
priority: P1
effort: "1–2d"
depends_on: [2]
---

# P3 — mio-gateway + Cliq inbound

## Overview

First real service. Webhook handler validates Cliq signature, normalizes
the payload to `mio.v1.Message`, publishes to `MESSAGES_INBOUND`, acks
the channel inside the deadline. **Gateway is dumb** — no business logic.

## Goal & Outcome

**Goal:** A POST to `/webhooks/zoho-cliq` with a valid Cliq payload publishes one `mio.v1.Message` to `MESSAGES_INBOUND` and returns 200 in <500ms p99.

URL slug uses **hyphen** per web URL convention (RFC 3986 / Google URL guidelines). Internal surfaces (registry slug, NATS subject token, metric label, GCS partition path, Go/Python identifier) use **underscore** (`zoho_cliq`) — those are not URLs. Router maps URL `zoho-cliq` → registry key `zoho_cliq` at route registration; the mapping is mechanical (`strings.ReplaceAll(slug, "-", "_")`) and lives in `server.go`.

**Outcome:** `nats consumer next MESSAGES_INBOUND ai-consumer` receives the published message; replaying the same Cliq webhook 5× produces exactly one stream message.

## Files

- **Create:**
  - `playground/cliq/captures/` — directory for live-captured Cliq webhook fixtures (DM, channel, thread, system-message, attachment). Populated in Step 0 before normalize.go is written.
  - `gateway/cmd/gateway/main.go` — entrypoint, reads config, starts HTTP server, calls `store.EnsureStreams()` on boot
  - `gateway/internal/config/config.go` — env-driven config + file-mounted secrets reader
  - `gateway/internal/server/server.go` — chi router, middleware order: **logging → recovery → OTel → Prometheus** (outermost first)
  - `gateway/internal/channels/zohocliq/handler.go` — webhook handler
  - `gateway/internal/channels/zohocliq/signature.go` — HMAC verify (`X-Webhook-Signature`, HMAC-SHA256, base64; ported from `playground/cliq/receiver/server.py:43–64`)
  - `gateway/internal/channels/zohocliq/normalize.go` — Cliq payload → `mio.v1.Message` (written **after** Step 0 captures land)
  - `gateway/internal/store/postgres.go` — pgx pool, sized `(GOMAXPROCS*2)+1` with `MIO_PGX_MAX_CONNS` override
  - `gateway/internal/store/idempotency.go` — `EnsureUniqueMessage(accountID, sourceID) (id uuid.UUID, fresh bool, err error)`
  - `gateway/internal/store/conversations.go` — `EnsureConversation(accountID, externalID, kind, parentExternalID) (Conversation, error)`; **on conflict, do NOT update display_name** (immutability rule)
  - `gateway/internal/store/migrate.go` — `//go:embed migrations/*.sql`, golang-migrate library mode
  - `gateway/internal/store/jetstream.go` — **gateway-authoritative** stream provisioning: `AddOrUpdateStream(MESSAGES_INBOUND, MESSAGES_OUTBOUND)` on startup. P7 bootstrap Job is verification-only (asserts existence; never creates).
  - `gateway/migrations/000001_init.up.sql` — `tenants`, `accounts`, `conversations`, `messages` tables (per "DB schema" below)
  - `gateway/migrations/000001_init.down.sql`
  - `gateway/internal/health/handler.go` — `/healthz` (no deps, returns 200 if process alive), `/readyz` (pings Postgres + flushes NATS, 503 if either fails)
  - `gateway/Dockerfile` — multi-stage: builder `golang:1.23-alpine` with `--mount=type=cache,target=/root/.cache/go-build` + `--mount=type=cache,target=/go/pkg/mod` for warm-build speed, `CGO_ENABLED=0 GOOS=linux` static binary, `-trimpath -ldflags="-s -w -X main.version=$BUILD_VERSION"`; runtime `gcr.io/distroless/static-debian12:nonroot` (UID 65532, no shell, no apt, ~2 MB base + ~20 MB binary = ~22 MB image); `EXPOSE 8080`; `ENTRYPOINT ["/gateway"]`. Build context is **repo root** (not `gateway/`) so single-module + `proto/gen/go` are reachable; `.dockerignore` (P0) keeps context small. (Research Section 3 picked distroless over alpine/scratch on attack-surface vs. debuggability tradeoff for production-grade POC.)
  - `gateway/integration_test/cliq_inbound_test.go` (Go test using `httptest` + ephemeral in-process NATS server with JetStream enabled; fixtures from `playground/cliq/captures/`)
- **Modify:**
  - `Makefile` — add `gateway-run`, `gateway-build`, `gateway-migrate`, `cliq-capture` (cloudflared tunnel + receiver into `playground/cliq/captures/`)
  - `deploy/docker-compose.yml` — add `gateway` service depending on `nats` + `postgres`; mount `/etc/mio/secrets/` from local dev secret dir

## DB schema (locked from research — foundation)

```sql
-- tenants: master tenant seeded for POC; mio.tenant_id env var resolves to its UUID
CREATE TABLE tenants (
  id          UUID PRIMARY KEY,
  slug        TEXT NOT NULL UNIQUE,
  status      TEXT NOT NULL DEFAULT 'active',
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- accounts: one row per (tenant, channel install). Cliq POC seeds one row.
CREATE TABLE accounts (
  id           UUID PRIMARY KEY,
  tenant_id    UUID NOT NULL REFERENCES tenants(id),
  channel_type TEXT NOT NULL,                 -- must be in proto/channels.yaml registry
  external_id  TEXT NOT NULL,                 -- platform install id (e.g. cliq team + bot_unique_name)
  display_name TEXT NOT NULL,
  attributes   JSONB NOT NULL DEFAULT '{}',
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (tenant_id, channel_type, external_id)
);

-- conversations: polymorphic via kind; one row per DM/group/channel/thread.
CREATE TABLE conversations (
  id                     UUID PRIMARY KEY,
  tenant_id              UUID NOT NULL REFERENCES tenants(id),
  account_id             UUID NOT NULL REFERENCES accounts(id),
  channel_type           TEXT NOT NULL,
  kind                   TEXT NOT NULL,       -- ConversationKind enum string
  external_id            TEXT NOT NULL,       -- platform-side opaque (cliq chat_id, slack channel, ...)
  parent_conversation_id UUID REFERENCES conversations(id),
  parent_external_id     TEXT,
  display_name           TEXT,
  attributes             JSONB NOT NULL DEFAULT '{}',
  created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (account_id, external_id)            -- the idempotent address
);
CREATE INDEX ON conversations (tenant_id);
CREATE INDEX ON conversations (account_id, kind);
CREATE INDEX ON conversations (parent_conversation_id) WHERE parent_conversation_id IS NOT NULL;

-- messages: idempotency live here; (account_id, source_message_id) unique catches replays.
CREATE TABLE messages (
  id                     UUID PRIMARY KEY,
  tenant_id              UUID NOT NULL REFERENCES tenants(id),
  account_id             UUID NOT NULL REFERENCES accounts(id),
  conversation_id        UUID NOT NULL REFERENCES conversations(id),
  thread_root_message_id UUID REFERENCES messages(id),
  source_message_id      TEXT NOT NULL,
  sender_external_id     TEXT NOT NULL,
  text                   TEXT NOT NULL DEFAULT '',
  attributes             JSONB NOT NULL DEFAULT '{}',
  received_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (account_id, source_message_id)
);
CREATE INDEX ON messages (tenant_id, conversation_id, received_at DESC);
```

`tenant_id` and `account_id` are NOT NULL on every table from row 1 — no
nullable-then-tighten dance, no `'.'` workspace strings, no migration-27
retrofit. RLS, `channel_users`, member roster: deferred (don't break the
wire when added later).

## JetStream stream/consumer (gateway-authoritative provisioning)

**Cross-phase contract:** gateway startup is the **single source of truth**
for stream provisioning. `gateway/internal/store/jetstream.go` calls
`jsm.AddOrUpdateStream(MESSAGES_INBOUND)` and `AddOrUpdateStream(MESSAGES_OUTBOUND)`
on every boot. Idempotent — repeats are no-ops if config matches.

The P7 `mio-jetstream-bootstrap` Job is **verification-only**: it asserts
both streams exist with the locked config and exits non-zero otherwise. It
**never creates or mutates** streams. This avoids two-writer races between
gateway boot and the K8s Job.

Gateway publishes to `MESSAGES_INBOUND` and reads `MESSAGES_OUTBOUND` (P5
sender pool consumes from MESSAGES_OUTBOUND; gateway only reads it for
delivery-status side-channel, deferred to P5).

```
Stream MESSAGES_INBOUND
  subjects:    mio.inbound.>
  retention:   limits
  max_age:     168h               # 7 days; archive lives in GCS sink
  storage:     file
  replicas:    3 in cluster, 1 locally
  duplicates:  120s                # NATS-side dedup window for Nats-Msg-Id

Stream MESSAGES_OUTBOUND
  subjects:    mio.outbound.>
  retention:   workqueue           # consumed once by sender pool
  max_age:     24h
  duplicates:  60s
```

Consumers created by the consuming services, not gateway:
- `ai-consumer` (P4) — pull, MaxAckPending=1 globally (graduation: shard by subject `mio.inbound.<ct>.<acct>.<conv>` when load forces, one consumer per shard)
- `sender-pool` (P5) — pull, MaxAckPending=32, on `MESSAGES_OUTBOUND`
- `gcs-archiver` (P6) — pull, MaxAckPending=64, on `MESSAGES_INBOUND`

## Migration tooling

Use `golang-migrate/migrate` (library mode). Versioned SQL files in
`gateway/migrations/`, embedded via `//go:embed` in
`gateway/internal/store/migrate.go`. Gateway calls `migrateUp()` on startup
gated by `MIO_MIGRATE_ON_START`:

- **Default `true`** in dev (`make up` just works).
- **Default `false`** in prod (P7 ships a separate `mio-gateway-migrate`
  Job; the running gateway must not race against itself across replicas).

On dirty-state failure, `golang-migrate` requires manual `migrate force` —
acceptable for POC's 4-table schema; revisit if the schema grows.

## Steps

### Step 0 — Pre-flight: Cliq verification (gates implementation)

`normalize.go` cannot be written from docs alone — Zoho's webhook spec is
sparse and the POC has only partially exercised payload variants. Before
any of Steps 2–8, capture **live** Cliq webhooks into
`playground/cliq/captures/` and answer the 6 open questions below. Without
these, `ConversationKind` mapping and the deadline budget are guesswork.

**0.1 Capture rig.** Reuse `playground/cliq/receiver/server.py` behind
`cloudflared tunnel`. Add a write-to-disk path: every received POST →
`playground/cliq/captures/<ISO-ts>-<operation>.json` containing
`{headers, body_raw, body_json, ack_ms}`. Run for ~10 minutes covering:

- DM bot → user (Message Handler)
- Channel post in a public channel (Participation Handler)
- Channel post in a **private** channel (need to create one)
- Thread reply
- Member-join / member-leave system event
- Message with image attachment, file attachment, link preview

**0.2 Answer the 6 questions** (record findings in
`playground/cliq/captures/FINDINGS.md`):

1. **DM signal** — what field name carries DM-ness? (`chat.is_dm`,
   `is_im`, presence-by-absence, …) — must be present in **every** DM
   payload.
2. **Private channel signal** — does Cliq emit `is_private` (or similar)
   for restricted channels? If absent, document the workaround
   (e.g. derive from `chat.kind`).
3. **Thread parent reference** — exact field name + value shape for the
   parent message id (maps to `messages.thread_root_message_id` and
   `conversations.parent_external_id`).
4. **System message sender** — for member-join/leave/system events, does
   `user`/`sender` exist, is it null, or is it a synthetic bot user?
   Decide null-handling policy (`sender_external_id="system"`,
   `is_bot=true`).
5. **Webhook deadline** — measure round-trip: receiver sleeps incrementally
   (250ms, 1s, 3s, 5s, 8s) and responds 200; record at what point Cliq
   stops waiting / retries / marks failed. Compare against assumed ≤5s.
6. **Response shape** — does Cliq accept `200 + empty body`? `204`?
   Require JSON? Test all three; pick the simplest that Cliq accepts and
   doesn't trigger a retry.

**0.3 Exit gate.** Step 0 is complete only when:

- ≥6 distinct fixture files exist in `playground/cliq/captures/`
- `FINDINGS.md` answers all 6 questions with a concrete
  field-name/value/timing
- The `ConversationKind` decision tree is locked in `FINDINGS.md`

### Step 1 — Schema, config, scaffolding

1. `golang-migrate` migration `000001_init` creates the four tables (per
   "DB schema" above). Seed one `tenants` row (master, slug="local") and
   one `accounts` row for the Cliq POC bot (`channel_type="zoho_cliq"`,
   `external_id=<cliq_team>+<bot_unique_name>`). UUIDs go to env via
   `MIO_TENANT_ID`, `MIO_ACCOUNT_ID`.
2. `config.go` reads env: `MIO_PORT`, `MIO_LOG_LEVEL`, `MIO_TENANT_ID`,
   `MIO_ACCOUNT_ID`, `MIO_NATS_URLS`, `MIO_POSTGRES_DSN`,
   `MIO_PGX_MAX_CONNS` (default `(GOMAXPROCS*2)+1`),
   `MIO_MIGRATE_ON_START` (default `true` in dev, `false` in prod),
   `MIO_GRACEFUL_SHUTDOWN_SECS` (default 15).
3. **Secrets via file mount, never env.** Read
   `MIO_CLIQ_WEBHOOK_SECRET` from `/etc/mio/secrets/cliq-webhook-secret`
   (TrimSpace). NATS auth token, Postgres password, future Cliq OAuth —
   same pattern under `/etc/mio/secrets/...`. Local dev mounts
   `./deploy/local-secrets/` to that path.
4. `pgx` pool: `MaxConns = MIO_PGX_MAX_CONNS or (GOMAXPROCS*2)+1`.
   `MinConns = 1`. Default idle timeout. Pre-warm by issuing one `SELECT 1`
   at startup so the first webhook isn't paying connection cost.

### Step 2 — Router + middleware

5. `chi` router. Middleware order (outermost to innermost):
   **logging → recovery → OTel → Prometheus**. Logging captures method,
   path, status, duration, request id. Recovery converts panics to 500 +
   error log. OTel root span starts at request entry. Prometheus
   histogram + counter wraps at the bottom so it sees the final status.
6. Routes: `POST /webhooks/zoho-cliq` → handler (URL hyphen per web convention; router converts to registry key `zoho_cliq` internally). `GET /healthz`,
   `GET /readyz`. `GET /metrics` (Prometheus exposition).

### Step 3 — Signature verification

7. Port `playground/cliq/receiver/server.py:43–64` to Go. Header:
   `X-Webhook-Signature` (case-insensitive via `r.Header.Get`).
   Algorithm: HMAC-SHA256. Encoding: prefer base64; accept hex as fallback
   if Step 0 confirms Cliq emits hex. Sign the **raw body bytes** before
   any unmarshal. Constant-time compare.
8. Bad signature → `401 {"error":"invalid signature"}` (not 403). Emit
   `mio_gateway_inbound_total{channel_type="zoho_cliq",outcome="bad_signature"}`.
   No rate-limit middleware in P3 (defer; emit metric, alert on spike).

### Step 4 — Normalize (informed by Step 0 captures)

9. `cliq.WebhookPayload` Go struct matches Step 0's findings exactly. No
   speculative fields.
10. `normalize.go` maps per Step 0's locked decision tree:
    - Cliq team + `bot_unique_name` → resolve to `accounts.id`
      (config-driven for POC; lookup-driven post-P9).
    - Cliq `chat.id` → `conversation.external_id`.
    - DM signal (Step 0 §1) → `ConversationKind=DM`.
    - Private flag (Step 0 §2) → `CHANNEL_PRIVATE` else `CHANNEL_PUBLIC`.
    - Thread parent ref (Step 0 §3) → `parent_external_id` on conversation;
      `thread_root_message_id` on messages (resolved via second lookup).
    - `user.{id,name,is_bot}` → `Sender{external_id,display_name,is_bot}`.
      If absent (Step 0 §4 system messages), synthesize
      `external_id="system"`, `is_bot=true`.
    - `message.text` → `Message.text`.
    - `message.attachments` → `attributes["cliq_attachments"]` JSONB
      (no per-channel attachment proto in v1).
    - Cliq message id → `source_message_id`.
    - Everything else → `attributes` map.

### Step 5 — Idempotent persistence + publish

11. `EnsureConversation`:
    ```sql
    INSERT INTO conversations (id, tenant_id, account_id, channel_type,
      kind, external_id, parent_conversation_id, parent_external_id,
      display_name, attributes)
    VALUES ($1,...,$10)
    ON CONFLICT (account_id, external_id) DO NOTHING
    RETURNING id;
    ```
    On `pgx.ErrNoRows` (conflict fired), `SELECT id FROM conversations
    WHERE account_id=$1 AND external_id=$2`. **Never UPDATE display_name
    or kind on conflict** — first-write-wins (immutability rule). Kind
    miscoding is logged CRITICAL and surfaces via metric; correction is
    deferred (out of P3).
12. `EnsureUniqueMessage`:
    ```sql
    INSERT INTO messages (id, tenant_id, account_id, conversation_id,
      thread_root_message_id, source_message_id, sender_external_id,
      text, attributes, received_at)
    VALUES ($1,...,$10)
    ON CONFLICT (account_id, source_message_id) DO NOTHING
    RETURNING id;
    ```
    Detect fresh-vs-dup via row count: `RETURNING id` returning a row =
    fresh; `pgx.ErrNoRows` = dup. On dup, increment
    `mio_idempotency_dedup_total{channel_type="zoho_cliq"}` and skip
    publish.
13. **Handler flow (publish-before-secondary-writes):**
    1. Read body (buffered).
    2. Verify signature.
    3. Unmarshal → `cliq.WebhookPayload`.
    4. `EnsureConversation` (FK requires it before message).
    5. `EnsureUniqueMessage` → returns `(id, fresh)`.
    6. If `fresh`: `sdk.PublishInbound(ctx, msg)` — schema-version
       enforced inside SDK (P2). Subject:
       `mio.inbound.zoho_cliq.<account_id>.<conversation_id>.<message_id>`.
    7. Ack Cliq: `200` with body shape from Step 0 §6.
    8. (Outside the deadline-critical path) any non-essential writes; OTel
       root span ends here.
14. OTel root span starts at signature-verify (not request entry — health
    checks shouldn't pollute traces). Trace id propagates into NATS
    message header `mio-trace-id`.

### Step 6 — Health + observability

15. `/healthz` — no deps; returns 200 unconditionally as long as the
    process is running.
16. `/readyz` — pings Postgres (`pgPool.Ping(ctx)`) AND flushes NATS
    (`natsConn.Flush(ctx)`) with 2s timeout. Either failure → 503.
17. Prometheus metrics:
    - `mio_gateway_inbound_latency_seconds{channel_type,direction,outcome}`
      histogram (p50/p95/p99)
    - `mio_gateway_inbound_total{channel_type,direction,outcome}` counter
    - `mio_idempotency_dedup_total{channel_type}` counter
    - **Label set is exactly `channel_type`, `direction`, `outcome`** —
      no `account_id`, no `conversation_id` (cardinality discipline per
      cross-phase contract).

### Step 7 — Stream provisioning

18. On boot, after pgx pool is up, before HTTP server starts,
    `store.EnsureStreams(ctx, js)` calls `js.AddOrUpdateStream(...)` for
    `MESSAGES_INBOUND` and `MESSAGES_OUTBOUND` with the locked config
    (subjects, retention, max_age, replicas, duplicates window). Failure
    fails fast — gateway exits 1 (do not start serving without streams).

### Step 8 — Tests + smoke

19. Integration test: post each captured fixture from
    `playground/cliq/captures/`. Assert:
    - Exactly one NATS message published per unique payload.
    - Replay each fixture 5× → still exactly one message; dedup metric
      increased by 4 per fixture.
    - `messages` and `conversations` rows have non-null `tenant_id`,
      `account_id`, correct `kind`.
    - Bad-signature variant returns 401 + bumps the
      `outcome="bad_signature"` counter.
20. Load test: `vegeta` 100 rps × 30s with the DM fixture; assert p99 <
    500ms and zero stream-message duplication.
21. Readiness probe test: stop Postgres → `/readyz` returns 503 within
    2s; restart → returns 200. Same for NATS.
22. Manual smoke: `make cliq-capture` + Cliq → see new message in
    `nats stream view MESSAGES_INBOUND`.

## Success Criteria

- [ ] **Step 0 artifacts exist:** ≥6 fixture files in `playground/cliq/captures/` covering DM, public channel, private channel, thread reply, system message, attachment; `FINDINGS.md` answers all 6 open questions
- [ ] Captured Cliq payload → one `mio.v1.Message` on `MESSAGES_INBOUND` with subject matching `mio.inbound.zoho_cliq.<account_id>.<conversation_id>.<message_id>`
- [ ] Published message has non-zero `tenant_id`, `account_id`, `conversation_id`, `conversation_external_id`, `conversation_kind`, `source_message_id`, `sender.external_id`
- [ ] DM fixture sets `conversation_kind=DM`; public-channel fixture sets `CHANNEL_PUBLIC`; private-channel fixture sets `CHANNEL_PRIVATE`; thread fixture sets `THREAD` with non-null `parent_external_id`
- [ ] Signature mismatch → 401 + `mio_gateway_inbound_total{channel_type="zoho_cliq",outcome="bad_signature"}` increments
- [ ] **Replay-5× per fixture → exactly 1 NATS stream message + 4 `mio_idempotency_dedup_total{channel_type="zoho_cliq"}` increments** (uniqueness from `(account_id, source_message_id)`)
- [ ] **Load test:** 100 rps × 30s → p99 handler latency < 500ms, zero duplicate stream messages
- [ ] **`/readyz` returns 503** within 2s when Postgres is down; same when NATS is down; both → 503
- [ ] **`/healthz` returns 200** even when Postgres + NATS are both down (process-only check)
- [ ] On boot, `MESSAGES_INBOUND` and `MESSAGES_OUTBOUND` exist with locked config (subjects, retention, max_age, replicas, duplicates) — verify via `nats stream info`
- [ ] Gateway exits 1 if stream provisioning fails (does not start serving in a bad state)

## Risks

- **Cliq deadline drift** — claimed ~5s is undocumented; could be tighter (3s) or with retries that mask slowness. Mitigation: Step 0 measures it directly; alert on `latency_seconds` p99 > 400ms (100ms safety margin).
- **Signature header drift** — POC uses `X-Webhook-Signature` HMAC-SHA256 base64. Cliq may have rotated to a new header name or added algorithm choices. Mitigation: Step 0 captures real headers; signature verifier accepts hex fallback; fail-fast 401 with metric.
- **Payload-shape drift across DM/channel/thread/system** — captured POC fixtures may be stale (Cliq UI updates change payload subtly). Mitigation: Step 0 re-captures all variants; struct definition matches captured fixtures exactly, no speculative fields.
- **Pool undersized under burst** — idempotency upsert holds a connection ~5–10ms; bursts exceeding `(GOMAXPROCS*2)+1` queue and inflate p99. Mitigation: pre-warm pool at boot; expose `MIO_PGX_MAX_CONNS` for hot tuning; add `pgx_pool_acquire_duration_seconds` metric (P7 follow-up).
- **Conversation kind miscoding becomes silent corruption** — `ON CONFLICT DO NOTHING` won't fix a wrongly-coded first row. Mitigation: log CRITICAL on kind-mismatch detection, emit metric, defer auto-correction to P5.
- **NATS publish latency exceeds budget when cluster is remote** — local dev compose is fast; GKE network adds 10–30ms. Mitigation: publish-before-secondary-writes; benchmark in P8; prom histogram catches regressions.
- **Migration dirty-state on prod** — `golang-migrate` requires manual `force` to recover. Mitigation: P3 schema is small (4 tables); P7 ships a separate migrate Job and gateway disables auto-migrate in prod.

## Out (deferred)

- **Outbound sender pool** — P5
- **Per-workspace rate limiting (outbound)** — P5
- **Inbound rate-limiting / bad-signature DDoS brake** — accepted as POC-stage risk. App-layer is metric-only (`mio_gateway_inbound_total{outcome="bad_signature"}` + alert). Before P9 second-channel ships, add ingress-level brake (NGINX `limit_req` or Cloud Armor rule) so a leaked Cliq webhook URL can't burn HMAC verification budget. Tracked here to avoid silently postponing past the POC.
- **Stream-config split-brain across gateway replicas** — `AddOrUpdateStream` is idempotent on identical config, but two replicas booting with disagreeing config (mid-upgrade) could ping-pong stream config. POC runs single replica until P7; before scaling P7 to 2 replicas, document the rolling-upgrade-must-be-sequential assumption in `jetstream.go` package comment.

## Research backing

[`plans/reports/research-260508-1056-p3-gateway-cliq-inbound-webhook.md`](../../reports/research-260508-1056-p3-gateway-cliq-inbound-webhook.md)

Validated picks: **chi** router, **pgx** with `(GOMAXPROCS*2)+1` pool, **`INSERT … ON CONFLICT DO NOTHING RETURNING id`** for idempotency upsert, **golang-migrate + `//go:embed`**, publish-before-secondary-writes deadline strategy, 401 (not 403) on bad signature, secrets via file mount (not env).

**Open Cliq questions that need live verification before/during P3 implementation** (cannot be answered from docs alone — capture from a live Cliq webhook):
1. DM signal — is `chat.is_dm` (or equivalent) present in every payload?
2. Private channel signal — does Cliq emit `is_private` for restricted channels?
3. Thread parent — exact field name + format for parent reference (maps to `parent_conversation_id`).
4. System messages (member-join etc.) — does the `user`/`sender` field exist?
5. Webhook deadline — measured round-trip vs claimed (target <500ms p99 keeps wide margin if true ≤5s).
6. Response shape — 200 with empty body vs 204; any required body content.

**Action: capture a live webhook into `playground/cliq/captures/` before writing `normalize.go`.** Without these, the `ConversationKind` mapping is guesswork.

Day-1 metrics to instrument: `mio_gateway_inbound_latency_seconds{channel_type,outcome}` histogram, `mio_gateway_inbound_total{channel_type,outcome}`, `mio_idempotency_dedup_total{channel_type}` (sanity-check redelivery rate).
