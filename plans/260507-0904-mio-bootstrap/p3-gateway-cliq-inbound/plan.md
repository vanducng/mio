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

**Outcome:** `nats consumer next MESSAGES_INBOUND ai-consumer` receives the published message; replaying the same Cliq webhook 5× produces exactly one stream message.

## Files

- **Create:**
  - `gateway/cmd/gateway/main.go` — entrypoint, reads config, starts HTTP server
  - `gateway/internal/config/config.go` — env-driven config
  - `gateway/internal/server/server.go` — chi router, middleware (logging, recovery, OTel)
  - `gateway/internal/channels/zohocliq/handler.go` — webhook handler
  - `gateway/internal/channels/zohocliq/signature.go` — HMAC verify (`X-Zoho-Webhook-Signature` or whatever Cliq uses; carry over from `playground/cliq/receiver/`)
  - `gateway/internal/channels/zohocliq/normalize.go` — Cliq payload → `mio.v1.Message`
  - `gateway/internal/store/postgres.go` — pgx pool + queries
  - `gateway/internal/store/idempotency.go` — `EnsureUniqueMessage(accountID, sourceID) (bool, error)`
  - `gateway/internal/store/conversations.go` — `EnsureConversation(accountID, externalID, kind, parentExternalID) (Conversation, error)` (upsert)
  - `gateway/migrations/000001_init.up.sql` — `tenants`, `accounts`, `conversations`, `messages` tables (per "DB schema" below)
  - `gateway/migrations/000001_init.down.sql`
  - `gateway/internal/health/handler.go` — `/healthz`, `/readyz`
  - `gateway/Dockerfile`
  - `gateway/integration_test/cliq_inbound_test.go` (Go test using `httptest` + ephemeral NATS)
- **Modify:**
  - `Makefile` — add `gateway-run`, `gateway-build`, `gateway-migrate`
  - `deploy/docker-compose.yml` — add `gateway` service depending on `nats` + `postgres`

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

## JetStream stream/consumer (locked here; provisioned in P7 charts)

Gateway publishes to `MESSAGES_INBOUND` and reads `MESSAGES_OUTBOUND`.
Defs land in `gateway/internal/store/jetstream.go` as idempotent
`AddOrUpdateStream` calls on startup so dev `make up` works without
separate provisioning. P7 takes the same definitions to chart values.

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
- `ai-consumer` (P4) — pull, MaxAckPending=1 per `account_id+conversation_id` pair (graduation: subject-shard if needed)
- `sender-pool` (P5) — pull, MaxAckPending=32, on `MESSAGES_OUTBOUND`
- `gcs-archiver` (P6) — pull, MaxAckPending=64, on `MESSAGES_INBOUND`

## Migration tooling

Use `golang-migrate/migrate` (CLI + library). Versioned SQL files in
`gateway/migrations/`. Embed via `//go:embed` in
`gateway/internal/store/migrate.go`; gateway runs migrations on startup if
`MIO_MIGRATE_ON_START=true`. Production path: separate Job in P7.

## Steps

1. Carry over Cliq signature verification from `playground/cliq/receiver/cliq_client.py` — port to Go, double-check the header name and HMAC algorithm against the captured POC.
2. Define request schema as `cliq.WebhookPayload` (Go struct matching what Cliq actually sends — see `playground/cliq/reports/researcher-260503-1012-cliq-message-capture-deep.md`).
3. Migration 1 creates the four tables above; seed one `tenants` row (master) and one `accounts` row for the Cliq POC bot. `MIO_TENANT_ID` and `MIO_ACCOUNT_ID` env vars resolve to those UUIDs.
4. `normalize.go` maps:
   - Cliq team + `bot_unique_name` → resolve to `accounts.id` (config-driven for POC, lookup-driven later)
   - Cliq `chat_id` → `conversation.external_id`
   - Cliq DM-vs-channel signal → `ConversationKind` (`DM` for one-on-one, `CHANNEL_PUBLIC` / `CHANNEL_PRIVATE` for team channels, `THREAD` if Cliq exposes a parent reference)
   - Cliq `sender` → `Sender{external_id, display_name, peer_kind, is_bot}`
   - Cliq `message.text` → `Message.text`
   - Cliq attachments → `[]Attachment`
   - Cliq message id → `Message.source_message_id`
   - Anything Cliq-specific that doesn't fit → `attributes` map
5. `handler.go` flow: read body → verify signature → unmarshal → normalize → `store.EnsureConversation` (upsert by `(account_id, external_id)`) → `store.EnsureUniqueMessage(account_id, source_message_id)` → if fresh, `sdk.PublishInbound` → 200; if dup, 200 silently.
6. Prometheus middleware: `mio_gateway_inbound_latency_seconds{channel_type,outcome}`, `mio_gateway_inbound_total{channel_type,outcome}`. (Note: `channel_type`, not `channel`, to align with proto + subject grammar.)
7. Integration test: post a captured Cliq payload (recorded JSON in `gateway/integration_test/fixtures/cliq-message.json`); assert exactly one NATS message; replay 5× and assert still exactly one. Verify `messages` and `conversations` rows have correct `tenant_id` + `account_id`.
8. Manual smoke: `cloudflared tunnel` to localhost (script already in `playground/cliq/`), point Cliq incoming webhook at it, type a message in Cliq, see `nats stream view MESSAGES_INBOUND` show it.

## Success Criteria

- [ ] Captured Cliq payload → one `mio.v1.Message` on `MESSAGES_INBOUND` (subject matches `mio.inbound.zoho_cliq.<account_id>.<conversation_id>`)
- [ ] Published message has non-zero `tenant_id`, `account_id`, `conversation_id`, `conversation_external_id`, `conversation_kind`, `source_message_id`, `sender.external_id`
- [ ] DM payload sets `conversation_kind=DM`; channel payload sets `CHANNEL_PUBLIC`/`CHANNEL_PRIVATE`
- [ ] Signature mismatch → 401 + metric `mio_gateway_inbound_total{outcome="bad_signature"}`
- [ ] 5× replay of the same payload → 1 stream message + 4 dedupe metric increments (uniqueness enforced by `(account_id, source_message_id)`)
- [ ] p99 handler latency under load (100 rps for 30s) < 500ms
- [ ] `/readyz` returns 503 when NATS or Postgres is down

## Risks

- **Cliq signature header & algorithm** — verify against the live POC, not docs alone (Zoho docs vs reality have drifted before — see `playground/cliq/reports/`)
- **Cliq payload variants** — DM vs channel vs thread; carry-over POC has captured all three; test fixtures must cover each
- **Postgres connection pool sizing** — under burst, idempotency upsert is the bottleneck; `pgx` pool tuned to `max_conns = 2*GOMAXPROCS`
- **Webhook deadline** — Cliq's exact deadline is undocumented; assume ≤5s, target <500ms p99 to stay well under

## Out (deferred to P5)

- Outbound sender pool — separate phase
- Per-workspace rate limiting — outbound concern
