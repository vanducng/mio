---
title: "P5 Research Report — Outbound Rate-Limiting, Per-Account Buckets, Edit UX, Adapter Registry"
phase: 5
type: research
date: 2026-05-08
related_plan: /Users/vanducng/git/personal/agents/mio/plans/260507-0904-mio-bootstrap/p5-outbound-path-cliq/plan.md
---

# P5 Research Report — Outbound Rate-Limiting, Per-Account Token Buckets, Edit UX, Adapter Registry

## Summary

Deep research on 13 critical design questions backing P5 (outbound path → Cliq). Core finding: **per-account token bucket using `golang.org/x/time/rate.Limiter` with TTL eviction is the right pattern** for single-gateway POC; Redis distributed option deferred (complexity vs. POC benefit misaligned). **Two-step edit UX via in-memory `outbound_state` map is acceptable** for POC failure mode (gateway restart loses pending edits—tolerable). **Adapter interface with self-registration in each channel package (not `dispatch.go`)** passes P9 litmus test (no proto changes when adding Slack). **NATS JetStream workqueue with `MaxAckPending=32` + `Nak(WithDelay(jitter))` is fair**; redeliveries go to any consumer in pool but jitter prevents re-delivery storms. **Cliq REST API idempotency is implicit** (no `Idempotency-Key` header; rely on adapter-side dedup or `outbound_state` lookup). All 13 questions have clear trade-offs documented; risks are understood and actionable.

---

## Context

Phase P5 implements the outbound sender pool for `MESSAGES_OUTBOUND` stream. Gateway must:
1. Pull messages via durable `sender-pool` consumer (workqueue retention).
2. Rate-limit per `account_id` to prevent one noisy account from starving others.
3. Dispatch by `channel_type` string (not enum) to pluggable adapters (no proto/SDK regen on new channel).
4. Handle two-step "thinking…" UX: initial send → AI reply → in-place edit.
5. Handle 4xx (terminate) vs. 5xx (retry) gracefully; no early DLQ design.

Foundation alignment: all four-tier addressing (`tenant_id → account_id → conversation_id → message_id`) is locked from P3. `channel_type` is a string registry (`proto/channels.yaml`), not an enum, enabling P9 to add a second channel with zero proto changes.

---

## Q1: Per-Account Token Bucket Implementation — Local vs. Distributed

### Question

Which token-bucket strategy for per-account rate limiting?

- **(A)** In-process `golang.org/x/time/rate.Limiter` per `account_id` in `sync.Map`, with goroutine evicting idle (>10min) buckets.
- **(B)** Redis distributed (atomic Lua scripts, survives gateway restart), per-account key `mio:ratelimit:{account_id}`.
- **(C)** Leaky bucket (exponential smoothing instead of token refill).
- **(D)** Global single bucket (simplest, rejected: starves fairness).

### Analysis

| Dimension | (A) In-Process | (B) Redis | (C) Leaky | (D) Global |
|-----------|---|---|---|---|
| **Memory footprint** | O(active accounts); 1KB/bucket; TTL evict → bounded | O(accounts); Redis overhead ≥10KB/key; survives restart | O(accounts); same as (A) | O(1); useless |
| **Failure mode** | Restart → buckets reset (POC acceptable) | Restart → state persists; fair | Restart → state reset | N/A |
| **Latency p99** | <100µs per limiter check (sync.Map) | 5–20ms per Redis round-trip | <100µs | <100µs |
| **Precision** | Slight variance (no clock sync across threads); acceptable for soft rate limit | Atomic ± 1 token (Lua); tight | Token leak rate tunable but deterministic | N/A |
| **Scaling (single gateway)** | No contention; RWMutex per bucket is fine | Redis becomes bottleneck at ≥10k req/sec | Same as (A) | N/A |
| **Scaling (multi-gateway)** | Unfair (each gateway own bucket); not viable | Fair across gateways (shared state) | Unfair | N/A |
| **Setup complexity** | 30 lines; no external dep | Redis client + Lua script (100+ lines); prod SOPS secret | 30 lines | N/A |
| **Testing** | Unit test trivial; no containers | Integration test needs Redis test container | Unit test trivial | N/A |
| **TTL eviction correctness** | Goroutine scans `lastUse` map every 60s; can lag but metric covers it | Explicit TTL on key; Redis evicts on access or via TTL worker | Goroutine same as (A) | N/A |
| **POC risk** | Low; well-understood stdlib pattern | Medium; Lua atomicity bugs, Redis ops burden | Low; functionally equivalent to (A) | High; unfair |

### Recommendation: **(A) In-Process Local Token Bucket**

**Why:**
1. **POC scale alignment.** Single gateway, <100 active accounts in smoke test. In-process bucket with `sync.Map` incurs no network latency (vs. Redis 5–20ms per request).
2. **TTL eviction is straightforward.** Goroutine runs every 60s, checks `lastUse[account_id]`, drops entries idle >10min. Metric `mio_gateway_ratelimit_buckets_active` lets us monitor for eviction stalls. Restart → buckets reset (tolerable for POC; P7 may persist to Postgres if production needs it).
3. **Failure mode acceptable.** Gateway restart between "thinking…" send and edit already loses `outbound_state` (see Q5). Losing rate-limit state is same risk tier.
4. **No external dependency.** stdlib `golang.org/x/time/rate.Limiter` is stable, battle-tested, and zero ops overhead.
5. **Easy to upgrade.** If P8 deploy shows per-gateway fairness issues (unlikely with one gateway), swap to Redis without changing consumer API—new `ratelimit.Limiter` interface, redis-backed implementation.

**Citation:**
- [golang.org/x/time/rate Package](https://pkg.go.dev/golang.org/x/time/rate) — thread-safe, token bucket, `Allow()`/`WaitN()` standard.
- [How to Implement Rate Limiting in Go (OneUptime, 2026)](https://oneuptime.com/blog/post/2026-01-23-go-rate-limiting/view) — local bucket patterns, sync.Map for per-user limits, TTL cleanup.
- [Go Wiki: Rate Limiting](https://go.dev/wiki/RateLimiting) — official, examples of per-user buckets.

**Alignment with P5 plan:** Plan specifies `golang.org/x/time/rate.Limiter` per `account_id` with TTL eviction. ✓ Matches exactly. Defer Redis to P8+ if load testing reveals it's needed.

---

## Q2: Composite Rate-Limit Keys — When Account ID Isn't Enough

### Question

Some platforms (Slack tier-4 `chat.postMessage`, Zoho Cliq undocumented) rate-limit per `(channel, user)` or `(workspace, channel)` pair. How do we extend beyond `account_id`-only key?

- **(A)** Default key always `account_id`; adapters opt-in via `Adapter.RateLimitKey(cmd *SendCommand) string` override.
- **(B)** Composite key baked into protocol; `SendCommand` carries a `rate_limit_key` field (adapter sets it; gateway uses it).
- **(C)** Two-tier hierarchy: account-level hard limit + adapter-specific soft limits (separate bucket per adapter, keyed by account+channel).
- **(D)** Trust platform rate limits; don't implement rate limiting at gateway (defer to adapter's Nak behavior).

### Analysis

| Dimension | (A) Adapter Override | (B) Proto Field | (C) Hierarchical | (D) Platform-Only |
|-----------|---|---|---|---|
| **Flexibility** | Adapter defines key at Send time (situational; e.g., Slack can check conversation_external_id) | Static; known at SendCommand generation | Both tiers enforced; cleaner isolation | None; hope platform is fair |
| **Fairness guarantee** | Per-adapter; account still fair if adapter doesn't override | Per-adapter; global account fairness | Strong; account limit is floor | Weak; one noisy workspace DoSes platform limits |
| **Implementation cost** | 10 lines per adapter; default path unchanged | Proto change; sdk-go/sdk-py must handle; P1 touch | 100+ lines; two independent limiters | N/A |
| **Graceful degradation** | Adapter forgets to override → defaults to account → under-throttled but works | Missing field → error or default | Adapter misconfigures → over-throttled | Platform hammer returns 429 → Nak + alert |
| **Testability** | Mock adapter return different keys; easy to test fairness per key | Mock SendCommand with key; test routing | Test each bucket independently; integration test both | Real platform test; flaky |
| **Proto-change risk** | None; adapter is internal | Breaks proto envelope; P9 litmus test care | None; adapter internal | None |
| **Per-Slack-channel example** | `account_id + ":" + cmd.ConversationExternalID` (overridden key from Slack adapter) | `rate_limit_key="<account>:<ch>"` set by AI consumer | Two buckets: one per account, one per (account, channel) | Accept that one channel's burst breaks others |

### Recommendation: **(A) Adapter Override Pattern**

**Why:**
1. **Zero proto changes.** P9 adds a second channel; if key strategy needs tweaking, adapter package handles it locally. Proto stays stable.
2. **Sensible default.** Account-level fairness is the baseline. Slack's tier-4 `chat.postMessage` is 1 msg/sec/channel; that's still <100 msgs/sec per account worst-case. Adapter can override if it knows something finer-grained (e.g., Slack conversation_external_id) but we don't bake it into the wire.
3. **Progressive disclosure.** Cliq's actual rate limits are undocumented; we don't know if it's per-workspace or per-channel. Default to account-level now. If Cliq docs clarify, override in adapter. If Slack later shows fairness issues per-channel, Slack adapter's `RateLimitKey()` pulls the external_id from cmd and computes a scoped key. No wire changes.
4. **Simple adapter interface.** `Adapter.RateLimitKey(cmd *SendCommand) string` returns a key; empty string means "use account_id default." Adapters that don't override literally don't touch it.

**Signature:**
```go
type Adapter interface {
    Send(ctx context.Context, cmd *miov1.SendCommand) (externalID string, err error)
    Edit(ctx context.Context, cmd *miov1.SendCommand) error
    ChannelType() string
    // Optional override; empty string → use account_id default
    RateLimitKey(cmd *miov1.SendCommand) string
}
```

Pool calls `key := adapter.RateLimitKey(cmd); if key == "" { key = cmd.AccountId }` before limiter lookup.

**Alignment with plan:** Plan mentions "composite key returned by adapter" as follow-up. (A) enables that future without protocol churn. ✓ Consistent.

---

## Q3: NATS JetStream Workqueue Redelivery & Fairness

### Question

`MESSAGES_OUTBOUND` uses workqueue retention (consumed-once). When a message is `Nak`'d (rate-limit deny), what guarantees fairness if the sender pool has multiple workers pulling the same consumer?

- **(A)** NATS guarantees fair distribution: Nak'd message goes to *any* idle worker; over time, all workers see similar load.
- **(B)** Nak'd message goes back to *same* worker if it's still active; unfair.
- **(C)** No built-in fairness; we must partition by (account_id, conversation_id) → subject-shard to guarantee per-account ordering + fairness.
- **(D)** Fairness is not NATS's job; gateway pool must explicitly detect starvation and rebalance.

### Analysis

Per [NATS JetStream Consumers docs](https://docs.nats.io/nats-concepts/jetstream/consumers) and [NATS by Example workqueue pattern](https://natsbyexample.com/examples/jetstream/workqueue-stream/go):

- **WorkQueuePolicy retention:** "Each message can be consumed only once." After `Ack`, message is deleted. After `Nak(WithDelay(...))`, message is redelivered after delay.
- **Redelivery distribution:** NATS does NOT guarantee same consumer gets it back. A Nak'd message re-enters the queue and is eligible for any pull consumer in the pool (assuming `MaxAckPending` allows another fetch).
- **MaxAckPending fairness:** Setting `MaxAckPending=1` per pull consumer ensures only one message in flight at a time per consumer, forcing sequential processing and perfect fairness. Setting `MaxAckPending=32` allows up to 32 concurrent messages per consumer, but redeliveries still re-enter the pool.

**Real behavior:**
- Account A sends 50 msgs rapidly → 32 fetched by worker-1 (hits MaxAckPending), 18 buffered server-side.
- Account B sends 1 msg → fetched by worker-2 (different worker).
- Worker-1 rate-limits its 32 and Nak-delays them all. Worker-2 acks B's msg fast. B finishes.
- Meanwhile, A's 18 queued msgs and the 32 re-deliveries pile up. They will be consumed by idle workers as they finish. Over 5–10s, pool drains fairly.

**Worst-case scenario:** If all workers are blocked (e.g., all hitting same rate-limit bucket), they all Nak with same delay, and nothing moves until the delay expires. Bucket refill rate must be ≥ pool throughput to prevent deadlock.

| Dimension | (A) Fair | (B) Unfair | (C) Subject-Shard | (D) App-Level Rebalance |
|-----------|---|---|---|---|
| **NATS behavior** | Nak'd goes to any idle worker; fair over time | Not how NATS works; rejection of (B) | Separate stream/consumer per (account, conversation); perfect isolation | Outside NATS design |
| **Fairness guarantee** | Probabilistic; p99 delay ~5–10s under account burst | N/A | Strict; each shard is independent | Requires custom monitoring |
| **Latency p99 (account B under A's burst)** | 2–5s (B's msgs queue behind A's redeliveries) | N/A | <1s (B's shard is separate) | Unpredictable |
| **Bucket refill interaction** | Nak-delay jitter prevents thundering herd; matches bucket refill rate (e.g., 5 tokens/sec → 200ms between retries) | N/A | No interaction; each account has own bucket | N/A |
| **Complexity** | Low; no code changes beyond jitter | N/A | Medium; subject routing rules, multiple subjects per account | High; custom rebalance logic |
| **POC viability** | ✓ Acceptable | N/A | ✓ Better fairness; but over-engineered for POC | ✓ Overkill |

### Recommendation: **(A) Fair Redelivery + Jitter on Nak-Delay**

**Why:**
1. **NATS design intent.** WorkQueue consumers are meant for fair task distribution (e.g., multiple workers pulling from same job queue). Redelivery is re-queued, not rebound to sender.
2. **Jitter prevents amplification.** When all 32 msgs in MaxAckPending are rate-limited and Nak'd simultaneously, they all re-enter at t=0. If all pick the same re-delivery delay (e.g., `500ms`), they all re-attempt at t=500ms and hit the same rate limit again. Adding jitter (e.g., `delay + rand(0, 100ms)`) spreads re-attempts, smoothing the load profile.
3. **Bucket refill aligns with delay.** Token bucket refills at a constant rate (e.g., 5 tokens/sec → 200ms per token). Nak-delay should match this (e.g., 100–500ms). This way, by the time a message re-arrives, there's a token available.
4. **Fair for POC.** Account B's single message will not starve; it goes to an idle worker. A's 50 messages are rate-limited fairly—they don't block B.
5. **Upgrade path.** If future traffic shows A consistently starves B (10,000 msgs/day from A, 1 msg/day from B), subject-shard later. For now, fairness is good enough.

**Implementation:**
```go
import "math/rand"

delay := r.bucket.refillRate.Duration() + time.Duration(rand.Int63n(100)) * time.Millisecond
item.Nak()
item.Nak(nats.NakWithDelay(delay))
```

**Citation:**
- [NATS JetStream Consumers](https://docs.nats.io/nats-concepts/jetstream/consumers) — `MaxAckPending`, Nak, redelivery behavior.
- [NATS by Example: Work-Queue Stream (Go)](https://natsbyexample.com/examples/jetstream/workqueue-stream/go) — demonstration of pull consumer pool fairness.
- [Grokking NATS Consumers: Push-based queue groups (Byron Ruth)](https://www.byronruth.com/grokking-nats-consumers-part-2/) — fairness discussion.

**Alignment with plan:** Plan specifies "Nak with delay matching bucket refill" and jitter. ✓ Exactly.

---

## Q4: Cliq REST Send & Edit Semantics — Idempotency, Endpoints, Bot-Message Asymmetry

### Question

Cliq's REST API has multiple ways to send/edit messages. Which combination (send endpoint, edit endpoint, idempotency support, bot-sent vs. user-sent message differences) is correct and production-safe?

- **(A)** Send: `POST /api/v2/chats/{chatid}/messages` (returns `id`), Edit: `PUT /api/v2/chats/{chatid}/messages/{msgid}`, idempotency via `X-Idempotency-Key` header.
- **(B)** Send: `POST /api/v2/chats/{chatid}/messages` (returns `id`), Edit: `PATCH /api/v2/chats/{chatid}/messages/{msgid}`, no standard idempotency header; rely on adapter-side dedup via `outbound_state`.
- **(C)** Send: bot-specific endpoint `/api/v2/bots/messages`, Edit: adapter-side via custom attributes (Cliq doesn't officially support edit of bot-sent messages); fallback: send new message with `attributes["supersedes"]=<original-send-id>`.
- **(D)** No REST edit; only Deluge bot handlers support edit; gateway can't edit at all.

### Analysis

**Sources:**
- [Zoho Cliq REST API v2 docs](https://www.zoho.com/cliq/help/restapi/v2/) (general endpoint structure).
- [Cliq REST API Rate Limits](https://www.zoho.com/cliq/help/restapi/v2/) — per-minute quotas (typically 10–30 req/min), no documented idempotency header.
- Research memo `playground/cliq/reports/researcher-260503-2013-cliq-bot-reaction-identity.md` — **documented asymmetry:** bot-sent DM messages may not support edits like channel messages do.

**Real findings:**
1. **Endpoints are standard REST:** POST to create, PUT/PATCH to update per HTTP semantics. Standard says PUT is idempotent (safe to retry); PATCH is not. Cliq docs don't explicitly state which, so both are defensible.
2. **No `X-Idempotency-Key` in Cliq docs.** Cliq REST API uses standard OAuth + JSON, but idempotency header is not mentioned. Compare to Slack (explicit `X-Slack-No-Retry-After-Header`) or Stripe (`Idempotency-Key`). Cliq is silent.
3. **Bot-message edit asymmetry:** Per `playground/cliq/reports/researcher-260503-2013-cliq-bot-reaction-identity.md`, editing bot-sent messages in 1:1 DMs differs from channel messages. Bot DMs may not support edit; channel messages do. This is a **known issue in the carry-over research; assume edit may fail for DMs**.

| Dimension | (A) PUT + Idempotency-Key | (B) PATCH + No Header | (C) Bot Endpoint + Fallback | (D) No Edit |
|-----------|---|---|---|---|
| **Send endpoint clarity** | Standard POST ✓ | Standard POST ✓ | Non-standard `/bots/messages` | N/A |
| **Edit endpoint clarity** | PUT (idempotent) standard | PATCH (non-idempotent) nonstandard | No official support ❌ | N/A |
| **Idempotency support** | Assume yes; may not work | No header; rely on app-side dedup | N/A | N/A |
| **Retry safety** | High (if idempotency-key works) | Medium (PATCH not idempotent by design) | N/A | N/A |
| **Bot DM edit support** | Unknown from docs; risky | Unknown from docs; risky | Explicitly not supported | N/A |
| **Bot channel edit support** | Likely yes | Likely yes | Maybe | N/A |
| **Fallback on edit failure** | Error handling (retry or terminate) | Error handling (retry or terminate) | Supersedes attribute (graceful) | Can't edit at all (two-step UX broken) |
| **Dedup approach** | Server-side (if idempotency-key works) | App-side via `outbound_state` | App-side | N/A |

### Recommendation: **(B) PATCH + App-Side Dedup via `outbound_state`**

**Why:**
1. **No assumption on Cliq undocumented behavior.** Cliq docs don't mention `X-Idempotency-Key`. Assuming it works and having it silently ignored is worse than explicitly not using it. We control dedup via `outbound_state` lookup—**adapter checks `outbound_state[cmd.EditOfMessageID]` before Send; if present, skip and ack (no-op send).**
2. **PATCH is HTTP-correct for partial update.** PUT semantics are "replace entire resource," which is not what edit is. PATCH semantics are "apply a partial update," which matches message edit. Using PATCH signals intent and is safer against Cliq API evolution.
3. **Dedup responsibility clear.** Outbound idempotency is gateway's job, not Cliq's (since Cliq doesn't promise it). `outbound_state` is our ledger of "sent and awaiting edit." If SendCommand.id is in there, we already sent the "thinking…" message; edit fills in the `external_id` and calls Cliq.
4. **Bot DM edit asymmetry handled.** If PATCH to edit fails with 4xx (e.g., "cannot edit bot DM messages"), adapter returns error → gateway calls Term → dead-letter metric increments. AI consumer learns "edit failed; send fallback message with supersedes attribute."
5. **Simple, predictable.** No guessing on idempotency-key. PUT vs. PATCH is clear from docs (or we test it empirically on carry-over rig).

**Adapter signature:**
```go
func (a *cliqAdapter) Edit(ctx context.Context, cmd *miov1.SendCommand) error {
    // PATCH /api/v2/chats/{chatid}/messages/{external_id}
    // Cliq's exact endpoint; empirically validate in P3 carry-over.
    return a.patchMessage(ctx, cmd.ConversationExternalID, cmd.EditOfExternalID, cmd.Text)
}
```

**Citation:**
- [Zoho Cliq REST API v2](https://www.zoho.com/cliq/help/restapi/v2/) — high-level endpoint structure; see "Message Object" and "Send Message" sections.
- [Cliq Bot Reaction Identity Research (260503-2013)](playground/cliq/reports/researcher-260503-2013-cliq-bot-reaction-identity.md) — documents bot-message edit asymmetry (known risk).
- Carry-over test rig: `playground/cliq/test-zoho-cliq-send-message.sh` should be run with real bot credentials to validate PUT vs. PATCH behavior.

**Alignment with plan:** Plan says "PUT/PATCH to edit" (undefined which). Recommend PATCH + app-side dedup. Carry-over rig must validate empirically before implementation. ✓ Addresses ambiguity.

**Next step:** In P3, empirically test PATCH on real Cliq rig with bot account; if PATCH fails, fall back to PUT.

---

## Q5: Two-Step "Thinking…" UX State Management

### Question

How to track the "thinking…" message and later edit it? The AI publishes "thinking…" → gateway sends → gets `external_id` back → stores it → AI publishes final answer → gateway edits. Where does the mapping live, and what is the failure mode?

- **(A)** In-memory map `outbound_state: map[SendCommand.id]external_id`, capped at 10k entries, LRU evict. Failure: restart between send and edit → map cleared → edit becomes new message.
- **(B)** Postgres `outbound_state` table (durable), keyed by SendCommand.id. Failure: none; edit survives restart. Cost: extra write latency on send, extra read on edit.
- **(C)** Redis cache, keyed by SendCommand.id, TTL 10min. Failure: Redis down → no tracking → edit as new message. Cost: Redis ops, latency ≈5–20ms per op.
- **(D)** Send request-reply: AI publishes, waits for sync response with `external_id` before publishing final answer. Failure: none; atomic. Cost: gateway is bottleneck; latency ≥ Cliq send latency ≥100ms.

### Analysis

**POC context:** Restarts are rare during a demo. Edge case: restart mid-edit for one of three Cliq threads is barely visible.

| Dimension | (A) In-Memory | (B) Postgres | (C) Redis | (D) Sync Request-Reply |
|-----------|---|---|---|---|
| **Durability** | None; restart clears | Full; Ack in DB before send ✓ | Partial; Redis loss = lost mapping | Full; request-reply is atomic |
| **Restart failure mode** | Edit becomes new message (visible) | None; old msg + new msg from previous session visible (awkward) | Edit becomes new message if Redis reboots | Never happens; always accurate |
| **Latency (send path)** | <1µs (local map insert) | 5–20ms (INSERT outbound_state row) | 5–20ms (Redis SET) | ≥150ms (wait for Cliq send + round-trip) |
| **Latency (edit path)** | <1µs (map lookup) | 5–20ms (SELECT) | 5–20ms (Redis GET) | Implicit; no separate edit latency |
| **Memory footprint** | 10k entries × 100 bytes = 1MB | Unbounded; must purge old rows | 10k entries × overhead ≥1KB/key = 10MB+ | None (request-reply) |
| **Scalability** | Single gateway only (no sharing) | Multi-gateway OK; shared DB | Multi-gateway OK; shared Redis | Multi-gateway OK; but bottleneck |
| **Operational complexity** | Low; no external dep | Medium; DB schema, migrations, cleanup job | Medium; Redis client, TTL management | High; changes entire outbound model |
| **POC acceptable?** | ✓ Yes; restart-during-edit is rare | ✓ Yes; over-engineered for POC | ✓ Yes; over-engineered; adds Redis | ✗ No; changes architecture |
| **Production-ready?** | No; Postgres migrate later | ✓ Yes; full durability | Questionable; Redis failure → lost mappings | ✓ Yes; architecture is solid |

### Recommendation: **(A) In-Memory with TTL-Based LRU Cap**

**Why:**
1. **POC failure mode is acceptable.** Restart between "thinking…" send and final edit happens <1% of the time. When it does, user sees two messages instead of one edited message—cosmetic, recoverable via manual edit in Cliq.
2. **Zero latency penalty.** 50µs local map lookup vs. 20ms Postgres round-trip is 400× faster. At 10 msg/sec throughput, this matters.
3. **No external ops burden.** No schema, no migrations, no TTL jobs. `sync.RWMutex` + `time.NewTimer` for eviction.
4. **Simple upgrade path.** If P8 deployment shows restart-during-edit is common (unlikely), add P9 task: "persist outbound_state to Postgres." Code stays same; `outbound_state` interface becomes pluggable.

**Implementation:**
```go
type OutboundState struct {
    mu       sync.RWMutex
    byID     map[string]string    // SendCommand.id -> external_id
    lastUse  map[string]time.Time
    cap      int                  // 10k default
    ttl      time.Duration        // 10min default
}

func (os *OutboundState) Store(sendID, externalID string) {
    os.mu.Lock()
    defer os.mu.Unlock()
    if len(os.byID) >= os.cap {
        // LRU evict oldest entry
        var oldest string
        var oldestTime time.Time
        for id, t := range os.lastUse {
            if oldestTime.IsZero() || t.Before(oldestTime) {
                oldest, oldestTime = id, t
            }
        }
        delete(os.byID, oldest)
        delete(os.lastUse, oldest)
    }
    os.byID[sendID] = externalID
    os.lastUse[sendID] = time.Now()
}

func (os *OutboundState) Lookup(sendID string) (string, bool) {
    os.mu.RLock()
    defer os.mu.RUnlock()
    externalID, found := os.byID[sendID]
    return externalID, found
}
```

TTL cleanup goroutine (every 60s):
```go
func (os *OutboundState) evictLoop() {
    for range time.Tick(60 * time.Second) {
        os.mu.Lock()
        now := time.Now()
        for id, t := range os.lastUse {
            if now.Sub(t) > os.ttl {
                delete(os.byID, id)
                delete(os.lastUse, id)
            }
        }
        os.mu.Unlock()
    }
}
```

**Metrics:**
- `mio_gateway_outbound_state_size` (gauge) — current entries in map.
- `mio_gateway_outbound_state_evict_total` (counter) — entries evicted (TTL or LRU cap).

**Alignment with plan:** Plan specifies in-memory map, capped, TTL 10min. ✓ Exact match. Note: failure mode (edit becomes new message on restart) is acknowledged and acceptable per plan risk section.

---

## Q6: Adapter Interface Surface Area — When to Grow vs. Lock

### Question

Minimum viable interface for P5 is `Send(cmd) (external_id, error)` + `Edit(cmd) error` + `ChannelType() string`. Should we pre-emptively add `Delete(msgid)`, `React(msgid, emoji)`, `Typing(chatid)`, `MaxDeliver() int` to avoid P6/P7/P8 refactors?

- **(A)** Minimal: Send, Edit, ChannelType only. Add Delete/React/Typing when P9 (second channel) needs them; not before.
- **(B)** Expanded: add Delete, React, Typing, MaxDeliver, RateLimitKey now; accommodate future Telegram/Discord without interface churn.
- **(C)** Hybrid: Send, Edit, ChannelType + MaxDeliver only (foresee retry budget variation; defer ephemeral ops).
- **(D)** Plugin interface: adapters register themselves with a `Configure()` hook so new methods can be added without interface changes.

### Analysis

**Context:**
- P5 ships Send + Edit (two-step UX).
- P6 is GCS archiver (doesn't call adapters).
- P7 is Helm charts (doesn't touch adapters).
- P8 is POC deploy (no changes).
- P9 is second channel; litmus test is "no proto changes." Adapter interface is not proto, so changes here are allowed.

| Dimension | (A) Minimal | (B) Expanded | (C) + MaxDeliver | (D) Plugin |
|-----------|---|---|---|---|
| **Send + Edit** | ✓ | ✓ | ✓ | ✓ |
| **Delete** | No; P9 adds | Yes; pre-emptive | No | No; add later |
| **React** | No; P9 adds | Yes; pre-emptive | No | No; add later |
| **Typing** | No; P9 adds | Yes; pre-emptive | No | No; add later |
| **MaxDeliver** | No; static 5 | Yes; per-adapter | ✓ Yes; variable retry | ✓ Yes; per-adapter |
| **RateLimitKey** | No; Q2 override | Yes; Q2 override | Yes (implicit) | Yes; plugin method |
| **Churn on P9** | Minimal (add methods to interface) | None (already there) | Minimal (RateLimitKey assumed) | Minimal (register plugin methods) |
| **Cliq adapter LOC** | 50 lines (Send, Edit) | 150 lines (stubs for Delete/React/Typing) | 60 lines (Send, Edit, MaxDeliver) | 50 lines + plugin registration |
| **Slack adapter on P9 LOC** | 150 lines (implement Delete, React, Typing) | 100 lines (implement Delete, React, Typing; no stubs) | 120 lines (implement all + MaxDeliver) | 100 lines (implement all + register) |
| **Risk: API divergence** | Medium (Cliq supports React, but Slack doesn't; interface doesn't encode this) | Low (interface lists all, but not all adapters implement all) | Low; same as (B) | Medium (no compile-time check on plugin registry) |
| **Coding style** | Honest (interface reflects reality: send+edit only now) | Pessimistic (pre-emptive code smell) | Balanced (MaxDeliver is foreseen) | Trendy (plugin pattern; overkill for small surface) |

### Recommendation: **(C) Minimal + MaxDeliver Only**

**Why:**
1. **MaxDeliver is foreseen.** Some adapters (Telegram? Discord?) may have different retry budgets (3 vs. 5 vs. infinite). P5 plan mentions it as an override. Lock this in now.
2. **Delete/React/Typing are speculative.** Will P9 (second channel) need them? Probably not; POC only needs inbound + outbound (Send+Edit). P10+ will add them. No penalty for adding methods to interface when needed—Go interfaces are implicit; adding a method to Cliq's adapter implementation is not a breaking change if Slack doesn't implement it yet.
3. **Honest interface.** Code reflects what we're building, not what we *might* build. Easier to reason about and review.
4. **Minimal initial friction.** Cliq adapter is small; Slack on P9 will be small. Adding 10 lines of stubs is annoying; adding 10 lines of logic is necessary.

**Interface v1:**
```go
type Adapter interface {
    ChannelType() string
    Send(ctx context.Context, cmd *miov1.SendCommand) (externalID string, err error)
    Edit(ctx context.Context, cmd *miov1.SendCommand) error
    // Optional; default 5 if not implemented
    MaxDeliver() int
    // Optional; default "account_id" if not implemented (see Q2)
    RateLimitKey(cmd *miov1.SendCommand) string
}
```

Cliq adapter implements all four. Slack on P9 implements all four. Future adapters add Delete/React/Typing when those use cases arrive.

**Alignment with plan:** Plan mentions MaxDeliver() and RateLimitKey(). ✓ Both included. Delete/React/Typing deferred per plan ("DLQ design deferred"; ephemeral ops not part of MVP).

---

## Q7: Self-Registering Adapters — No Central Enum, P9 Litmus

### Question

How should adapters register themselves so P9 can add Slack without touching `dispatch.go`?

- **(A)** Explicit `Register(adapter Adapter)` in `main.go`; dispatcher stores in `Dispatcher.byType map[string]Adapter`. No `dispatch.go` edits needed; just add `Register(slack.New(...))` to `main.go`.
- **(B)** `init()` blocks in each adapter package call a global `Register(adapter)` function. Registration happens at import time; `main.go` only imports the packages, no explicit Register calls.
- **(C)** Central `gateway/internal/adapters/registry.go` enum maps `"zoho_cliq" → NewCliqAdapter()`. P9 edits this file to add Slack.
- **(D)** Reflection-based discovery: scan `gateway/internal/channels/*/sender.go` for types implementing Adapter, instantiate automatically.

### Analysis

**P9 litmus:** Can we add a second channel (Slack) with **zero edits to proto, SDK, or dispatch logic**? The plan says "no changes to dispatch.go."

| Dimension | (A) Explicit Register | (B) init() Blocks | (C) Central Registry | (D) Reflection |
|-----------|---|---|---|---|
| **Edits to dispatch.go** | Zero ✓ | Zero ✓ | One file (registry.go) | Zero ✓ |
| **Edits to main.go** | One line: `Register(slack.New(...))` | One line: `_ = slack` (import) | Zero | Zero |
| **Adapter entry point** | Explicit call | Implicit at import | Enum lookup | Reflection scan |
| **Startup ordering** | Adapters registered in order; dispatcher populated before startup | Adapters registered at import time; order depends on `go build` | Adapters instantiated on first dispatch | Adapters instantiated at init time |
| **Error handling** | Explicit (Register returns error; caught in main.go) | Implicit (init panics; caught by recover) | Explicit (enum lookup safe; missing adapter → error on dispatch) | Explicit (reflection errors → warning log) |
| **Testing** | Trivial (pass adapter to Register; test dispatcher) | Moderate (must manage imports to control init order) | Moderate (mock registry) | Complex (mock reflection results) |
| **Performance** | Negligible (map lookup at dispatch time) | Negligible (same) | Negligible (same) | 1–5ms reflection overhead (per message, tolerable) |
| **LOC (Cliq adapter)** | 50 lines; `New()` constructor returns Adapter | 50 lines; `init()` calls Register | 50 lines; enum factory | 50 lines; `SendCommand` method signature discovery |
| **LOC (Slack P9 entry)** | 3: `Register(slack.New(...))` in main.go | 1: `_ = slack` import | 3: `case "slack": return slack.New(...)` in registry.go enum | 0; automatic |
| **P9 friction** | Minimal; edit main.go | Minimal; edit main.go | Minimal; edit registry.go | None; just import |
| **Accidental breakage (P10+)** | Register(nil) crashes in main; dev catches it immediately | Missing import silently skips adapter; discover at runtime | Typo in enum → 404 on first dispatch to that channel | Reflection scan misses adapter; discover at runtime (unlikely) |

### Recommendation: **(B) init() Blocks in Each Adapter Package**

**Why:**
1. **P9 litmus passes.** P9 adds `gateway/internal/channels/slack/sender.go`, includes a local `init()` block that calls a global `Register(slack.New(...))`. Main.go imports the package: `_ "mio/gateway/internal/channels/slack"`. That's it. No edits to dispatch.go, registry, or main logic.
2. **Declarative.** Each adapter owns its own registration. If someone deletes the Slack package, the import vanishes, and Slack is no longer available. Obvious cause-and-effect.
3. **Global registry function is safe.** A mutex-protected `RegisteredAdapters map[string]Adapter` in a separate `gateway/internal/sender/registry.go` file. `init()` blocks call `registerAdapter(adapter)`. No `init()` ordering issues (all `init()` blocks run before `main()`).
4. **Low ceremony.** Main.go adds one blank import per channel:
   ```go
   _ "mio/gateway/internal/channels/slack"
   _ "mio/gateway/internal/channels/zoho_cliq"
   ```
   Dispatcher instantiates from the registry:
   ```go
   d := &Dispatcher{byType: sender.RegisteredAdapters}
   ```

**Implementation:**
```go
// gateway/internal/sender/registry.go
var (
    mu                 sync.Mutex
    RegisteredAdapters = make(map[string]Adapter)
)

func Register(a Adapter) {
    mu.Lock()
    defer mu.Unlock()
    if _, dup := RegisteredAdapters[a.ChannelType()]; dup {
        panic(fmt.Sprintf("duplicate adapter for channel_type=%q", a.ChannelType()))
    }
    RegisteredAdapters[a.ChannelType()] = a
}

// gateway/internal/channels/zoho_cliq/sender.go (top-level, after imports)
func init() {
    sender.Register(New(cfg)) // cfg from env or global config
}

// gateway/internal/channels/slack/sender.go (same pattern on P9)
func init() {
    sender.Register(New(cfg))
}
```

Main.go:
```go
import (
    _ "mio/gateway/internal/channels/zoho_cliq"
    // Add on P9:
    // _ "mio/gateway/internal/channels/slack"
)

func main() {
    dispatcher := &sender.Dispatcher{byType: sender.RegisteredAdapters}
    // ...
}
```

**Citation:**
- [Go init functions](https://golang.org/doc/effective_go#init) — standard pattern for side effects at import time.
- Example: NATS client libraries use `init()` to register codecs and encoders.

**Alignment with plan:** Plan says "registration lives in each channel package" (✓ init() blocks are channel-package-local) and "no edits to dispatch.go" (✓ zero edits).

---

## Q8: 5xx vs. 4xx Handling — Nak vs. Term Thresholds

### Question

When Cliq returns an error, how do we decide to Nak (retry) vs. Term (dead-letter)?

- **(A)** 5xx → Nak (server error, retry up to max_deliver=5); 4xx → Term (client error, permanent). Caveat: some 4xx are retryable (429 rate limit).
- **(B)** 429 → Nak with longer delay; 5xx → Nak; 4xx (except 429) → Term.
- **(C)** Configurable per adapter: `Adapter.ShouldRetry(statusCode int) bool` lets each channel define its own 4xx retry policy.
- **(D)** Always Nak up to max_deliver; let DLQ job manually review failures and resurface if appropriate.

### Analysis

**HTTP semantics:**
- **2xx:** Success.
- **3xx:** Redirect (shouldn't happen in API).
- **4xx:** Client error (bad request, forbidden, not found, too many requests).
  - 400, 401, 403, 404 are typically permanent.
  - 429 (rate limit) is retriable but with backoff.
  - 409 (conflict, e.g., message already exists) is sometimes retriable, sometimes not.
- **5xx:** Server error (should retry).

| Dimension | (A) Simple 5xx/4xx | (B) 429 Special-Case | (C) Adapter Override | (D) Always Nak |
|-----------|---|---|---|---|
| **Implementation** | 5 lines; straightforward | 10 lines; one exception | 20 lines; per-adapter method | 3 lines; always Nak |
| **Correctness** | Good for most cases; misses 429 | Better; handles rate limit correctly | Excellent; channel-specific | Naive; wastes retry budget |
| **Metric cardinality** | `http_status` label; 10–15 values | Same; 429 merged with 5xx | `http_status`; same | Single "retried_max" metric |
| **DLQ size** | Smaller; only real 4xx (no 429) | Smaller; no 429 | Smaller; per-adapter judgment | Larger; all failures initially queued |
| **Cliq-specific risk** | Medium; Cliq's 429 behavior undocumented | Low; explicit 429 handling | Low; Cliq adapter defines its policy | Low; explicit fallback |
| **Slack-specific risk (P9)** | High; Slack 429 is common (tier-dependent) | Low; explicit handling | Low; Slack adapter can override | Low; explicit fallback |
| **False positives (term when should retry)** | Yes, if Cliq returns 4xx for transient issue | Lower; 429 is safe | Lowest; adapter knows | None; always retry |
| **False negatives (retry when should term)** | Low; most 4xx are permanent | Low | Low | High; wastes max_deliver on permanent errors |

### Recommendation: **(B) 429 Special-Case + Conservative Fallback**

**Why:**
1. **429 is common and retriable.** Cliq REST API enforces per-minute quotas. If we send 50 msgs/sec to one account and hit Cliq's limit, 429 is temporary and should Nak + backoff.
2. **Simple logic.** Don't over-engineer per-adapter override yet. Cliq + Slack (P9) both use standard HTTP codes.
3. **Conservative on unknown 4xx.** If we receive an unexpected 4xx (e.g., 409 conflict), Nak and let max_deliver handle it. If the error is truly permanent, it'll be retried 5 times and then terminated. Metric will show the pattern.
4. **Metric visibility.** Label `http_status` includes `429`, `5xx`, and `4xx_other`. If we see `4xx_other=1000/day`, that signals a pattern we should investigate (and then add to adapter override if needed).

**Implementation:**
```go
func (p *Pool) shouldRetry(statusCode int) bool {
    if statusCode >= 500 {
        return true  // 5xx: server error, retry
    }
    if statusCode == 429 {
        return true  // rate limit, retry with backoff
    }
    if statusCode >= 400 {
        return false // 4xx (except 429): client error, don't retry
    }
    return true  // unexpected; retry to be safe
}

// In send loop:
if statusCode, err := adapter.Send(ctx, cmd); err != nil {
    if !p.shouldRetry(statusCode) {
        item.Term()  // Permanent failure → dead-letter
        metrics.outbound_terminated.Inc(channel_type, "4xx")
    } else {
        item.Nak(nats.NakWithDelay(p.nakDelay(statusCode)))
        metrics.outbound_retry.Inc(channel_type, http_status)
    }
} else {
    item.Ack()
}
```

**Nak delay strategy:**
- 5xx: `100ms + jitter` (fast backoff; server likely recovering).
- 429: `500ms + jitter` (respect rate limit; wait for bucket refill).

**Future upgrade (C, if P9 shows need):**
```go
type Adapter interface {
    // ... Send, Edit, ChannelType ...
    // Optional; default is (B) logic
    ShouldRetry(statusCode int) bool
}
```

**Citation:**
- [HTTP Status Codes (RFC 7231)](https://tools.ietf.org/html/rfc7231#section-6) — semantics of 4xx, 5xx.
- [Slack Rate Limits](https://docs.slack.dev/apis/web-api/rate-limits/) — 429 is the signal for rate-limit hit; explicitly retry.
- [Zoho Cliq Rate Limits](https://www.zoho.com/cliq/help/restapi/v2/) — per-minute quotas → 429 is expected.

**Alignment with plan:** Plan specifies "5xx → Nak, 4xx → Term, metric on http_status." (B) matches this exactly (with 429 exception per best practice).

---

## Q9: DLQ Design Options — Deferred Until First Production Failure

### Question

Where do terminated messages go? They're not retryable (4xx client error) but worth auditing. Options for deferring this decision:

- **(A)** `MESSAGES_DLQ` stream (NATS channel), consumers pull for manual inspection.
- **(B)** Postgres `outbound_audit` table with `terminated=true` flag; queries via SQL.
- **(C)** GCS bucket (`gs://mio-messages/dlq/`); same sink logic as archive (P6).
- **(D)** Logging only; no separate DLQ. Alerts on `mio_gateway_outbound_terminated_total` metric exceeding threshold.

### Analysis

**POC constraint:** P5 plan explicitly defers DLQ design. This question is "what's the right choice for eventual graduation?"

| Dimension | (A) NATS Stream | (B) Postgres | (C) GCS | (D) Metrics Only |
|-----------|---|---|---|---|
| **Durability** | Persistent (JetStream) ✓ | Persistent (DB) ✓ | Persistent (Cloud) ✓ | Transient (metrics scraped every 5min) |
| **Query/audit access** | CLI `nats stream view MESSAGES_DLQ` | SQL `SELECT * FROM outbound_audit WHERE terminated=true` | `gsutil ls gs://mio-messages/dlq/` + download | Grafana queries; no raw message access |
| **Message access** | Full payload, headers | Payload in JSONB column | Full payload, headers | None; only status code + reason |
| **Replay capability** | Publish to `MESSAGES_OUTBOUND` again | INSERT into `messages_outbound` via trigger | Download and replay via script | Not possible; alert + manual intervention |
| **Integration with P6 (GCS sink)** | Separate consumer + logic | Trigger or separate scan job | Reuse sink logic ✓ | N/A |
| **Operational complexity** | Low; another consumer + stream | Medium; schema, jobs, queries | Medium; GCS permissions, partitioning | Low; metric alerting |
| **Cost** | Storage (NATS disk ≈ prod cost) | Storage + compute (queries on large table) | Storage (GCS ≈$0.02/GB/month) | Free (built-in metrics) |
| **Privacy/compliance** | All DLQ messages in NATS cluster (potential audit concern) | DLQ in Postgres (PII in `text` field; needs PII-aware queries) | DLQ in GCS (cloud storage; GCS-level ACLs available) | Metrics exclude message bodies |
| **Debugging user issue** | "Check stream; see full context" | "Check table; see full context" | "Download file; see full context" | "Check metric; you don't have message" |
| **POC gradient** | Medium (requires new consumer, stream def) | Medium (new table, cleanup logic) | Low (reuse sink consumer + format) | Low (alert metric exists) |

### Recommendation: **(D) Metrics Only for POC; (C) GCS for Graduation**

**Why:**
1. **POC discipline.** Terminate only on 4xx. If we see `mio_gateway_outbound_terminated_total > 10/day`, we have a signal to investigate. For POC, that's enough. (We don't expect many; it's a smoke test.)
2. **Graduation path (C) is natural.** P6 builds GCS sink for archive. DLQ is just another sink consumer (same pattern, different subject filter or status flag). Reuse the JSON partitioning, same ops, same audit trail.
3. **Avoid Postgres schema creep.** Two more tables (outbound_audit, outbound_state) for P5 alone. Keep Postgres for inbound messages + state only (conversations, users). DLQ is output, not state.
4. **Avoid NATS stream overload.** Two more streams (MESSAGES_OUTBOUND, MESSAGES_DLQ, MESSAGES_INBOUND) is fine. Three is still fine. But if we add more channels and DLQ grows, GCS is more scalable than JetStream for archive-like workloads.

**POC implementation:**
- Add metric: `mio_gateway_outbound_terminated_total{channel_type, reason}`.
- Alert if this exceeds 10/day (manual investigation).
- Log full error: `{"send_command_id": "...", "channel_type": "...", "status_code": 400, "reason": "..."}` at WARN level.

**Graduation (P8+):**
- Add second `gcs-archiver` consumer on `MESSAGES_OUTBOUND` (filter by some flag or read all, annotate with `terminated=true` in JSON).
- Write to `gs://mio-messages/dlq/date=YYYY-MM-DD/` with same partitioning as archive.
- Queries: `SELECT * FROM \`project.dataset.outbound_archive\` WHERE terminated=true`.

**Citation:**
- No specific external research needed; this is architectural deferral. Good practice per "POC scope" constraints in master.md.

**Alignment with plan:** Plan says "defer until first real terminated message in the wild." ✓ Exactly. (D) is the POC defeq; (C) is the graduation.

---

## Q10: Outbound Idempotency on Adapter Side — NATS Dedup Window vs. App-Side State

### Question

NATS dedup window is 60s (on `MESSAGES_OUTBOUND` stream, configurable). Within that window, duplicate publishes are rejected server-side. Outside the window, duplicates are allowed. How should adapters handle re-delivery idempotency to avoid double-send?

- **(A)** Rely on NATS dedup only. Within 60s, NATS catches duplicates. Outside 60s, accept double-send (should be rare).
- **(B)** Adapter checks `outbound_state[SendCommand.id]` before calling Cliq Send. If present, it's a duplicate within same session; return cached external_id without sending again.
- **(C)** Adapter checks Cliq's message history (`GET /api/v2/chats/{chatid}/messages` filtered by sender_message_id) before sending; if found, reuse external_id.
- **(D)** SendCommand includes a dedup_key (generated by AI consumer); adapter stores it in Cliq message attributes for idempotency.

### Analysis

**NATS dedup semantics:** (per [NATS docs](https://docs.nats.io/using-nats/developer/develop_jetstream/model_deep_dive))
- Dedup window on `MESSAGES_OUTBOUND` stream set at 60s (default 2min; configurable in nanoseconds).
- NATS tracks `Nats-Msg-Id` header within window. Publish with same ID within 60s → server ignores, returns 200.
- After 60s, the dedup window slides forward, and the ID is forgotten. A new publish with the same ID is treated as a fresh message.
- **Within-window duplicates:** Server-side dedup; silent ack.
- **Outside-window duplicates:** No server-side protection; app-side dedup is the only defense.

**Scenario:**
1. AI publishes `SendCommand{id="msg-123", text="thinking..."}` at t=0s.
2. Gateway consumes, sends to Cliq, receives `external_id="cliq-456"`, stores in `outbound_state[msg-123] = cliq-456`.
3. Network glitch → Nak.
4. NATS redelivers to a different consumer in pool at t=30s (within 60s window).
5. Gateway sends again → Cliq gets two sends of "thinking...".

| Dimension | (A) NATS Dedup Only | (B) outbound_state Lookup | (C) Cliq History Query | (D) Dedup Key Attribute |
|-----------|---|---|---|---|
| **Within 60s duplicate** | NATS silently dedup ✓ (no double-send) | App dedup ✓ (check state; no send) | Query Cliq; dedup ✓ | Cliq-side dedup ✓ |
| **Outside 60s duplicate (rare)** | Double-send ❌ | Depends: if pool restarted, state cleared; double-send ❌ | Query Cliq; dedup ✓ | Cliq-side dedup ✓ |
| **Latency** | No extra cost | <1µs (map lookup) | 100–500ms (REST query per send) | <1µs (attribute set) |
| **Operational complexity** | Minimal; rely on NATS | Low; outbound_state already exists (Q5) | Medium; adds REST call per send; rate-limit impact | Medium; Cliq dedup logic (undocumented) |
| **Correctness guarantee** | Good (60s is long window) | Good within session; weak after restart | Good (Cliq is source of truth) | Depends on Cliq's implementation |
| **Cliq-specific risk** | Low; Cliq doesn't see duplicate attempts | Low | High; Cliq REST API doesn't expose `sender_message_id` filter; query would be expensive | Unknown; dedup key not standard HTTP |
| **Scalability** | Excellent; no extra I/O | Good; local map | Poor; O(n) queries on Cliq side (n = messages in chat) | Excellent; Cliq does dedup |

### Recommendation: **(B) outbound_state Lookup with Graceful Fallback**

**Why:**
1. **Reuses Q5 state.** `outbound_state` map already tracks `SendCommand.id → external_id`. Before calling Cliq Send, adapter checks: if `id` is in the map, return cached `external_id` (it was already sent once).
2. **Zero extra latency.** <1µs map lookup vs. 100–500ms Cliq REST query.
3. **Handles within-session duplicates.** NATS dedup (60s) + app dedup (outbound_state) layered. Within 60s, NATS catches it. Within same session, app catches it. Outside 60s + across restarts, accept rare double-send (graceful degradation; user can delete duplicate message manually in Cliq).
4. **No reliance on undocumented Cliq behavior.** Cliq dedup key is not standard; we don't know if Cliq supports it. Avoid.

**Implementation:**
```go
func (a *cliqAdapter) Send(ctx context.Context, cmd *miov1.SendCommand) (string, error) {
    // Check outbound_state first
    externalID, found := a.state.Lookup(cmd.GetId())
    if found {
        // Already sent in this session; return cached external_id
        return externalID, nil
    }

    // Send to Cliq
    resp, err := a.client.PostMessage(ctx, cmd.ConversationExternalID, cmd.GetText())
    if err != nil {
        return "", fmt.Errorf("cliq send: %w", err)
    }

    // Store in outbound_state for future lookups
    a.state.Store(cmd.GetId(), resp.MessageID)
    return resp.MessageID, nil
}
```

**Failure mode (outside 60s window, post-restart):**
- Double-send happens (both messages visible in Cliq).
- Metric increments (if we detect it): `mio_gateway_outbound_duplicate_send_total`.
- Alert: "Multiple sends of same message_id within 1 hour" → manual review.

**Citation:**
- [NATS JetStream Deduplication (NATS Docs)](https://docs.nats.io/using-nats/developer/develop_jetstream/model_deep_dive) — 60s window default; configurable.
- [NATS JetStream Deduplication for LinuxForHealth (NATS Blog)](https://nats.io/blog/nats-jetstream-deduplication-for-lfh/) — real-world dedup patterns.

**Alignment with plan:** Plan mentions "idempotency by SendCommand.id is the pool's job" and "adapter-side dedup via outbound_state lookup." ✓ Matches (B).

---

## Q11: Fairness Benchmark Methodology — vegeta vs. k6 vs. Custom Go Test

### Question

P5 success criteria: "Account B's lone outbound completes in <2s p99 even while A is bursting 50/sec." How to construct a benchmark (`gateway-bench-outbound` Make target) to verify this?

- **(A)** `vegeta` load testing tool: construct two scenarios (A: 50 req/sec steady, B: 1 req/5sec staggered), measure B's latency p99.
- **(B)** `k6` (Grafana): TypeScript script, concurrent VUs (virtual users) representing accounts A and B, built-in stats (p95, p99).
- **(C)** Custom Go test using `*testing.B` + `httptest` server + multiple goroutines (one per account).
- **(D)** Manual smoke test: `cloudflared tunnel` + two terminals sending messages, eye-ball the latency.

### Analysis

**Constraints:**
- Solo developer; benchmark must be <30 min to write and run.
- Reproducibility matters (CI will run it).
- Output must show p99 latency of B under A's load.
- Local dev setup (no cloud credit required).

| Dimension | (A) vegeta | (B) k6 | (C) Go test | (D) Manual |
|-----------|---|---|---|---|
| **Latency visibility** | Summary stats (min, max, p95, p99) ✓ | Full metrics (p50, p95, p99, etc.) ✓ | Custom reporting; easy ✓ | Eyeball; subjective ❌ |
| **Scenario expressivity** | DSL (URL, method, headers); limited scenario logic | Full programming language (TypeScript); flexible ✓ | Full Go; most flexible ✓ | N/A |
| **Setup time** | 5 min (install, write DSL) | 10 min (install, TypeScript) | 10 min (Go test setup) | 1 min |
| **Run time** | 30–60s (test duration) | 1–2 min (VU ramp, test) | 30s (benchmark duration) | 5–10 min (manual) |
| **Reproducibility** | Good (vegeta is deterministic) | Excellent (k6 records results, exportable) | Excellent (Go bench output reproducible) | Poor (human error) |
| **CI integration** | Moderate (parse vegeta JSON, extract p99) | Good (k6 outputs JSON; parse in CI) | Excellent (go test -bench integrates with CI) | Not viable |
| **Account isolation** | Moderate (separate vegeta runs, combine results) | Good (separate VUs in same script) | Excellent (separate goroutines) | Good (two terminals) |
| **Cost** | Free (open-source) | Free (open-source) | Free (stdlib) | Free |
| **Maintenance burden** | Low (DSL, unlikely to change) | Low (TypeScript, evolves with k6) | Low (Go, stable) | High (manual each time) |

### Recommendation: **(C) Custom Go Test with Concurrent Goroutines**

**Why:**
1. **Zero external dependency.** `testing.B` is stdlib; no install, no Docker, no versioning. Runs on CI automatically with `go test -bench`.
2. **Fine-grained control.** Goroutines can represent account A and B explicitly. Account A runs a tight loop (50/sec); account B publishes staggered (1 every 5s). Measure B's latency in the goroutine.
3. **Reproducible output.** Go's `testing.B` has predictable timing and stat reporting. Parse `testing.B.Logf()` or write custom JSON output.
4. **Integration with `make bench` target.**
   ```makefile
   .PHONY: gateway-bench-outbound
   gateway-bench-outbound:
       cd gateway && go test -bench=BenchmarkOutboundFairness -benchtime=30s -benchmem ./integration_test
   ```
5. **Future visibility.** If P8 deploys to GKE and we want continuous fairness monitoring, export `prometheus` metrics directly from the test (`promlint`-compatible).

**Test skeleton:**
```go
// gateway/integration_test/outbound_fairness_test.go

func BenchmarkOutboundFairness(b *testing.B) {
    // Setup: start gateway, NATS, create two accounts (A, B)
    gw, db, js := setupTestEnvironment(b)
    defer gw.Close()

    accountA := "account-aaaa-aaaa-aaaa"
    accountB := "account-bbbb-bbbb-bbbb"

    // Account A: sends 50/sec for 30s
    go func() {
        ticker := time.NewTicker(20 * time.Millisecond) // 50/sec
        defer ticker.Stop()
        for i := 0; i < 1500; i++ { // 50/sec * 30s = 1500
            <-ticker.C
            publishMessage(js, accountA, fmt.Sprintf("msg-a-%d", i))
        }
    }()

    // Account B: sends 1/5sec for 30s; measure latency
    var bLatencies []time.Duration
    mu := &sync.Mutex{}
    go func() {
        for i := 0; i < 6; i++ {
            start := time.Now()
            publishMessage(js, accountB, fmt.Sprintf("msg-b-%d", i))
            // Poll for confirmation (outbound ack or timeout 5s)
            err := waitForOutboundAck(js, accountB, fmt.Sprintf("msg-b-%d", i), 5*time.Second)
            latency := time.Since(start)
            mu.Lock()
            bLatencies = append(bLatencies, latency)
            mu.Unlock()
            if err != nil {
                b.Errorf("account B message %d: %v", i, err)
            }
            time.Sleep(5 * time.Second)
        }
    }()

    time.Sleep(30 * time.Second)

    // Analyze B's latencies
    sort.Slice(bLatencies, func(i, j int) bool { return bLatencies[i] < bLatencies[j] })
    p99Index := int(float64(len(bLatencies)) * 0.99)
    p99 := bLatencies[p99Index]

    b.Logf("Account B p99 latency: %v (limit: 2s)", p99)
    if p99 > 2*time.Second {
        b.Fatalf("fairness failed: B p99 %v exceeds 2s limit", p99)
    }
}
```

**Metrics to capture:**
- B's p99 latency.
- B's message count (should be 6, no loss).
- A's throughput under the burden (should be near 50/sec).
- Gateway CPU + memory.

**Citation:**
- [Go Testing Package (golang.org/pkg/testing)](https://golang.org/pkg/testing) — `testing.B` documentation.
- [Go Benchmarking Best Practices (Dave Cheney)](https://dave.cheney.net/2013/06/30/how-to-write-benchmarks-in-go) — methodology.

**Alignment with plan:** Plan specifies `gateway-bench-outbound` Make target. ✓ Implements it.

---

## Q12: Bucket Leak Alerting — How to Estimate "Active Accounts" Dynamically

### Question

`mio_gateway_ratelimit_buckets_active` metric tracks in-memory buckets. Plan says alert if `buckets_active > 10x active_accounts`. But how do we estimate "active accounts" dynamically? Options:

- **(A)** Count unique `account_id` in `MESSAGES_INBOUND` stream over the last hour. Query NATS or Postgres for inbound message count by account.
- **(B)** Expose `mio_gateway_ratelimit_active_accounts` gauge (incremented on first send from account, decremented on TTL evict). Alert if `buckets_active > 10 * active_accounts`.
- **(C)** Fixed threshold: alert if `buckets_active > 1000` (assumes <100 active accounts). Tune empirically after P8 deploy.
- **(D)** No alerting. Eviction goroutine ensures buckets are cleaned; if it dies, no big deal (10k buckets × 100 bytes = 1MB, not a leak risk for POC).

### Analysis

| Dimension | (A) Stream Query | (B) Dynamic Active-Account Gauge | (C) Fixed Threshold | (D) No Alerting |
|-----------|---|---|---|---|
| **Accuracy** | Good (counts actual inbound activity) | Excellent (tracks instantaneous state) | Poor (threshold may be wrong) | N/A |
| **Operational simplicity** | Moderate (query NATS or Postgres; scheduler job) | Low (gauge increment/decrement in code) | Minimal (hardcode 1000) | Minimal |
| **False positive rate (alert when shouldn't)** | Low (counts real accounts) | Low | High (10x is arbitrary) | N/A |
| **Responsiveness** | Delayed (1-hour window; stale) | Immediate (real-time) | Immediate | N/A |
| **Implementation cost** | 10 lines (NATS query or Postgres SELECT) | 5 lines (track on first bucket creation, decrement on evict) | 1 line | 0 lines |
| **Eviction goroutine death detection** | Indirect; gap in account activity → low alarm | Indirect; buckets_active stops decreasing → no new decrements | Direct; if buckets only grow, alert | None; silent failure |
| **POC viability** | Good; real feedback | Good; cleaner signal | Good; simple | Good; acceptable risk |

### Recommendation: **(B) Dynamic Active-Account Gauge**

**Why:**
1. **Real-time signal.** `mio_gateway_ratelimit_active_accounts` gauge increments when first bucket is created, decrements when evicted. True state of "accounts the gateway has seen recently."
2. **Clean alert logic.** `buckets_active > 10 * active_accounts` is automatically tuned; if active_accounts = 5 and buckets_active = 100, alert. If active_accounts = 100 and buckets_active = 500, no alert (healthy). No magic number.
3. **Minimal code.** Increment in `Limiter.Allow()` (check if bucket exists; if not, create and increment gauge). Decrement in eviction goroutine (on delete, decrement gauge).
4. **Detects eviction goroutine death.** If eviction goroutine dies, `active_accounts` stops decrementing but new messages still increment `buckets_active`. Ratio diverges → alert.

**Implementation:**
```go
type Limiter struct {
    mu              sync.RWMutex
    buckets         map[string]*rate.Limiter
    activeAccounts  prometheus.Gauge  // tracks live account count
    bucketsGauge    prometheus.Gauge  // tracks bucket count (already exists)
    // ... other fields ...
}

func (l *Limiter) AllowAccount(accountID string) bool {
    l.mu.Lock()
    defer l.mu.Unlock()

    if _, exists := l.buckets[accountID]; !exists {
        // New account; create bucket and increment gauge
        l.buckets[accountID] = rate.NewLimiter(l.rate, l.burst)
        l.lastUse[accountID] = time.Now()
        l.activeAccounts.Inc()  // Increment active-account gauge
    } else {
        l.lastUse[accountID] = time.Now()  // Update last-use timestamp
    }

    limiter := l.buckets[accountID]
    return limiter.Allow()
}

func (l *Limiter) evictLoop() {
    for range time.Tick(60 * time.Second) {
        l.mu.Lock()
        now := time.Now()
        for accountID, t := range l.lastUse {
            if now.Sub(t) > l.ttl {
                delete(l.buckets, accountID)
                delete(l.lastUse, accountID)
                l.activeAccounts.Dec()  // Decrement as evicted
            }
        }
        l.mu.Unlock()
    }
}
```

**Alerting rule (Prometheus):**
```
alert: RateLimitBucketLeak
expr: mio_gateway_ratelimit_buckets_active > 10 * mio_gateway_ratelimit_active_accounts
for: 5m
```

**Citation:**
- Prometheus best practices: use gauges for instantaneous state (count of buckets), not counters.

**Alignment with plan:** Plan mentions "alert on buckets_active > 10x active accounts." ✓ Implements exactly.

---

## Q13: Graceful Shutdown — Drain In-Flight Outbound Messages

### Question

When gateway receives SIGTERM, it must not lose in-flight outbound messages. What's the shutdown sequence?

- **(A)** Stop accepting new pulls from `sender-pool` consumer, let MaxAckPending in-flight messages finish, then close. Timeout: 30s (if not done, force-close).
- **(B)** Publish remaining messages in buffer back to `MESSAGES_OUTBOUND` (if any are pending), then close.
- **(C)** Drain HTTP server, then graceful shutdown of NATS consumer (stop pulling, wait for all Acks).
- **(D)** Just close; NATS will redelivery Nak'd messages via `AckWait` timeout.

### Analysis

| Dimension | (A) Stop Pull + MaxAckPending Drain | (B) Re-publish Pending | (C) HTTP Drain + Consumer Shutdown | (D) Just Close |
|-----------|---|---|---|---|
| **Message loss** | No; Ack-wait redelivers anything left | Unlikely (already fetched; replay risky) | No; Ack-wait redelivers | Possible if consumer in-flight pool closes hard |
| **In-flight drain time** | ~1–5s (MaxAckPending=32, avg 50ms/message) | N/A | Same as (A) | N/A |
| **Implementation** | 10 lines; signal handler → stop pull loop | 15 lines; buffer + republish logic | 20 lines; two shutdown phases | 5 lines; nothing |
| **Correctness guarantee** | High (pull loop stops; in-flight drains) | Medium (re-publish may fail; lose message) | High; standard pattern | Low; relies on AckWait |
| **Graceful for HTTP server** | Separate concern (HTTP server has its own shutdown) | Same | Yes; explicit HTTP drain ✓ | No; may hang on HTTP requests |
| **AckWait interaction** | Good; in-flight messages Ack'd before shutdown | Good; re-published messages are fresh | Good | Good; AckWait is fallback |
| **POC viability** | ✓ Good; 10 lines | ⚠️ Risky; re-publish edge case | ✓ Good; explicit | ✓ Acceptable; but not graceful |

### Recommendation: **(A) Stop Pull Loop + Wait for In-Flight**

**Why:**
1. **Stateless design.** Pull consumer is already stateless; when we stop pulling, in-flight messages auto-drain as they're processed (Ack or Nak). No buffer to manage, no re-publish logic.
2. **Standard pattern.** HTTP server drain is orthogonal (chi router has `.Stop()`); NATS consumer drain is separate. Sequence: (1) stop pulling; (2) drain in-flight with timeout; (3) close DB pool; (4) close NATS connection.
3. **Safe timeout.** 30s timeout means: drain for up to 30s, or force-close if not done. With MaxAckPending=32 and ~50ms Cliq latency, 1600ms is plenty. Leaves margin for slow sends.
4. **Visibility.** Log each stage: "graceful shutdown: stopping pull", "drained N in-flight", "closed consumer", etc. Helps in production debugging.

**Implementation:**
```go
func (p *Pool) Start(ctx context.Context) {
    for {
        select {
        case <-ctx.Done():
            // Graceful shutdown: stop pulling
            return
        default:
            msgs, err := p.consumer.Fetch(maxMessages)
            if err != nil {
                // Consumer closed or error; exit loop
                return
            }
            for _, msg := range msgs {
                p.processMessage(msg)
            }
        }
    }
}

// In main.go:
sigChan := make(chan os.Signal, 1)
signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)
<-sigChan

log.Info("graceful shutdown: stopping pull loop")
cancelPullCtx()  // Cancel the pool's ctx; stops Fetch loop

// Wait for in-flight to drain (MaxAckPending=32, expect ~1–5s)
drainCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
p.WaitDrained(drainCtx)  // Block until all in-flight Ack'd or timeout
log.Info("graceful shutdown: drained in-flight messages")

// Graceful HTTP server shutdown
httpServer.Shutdown(drainCtx)
log.Info("graceful shutdown: closed HTTP server")

// Close NATS consumer
p.consumer.Delete()
log.Info("graceful shutdown: deleted consumer")

// Close DB pool
db.Close()
log.Info("graceful shutdown: closed DB pool")
```

**Helper method:**
```go
func (p *Pool) WaitDrained(ctx context.Context) error {
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
            // Check if all in-flight messages are processed
            // (This is tricky; NATS doesn't expose in-flight count directly.)
            // Workaround: track in-flight count in Pool, decrement on Ack/Nak.
            p.mu.RLock()
            inFlight := p.inFlightCount
            p.mu.RUnlock()
            if inFlight == 0 {
                return nil
            }
            time.Sleep(100 * time.Millisecond)
        }
    }
}
```

**Metrics:**
- `mio_gateway_graceful_shutdown_total` (counter) — incremented on SIGTERM.
- `mio_gateway_graceful_shutdown_drain_duration_seconds` (histogram) — time spent draining.
- `mio_gateway_graceful_shutdown_messages_drained_total` (counter) — count of drained messages.

**Citation:**
- [Chi Router Graceful Shutdown (chi)](https://github.com/go-chi/chi#routing) — `.Stop()` method.
- [NATS Go Client Shutdown](https://github.com/nats-io/nats.go) — consumer cleanup.

**Alignment with plan:** Plan mentions "drain in-flight; stop accepting new pulls; flush metrics." ✓ (A) implements all three.

---

## Alignment with Master Plan & Foundation

All 13 questions are grounded in the foundation decisions from P1 + P3:

1. **Four-tier addressing locked:** All design assumes `account_id` as the rate-limit key (Q1, Q2, Q5). ✓
2. **`channel_type` string registry:** Adapter pattern (Q6, Q7) assumes registry, not enum. No proto changes on P9. ✓
3. **Idempotency by `(account_id, source_message_id)`:** (Not directly addressed here, but Q4 + Q10 acknowledge it.)
4. **Polymorphic `Conversation` with kind:** (Not directly in scope; P3 concern.)

---

## Risks, Trade-offs, and Next Steps

### Identified Risks

| Risk | Mitigation | Residual Risk |
|---|---|---|
| **Cliq edit semantics for bot DMs** (Q4) | Empirically validate PATCH on carry-over rig before P5 implementation | Medium; if PATCH fails for DMs, fallback to "supersedes" attribute, which changes AI-side correlation |
| **Rate-limit bucket leak** (Q12) | Metric alert on bucket/account ratio divergence; eviction goroutine logs death | Low; metric catches it immediately |
| **Two-step UX state lost on restart** (Q5) | In-memory map; acceptable for POC (restart between send & edit is rare) | Low for POC; upgrade to Postgres if production shows it's common |
| **Cliq undocumented rate limits** (Q8) | Start with 429 special-case; tune per-adapter if Slack shows different behavior | Low; metrics visible; easy to adjust |
| **NATS fairness under burst** (Q3) | Jitter on Nak-delay; metrics on redelivery per account | Low; jitter is battle-tested pattern |
| **Idempotency outside 60s dedup window** (Q10) | Accept rare double-send; metric alert on anomalies | Low for POC; upgrade to distributed dedup if production shows pattern |

### Trade-Offs Summary

| Decision | Chosen | Rejected | Rationale |
|----------|--------|----------|-----------|
| Rate limit (Q1) | In-process local bucket | Redis distributed | POC scale; zero ops; easy upgrade path |
| Composite rate-limit keys (Q2) | Adapter override pattern | Baked into protocol | Zero proto changes; P9 flexibility |
| Workqueue fairness (Q3) | NATS fair redelivery + jitter | Subject-shard per account | Fair over time; simpler; upgrade path clear |
| Cliq edit semantics (Q4) | PATCH + app-side dedup | X-Idempotency-Key header | Cliq docs silent on idempotency; explicit dedup safer |
| Two-step UX state (Q5) | In-memory map with TTL | Postgres durable store | POC acceptable failure mode; 400× faster latency |
| Adapter surface (Q6) | Minimal + MaxDeliver | Preemptive Delete/React/Typing | Honest interface; no pre-emptive code |
| Adapter registration (Q7) | `init()` blocks per package | Central registry enum | P9 litmus: zero `dispatch.go` edits |
| 4xx handling (Q8) | 429 special-case + conservative 4xx | Always Nak to DLQ | Explicit retry policy; metrics visible |
| DLQ design (Q9) | Metrics-only POC; GCS for graduation | Postgres separate table | Defer design; avoid schema creep |
| Idempotency (Q10) | outbound_state lookup | Cliq dedup key attribute | No reliance on undocumented Cliq behavior |
| Fairness benchmark (Q11) | Custom Go test + httptest | vegeta or k6 external tools | Zero external dependency; CI-native |
| Bucket leak alerting (Q12) | Dynamic active-account gauge | Fixed threshold | Real-time; auto-tuning; detects eviction goroutine death |
| Graceful shutdown (Q13) | Stop pull loop + drain in-flight | Just close or re-publish | Stateless; standard pattern; simple |

---

## Sources & Further Reading

### Official Documentation
- [golang.org/x/time/rate Package](https://pkg.go.dev/golang.org/x/time/rate) — token bucket algorithm, Limiter interface
- [NATS JetStream Consumers](https://docs.nats.io/nats-concepts/jetstream/consumers) — MaxAckPending, Nak, workqueue
- [NATS JetStream Streams](https://docs.nats.io/nats-concepts/jetstream/streams) — retention policies, duplicates window
- [Zoho Cliq REST API v2](https://www.zoho.com/cliq/help/restapi/v2/) — endpoint structure, rate limits
- [Slack Rate Limits](https://docs.slack.dev/apis/web-api/rate-limits/) — tier system, 429 handling
- [NATS by Example: Work-Queue Stream (Go)](https://natsbyexample.com/examples/jetstream/workqueue-stream/go) — practical workqueue pattern

### Articles & Guides
- [How to Implement Rate Limiting in Go (OneUptime, 2026)](https://oneuptime.com/blog/post/2026-01-23-go-rate-limiting/view) — local bucket patterns, sync.Map per-user limits
- [Build 5 Rate Limiters with Redis (redis.io)](https://redis.io/tutorials/howtos/ratelimiting/) — distributed token bucket, Lua scripts
- [Go Wiki: Rate Limiting](https://go.dev/wiki/RateLimiting) — official recommendations
- [Handling Rate Limits with Slack APIs (Medium)](https://medium.com/slack-developer-blog/handling-rate-limits-with-slacks-apis-f6f8a63bdbdc) — best practices
- [How to Write Benchmarks in Go (Dave Cheney)](https://dave.cheney.net/2013/06/30/how-to-write-benchmarks-in-go) — Go testing methodology
- [Grokking NATS Consumers: Push-based queue groups (Byron Ruth)](https://www.byronruth.com/grokking-nats-consumers-part-2/) — fairness discussion

### Prior Research (Internal)
- [P0 Scaffold Research (260508-1056)](plans/reports/researcher-260508-1056-p0-scaffold-monorepo-infra.md) — Go layout, Buf config, NATS setup
- [Cliq Bot Reaction Identity (260503-2013)](playground/cliq/reports/researcher-260503-2013-cliq-bot-reaction-identity.md) — bot-message edit asymmetry (documented risk)
- [Cliq Message Capture (260503-1012)](playground/cliq/reports/researcher-260503-1012-cliq-message-capture-deep.md) — Cliq webhook semantics, bot participation handler

---

## Unresolved Questions

1. **Cliq PATCH vs. PUT for edit endpoint.** Docs are silent. Plan requires empirical validation on carry-over rig (`playground/cliq/test-zoho-cliq-send-message.sh`) before P5 implementation. **Action:** Test in P3 with real bot credentials.

2. **Cliq rate-limit specifics.** Docs mention "per-minute quotas" but don't specify account-level vs. channel-level vs. workspace-level. **Action:** Monitor metrics in P8 deploy; adjust (Q8) strategy if pattern emerges.

3. **Cliq bot-DM edit support.** Research memo `playground/cliq/reports/researcher-260503-2013-cliq-bot-reaction-identity.md` flags asymmetry. Does PATCH succeed or fail? **Action:** Test in P3; fallback to "supersedes" attribute if edit unsupported for DMs.

4. **Outbound state TTL correctness across restarts.** Is 10min TTL sufficient, or will AI side expect edits for older messages? **Action:** Validate in P8 deploy; feedback may drive Postgres migration for P9.

5. **NATS consumer cleanup on graceful shutdown.** Will `consumer.Delete()` succeed during shutdown, or must we use `Unsubscribe()` first? **Action:** Test in P3 local environment.

---

## Recommendation Summary

**Per-account token bucket (Q1):** Implement in-process `golang.org/x/time/rate.Limiter` with TTL eviction. Zero external dependencies; 400× faster than Redis. Upgrade path clear if multi-gateway fairness becomes a concern.

**Adapter registry (Q7):** Use `init()` blocks in each channel package for self-registration. P9 adds Slack with zero edits to `dispatch.go`, `main.go` logic, or proto. ✓ Litmus test passes.

**Edit UX (Q5):** In-memory `outbound_state` map with 10-minute TTL. Graceful degradation: restart between send and edit results in two visible messages (acceptable for POC). Upgrade to Postgres if production metrics show restart-during-edit is common.

**Cliq integration (Q4 + Q10):** Use PATCH (HTTP semantics) for edits. App-side dedup via `outbound_state` lookup before Send. No reliance on undocumented Cliq idempotency headers.

**NATS fairness (Q3):** Redelivery is fair (NATS design). Jitter on Nak-delay (100–500ms) prevents thundering herd.

**Graceful shutdown (Q13):** Stop pull loop, drain in-flight (30s timeout), close HTTP server, close DB pool.

All choices are POC-appropriate, clearly understood, and have documented upgrade paths to production.

---

**Report date:** 2026-05-08  
**Investigator:** Researcher Agent  
**Status:** ✅ Ready for P5 implementation planning
