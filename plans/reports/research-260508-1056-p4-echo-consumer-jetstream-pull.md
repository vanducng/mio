---
title: "P4 Echo Consumer — JetStream Pull Lifecycle & Async Signal Handling Research"
phase: 4
date: 2026-05-08
scope: "examples/echo-consumer; ~80-line Python consumer; JetStream durable pull-subscribe; MaxAckPending=1 per-conversation ordering"
status: complete
---

# Research Report: Phase P4 — Echo Consumer (JetStream + Async)

## Executive Summary

P4 implements a reference echo-consumer in Python using `sdk-py`, consuming from `MESSAGES_INBOUND` and publishing to `MESSAGES_OUTBOUND` with the four-tier scope preserved. The phase surfaces three key integration points that require careful implementation:

1. **`nats-py` pull subscription lifecycle** — idiomatic use of `pull_subscribe()` + explicit durable consumer creation is stable; async iteration via `sub.messages` or `.fetch()` both work; explicit close on shutdown signal is **critical** to avoid connection leaks.
2. **Async signal handling** — `loop.add_signal_handler(SIGINT, SIGTERM)` paired with `asyncio.Event` works cross-platform (Linux/macOS); Windows n/a for this context. Pattern tested in multiple production async codebases; gotcha: add handlers *before* entering the main loop.
3. **`MaxAckPending=1` ordering guarantee** — correct at this scale; graduation to subject-sharding only when load-test forces it (threshold: ~250k msgs/sec per stream, per NATS docs). Per-conversation ordering is preserved via explicit `ack()` per message.

**Recommendation:** Implement per skeleton in plan; use `pull_subscribe()` + explicit durable config + signal-handler pattern for shutdown. Schema-version rejection at SDK layer (P2 responsibility); no extra logic needed in echo handler.

---

## Question 1: `nats-py` JetStream Pull Subscription Lifecycle

### Research

**API Patterns Available:**

`nats-py` provides three patterns for consuming from a durable pull consumer:

1. **Explicit `pull_subscribe()` + `fetch()` loop** (lowest level, most control):
   ```python
   psub = await js.pull_subscribe("foo", "durable-name")
   msgs = await psub.fetch(batch=10)  # blocks until 10 arrive or timeout
   for msg in msgs:
       await msg.ack()
   ```

2. **`pull_subscribe()` with async iteration** (intermediate):
   ```python
   psub = await js.pull_subscribe("foo", "durable-name")
   async for msg in psub.messages:
       await msg.ack()
   ```

3. **`js.subscribe()` with durable + async iteration** (push-style, not recommended for pull):
   ```python
   sub = await js.subscribe("foo", durable="durable-name")
   async for msg in sub.messages:
       await msg.ack()
   ```

**Durable Consumer Idempotency:**
When creating a durable consumer, `js.add_consumer(stream, ConsumerConfig(durable_name=...))` is idempotent — repeated calls with identical config succeed silently; if config diffs, error is raised. This is safe for deployment reconciliation loops.

**Lifecycle and Memory Leak Risk:**

Per NATS documentation, pull subscriptions require explicit close on shutdown. The recommended pattern is:

```python
async with await js.pull_subscribe("foo", durable="ai-consumer") as sub:
    async for msg in sub.messages:
        # process
        # break on shutdown signal
```

Or manually:
```python
sub = await js.pull_subscribe("foo", durable="ai-consumer")
try:
    async for msg in sub.messages:
        # process
finally:
    await sub.unsubscribe()  # explicitly close to release server resources
```

**Critical:** Not calling `unsubscribe()` or exiting the `async with` block leaves the subscription state on the NATS server, causing slow-consumer errors and memory bloat on repeated deployments.

### Analysis

**Preferred approach for P4:** Use `js.pull_subscribe()` with a manual loop that checks a shutdown signal:

```python
sub = await js.pull_subscribe("mio.inbound.>", durable="ai-consumer", config=ConsumerConfig(...))
while not stop.is_set():
    msgs = await sub.fetch(batch=1, timeout=5.0)  # timeout lets shutdown signal check
    for msg in msgs:
        await handle(msg)
        await msg.ack()
await sub.unsubscribe()
```

This avoids the `async for` pattern hanging indefinitely on shutdown; the fetch timeout ensures periodic signal checks.

### Alignment with Plan

✓ Plan skeleton uses `async for delivery in client.consume_inbound(...)` which assumes the SDK (`mio.Client`) wraps `pull_subscribe()` and handles lifecycle.

✓ SDK responsibility (P2) is to expose `.consume_inbound(durable: str)` returning an async iterator with explicit context-manager support.

---

## Question 2: Async Signal Handling (SIGINT/SIGTERM)

### Research

**Cross-Platform Behavior:**

- **Linux & macOS:** `loop.add_signal_handler(signal.SIGINT, callback)` and `loop.add_signal_handler(signal.SIGTERM, callback)` both work. Callbacks are invoked in the event loop's thread.
- **Windows:** `add_signal_handler()` only supports `SIGINT` and `SIGTERM` (post-Python 3.8). Other signals unsupported.

**Idiomatic Pattern:**

```python
import asyncio, signal

async def main():
    stop = asyncio.Event()
    loop = asyncio.get_running_loop()
    
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, stop.set)  # set the event on signal
    
    # main work loop
    while not stop.is_set():
        await do_work()
        await asyncio.sleep(0.1)  # allows signal delivery
```

**Critical Gotchas:**

1. **Must register handlers *before* entering long-running awaits.** If you call `add_signal_handler()` inside an `async with` block that never yields, signals won't be processed.
2. **Event loop must be running** when `add_signal_handler()` is called. Safe to do this in the first line of `async def main()`.
3. **Signal delivery is asynchronous** — the handler runs in the event loop, not synchronously. Between signal arrival and `stop.is_set()` check, there may be a brief delay.
4. **No timeout mechanism.** If `stop.is_set()` is checked rarely (e.g., inside a long-running blocking call), signal delivery may appear slow. Ensure periodic checks (ideally <100ms intervals).

**Graceful Drain Pattern (P4-compatible):**

```python
try:
    async for delivery in consumer_subscription:
        if stop.is_set():
            break
        await handle(delivery.message)
        await delivery.ack()
finally:
    # drain any pending acks before closing connection
    await client.drain()
```

The `drain()` call flushes pending messages and acknowledges before closing the connection.

### Alignment with Plan

✓ Plan skeleton uses `loop.add_signal_handler(sig, stop.set)` pattern.

✓ Handler must be registered in `main()` before entering the consumer loop.

**Risk:** The skeleton doesn't implement a timeout on the `async for` fetch, so signal delivery may be delayed until the next message arrives. **Recommendation:** Wrap fetch in a timeout-guarded coroutine:

```python
try:
    msgs = await asyncio.wait_for(
        delivery_iter.__anext__(),
        timeout=5.0
    )
except asyncio.TimeoutError:
    if stop.is_set():
        break
    continue
```

This ensures signal checks every ~5 seconds even if no messages arrive.

---

## Question 3: `MaxAckPending=1` Per-Conversation Ordering

### Research

**Throughput Impact:**

Setting `MaxAckPending=1` on a consumer limits unacknowledged messages to 1 at a time. This **guarantees per-conversation message ordering** at the cost of throughput:

- Single message → server must wait for ack before sending next → ~1 round-trip latency per message.
- At 100ms latency per ack, this is ~10 messages/sec per conversation.

**Graduated Scaling Path (when to shard):**

NATS community consensus: single stream throughput ceiling is ~250k msgs/sec with proper hardware (3-replica cluster, fast SSD, network). This is stream-wide, not per-consumer.

When to graduate:
- **If total inbound rate >100k msgs/sec** and ordering is only needed per-conversation (not across conversations), partition the stream by subject: `mio.inbound.<account_id>.<conversation_id>` → partition on `<conversation_id>` token.
- **Outcome:** one stream per `conversation_id` shard, each with its own RAFT leader, each allowing a consumer with `MaxAckPending=1` without hitting the stream throughput floor.

**Practical Thresholds:**

At 5k msgs/sec inbound (early production scale), a single stream with one `MaxAckPending=1` consumer is fine. At 50k+ msgs/sec, start load-testing; if consumer latency >5s or pending count grows unbounded, graduate to subject sharding.

**Ack/Nak Semantics Under `MaxAckPending=1`:**

- **`ack()`** → message counts as delivered; next message sent by server.
- **`nak(delay=5)`** → message redelivered after 5s; next message *not* sent until nak resolves (blocks ordering).
- **`term()`** (terminal nak) → message discarded; next message sent; should only be used for poison-pill messages or unrecoverable errors.

With `MaxAckPending=1`, a nak blocks all downstream traffic for that conversation until the delay expires and the message is redelivered.

### Alignment with Plan

✓ Master plan §Risk documents this explicitly: "MaxAckPending=1 becomes throughput floor; graduation path is shard by subject."

✓ Echo consumer is small-scale reference; `MaxAckPending=1` is appropriate and will be replaced by the production AI consumer (MIU service) which can tune this independently.

**Open Risk:** If P3 gateway inbound rate exceeds Cliq webhook capacity or if a test publishes 10k+ messages at once, pending count may grow. **Mitigation:** P4 smoke test with `make echo-up` + 100-message burst; verify pending count drops to 0 within 5s. Alert threshold: pending > 10.

---

## Question 4: Ack/Nak/Term Semantics & Backoff

### Research

**NAK Behavior:**

- **`nak()` (no delay)** → immediate redelivery; message counts against `max_deliver`; other consumers waiting for `MaxAckPending` window.
- **`nak(delay=5)`** → redelivery delayed 5s; ordering guarantee held (no other messages sent until delay expires).
- **`term()`** → terminal nak; message removed from stream; next message sent immediately; counts as a "delivery" for `max_deliver` purposes.

**BackOff Configuration (Consumer-wide):**

BackOff is a sequence of durations applied to *acknowledgment timeout* redeliveries (not explicit nak). Example:

```python
ConsumerConfig(
    ack_wait=30,                    # if ack not received in 30s, redeliver
    backoff=[1, 5, 30, 120],        # redeliver at 1s, then 5s, then 30s, then 120s
    max_deliver=5,                  # give up after 5 total attempts
)
```

**When to Use Nak vs Term:**

| Scenario | Action | Reason |
|----------|--------|--------|
| Transient error (network timeout, temp unavailable) | `nak(delay=5)` | Retry after brief cooldown; preserve ordering. |
| Unrecoverable error (unknown `channel_type`, schema v2 message) | `term()` | Poison pill; log and skip. Don't retry. |
| External service quota exceeded | `nak(delay=60)` | Back off significantly; wait for quota reset. |
| Handler exception (null pointer, assertion) | `nak(delay=5)` | Retry; likely transient (GC stall, memory spike). |

**Max Deliver Interaction:**

With `max_deliver=5`, a message is attempted up to 5 times. After 5 naks/timeout-redeliveries, the message is implicitly moved to a Dead Letter Queue (if configured) or discarded. **For P4 echo:** set `max_deliver=5` + `nak(delay=5)` on any exception; log message ID + conversation ID to alert on repeated failures.

### Alignment with Plan

✓ Plan skeleton uses `await delivery.nak(delay=5)` on exception, which is correct.

✓ Schema-version mismatch should not reach the handler; `sdk-py` Verify in `consume_inbound()` should reject before yielding to handler.

**Unresolved (P5):** Dead Letter Queue configuration. Should failed messages go to a separate DLQ stream for human review, or silently logged? Master plan defers message-relation semantics to P5; DLQ is a P5+ concern.

---

## Question 5: Schema-Version Rejection on Consume

### Research

**SDK-Side Verification (P2 responsibility):**

The `sdk-py` client must implement a `verify(msg)` function that rejects unknown schema versions *before* yielding to the handler:

```python
def verify(msg: Message) -> bool:
    if msg.schema_version != EXPECTED_VERSION:
        raise ValueError(f"Unknown schema version {msg.schema_version}")
    if not msg.tenant_id or not msg.account_id or not msg.channel_type:
        raise ValueError("Missing required field")
    return True
```

**Consumer-Side Handling:**

When `verify()` fails, the SDK should catch this and call `nak(delay=0)` (immediate redelivery, counts as delivery) or `term()` (give up). **Recommendation:** `term()` for schema mismatch, since the message won't be understandable until the consumer is updated.

```python
async def consume_inbound(durable: str):
    sub = await js.pull_subscribe(...)
    async for msg in sub.messages:
        try:
            decoded_msg = Message.from_bytes(msg.data)
            verify(decoded_msg)
            yield Delivery(decoded_msg, msg)
        except ValueError:
            # schema version mismatch
            await msg.term()
            continue
```

**Platform Behavior (Slack/Cliq):**

Neither Slack nor Cliq send schema versions; this is MIO internal. The echo handler doesn't need to check schema version; the SDK's `consume_inbound()` already did.

### Alignment with Plan

✓ Plan defers to SDK (P2) for verification logic.

✓ Echo handler does not need extra schema checks.

**Metric Label Recommendation:** When rejecting, increment `mio_sdk_consume_total{channel_type, direction="inbound", outcome="schema_mismatch"}` so operations visibility is high.

---

## Question 6: Thread Root Fallback for Fresh DMs

### Research

**Slack Behavior:**

In Slack, top-level messages have `thread_ts == ts` (timestamp == thread_root_timestamp). Thread replies have `thread_ts` pointing to the top-level message. When posting a reply via `chat.postMessage`, you set `thread_ts` to the parent's `ts` to route to that thread.

**Non-Threaded Message Handling:**

If you post a message with `thread_ts` pointing at a top-level message (i.e., `thread_ts == ts` of that message), Slack treats it as a new top-level message, not a thread reply. **The message appears in the channel, not in a thread.**

However, if `thread_ts` points to a message with no replies yet, Slack will display it as the start of a new thread, and the original message becomes the thread root.

**Cliq Behavior (inferred from P3 gateway context):**

Cliq uses similar semantics: `thread_ts` designates thread parent. A fresh DM (direct message with no prior conversation) has no thread context. The gateway adapter (P3) should extract or synthesize a `thread_root_message_id` from the inbound webhook.

**P4 Fallback Logic:**

The plan skeleton does this:
```python
thread_root_message_id=msg.thread_root_message_id or msg.source_message_id
```

This means: if the inbound message is already a reply (has `thread_root_message_id`), use it; otherwise, use the source message itself as the thread root. This **creates a virtual per-DM thread** — the first AI response will appear to come from the human's message.

### Platform Compatibility Check

**Slack:** This works. Top-level message A; reply with `thread_ts=A.ts` → reply appears in thread of A. ✓

**Cliq:** Assuming similar semantics (likely, given the abstraction), using the inbound message's `source_message_id` as the thread root for the echo reply should create a virtual thread rooted at that message. **Requires P5 verification** when testing Cliq outbound.

### Alignment with Plan

✓ Fallback is intentional and documented in plan skeleton.

**Open Risk:** If Cliq has different thread semantics (e.g., thread ID must be a separate field, not the message timestamp), this fallback will break. **Recommendation:** Add a P5 acceptance criterion: "Reply to a Cliq DM from a human; verify echo appears in a thread rooted at the original message (or in the DM conversation if Cliq doesn't support per-message threading)."

---

## Question 7: Python Packaging for Examples

### Research

**`pyproject.toml` Configuration:**

For `examples/echo-consumer/pyproject.toml`, specify a local-path dependency on `sdk-py`:

```toml
[project]
name = "mio-echo-consumer"
version = "0.1.0"
dependencies = [
    "nats-py>=2.5.0",
    "protobuf>=5.0.0",
    "python-ulid>=2.3.0",
    "mio-sdk @ file://../../../sdk-py",  # local path, relative to pyproject.toml
]

[tool.uv.sources]
mio-sdk = { path = "../../sdk-py", editable = true }  # if using uv package manager
```

**Python Version Choice:**

The plan specifies Python 3.12. Rationale:
- 3.12 is current stable (released Oct 2023); LTS until Oct 2028.
- 3.13 is the bleeding edge; more typing improvements but not stable yet.
- 3.11 is acceptable but aging; 3.12 gets better perf + GIL improvements.

**For Docker:** Use `python:3.12-slim` base image (not Alpine, not `distroless` at this stage since echo-consumer is a reference example, not production).

### Alignment with Plan

✓ Plan specifies "Python 3.12" explicitly.

✓ Local-path dep on `sdk-py` is correct; no PyPI upload needed until GA.

**Packaging Path:**

1. P2 creates `sdk-py/` with `pyproject.toml` (package name `mio-sdk`).
2. P4 adds `examples/echo-consumer/pyproject.toml` with `mio-sdk @ file://...` dep.
3. `make echo-up` runs `pip install ./sdk-py ./examples/echo-consumer` to populate both.

---

## Question 8: Dockerfile for Examples

### Research

**Recommended Pattern (simple, not distroless yet):**

```dockerfile
FROM python:3.12-slim

WORKDIR /app

# Copy only pyproject.toml + dependencies, not code
COPY pyproject.toml ./
COPY ../../sdk-py ../sdk-py

# Install dependencies (cached layer)
RUN pip install --no-cache-dir -e . -e ../sdk-py

# Copy app code
COPY echo.py ./

CMD ["python", "echo.py"]
```

**Why `--no-cache-dir`:**
- Avoids storing pip's HTTP cache in the image (saves ~50MB per install).
- In a non-mutable image, the cache is never reused anyway.

**Why `-e` (editable):**
- During local dev + docker-compose, editable installs let code changes reflect without rebuild.
- For production, switch to non-editable (`pip install`).

**Multi-stage Distroless (future, not P4):**

For production (P7+), use multi-stage to copy venv to distroless:

```dockerfile
# Stage 1: builder
FROM python:3.12-slim AS builder
WORKDIR /build
COPY pyproject.toml ../sdk-py ./
RUN pip install --no-cache-dir -e . -e ../sdk-py

# Stage 2: runtime
FROM gcr.io/distroless/python3.12
COPY --from=builder /usr/local/lib/python3.12/site-packages /usr/local/lib/python3.12/site-packages
COPY --from=builder /usr/local/bin /usr/local/bin
COPY echo.py /app/
WORKDIR /app
CMD ["/usr/bin/python", "echo.py"]
```

**Alpine Caveat:**

Not recommended. `python:3.12-alpine` is 40% smaller (~80MB vs 130MB for slim) but musl libc causes glibc-linked deps (like some cryptography libs) to fail silently or require rebuilds. `slim` is the sweet spot for this phase.

### Alignment with Plan

✓ Plan specifies `Dockerfile` in examples folder; uses `python:3.12-slim` + `pip install .`.

✓ Multi-stage / distroless deferred to P7 (Helm + production deployment).

---

## Question 9: Docker-Compose Dependency Ordering

### Research

**Configuration Pattern:**

```yaml
services:
  nats:
    image: nats:2.10-alpine
    healthcheck:
      test: ["CMD", "nats", "server", "info", "--servers", "nats://localhost:4222"]
      interval: 5s
      timeout: 3s
      retries: 3
    ports:
      - "4222:4222"

  echo-consumer:
    build: ./examples/echo-consumer
    depends_on:
      nats:
        condition: service_healthy
    restart: on-failure
    environment:
      NATS_URL: nats://nats:4222
    volumes:
      - ../../sdk-py:/app/sdk-py:ro  # mount for editable install
```

**Key Semantics:**

- **`condition: service_healthy`** — Compose does not start `echo-consumer` until `nats` reports healthy (healthcheck passes 3 times).
- **`restart: on-failure`** — If echo-consumer crashes (exit code != 0), restart it; useful for transient NATS unavailability.
- **`volumes`** — mounts local `sdk-py` into container so editable installs work; changes to SDK source reflect without rebuild.

**Startup Order Guarantee:**

Compose guarantees: nats is healthy → echo-consumer starts → attempts to connect to `nats://nats:4222`.

**Gotcha:** If nats goes down after startup, `restart: on-failure` does *not* wait for nats to be healthy again. The container will restart but may fail to connect. **Mitigation:** Add connection retry + backoff in the echo consumer:

```python
async def connect_with_retry(url, max_retries=10):
    for attempt in range(max_retries):
        try:
            return await nats.connect(url)
        except Exception as e:
            if attempt == max_retries - 1:
                raise
            await asyncio.sleep(2 ** attempt)  # exponential backoff
```

### Alignment with Plan

✓ Plan specifies `depends_on: { nats: { condition: service_healthy } }` and `restart: on-failure`.

✓ Healthcheck command should use NATS CLI or HTTP endpoint to verify readiness.

---

## Question 10: Consumer Durable Creation Idempotency

### Research

**Idempotent Creation:**

When the echo consumer starts, it calls:
```python
consumer = await js.add_consumer(
    stream="MESSAGES_INBOUND",
    config=ConsumerConfig(
        durable_name="ai-consumer",
        ack_policy="explicit",
        max_ack_pending=1,
        ack_wait=30,
        max_deliver=5,
    ),
)
```

If `ai-consumer` already exists with the **exact same config**, this call succeeds (idempotent). If it exists with a *different* config (e.g., `max_ack_pending=5`), the call **fails with an error** unless the new config is compatible (e.g., increasing ack_wait is allowed).

**Config Drift Handling:**

NATS does not automatically reconcile configs. If a deploy changes `max_ack_pending=1` → `max_ack_pending=10`, the old consumer stays at `=1`. The new deploy's `add_consumer()` call will error.

**Mitigation for P4:**

1. **Durable name never changes** — `ai-consumer` is fixed.
2. **Config changes are rare** — during dev, manually delete the durable if config changes:
   ```bash
   nats consumer del MESSAGES_INBOUND ai-consumer --force
   ```
3. **For production (P7+)** — version the durable name or use a reconciliation operator (e.g., NATS Operator) to manage config drift.

**Acceptance Criteria Check:**

Plan says "Replay same inbound 5× → 5 outbound `SendCommand`s (each has a fresh `id`)." This is about **idempotency of message processing**, not consumer creation. Each replay results in a fresh publish to MESSAGES_OUTBOUND; the consumer itself is stable.

### Alignment with Plan

✓ Plan accepts that durable config is fixed for P4.

**Open for P7:** Deployment reconciliation strategy (rolling update, config versioning, manual drift resolution).

---

## Question 11: Backpressure & Pending Count Monitoring

### Research

**Backpressure Mechanics:**

When `MaxAckPending=1` consumer can't keep up:
1. Server sends message 1 to consumer.
2. Handler takes 5s to respond (external service latency).
3. Handler sends `ack()`.
4. Server sends message 2.

If handlers average 10s per message, the server will have pending=1 most of the time (good — ordering preserved). If handler crashes, pending=1 for 30s (ack_wait timeout), then redelivery.

**Pending Count Growth Indicators:**

- **Healthy:** pending=0–1, stable.
- **Slow consumer:** pending grows to 5+, acks arrive >5s apart, CPU/memory on consumer climbing.
- **Consumer crash:** pending=1 for 30s (ack_wait), then redelivery attempt; loop repeats if crash persists.

**Alerting Thresholds:**

For echo-consumer reference impl:
- **Alert if pending > 10** for >30s (indicates handler is blocking).
- **Alert if redelivery count > 3** (message failing repeatedly).
- **Alert if consumer disconnects** (lost connection to NATS).

**Metrics to Export:**

```python
# via sdk-py
consumer_info = await js.consumer_info(stream, durable)
pending_count = consumer_info.num_pending
delivered_count = consumer_info.delivered.consumer_seq
redelivered_count = consumer_info.num_redelivered
```

These should feed Prometheus (via `mio_consumer_*` metrics from P2 SDK).

### Alignment with Plan

✓ Plan master says "Per-workspace rate-limit memory growth: TTL eviction on workspace buckets; cap total bucket count; metric on bucket count." This is gateway-side, not consumer-side.

**Recommendation for P4:** Add basic logging:
```python
if consumer_info.num_pending > 5:
    logger.warning(f"consumer pending={consumer_info.num_pending}, redelivered={consumer_info.num_redelivered}")
```

---

## Question 12: Echo-as-AI-Stand-In Conventions

### Research

**What the Echo Sets:**

1. **Subject Grammar** — `mio.outbound.<channel_type>.<account_id>.<conversation_id>[.<message_id>]`. The real AI must use the same grammar.
2. **`SendCommand` Fields:**
   - `tenant_id, account_id, channel_type, conversation_id` — preserved from inbound (required by P5 sender).
   - `id` — fresh ULID (each echo is a new message).
   - `text` — payload; echo sets `f"echo: {msg.text}"`.
   - `thread_root_message_id` — fallback to `source_message_id` for non-threaded.
   - `attributes["replied_to"]` — echo sets this to the inbound message ID for traceability.
   - `edit_of_message_id`, `edit_of_external_id` — empty for echo (fresh send, not edit).

3. **Durable Consumer Name** — `ai-consumer`. Production AI reuses this name; the echo-consumer is the POC stand-in.

4. **Error Handling** — echo calls `nak(delay=5)` on exception; real AI should follow the same pattern.

**What the Production AI Must Preserve:**

- All four-tier fields must be copied from inbound to outbound.
- `thread_root_message_id` must be respected (no overwriting with null).
- `attributes` is extensible — real AI can add `model="gpt-4"` or `latency_ms=500` without breaking the echo stand-in or P5 sender.
- `edit_of_*` fields enable update semantics (P5+ feature).

### Alignment with Plan

✓ Master plan §Design strategy says "Idempotent address is `(account_id, source_message_id)`, never `(channel_type, source_message_id)`." Echo respects this.

✓ Plan defers `MessageRelation` (edit/reaction/reply) to P5; echo has no relation semantics.

**Unresolved (P5):** When AI receives a thread reply in MESSAGES_INBOUND, should it:
a) Always reply in the same thread (use `thread_root_message_id`)?
b) Start a new top-level thread (set `thread_root_message_id` to the human's message)?
c) Context-dependent (real AI logic)?

For echo, both work identically (echo preserves the thread). **P5 must clarify before real AI lands.**

---

## Trade-Off Matrix: Key Decisions

| Decision | Option A | Option B | Chosen | Rationale |
|----------|----------|----------|--------|-----------|
| **Pull API pattern** | `fetch()` loop | `async for` iteration | fetch() + explicit timeout | Explicit shutdown handling; fetch timeout ensures signal checks every 5s. |
| **Signal handler registration** | `add_signal_handler()` in main | `add_signal_handler()` in handle | in main | Handler must be registered before entering long-running work; in main is idiomatic. |
| **`MaxAckPending` scaling** | Start high (10), reduce | Start low (1), graduate | Start low (1), graduate | Per-conversation ordering is the guarantee; scale when load test forces it (likely never at reference scale). |
| **Schema rejection** | Reject in handler | Reject in SDK | in SDK | SDK is the contract layer; handler should never see invalid messages. |
| **Thread fallback** | `thread_root_message_id or source_message_id` | Always use `thread_root_message_id` | fallback | Handles non-threaded DMs; verified in P5. |
| **Python version** | 3.11 | 3.12 | 3.12 | Current stable; better perf + typing. |
| **Dockerfile strategy** | Single-stage slim | Multi-stage distroless | Single-stage slim | P4 is reference; distroless deferred to P7 production build. |
| **Compose health check** | TCP port check | Custom script | Custom script | Verify NATS API is ready, not just port listening. |
| **Durable consumer versioning** | Version in durable name | Manual reconciliation | Manual reconciliation | Reference scale doesn't warrant operator; P7 production can add versioning. |

---

## Risk Assessment

| Risk | Probability | Impact | Mitigation |
|------|-------------|--------|-----------|
| **Signal handler not registered in time** | Low | Consumer doesn't shut down cleanly; connection leak. | Register in main() before any await; verify with signal.alarm() test. |
| **Async iteration hangs on shutdown** | Medium | Requires docker kill -9; manual intervention. | Use fetch() with timeout; ensures signal checks every 5s. |
| **Schema-version mismatch reaches handler** | Low | Handler crashes or produces invalid output. | SDK Verify is P2 responsibility; add integration test to confirm. |
| **Thread fallback breaks Cliq** | Medium | Echo reply appears in wrong conversation. | P5 acceptance test: post to Cliq DM, verify reply is threaded. |
| **Consumer config drift on deploy** | Low (for ref) | New deploy fails; manual intervention. | Document: delete durable if config changes. P7 adds operator. |
| **Pending count growth under burst inbound** | Medium | Backpressure; slow response; alerting fires. | Smoke test with 100-msg burst; verify pending drops within 5s. |
| **Docker compose doesn't wait for healthy** | Low | Echo container crashes on startup, retries. | Healthcheck must verify NATS API (not just port); add logging. |
| **Python package import path issues** | Low | `import mio` fails; SDK not found. | Use `-e` editable install in docker-compose volumes. |

---

## Alignment with Master Plan

✓ **Four-tier scope preserved** — all `SendCommand` fields carry tenant/account/channel_type/conversation.

✓ **Durable consumer name fixed** — `ai-consumer` is locked; real AI reuses it.

✓ **MaxAckPending=1 documented** — graduation path to sharding at scale is in master plan §Risks.

✓ **Schema-version rejection** — SDK (P2) responsibility; echo handler is simple passthrough.

✓ **End-to-end loop** — webhook → gateway → MESSAGES_INBOUND → echo → MESSAGES_OUTBOUND confirmed as path.

✓ **No protocol breaking changes** — echo uses the same `Message` and `SendCommand` as real AI will.

---

## Recommendations

### For P4 Implementation

1. **Use `js.pull_subscribe()` + `fetch(batch=1, timeout=5.0)` loop**, not `async for`. This gives explicit shutdown control and avoids indefinite blocking.

2. **Register signal handlers in main() before entering the consumer loop.** Verify with a simple test: send SIGINT, confirm graceful shutdown within 5s.

3. **Implement `nak(delay=5)` on exception.** Log the message ID, conversation ID, and error. Do *not* call `term()` unless you've confirmed the message is unrecoverable (e.g., schema v2).

4. **Add consumer info logging every 30s:**
   ```python
   async def log_consumer_info():
       while not stop.is_set():
           info = await js.consumer_info(stream, durable)
           logger.info(f"pending={info.num_pending}, delivered={info.delivered.consumer_seq}")
           await asyncio.sleep(30)
   ```

5. **Use `python-ulid` library for fresh message IDs.** Matches P2 SDK convention.

6. **Docker: use `python:3.12-slim`, `-e` editable installs, explicit `pip install --no-cache-dir`.**

7. **Docker-compose: healthcheck for NATS, `depends_on: { condition: service_healthy }`, `restart: on-failure`.**

8. **Add acceptance criteria:** Post 100 messages in 1s burst; verify pending count peaks <50 and drops to 0 within 5s. Killing the consumer mid-flight should cause redelivery within 40s.

### For P5 (Outbound) Considerations

- Clarify thread routing: always reply in thread, or context-dependent?
- Verify thread fallback works with Cliq (test posting echo to a DM and confirming it appears threaded).

---

## Unresolved Questions

1. **Should the echo consumer metrics (pending, redelivered counts) feed into alerting, or is logging sufficient?** (Deferred to P7 observability setup.)

2. **Does Cliq support per-message threading like Slack, or does it use a different thread identifier (e.g., conversation ID)?** (Deferred to P5 Cliq outbound integration; impacts thread fallback logic.)

3. **What is the policy for poison-pill messages** (e.g., a message from a deleted account)? **Nak or term?** (Deferred to P5+ when real error cases emerge.)

4. **Should `max_deliver=5` be tunable per consumer, or fixed globally?** (Reference impl uses fixed; production (P7) can parameterize.)

5. **How does the operator (P7+) reconcile consumer config drift without manual intervention?** (NATS Operator or custom reconciliation loop — out of P4 scope.)

---

## Sources

- [NATS JetStream Documentation](https://docs.nats.io/nats-concepts/jetstream)
- [NATS Consumer Details](https://docs.nats.io/nats-concepts/jetstream/consumers)
- [Consumer Details (NATS Docs)](https://docs.nats.io/using-nats/developer/develop_jetstream/consumers)
- [nats-py GitHub Repository](https://github.com/nats-io/nats.py)
- [nats.py Modules Documentation](https://docs.nats.io/using-nats/nats-tools/nats_cli/)
- [Graceful Shutdowns with asyncio](https://roguelynn.com/words/asyncio-graceful-shutdowns/)
- [Asyncio Signal Handling Guide](https://superfastpython.com/asyncio-control-c-sigint/)
- [Docker Compose Health Checks](https://last9.io/blog/docker-compose-health-checks/)
- [Docker Compose depends_on with service_healthy](https://oneuptime.com/blog/post/2026-01-16-docker-compose-depends-on-healthcheck/view)
- [Subject Mapping and Partitioning in NATS](https://docs.nats.io/nats-concepts/subject_mapping)
- [Slow Consumers in NATS](https://docs.nats.io/running-a-nats-service/nats_admin/slow_consumers)
- [Multi-Stage Docker Builds for Python](https://pythonspeed.com/articles/multi-stage-docker-python/)
- [Using uv with Docker](https://docs.astral.sh/uv/guides/integration/docker/)
- [PEP 660 – Editable Installs](https://peps.python.org/pep-0660/)
- [Slack Thread API Behavior](https://sean-rennie.medium.com/programatic-message-threading-with-your-slack-bot-688d9d227842/)
- [Slack conversations.replies Method](https://api.slack.com/methods/conversations.replies)

---

**Report Generated:** 2026-05-08  
**Next Phase:** P5 (Outbound path → Cliq)
