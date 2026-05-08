---
phase: 5
title: "Outbound path → Cliq"
status: pending
priority: P1
effort: "1d"
depends_on: [3, 4]
---

# P5 — Outbound path → Cliq

## Overview

Gateway gains a sender pool that drains `MESSAGES_OUTBOUND`, applies the
per-account rate limit (one bucket per `account_id`, not per-`channel_type`
nor a global one), dispatches by `channel_type` string to the matching
adapter, calls Cliq REST, and handles the two-step "thinking…" UX
(initial send → edit-in-place when AI returns the real answer).

**Foundation alignment**: dispatch is by `channel_type` *string* against
the `proto/channels.yaml` registry. There is no `Channel.Type` enum;
adding a channel = new entry in YAML + new package under
`gateway/internal/channels/<slug>/`. No proto regen, no SDK redeploy.

## Goal & Outcome

**Goal:** `MESSAGES_OUTBOUND` is drained reliably; outbound delivery to Cliq honors per-account token buckets; `SendCommand{edit_of_message_id, edit_of_external_id}` results in a Cliq edit, not a new message.

**Outcome:** Echo loop produces visible Cliq replies in the original thread. Bursting one account's outbound (50 messages) does not delay another account's outbound (still <2s p99).

## Files

- **Create:**
  - `gateway/internal/sender/pool.go` — pull-fetch loop, worker pool sized via env; graceful shutdown drain
  - `gateway/internal/sender/dispatch.go` — defines `type Dispatcher struct { byChannel map[string]Adapter }` with `func New(adapters []Adapter) *Dispatcher` (panics on duplicate `ChannelType()` slug) and `func (d *Dispatcher) ForCommand(cmd *miov1.SendCommand) Adapter` (returns adapter for `cmd.ChannelType`; nil → 4xx terminate). **Zero adapter-specific branches** (P9 litmus); lookup table populated from `sender.RegisteredAdapters()` at `main.go` startup, after every adapter package's `init()` has run.
  - `gateway/internal/sender/adapter.go` — minimal `Adapter` interface (Send, Edit, ChannelType, MaxDeliver, RateLimitKey)
  - `gateway/internal/sender/registry.go` — mutex-protected global `registerAdapter(Adapter)` called from each channel package's `init()`
  - `gateway/internal/channels/zohocliq/init.go` — package-local `init()` that constructs the Cliq adapter and calls `sender.RegisterAdapter(...)` (the litmus surface — P9 mirrors this for Slack)
  - `gateway/internal/channels/zohocliq/sender.go` — REST client implementing `sender.Adapter` (Send + Edit + MaxDeliver + RateLimitKey)
  - `gateway/internal/channels/zohocliq/sender_edit.go` — edit-in-place via `PATCH /api/v2/chats/{chatid}/messages/{msgid}` (PUT vs PATCH verified in P3 — see Notes)
  - `gateway/internal/store/outbound_state.go` — in-memory map keyed by `SendCommand.id` for two-step UX (returns platform `external_id` after first send); LRU cap 10k, TTL 10m
  - `gateway/internal/ratelimit/account.go` — per-account `golang.org/x/time/rate.Limiter`, TTL-evicted, dynamic active-account gauge
  - `gateway/internal/ratelimit/account_test.go`
  - `gateway/integration_test/cliq_outbound_test.go`
  - `gateway/integration_test/cliq_outbound_fairness_test.go` — custom Go bench with concurrent goroutines (A bursts 50/sec, B single message, asserts B p99 <2s)
- **Modify:**
  - `gateway/cmd/gateway/main.go` — blank-import each channel package (e.g. `_ "mio/gateway/internal/channels/zohocliq"`); start sender pool alongside HTTP server; bind SIGTERM → graceful shutdown
  - `Makefile` — `gateway-bench-outbound` target wraps the fairness Go test

## Adapter interface (minimal — research-validated)

```go
// gateway/internal/sender/adapter.go
type Adapter interface {
    // Send a fresh outbound; returns the platform's external message id
    // so we can later edit it. Idempotency by SendCommand.id is the
    // pool's job (NATS dedup + outbound_state lookup), not the adapter's.
    Send(ctx context.Context, cmd *miov1.SendCommand) (externalID string, err error)

    // Edit an existing message; cmd.edit_of_external_id is the platform id.
    Edit(ctx context.Context, cmd *miov1.SendCommand) error

    // ChannelType returns the registry slug this adapter handles.
    ChannelType() string                    // e.g. "zoho_cliq"

    // MaxDeliver overrides the consumer's max_deliver for this channel.
    // Cliq returns 5 (default); flaky channels can return higher.
    MaxDeliver() int

    // RateLimitKey returns the bucket key for this command. Empty string
    // means "use account_id default". Slack-style adapters return composite
    // "account_id:conversation_external_id" for per-conversation fairness.
    RateLimitKey(cmd *miov1.SendCommand) string
}
```

**Deliberately omitted** (per research recommendation, not adopted until two channels need them): `Delete`, `React`, `Typing`. YAGNI; minimal interface keeps Cliq adapter ~60 LOC and Slack adapter on P9 lean.

### Self-registration via init() blocks (the P9 litmus surface)

```go
// gateway/internal/sender/registry.go
var (
    regMu      sync.Mutex
    registered []Adapter
)
func RegisterAdapter(a Adapter) { regMu.Lock(); registered = append(registered, a); regMu.Unlock() }
func RegisteredAdapters() []Adapter { regMu.Lock(); defer regMu.Unlock(); return slices.Clone(registered) }

// gateway/internal/channels/zohocliq/init.go
func init() {
    sender.RegisterAdapter(New( /* config from env */ ))
}

// gateway/cmd/gateway/main.go
import _ "mio/gateway/internal/channels/zohocliq"  // P9: add `_ "mio/gateway/internal/channels/slack"`
```

`dispatch.go` builds its `map[string]Adapter` from `sender.RegisteredAdapters()` at startup. **No adapter-specific branches anywhere** — that is the P9 litmus.

**`init()` ordering risk** acknowledged: registration is order-independent because dispatch builds the map *after* all `init()` blocks run (Go guarantees `init` precedes `main`). If two adapters register the same `ChannelType()` slug, dispatch panics at startup — fail fast.

## Per-account rate limit

```go
// gateway/internal/ratelimit/account.go
type Limiter struct {
    mu      sync.RWMutex
    buckets map[string]*rate.Limiter      // key: returned by Adapter.RateLimitKey, default account_id
    rate    rate.Limit                    // tokens/sec; default 5
    burst   int                           // burst size; default 10
    ttl     time.Duration                 // 10m idle eviction
    lastUse map[string]time.Time
}
```

**Default key is `account_id`** (UUID). Adapter override pattern: pool calls
`key := adapter.RateLimitKey(cmd); if key == "" { key = cmd.AccountId }`
before bucket lookup. Slack/Discord adapters can later return the composite
`account_id:conversation_external_id` for per-conversation fairness — no
wire-format change needed.

Reasons `account_id` is the right default key:
1. `account_id` already encodes the (tenant, channel install) pair — the unit a platform's rate limit attaches to.
2. One tenant running two Slack workspaces gets two `account_id`s → two buckets, no cross-throttling.
3. Per-(workspace, channel) limits are an adapter override, not a core concept — the wire stays clean.

**Eviction goroutine**: scans `lastUse` every 60s, drops entries idle > `ttl`. Two metrics:
- `mio_gateway_ratelimit_buckets_active` (gauge) — current bucket count.
- `mio_gateway_ratelimit_buckets_evicted_total` (counter) — eviction confirms goroutine is alive.

Alert rule: `buckets_active > 10× active_account_count` for 5min → eviction goroutine likely dead.

## Steps

1. **Adapter self-registration**. Each channel package (`gateway/internal/channels/zohocliq/init.go`, later `.../slack/init.go`) carries an `init()` block that builds an adapter from env config and calls `sender.RegisterAdapter(...)`. `main.go` only blank-imports the package: `_ "mio/gateway/internal/channels/zohocliq"`. After all `init()` blocks finish, `dispatch.New(sender.RegisteredAdapters())` builds the `map[channel_type]Adapter`. **`dispatch.go` has zero adapter-specific branches** — this is the P9 zero-edit litmus.

2. **Sender pool boots**. `main.go` builds `dispatcher := sender.New(sender.RegisteredAdapters())` once after all adapter `init()`s have run, then constructs `pool := sender.NewPool(dispatcher, ...)`. `sender.Pool` opens a pull subscription on the `sender-pool` durable consumer (`MaxAckPending=32`, `ack_wait=30s`, on `MESSAGES_OUTBOUND`; consumer provisioned at gateway startup, authoritative source). Worker count comes from env (`MIO_SENDER_WORKERS`, default 8). Each worker fetches a batch and calls `dispatcher.ForCommand(cmd).Send(...)` or `.Edit(...)` per message; an unregistered `channel_type` returns nil → message is `Term`'d with `reason="other"`.

3. **Rate-limit gate** (per command, before HTTP call):
   - `key := adapter.RateLimitKey(cmd); if key == "" { key = cmd.AccountId }`.
   - `limiter.Allow(key)` — if false, `msg.Nak(WithDelay(jitter))` where jitter draws from `[bucket_refill_interval, 2*bucket_refill_interval]`. Don't burn another worker slot retrying immediately.
   - Eviction goroutine (60s tick) prunes idle keys > 10min and updates the `mio_gateway_ratelimit_buckets_active` gauge.

4. **Outbound dedup** (idempotency address `out:<send_command.id>`):
   - Adapter checks `outbound_state[cmd.Id]` *before* HTTP. If present, skip the send and return the cached `external_id` with success — handles within-session re-deliveries even after the 60s NATS dedup window.
   - Otherwise call adapter `Send`/`Edit` and write `(cmd.Id → external_id)` into `outbound_state` on success.

5. **Cliq REST call**. `Send`: `POST /api/v2/chats/{chatid}/messages`, returns `id`. `Edit`: `PATCH /api/v2/chats/{chatid}/messages/{msgid}` (PUT vs PATCH is a P3 verification item — see Notes; adapter wires through whichever P3 confirms). No `Idempotency-Key` header (Cliq doesn't document one); we rely solely on `outbound_state`.

6. **HTTP outcome routing**:
   - **429** — read `Retry-After` header *first*, then compute `delay = max(retryAfter, jitter(bucket_refill))`, then `msg.Nak(WithDelay(delay))`. Ignoring `Retry-After` causes re-deliveries to drain the bucket faster than refill — explicit research warning.
   - **5xx / network** — `Nak`, rely on `MaxDeliver()` (default 5). Increment `mio_gateway_outbound_retry_total{channel_type, http_status}` (status bucketed: `5xx`, `4xx`, `429`, `2xx`, `network`).
   - **4xx (excluding 429)** — `Term`. Increment `mio_gateway_outbound_terminated_total{channel_type, reason}` where `reason` is bounded (`auth`, `not_found`, `bad_request`, `forbidden`, `other`). DLQ stream is **deferred** — metrics-only is the POC contract until the first real terminated message in the wild justifies a stream.
   - **2xx** — `Ack`, write `outbound_state`, increment `mio_gateway_outbound_sent_total{channel_type, outcome="ok"}`.

7. **Two-step "thinking…" UX** (in-memory map, restart-during-edit fails open):
   - AI publishes initial `SendCommand{id=A, text="thinking…", edit_of_message_id="", edit_of_external_id=""}`. Gateway sends → Cliq returns `external_id=X` → `outbound_state[A] = X`.
   - AI publishes the final `SendCommand{id=B, attributes["replaces_send_id"]=A, edit_of_message_id=A, edit_of_external_id=""}` (AI knows `A` from its own bookkeeping; doesn't know `X`). Reusing the same `id=A` is also valid — gateway treats the `attributes["replaces_send_id"]` correlator as authoritative.
   - Gateway resolves: `external_id := outbound_state[cmd.attributes["replaces_send_id"]]` (or `cmd.EditOfMessageId`) → fills in `cmd.EditOfExternalId = external_id` → adapter `Edit`.
   - **Failure mode (documented, accepted for POC)**: gateway restarts between the "thinking" send and the final edit → `outbound_state` is empty → resolver miss → adapter falls back to `Send` instead of `Edit` (a fresh "answer" message appears, the "thinking…" is left dangling). Metric: `mio_gateway_outbound_edit_fallback_total{channel_type, reason="state_missing"}`. Persistent `outbound_state` is deferred until this metric is non-zero in production.

8. **Schema-version Verify on consume**. SDK consumer wraps `nats.Msg` → calls SDK `Verify()` → rejects `schema_version > sdk_max_version` with `Term` + metric `mio_gateway_outbound_schema_reject_total`. AI publishes outbound; gateway is the consumer here.

9. **Graceful shutdown** (SIGTERM → drain):
   - Stop pulling new messages from `sender-pool`.
   - Wait for in-flight workers up to 30s (`MIO_SHUTDOWN_DRAIN=30s`).
   - Any worker still in HTTP flight at deadline: cancel context → message left un-Acked → JetStream redelivers on next start (idempotency catches it via `outbound_state` if same gateway instance, or via NATS dedup window if rapid restart).
   - Then exit.

10. **Fairness benchmark** (`gateway/integration_test/cliq_outbound_fairness_test.go`, run via `make gateway-bench-outbound`):
    - Custom Go test (no vegeta/k6 — heavier than needed for a single-assertion internal bench).
    - Goroutine A bursts 50 messages/sec to account `A` for 10s. Goroutine B sends 1 message/5s to account `B` throughout.
    - Mock the adapter's HTTP call with a 50ms sleep so the rate-limiter is the actual gate.
    - Collect B's per-message end-to-end latency, sort, compute p99. Assert `p99 < 2s`.

## Success Criteria

- [ ] Echo loop produces a visible Cliq reply in the same thread within 5s of a user's message
- [ ] `SendCommand{edit_of_message_id: ..., edit_of_external_id: ...}` results in a Cliq edit, not a new message
- [ ] Sending with only `edit_of_message_id` / `attributes["replaces_send_id"]` (no external) → gateway resolves via `outbound_state` and edits successfully
- [ ] **429 with `Retry-After`** → Nak delay is `max(Retry-After, jitter)` (verifiable in test that mocks `Retry-After: 7` and asserts the next redelivery is >7s later, not jitter-only)
- [ ] **Restart-during-edit produces a fresh message** (not a hang or crash) and `mio_gateway_outbound_edit_fallback_total{reason="state_missing"}` increments — POC failure-mode contract
- [ ] **Fairness bench passes**: `make gateway-bench-outbound` reports B p99 < 2s while A bursts 50/sec
- [ ] **`dispatch.go` has zero adapter-specific branches** (grep test in CI: `! grep -E "zoho|slack|cliq|telegram" gateway/internal/sender/dispatch.go`)
- [ ] Cliq 4xx (non-429) → message terminated, metric `mio_gateway_outbound_terminated_total{channel_type="zoho_cliq", reason}` increments with bounded `reason`
- [ ] Cliq 5xx → message Nak'd, retried up to `MaxDeliver()` (Cliq default 5); final failure → terminate + alert metric
- [ ] No adapter is hardcoded in `dispatch.go`; registration lives in each channel package's `init()`
- [ ] `mio_gateway_ratelimit_buckets_active` is bounded after eviction loop runs (not monotonic) and `_evicted_total` is incrementing
- [ ] **Graceful shutdown**: SIGTERM drains in-flight within 30s; un-Acked messages redeliver on next start without double-send (idempotency via `outbound_state` + NATS dedup)
- [ ] Adapter interface stays minimal (Send, Edit, ChannelType, MaxDeliver, RateLimitKey only) — no Delete/React/Typing methods committed

## Risks

- **Cliq edit semantics for bot messages** — verify edits work with bot-sent messages (asymmetry between bot DMs and channel messages — see `playground/cliq/reports/researcher-260503-2013-cliq-bot-reaction-identity.md`). If a class of edits is impossible, fallback: send a new message with `attributes["supersedes"]=<send_id>` and let consumers render only the latest. Tied to P3 verification.
- **Adapter `init()` ordering** — Go runs all `init()` blocks before `main()`, but two adapters claiming the same `ChannelType()` slug would silently last-write-wins. Mitigation: `RegisterAdapter` panics on duplicate slug (fail-fast at startup). Secondary: dispatch builds its lookup map *after* all `init()` runs, so registration order is irrelevant.
- **429 cascade if `Retry-After` ignored** — re-deliveries land before the bucket refills, drain the bucket faster than recovery, sustained 429 storm. Mitigation: explicit step 6 in this plan, integration test on the path. Without this, the rate-limiter becomes the cause of its own failures.
- **Two-step UX state loss on restart** — `outbound_state` is in-memory; restart between "thinking…" send and final edit clears it. Three paths considered:
  - **(A)** Synchronous request/reply on send — gateway becomes the bottleneck; rejected.
  - **(B)** In-memory map keyed by `SendCommand.id`, AI passes correlator via `attributes["replaces_send_id"]` — **picked**. Failure mode: edit becomes a fresh message; metric `mio_gateway_outbound_edit_fallback_total` makes the failure visible. Tolerable for POC.
  - **(C)** Persist to Postgres `outbound_state` — deferred until (B)'s metric goes non-zero in production.
- **Bucket eviction goroutine dies silently** — buckets accumulate, memory grows, eventually OOM. Mitigation: `mio_gateway_ratelimit_buckets_evicted_total` counter (must increase on a non-empty system) and `mio_gateway_ratelimit_buckets_active > 10× active accounts` alarm. Recovery routine on goroutine panic via `defer recover()` + log + restart the loop.
- **`MaxDeliver=5` too aggressive** — some channels are permanently flaky on transient errors; tune per channel via `Adapter.MaxDeliver()` returning the override (interface method already in place — no refactor needed).
- **Idempotency on outbound** — NATS dedup window (60s) catches rapid re-delivery; `outbound_state` lookup catches within-session duplicates (idempotency address `out:<send_command.id>`). Outside both windows, accept rare double-send as a documented degradation; user can delete duplicate manually.
- **Metric label cardinality** — never include `account_id` / `tenant_id` / `conversation_id` as labels. `http_status` is bucketed (`2xx/4xx/429/5xx/network`), `reason` is bounded (`auth/not_found/bad_request/forbidden/other/state_missing`).

## Notes

- **DLQ design**: metrics-only for POC (`mio_gateway_outbound_terminated_total{channel_type, reason}`). Stream-based DLQ (`MESSAGES_DLQ`) vs Postgres `outbound_audit` table deferred until the first real terminated message shows up. The decision should follow real evidence, not premature design.
- **`outbound_state` size cap**: 10k entries default, LRU evict; metrics `mio_gateway_outbound_state_size` (gauge), `mio_gateway_outbound_state_evict_total` (counter).
- **Stream/consumer provisioning** is the gateway's startup responsibility (authoritative source). `MESSAGES_OUTBOUND` stream and `sender-pool` consumer are created idempotently — no separate bootstrap Job. Aligns with master plan + P7.
- **Metric label discipline**: `channel_type`, `direction`, `outcome` are the core labels. Phase-specific: `http_status` (bounded buckets), `reason` (bounded enum). Never `account_id` / `tenant_id` / `conversation_id` (cardinality bomb).
- **Idempotency address (outbound)**: `out:<send_command.id>` — used as the NATS publish dedup key by the AI publisher and the `outbound_state` lookup key by the gateway.
- **Cliq edit endpoint shape (PUT vs PATCH)**: P3 verification item. The adapter is written against PATCH per Cliq's documented pattern (`PATCH /api/v2/chats/{chatid}/messages/{msgid}`); if P3 reveals PUT is required, swap inside `sender_edit.go` (one-line change).
- **SDK Verify on publish**: AI is the publisher of outbound; gateway is the consumer. SDK enforces `schema_version` on publish; consumer-side Verify in step 8 is a defense-in-depth check.

## Research backing

[`plans/reports/research-260508-1056-p5-outbound-rate-limit-edit-ux.md`](../../reports/research-260508-1056-p5-outbound-rate-limit-edit-ux.md)

Validated deltas (1500+ lines, 13 questions):
- **In-process `golang.org/x/time/rate.Limiter`** per `account_id` confirmed for POC. Redis-backed limiter deferred until multi-replica fairness becomes empirically broken (single-replica is fine for POC).
- **Adapter self-registration via `init()` blocks** in each channel package — confirms P9 zero-edit litmus passes. `main.go` only needs `_ "mio/gateway/internal/channels/slack"` blank import.
- **429 handling is special**: respect `Retry-After` header before applying jitter, otherwise re-deliveries drain the bucket faster than refill.
- **Two-step UX state survives the trade-off**: in-memory map + caller correlator (`SendCommand.id`). Restart-during-edit produces a fresh message instead of an edit — tolerable for POC, document the failure mode.
- **DLQ as metrics-only for POC**: `mio_gateway_outbound_terminated_total{reason}` + manual GCS dump if anything terminates. Stream-based DLQ deferred.
- **Adapter interface stays minimal** — `Send`, `Edit`, `ChannelType`, `MaxDeliver()` only. Don't add `Delete`/`React`/`Typing` until two channels need them.
- **Fairness benchmark**: custom Go test with concurrent goroutines (vegeta/k6 are heavier than needed for an internal fairness assertion).

Pre-implementation open questions (ride on P3 verification): Cliq edit endpoint shape (PUT vs PATCH), bot-message edit asymmetry against POC carry-over evidence, idempotency-header support.
