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
  - `gateway/internal/sender/pool.go` — pull-fetch loop, worker pool sized via env
  - `gateway/internal/sender/dispatch.go` — registry-backed `channel_type` → adapter; lookup table built at startup from `gateway/internal/channels/*/sender.go` registrations
  - `gateway/internal/sender/adapter.go` — `Adapter` interface (Send, Edit) so each channel package implements it
  - `gateway/internal/channels/zohocliq/sender.go` — REST client implementing `sender.Adapter`
  - `gateway/internal/channels/zohocliq/sender_edit.go` — edit-in-place
  - `gateway/internal/store/outbound_state.go` — in-memory map keyed by `SendCommand.id` for two-step UX (returns platform `external_id` after first send)
  - `gateway/internal/ratelimit/account.go` — per-account token bucket, TTL-evicted
  - `gateway/internal/ratelimit/account_test.go`
  - `gateway/integration_test/cliq_outbound_test.go`
- **Modify:**
  - `gateway/cmd/gateway/main.go` — start sender pool alongside HTTP server; bind shutdown signal
  - `Makefile` — `gateway-bench-outbound` for the rate-limit fairness test

## Adapter interface

```go
// gateway/internal/sender/adapter.go
type Adapter interface {
    // Send a fresh outbound; returns the platform's external message id
    // so we can later edit it. Idempotency by SendCommand.id is the
    // pool's job, not the adapter's.
    Send(ctx context.Context, cmd *miov1.SendCommand) (externalID string, err error)

    // Edit an existing message; cmd.edit_of_external_id is the platform id.
    Edit(ctx context.Context, cmd *miov1.SendCommand) error

    // ChannelType returns the registry slug this adapter handles.
    ChannelType() string                    // e.g. "zoho_cliq"
}
```

Dispatch table:
```go
// gateway/internal/sender/dispatch.go
type Dispatcher struct {
    byType map[string]Adapter             // channel_type -> adapter
}
func (d *Dispatcher) ForCommand(cmd *miov1.SendCommand) (Adapter, error) {
    a, ok := d.byType[cmd.ChannelType]
    if !ok {
        return nil, fmt.Errorf("no adapter for channel_type=%q", cmd.ChannelType)
    }
    return a, nil
}
```

Adding a channel later (P9): one new `Register(zohocliq.New(...))`-style
call in `main.go`, no changes to `dispatch.go`.

## Per-account rate limit

```go
// gateway/internal/ratelimit/account.go
type Limiter struct {
    mu      sync.RWMutex
    buckets map[string]*rate.Limiter      // key: account_id (UUID)
    rate    rate.Limit                    // tokens/sec; default 5
    burst   int                           // burst size; default 10
    ttl     time.Duration                 // 10m idle eviction
    lastUse map[string]time.Time
}
```

Reasons account_id is the right key (not workspace, not channel_type):
1. `account_id` already encodes the (tenant, channel install) pair — the unit a platform's rate limit attaches to.
2. One tenant running two Slack workspaces gets two `account_id`s → two buckets, no cross-throttling. The earlier "workspace_id" model collapsed these.
3. Some platforms rate-limit per (workspace, channel) — that's a follow-up: the limiter accepts a composite key returned by the adapter (`Adapter.RateLimitKey(cmd) string`) but the default key is `account_id`.

Eviction goroutine: scans `lastUse` every 60s, drops entries idle > `ttl`. Metric: `mio_gateway_ratelimit_buckets_active` to alert if eviction stalls.

## Steps

1. `sender.pool` pulls in batches from the `sender-pool` durable consumer (`MaxAckPending=32`, `ack_wait=30s`, on `MESSAGES_OUTBOUND`); per-message worker dispatches via `Dispatcher.ForCommand`.
2. `ratelimit.account` — `golang.org/x/time/rate.Limiter` per `account_id`, `sync.Map` keyed; eviction goroutine purges idle (>10min) buckets.
3. Cliq sender: REST POST to send (carry over from `playground/cliq/test-zoho-cliq-send-message.sh`); PUT/PATCH to edit. Sets idempotency header where Cliq supports it; otherwise relies on adapter-side caller dedup via `outbound_state`.
4. On rate-limit deny → `Nak(WithDelay(jitter))` with backoff matching the bucket refill (don't drain bucket from re-deliveries).
5. On 5xx / network → `Nak`, rely on `max_deliver=5`. Capture last error in metric `mio_gateway_outbound_retry_total{channel_type, http_status}`.
6. On 4xx (permanent) → `Term` (move to dead-letter; design deferred — see Notes). Metric `mio_gateway_outbound_terminated_total{channel_type, reason}`.
7. **Two-step UX** (the "thinking…" pattern):
   - AI publishes initial `SendCommand` with `text="thinking..."`, `edit_of_message_id=""`, `edit_of_external_id=""`.
   - Gateway sends, receives Cliq's `external_id`, writes `(SendCommand.id → external_id)` into `outbound_state` (in-memory map, capped, TTL 10m).
   - AI publishes the final `SendCommand` with same `id`-prefix or correlator in `attributes["replaces_send_id"]`, fields `edit_of_message_id=<ai-side mio id of the thinking msg>`, `edit_of_external_id=""` (AI doesn't know it).
   - Gateway looks up `outbound_state[edit_of_message_id]` → fills in `edit_of_external_id` → adapter Edit.
8. Fairness benchmark: tag account A and B; burst 50 to A, single message to B; measure B's latency under A's burst. Pass when B p99 < 2s.

## Success Criteria

- [ ] Echo loop produces a visible Cliq reply in the same thread within 5s of a user's message
- [ ] `SendCommand{edit_of_message_id: ..., edit_of_external_id: ...}` results in a Cliq edit, not a new message
- [ ] Sending with only `edit_of_message_id` (no external) → gateway resolves via `outbound_state` and edits successfully
- [ ] Account fairness: account B's lone outbound completes in <2s p99 even while A is bursting 50/sec
- [ ] Cliq 4xx → message terminated, metric `mio_gateway_outbound_terminated_total{channel_type="zoho_cliq"}` increments
- [ ] Cliq 5xx → message Nak'd, retried up to `max_deliver=5`; final failure → terminate + alert metric
- [ ] No adapter is hardcoded in `dispatch.go`; registration lives in each channel package
- [ ] `mio_gateway_ratelimit_buckets_active` is bounded after eviction loop runs (not monotonic)

## Risks

- **Cliq edit semantics for bot messages** — verify edits work with bot-sent messages (there's a known asymmetry between bot DMs and channel messages — see `playground/cliq/reports/researcher-260503-2013-cliq-bot-reaction-identity.md`). If a class of edits is impossible, fallback: send a new message with `attributes["supersedes"]=<send_id>` and let consumers render only the latest.
- **Rate-limit bucket leak** — if eviction goroutine dies, buckets accumulate. Alert on `mio_gateway_ratelimit_buckets_active > N` (N tunable; alarm at 10x active accounts).
- **Two-step UX state** — `outbound_state` is in-memory; on gateway restart between "thinking…" send and final edit, the map is gone. Two paths considered:
  - **(A)** Synchronous request/reply on send — gateway becomes the bottleneck; rejected.
  - **(B)** In-memory map keyed by `SendCommand.id`, AI passes the same id correlator — picked. Failure mode: edit becomes a fresh message; tolerable for POC.
  - **(C)** Persist the mapping to Postgres `outbound_state` table — defer until restart-during-edit shows up in production.
- **`max_deliver=5` too aggressive** — some channels are permanently flaky on transient errors; tune per channel via `Adapter.MaxDeliver()` returning the override.
- **Idempotency on outbound** — NATS dedup window protects against re-delivery within 60s; downstream adapter must still tolerate duplicate `Send` for the same `SendCommand.id` (look it up in `outbound_state` first; if present, no-op + ack).

## Notes

- DLQ design: defer until first real terminated message in the wild — `MESSAGES_DLQ` stream vs a `terminated` flag on a Postgres `outbound_audit` table is a real call worth waiting on.
- `outbound_state` size cap: 10k entries default, LRU evict; metric `mio_gateway_outbound_state_size`.
- Metric label discipline: only `channel_type` and `outcome`. Never `account_id` or `tenant_id` as labels — cardinality bomb (master.md → Risks).
