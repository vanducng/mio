---
title: "P2 Research Report: SDK Design (Go + Python) for NATS JetStream"
date: 2026-05-08
phase: 2
status: complete
---

# P2 Research Report: SDK Design for NATS JetStream Client Libraries

## Executive Summary

Phase P2 builds thin NATS JetStream wrappers in Go and Python that enforce MIO conventions: idempotency via `Nats-Msg-Id` headers, OTel trace propagation, bounded-cardinality Prometheus metrics, schema-version verification, and subject-builder safety. Research validates:

1. **Use the new `jetstream` package in `nats-io/nats.go`** (v2 API, recommended by maintainers) for Go, with pull consumers as the primary pattern.
2. **Use `nats-py` (native async)** for Python; no sync option needed. Async-first matches the AI consumer workload.
3. **Idempotency via `Nats-Msg-Id` dedup window** is production-ready; 2-minute default window covers inbound redeliveries. Namespace by `account_id` to isolate tenants.
4. **W3C `traceparent` header over NATS headers** is safe and supported; ASCII constraints allow standard OTel propagators.
5. **Prometheus cardinality discipline**: `channel_type`, `direction`, `outcome` are safe labels. **Reject** `account_id`, `tenant_id`, any UUID/ID as labels.
6. **Pull consumer ergonomics**: return `<-chan Delivery` in Go (caller-owned lifecycle); use async iterator (`async for`) in Python; `MaxAckPending=1` enforced for ordering.
7. **Schema-version verification on publish only**; reject mismatches. Consume-side skips validation (asymmetry intentional: publisher is authoritative).
8. **Subject token safety**: reject `.` in tokens, validate `channel_type` against registry at publish time.
9. **API surface parity Go ↔ Python**: keyword argument patterns, naming convention (snake_case Python, PascalCase Go), options builders.
10. **Codegen from `proto/channels.yaml`**: `tools/genchanneltypes/` generates Go map; simple enough to regenerate on every `make proto-gen`.
11. **Integration testing**: use `docker-compose` + ephemeral NATS (existing `playground/nats/`); `go test -tags=integration` and `pytest -m integration`.
12. **Python packaging**: **recommend `uv` for this project** (fast, low-config, single-tool replacement). Mirror with `poetry` if team policy requires.

---

## Q1: NATS JetStream Go Client — `nats-io/nats.go` New `jetstream` Package vs Legacy

### Context

`nats-io/nats.go` has two JetStream APIs:
- **Legacy**: `JetStream`, `SubscribeSync`, `PullSubscribe` (in `nats` package).
- **New v2**: `jetstream` sub-package (recommended as of 2024).

### Trade-off Matrix

| Dimension | Legacy API | New `jetstream` Package |
|---|---|---|
| **Maturity** | Stable but dated (2018–2022) | GA since 2024; actively developed |
| **Pull Consumer UX** | `Fetch()` returns `[]*Msg` array; call loops; cumbersome | `Consume()` returns context; `Fetch()` iterator; clean |
| **Push Consumer UX** | `Subscribe()` with callback; simpler | Discouraged for scalability; pull preferred |
| **Resource Efficiency** | Less optimal; less control over flow | Optimized for production workloads |
| **Documentation** | Sparse; buried in blog posts | Official NATS docs, examples on natsbyexample.com |
| **Breaking Changes** | Low; unlikely to change | Under active development; watch for semver bumps |
| **Recommendation for MIO** | ❌ Avoid for new projects | ✅ Use for P2 |

### Sources & Justification

- [NATS `jetstream` package on pkg.go.dev](https://pkg.go.dev/github.com/nats-io/nats.go/jetstream) — Official reference; explicitly states "recommended for new code."
- [NATS `jetstream` README](https://github.com/nats-io/nats.go/blob/main/jetstream/README.md) — Clarity on pull vs push, comparison table.
- [NATS docs on Consumers](https://docs.nats.io/nats-concepts/jetstream/consumers) — Architecture-level guidance; recommends pull for scalability.

### Adoption Risk

**Low.** The `jetstream` package is stable (nats-server 2.9.0+, already in GKE default builds). No major breaking changes expected within 12 months. Testcontainers integration exists for CI.

### Decision

**Use the new `jetstream` package.** It is the official recommendation, pull consumers are the correct pattern for MIO (ordered, backpressure-aware), and the API is cleaner. The legacy API is maintained but not the focus of future development.

---

## Q2: NATS JetStream Python Client — `nats-py` (Async-Only)

### Context

Python NATS clients:
- **`nats-py`** (formerly `asyncio-nats-client`): async/await, actively maintained, JetStream support.
- **`asyncio-nats-client`**: deprecated alias; don't use directly.
- **`nats-core`**: new (2025), lean, no deps, Python 3.13+ only — too young for MIO.

### Trade-off Matrix

| Dimension | `nats-py` | `nats-core` |
|---|---|---|
| **Sync API** | ❌ None; async-only | ❌ None; async-only |
| **JetStream Support** | ✅ Full | ⚠️ In development |
| **Python Version** | 3.7+ | 3.13+ only |
| **Maintenance Cadence** | Active (monthly releases) | Alpha / emerging |
| **Documentation** | Comprehensive; nats-io.github.io | Minimal; README only |
| **Production Readiness** | ✅ Yes | ❌ No (as of May 2026) |

### Sources & Justification

- [nats-py on PyPI](https://pypi.org/project/nats-py/) — Official package; 10k+ weekly downloads.
- [nats-py v2.0.0 release notes](https://nats-io.github.io/nats.py/releases/v2.0.0.html) — Stability landmarks.
- [nats-py documentation](https://nats-io.github.io/nats.py/) — Complete API surface and examples.
- [OneUptime NATS Python guide (Feb 2026)](https://oneuptime.com/blog/post/2026-02-02-nats-python/view) — Confirms nats-py as the production choice.

### Async-Only Design

MIO's AI consumer is already async (LangGraph + asyncio). **Async-only is a feature, not a limitation.** The SDK does not expose a sync API; users import `asyncio` and use the async client directly. Document this explicitly in the README.

### Adoption Risk

**Very low.** `nats-py` is the de-facto standard for Python NATS. Breaking changes are rare (only at major semver bumps, e.g., v1→v2). Dependency on `asyncio` is stable.

### Decision

**Use `nats-py` (async-only).** No sync fallback. Document the async requirement in the SDK. If a future consumer needs sync, spawn a thread pool; that's a consumer problem, not an SDK problem (KISS).

---

## Q3: Idempotency via `Nats-Msg-Id` — Dedup Window, Namespacing, Edge Cases

### Context

NATS JetStream deduplicates messages within a sliding `duplicate_window`. The plan specifies `inb:<account_id>:<source_message_id>` for inbound and `out:<send_command_id>` for outbound.

### Dedup Window Semantics

| Property | Behavior |
|---|---|
| **Default Duration** | 2 minutes (120 seconds) |
| **Scope** | Per-stream (not per-subject) |
| **Retention** | Sliding window; oldest entries evicted as new ones enter |
| **Client Responsibility** | Set `Nats-Msg-Id` header on publish; SDK must do this |
| **Server Behavior** | If ID matches an entry in the window, message is silently dropped (return `ErrDuplicateID` or dup detection ACK) |

### Namespacing Strategy

The plan's `inb:<account_id>:<source_message_id>` is correct because:

1. **Isolation**: Two tenants with the same `source_message_id` (e.g., both from Slack workspace with ID "12345") won't collide if they use different `account_id`s (UUIDs).
2. **Readability**: The prefix (`inb`, `out`) is visible in logs and NATS admin tools.
3. **Debuggability**: Operators can grep the message ID from a channel webhook and find it in JetStream.

**Outbound**: `out:<send_command_id>` (ULID is globally unique; no account isolation needed).

### Edge Cases at Window Boundary

**Scenario**: Gateway publishes at T=119s, dedup entry expires at T=120s. Second publish at T=121s.

**Result**: Dedup window has already evicted the first ID. Second publish succeeds, creating a duplicate in the stream.

**Mitigation**: This is **by design**. The 2-minute window covers typical retry backoff (exponential, usually <30s). For safety beyond the window, rely on the Postgres `(account_id, source_message_id)` unique constraint (defense in depth, as documented in `system-architecture.md` §7).

**For MIO**: 2-minute window is sufficient because:
- Cliq webhook retries within ~30s.
- Gateway acks the channel before publishing to JetStream (order: upsert DB → publish → ack channel).
- If the DB upsert succeeded but JetStream publish failed, the channel will retry; the DB constraint catches it.

### Sources & Justification

- [NATS JetStream Model Deep Dive](https://docs.nats.io/using-nats/developer/develop_jetstream/model_deep_dive) — Dedup window mechanics.
- [NATS JetStream Playbook (Medium, Nikulsinh Rajput)](https://medium.com/@hadiyolworld007/nats-jetstream-playbook-exactly-once-minus-the-bloat-02fd9d5a051c) — Practical "exactly-once" patterns.
- [NATS Blog: Infinite Message Deduplication](https://nats.io/blog/new-per-subject-discard-policy/) — Per-subject dedup advances (future; not in 2.9).

### Decision

**Implement the plan's namespacing as-is.** Add a comment in the SDK code explaining the window edge case and the Postgres fallback. Test with a 2-minute replay scenario locally (see Q11).

---

## Q4: OTel Propagation via NATS Headers — W3C `traceparent`, Span Kinds, Context Extraction

### Context

OpenTelemetry trace context flows: channel webhook → gateway (publisher) → NATS message header → AI consumer (subscriber). W3C `traceparent` header is the standard format.

### W3C `traceparent` Header

**Format**: `traceparent: "00-{trace_id}-{span_id}-{flags}"`

Example: `traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"`

**Character Set**: Hex digits only (0–9, a–f); always valid ASCII. ✅ **Safe for NATS headers** (ASCII 48–57, 97–102).

### NATS Header Constraints

| Property | Constraint | Impact |
|---|---|---|
| **Field Name** | Printable ASCII (33–126), no colons | `traceparent` is safe |
| **Field Value** | ASCII except CR, LF (can include tab) | W3C format is safe |
| **Multiple Values** | One field per header (NATS semantics) | OTel SDK handles this; SDK sets one `traceparent` |

### Span Kind in MIO

**Publisher (gateway, AI consumer on outbound)**: `PRODUCER` span.
- Starts when `PublishInbound()` or `PublishOutbound()` is called.
- Ends when JetStream returns PubAck.
- Attributes: `channel_type`, `subject`, `message.id`.

**Subscriber (AI consumer on inbound, sender-pool on outbound)**: `CONSUMER` span.
- Starts when message is fetched.
- Context extracted from `traceparent` header; spans are linked, not parent–child (async model).
- Ends when Ack or Nak is sent.

### Context Extraction on Consume Side

OTel SDK provides a propagator (e.g., `TraceContextPropagator` in Go, `TraceContextPropagator` in Python). On consume:

```go
// Go
ctx := nc.Context()
span := trace.SpanFromContext(ctx)  // nil if no traceparent

// Extract from headers
propagator := otel.GetTextMapPropagator()
newCtx := propagator.Extract(ctx, otel.NewCompositeCarrier(msg.Headers))
span := trace.SpanFromContext(newCtx)  // Populated from traceparent
```

```python
# Python
propagator = TraceContextPropagator()
ctx = propagator.extract(msg.headers)
span = trace.get_current_span()  # Use ctx from extract
```

### Sources & Justification

- [OpenTelemetry Context Propagation spec](https://opentelemetry.io/docs/concepts/context-propagation/) — Defines propagators and carriers.
- [OTel Messaging Semantic Conventions](https://opentelemetry.io/docs/specs/semconv/messaging/messaging-spans/) — Span kind PRODUCER/CONSUMER, attributes for messaging.
- [W3C TraceContext format](https://w3.org/TR/trace-context/) — Standard; no NATS-specific encoding needed.
- [OTel Trace Context Propagation with Message Brokers (Tracetest Blog)](https://tracetest.io/blog/opentelemetry-trace-context-propagation-with-message-brokers-and-go) — Practical Go example.

### Adoption Risk

**Very low.** W3C TraceContext is a W3C standard; NATS headers support ASCII values. OTel SDKs have built-in propagators. No custom encoding needed.

### Decision

**Inject `traceparent` header on publish; extract on consume.** Use OTel's standard propagators (no custom logic). Document span kinds. Test round-trip tracing in integration tests (Q11).

---

## Q5: Prometheus Metric Cardinality Discipline — Safe vs Unsafe Labels

### Context

High-cardinality labels (unbounded unique values) cause Prometheus memory bloat and query slowdown. MIO must avoid this.

### Safe vs Unsafe Labels

| Label | Cardinality | Status | Rationale |
|---|---|---|---|
| `channel_type` | ~5–10 max | ✅ Safe | Finite registry (`zoho_cliq`, `slack`, `telegram`, etc.) |
| `direction` | 2 (`inbound`, `outbound`) | ✅ Safe | Fixed set |
| `outcome` | ~5 (`success`, `error`, `dedup`, `timeout`, `invalid`) | ✅ Safe | Enumerable |
| `account_id` | Unbounded (one per customer) | ❌ **BOMB** | Explodes with scale; move to trace spans |
| `tenant_id` | Unbounded | ❌ **BOMB** | Same as account_id |
| `conversation_id` | Unbounded | ❌ **BOMB** | Millions per account |
| `message_id` | Unbounded | ❌ **BOMB** | Never use |
| `request_id` | Unbounded | ❌ **BOMB** | Never use |

### Metric Design for MIO

**Publish metrics** (per publish attempt):
```
mio_sdk_publish_total{channel_type, direction, outcome}
mio_sdk_publish_latency_seconds{channel_type, direction} (histogram)
```

**Consume metrics** (per consume/ack):
```
mio_sdk_consume_total{channel_type, direction, outcome}
mio_sdk_consume_latency_seconds{channel_type, direction} (histogram)
```

**Histogram buckets** for latency: `[0.001, 0.01, 0.1, 1, 10]` seconds (publish/consume typically <100ms; 10s is an outlier ceiling).

### Alternative for Account-Level Observability

If operators need per-account metrics:
- **Option A** (correct): Emit to a **span attribute** (unbounded) or log field, not a Prometheus label.
- **Option B** (at scale): Use a separate time-series database (e.g., BigQuery, Datadog) for high-cardinality dimensions; Prometheus for aggregate counts only.
- **Option C** (if unavoidable): Cap label cardinality with a relabeling rule in Prometheus scrape config; map top 50 accounts to name, rest to `_other`.

**For P2**: Do not add `account_id` labels. Document the pattern; if telemetry demands it, implement at P7 (deployment config, not SDK).

### Sources & Justification

- [Prometheus Best Practices: Naming](https://prometheus.io/docs/practices/naming/) — Label design guidance.
- [How to Manage High-Cardinality Metrics (Last9 Blog)](https://last9.io/blog/how-to-manage-high-cardinality-metrics-in-prometheus/) — Detailed cardinality-bomb scenarios and fixes.
- [Prometheus Cardinality Bomb (OpenObserve Blog)](https://openobserve.ai/blog/prometheus-data-cardinality/) — Real-world case studies; rule of thumb: "avoid labels with 10k+ unique values."
- [CNCF: Prometheus Labels Best Practices](https://www.cncf.io/blog/2025/07/22/prometheus-labels-understanding-and-best-practices/) — Latest (2025) standards.

### Adoption Risk

**Very low.** This is a design choice, not a library dependency. Enforce via code review.

### Decision

**Use `channel_type`, `direction`, `outcome` only.** Reject any PR adding account/tenant/ID labels. Document in CONTRIBUTING.md and code comments.

---

## Q6: Pull Consumer Ergonomics — Go `<-chan Delivery` vs Python Async Iterator

### Context

Two patterns:
1. **Go**: Return `<-chan Delivery`; caller controls lifecycle (fetch, ack, nak, loop).
2. **Python**: Async iterator (`async for msg in consumer.messages`); cleaner but less flexible.

### Trade-off Matrix

| Aspect | Go `<-chan` | Python `async for` |
|---|---|---|
| **Control** | Caller owns fetch loop; explicit Ack/Nak | Iterator abstracts loop; implicit iteration |
| **Backpressure** | Built-in (channel buffer size = MaxAckPending) | Implicit in async iteration |
| **Error Handling** | Try-catch around channel read | Try-catch around async iteration |
| **Cancellation** | ctx-aware; caller passes context | ctx-aware; caller cancels context |
| **Resource Cleanup** | Explicit Unsubscribe() | Iterator cleanup on context cancel |
| **Ergonomics** | Explicit (more boilerplate) | Implicit (more Pythonic) |

### Design Decision

**Go**: Return `<-chan *jetstream.Delivery` (from the new jetstream package).
- Caller signature: `for msg := range deliveryChan { msg.Ack(); ... }`
- Lifetime: Caller owns the loop; SDK provides the channel.

**Python**: Return `AsyncIterator[nats.aio.subscription.Msg]` (from nats-py pull consumer).
- Caller signature: `async for msg in consumer.messages: await msg.ack(); ...`
- Lifetime: nats-py manages the iterator; caller iterates.

### MaxAckPending, AckWait, MaxDeliver, Nak(WithDelay)

| Parameter | MIO Default | Rationale |
|---|---|---|
| **MaxAckPending** | 1 | Per-thread ordering (global, for now) |
| **AckWait** | 30s (NATS default) | Sufficient for AI consumer processing (2–30s) |
| **MaxDeliver** | 5 | Fail after 5 retries; move to DLQ (future P5) |
| **NakWithDelay** | 1s (initial), exponential backoff | Retry with backoff; e.g., `Nak(time.Second, nats.BackoffPolicy(...))` |

SDK defaults should be **baked in**; callers pass options to override (e.g., `WithMaxAckPending(1)`, `WithAckWait(60s)`).

### Sources & Justification

- [NATS Consumer Details](https://docs.nats.io/nats-concepts/jetstream/consumers) — MaxAckPending, AckWait semantics.
- [NATS by Example: Pull Consumers (Go)](https://natsbyexample.com/examples/jetstream/pull-consumer/go) — Idiomatic channel-based iteration.
- [nats-py Async Iterator Protocol](https://nats-io.github.io/nats.py/) — `messages` async iterator.
- [How to Build NATS Consumers (OneUptime, Feb 2026)](https://oneuptime.com/blog/post/2026-02-02-nats-consumers/view) — Latest patterns.

### Adoption Risk

**Very low.** Both patterns are standard in their respective languages. No migration cost.

### Decision

**Go**: Return `<-chan *jetstream.Delivery`; caller owns the loop.
**Python**: Use `nats-py` pull consumer's `async for msg in consumer.messages`; wrap in SDK method returning async iterator.

Both enforce `MaxAckPending=1` by default (overridable). Document the lifecycle: when is the consumer closed? (Answer: caller calls `sub.unsubscribe()` in Go; context cancellation in Python.)

---

## Q7: Schema-Version Verification — When, Reject vs Warn, Asymmetry

### Context

The plan specifies schema-version checks on messages. MIO wire format is `mio.v1.Message` (proto3). Questions:
1. Verify on publish, consume, or both?
2. Reject (hard error) or warn (log, continue)?
3. Asymmetry: publish validates, consume doesn't. Why?

### Decision: Publish-Only Verification with Hard Reject

**Publish-side**:
- SDK checks `schema_version == 1`; rejects if mismatch or missing.
- Also validates: `tenant_id`, `account_id`, `channel_type`, `conversation_id` all non-empty.
- Hard reject: raise `ValueError` / `error` (caller must fix).

**Consume-side**:
- No verification. Silently accepts any `schema_version`.
- Rationale: Publisher is authoritative on schema. Consumers must be backward-compatible (e.g., v1 consumer reads v2 message if only new optional fields are added). If a v2 publisher has shipped, v1 consumers **must not crash**; they can warn in logs and handle gracefully.

### Asymmetry Rationale

In a decoupled system, **publishers upgrade first** (controlled), then **consumers follow** (decoupled). Schema evolution goes: v1 publisher → v2 publisher (with backward-compat) → v1 consumer (still works) → v2 consumer (added schema_version=2 handling).

If consumers rejected unknown schema versions, we'd have a **cascading blocker**: any publisher upgrade would break all consumers until they're redeployed.

**Exception**: If the message is structurally invalid (e.g., can't unmarshal the proto), both sides fail (protobuf deserialization error). That's OK; it's a data integrity issue, not a version issue.

### Verification Implementation

```go
// Go
func Verify(msg *pb.Message) error {
    if msg.SchemaVersion != 1 {
        return fmt.Errorf("unsupported schema version: %d", msg.SchemaVersion)
    }
    if msg.TenantId == "" || msg.AccountId == "" || msg.ChannelType == "" || msg.ConversationId == "" {
        return fmt.Errorf("missing required fields")
    }
    // Check channel_type against Known registry
    if !Known[msg.ChannelType] {
        return fmt.Errorf("unknown channel_type: %s", msg.ChannelType)
    }
    return nil
}

// Publish call
if err := Verify(msg); err != nil {
    return err  // Reject
}
```

```python
# Python
def verify(msg: pb.Message) -> None:
    if msg.schema_version != 1:
        raise ValueError(f"unsupported schema version: {msg.schema_version}")
    if not (msg.tenant_id and msg.account_id and msg.channel_type and msg.conversation_id):
        raise ValueError("missing required fields")
    if msg.channel_type not in KNOWN:
        raise ValueError(f"unknown channel_type: {msg.channel_type}")
```

### Sources & Justification

- [Protobuf Best Practices (2025)](https://protobuf.dev/best-practices/dos-donts/) — Schema evolution patterns; never remove fields, always add with default.
- [Protovalidate (bufbuild)](https://github.com/bufbuild/protovalidate) — Code-generated validation; not used here but pattern is similar.
- [Protocol Buffer Evolution (OneUptime, Jan 2026)](https://oneuptime.com/blog/post/2026-01-24-protocol-buffer-evolution/view) — Publisher-first upgrade strategy.

### Adoption Risk

**Very low.** No dependency on external validation libraries. Simple checks.

### Decision

**Publish-side: hard reject.** Consume-side: no check. Document the asymmetry in the SDK README.

---

## Q8: Subject Builder Safety — Token Validation, Registry Check, UUID/ULID Safety

### Context

Subject grammar: `mio.<direction>.<channel_type>.<account_id>.<conversation_id>[.<message_id>]`

Tokens are dot-separated. NATS subject matching relies on dots as wildcards; a token with an embedded dot breaks filters.

### Token Validation Rules

| Rule | Reason | Example |
|---|---|---|
| No `.` in any token | NATS subject matching breaks | ❌ `mio.inbound.slack.acct-123.conv.with.dots` |
| No empty tokens | Parser ambiguity | ❌ `mio.inbound..account` |
| UUIDs / ULIDs safe | Only hex + hyphens; no dots | ✅ `mio.inbound.slack.550e8400-e29b-41d4-a716-446655440000.conv-uuid` |
| `channel_type` from registry | Enforced at publish | ✅ `mio.inbound.zoho_cliq...` |

### Subject Builder Implementation

```go
// Go
func Inbound(channelType, accountID, conversationID string) (string, error) {
    if err := validateToken(channelType); err != nil {
        return "", fmt.Errorf("invalid channel_type: %w", err)
    }
    if !Known[channelType] {
        return "", fmt.Errorf("unknown channel_type: %s", channelType)
    }
    // ... validate accountID, conversationID for dots, empty
    return fmt.Sprintf("mio.inbound.%s.%s.%s", channelType, accountID, conversationID), nil
}

func validateToken(t string) error {
    if t == "" {
        return errors.New("token cannot be empty")
    }
    if strings.Contains(t, ".") {
        return errors.New("token cannot contain '.'")
    }
    return nil
}
```

```python
# Python
def inbound(channel_type: str, account_id: str, conversation_id: str) -> str:
    if not channel_type or "." in channel_type or channel_type not in KNOWN:
        raise ValueError(f"invalid or unknown channel_type: {channel_type}")
    if not account_id or "." in account_id:
        raise ValueError(f"invalid account_id")
    if not conversation_id or "." in conversation_id:
        raise ValueError(f"invalid conversation_id")
    return f"mio.inbound.{channel_type}.{account_id}.{conversation_id}"
```

### Registry Check at Build Time

The `Known` map is generated by `tools/genchanneltypes/` from `proto/channels.yaml`. Before publishing, the SDK checks if `channel_type` is in `Known`. If a new channel is added to the YAML but the SDK is not regenerated, publishes will fail. **This is intentional**: it forces SDK regeneration on channel registry changes (tight coupling via code-gen, which is OK for a convention enforcer).

### Sources & Justification

- [NATS Subject-Based Messaging](https://docs.nats.io/nats-concepts/subjects) — Dot semantics, wildcard rules.
- Plan's subject grammar (§5 of `system-architecture.md`) — Rationale for each dimension.

### Adoption Risk

**Very low.** Simple string validation; no external dependencies.

### Decision

**Implement token validators in both Go and Python.** Registry check happens at publish time (via `Known` map). Document in code that subjects are intentionally strict.

---

## Q9: API Surface Parity Go ↔ Python — Naming, Options Pattern

### Context

Gateway (Go) and AI consumer (Python) both use the SDK. They must have equivalent APIs to avoid cognitive load and reduce bugs.

### Naming Convention

| Language | Naming | Example |
|---|---|---|
| **Go** | PascalCase (public), camelCase (private) | `PublishInbound`, `ConsumeInbound`, `WithMaxAckPending` |
| **Python** | snake_case (public), \_snake_case (private) | `publish_inbound`, `consume_inbound`, `with_max_ack_pending` (if using builder) |

### Options Pattern

**Go**: Functional options (idiomatic).
```go
func New(url string, opts ...ClientOption) (*Client, error) { ... }

client, _ := sdk.New("nats://localhost:4222",
    sdk.WithName("gateway"),
    sdk.WithCreds("user", "pass"),
    sdk.WithTracerProvider(tp),
)
```

**Python**: Kwargs or builder; kwargs is simpler for Python.
```python
client = await Client.new(
    url="nats://localhost:4222",
    name="ai-consumer",
    creds=("user", "pass"),
    tracer_provider=tp,
)
```

Both should support the same logical options:
- `name` (for debugging, appears in NATS logs)
- `creds` (user/pass or NKey)
- `tracer_provider` (OTel SDK)
- `metrics_registry` (Go) / `metrics_namespace` (Python)
- `max_ack_pending` (default: 1)
- `ack_wait` (default: 30s)

### Method Signatures

| Method | Go | Python |
|---|---|---|
| **Publish inbound** | `PublishInbound(ctx, *Message) error` | `await publish_inbound(msg: Message) -> None` |
| **Consume inbound** | `ConsumeInbound(ctx, durable) (<-chan Delivery, error)` | `async def consume_inbound(durable: str) -> AsyncIterator[Message]` |
| **Metric name** | `mio_sdk_publish_total` | `mio_sdk_publish_total` (same; prometheus-client uses the same metric name) |

### Trade-off: Kwargs vs Builder in Python

**Option A (kwargs):**
```python
await client.publish_inbound(msg, context=ctx, timeout=5)
```
**Pros**: Pythonic, flexible, no boilerplate.
**Cons**: Harder to add new options later (ABI breaking).

**Option B (builder):**
```python
await client.publish_inbound(msg).with_timeout(5).execute()
```
**Pros**: Chainable, future-proof.
**Cons**: Verbose, not Pythonic.

**Recommendation**: Use **kwargs for P2**. If P3/P4 demands complex option chaining, refactor then.

### Sources & Justification

- [Go Functional Options Pattern](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis) — Idiomatic in Go.
- [Python API Design Best Practices (PEP 20)](https://www.python.org/dev/peps/pep-0020/) — "Explicit is better than implicit"; kwargs align with this.

### Adoption Risk

**Very low.** Naming conventions are language-standard. Options patterns are well-established in both languages.

### Decision

**Go**: Functional options pattern. **Python**: Kwargs on methods. Enforce parity via code review: side-by-side comparison of each public method before merging.

---

## Q10: Code Generation — `tools/genchanneltypes/` Design, Drift Detection

### Context

The `proto/channels.yaml` file is the source of truth for channel registry. The SDK needs a `Known` map (Go) / `KNOWN` dict (Python) that lists active channels. This must be generated and regenerated on every `make proto-gen`.

### Codegen Tool Design

**File**: `tools/genchanneltypes/main.go` (or equivalent in Python; Go is simpler for codegen).

**Input**: `proto/channels.yaml`
```yaml
channels:
  - slug: zoho_cliq
    name: "Zoho Cliq"
    status: active
  - slug: slack
    name: "Slack"
    status: active
  - slug: telegram
    name: "Telegram"
    status: active
  - slug: discord
    name: "Discord"
    status: inactive
```

**Output**: `sdk-go/channeltypes.go`
```go
package sdk

var Known = map[string]bool{
    "zoho_cliq": true,
    "slack":     true,
    "telegram":  true,
    // Note: discord is inactive; excluded
}
```

**Output**: `sdk-py/mio/channeltypes.py`
```python
KNOWN = {
    "zoho_cliq",
    "slack",
    "telegram",
}
```

### Regeneration Trigger

Add to `Makefile`:
```makefile
proto-gen: \
	protoc-go \
	protoc-py \
	genchanneltypes  # <— add this

genchanneltypes:
	cd tools/genchanneltypes && go run main.go ../../proto/channels.yaml ../../sdk-go/channeltypes.go ../../sdk-py/mio/channeltypes.py
```

### Drift Detection in CI

CI should verify that generated files are up-to-date:
```bash
# In GitHub Actions or local test
make proto-gen
git diff --exit-code sdk-go/channeltypes.go sdk-py/mio/channeltypes.py
```

If the diff is non-empty, the job fails. This forces developers to regenerate before commit.

### Complexity Assessment

**Complexity**: Low. The codegen tool is ~100 lines of Go (YAML parse → template → write). No external dependencies needed (Go std lib `encoding/yaml`, `text/template`).

**When to regenerate**: Always, as part of `make proto-gen`. Cheap operation.

### Sources & Justification

- Plan's P2 (Step 7): `channeltypes.go` generated from `proto/channels.yaml`.
- Buf's approach to plugin-based code generation (though MIO's tool is simpler).

### Adoption Risk

**Very low.** Standard template-based codegen. Failures are easy to debug (diff the output).

### Decision

**Implement `tools/genchanneltypes/main.go`.** Hook into `make proto-gen`. Add CI drift check. Document the process in CONTRIBUTING.md.

---

## Q11: Integration Testing — Ephemeral NATS, pytest-asyncio, go test -tags=integration

### Context

Integration tests must spin up a real NATS server and verify publish/consume round-trips. Playgrounds exist: `playground/nats/` (docker-compose).

### Go Integration Testing

**Setup**: Use existing `playground/nats/docker-compose.yml` or Testcontainers.

**Pattern** (recommended for P2):
```go
// sdk-go/client_test.go
// +build integration

func TestPublishConsumeRoundTrip(t *testing.T) {
    // Start NATS via docker-compose (in Makefile or GitHub Actions)
    // nc, _ := nats.Connect("nats://localhost:4222")
    // js, _ := nc.JetStream()
    
    // Test publish
    msg := &pb.Message{
        SchemaVersion: 1,
        TenantId:      "t1",
        AccountId:     "a1",
        ChannelType:   "zoho_cliq",
        ConversationId: "c1",
        // ... payload
    }
    
    err := client.PublishInbound(ctx, msg)
    if err != nil {
        t.Fatalf("publish failed: %v", err)
    }
    
    // Test consume
    durable := "test-durable"
    deliveryChan, _ := client.ConsumeInbound(ctx, durable)
    msg := <-deliveryChan
    msg.Ack()
    // Verify payload
}
```

**Run**: `make test` (all tests), `go test ./sdk-go -tags=integration` (only integration).

### Python Integration Testing

**Setup**: Same docker-compose or Testcontainers.

**Pattern**:
```python
# sdk-py/tests/test_client.py
import pytest
import asyncio

@pytest.mark.asyncio
async def test_publish_consume_round_trip():
    client = await Client.new("nats://localhost:4222")
    
    msg = Message(
        schema_version=1,
        tenant_id="t1",
        account_id="a1",
        channel_type="zoho_cliq",
        conversation_id="c1",
    )
    
    await client.publish_inbound(msg)
    
    async for received_msg in client.consume_inbound("test-durable"):
        await received_msg.ack()
        assert received_msg.account_id == "a1"
        break
```

**Run**: `pytest sdk-py/tests/ -v` (all), `pytest -m integration` (integration-tagged).

### Shared Test Scenarios

Both Go and Python should test:

1. **Idempotency**: Publish same `(account_id, source_message_id)` twice within 2 min; verify only one message in stream.
2. **OTel propagation**: Publish with traceparent header; consume and extract; verify span ID matches.
3. **Schema validation**: Publish with `schema_version=2`; verify rejection.
4. **Missing fields**: Publish with empty `tenant_id`; verify rejection.
5. **Invalid channel_type**: Publish with `channel_type` not in `Known`; verify rejection.
6. **Ack/Nak flow**: Consume, ack; verify message is removed. Consume, nak; verify redelivery.
7. **Subject builder**: `Inbound("zoho_cliq", "a1", "c1")` → correct subject format.
8. **Metrics**: Verify `mio_sdk_publish_total` incremented; no `account_id` labels.

### Docker Compose Integration

Existing `playground/nats/docker-compose.yml` should be reused. Add to Makefile:
```makefile
test:
	docker-compose -f playground/nats/docker-compose.yml up -d
	go test ./... -tags=integration
	pytest sdk-py/tests/ -v
	docker-compose -f playground/nats/docker-compose.yml down
```

Or use GitHub Actions' `services` block for CI.

### Sources & Justification

- [NATS and Docker](https://docs.nats.io/running-a-nats-service/nats_docker) — Container setup.
- [NATS Docker Compose Blog](https://nats.io/blog/docker-compose-plus-nats/) — Real-world example.
- [Testcontainers for Go NATS](https://golang.testcontainers.org/modules/nats/) — Alternative to docker-compose.
- [pytest-asyncio docs](https://pytest-asyncio.readthedocs.io/) — Python async test runner.
- [NATS by Example](https://natsbyexample.com/) — Examples for all patterns.

### Adoption Risk

**Very low.** Docker is assumed available in dev environment. Testcontainers is optional (add if Dockerfile-based testing is needed).

### Decision

**Use docker-compose from `playground/nats/`.** Add 8 integration tests (listed above) across both SDKs. Run in CI on every commit. If a test fails, the SDK code cannot merge.

---

## Q12: Python Packaging — `uv` vs `poetry` vs `setuptools`

### Context

The project needs to choose a Python package manager for `sdk-py`. Three options:
1. **`uv`** (2024–2025): Fast, Rust-based, zero-config, replaces pip/pip-tools/pipx/poetry.
2. **`poetry`** (mature, 2018–2025): Complete ecosystem, lock files, publishing, publishing.
3. **`setuptools`** (legacy, 1990–2025): Bare-bones, requires `pip` + `build` + `twine`.

### Trade-off Matrix

| Dimension | `uv` | `poetry` | `setuptools` |
|---|---|---|---|
| **Install speed** | Subsecond | Seconds | Seconds |
| **Learning curve** | Flat (one tool) | Moderate | Steep (3+ tools) |
| **Lock file** | Yes (`uv.lock`) | Yes (`poetry.lock`) | Manual (`requirements.txt`) |
| **Publishing** | `uv publish` | `poetry publish` | `twine upload` |
| **Local path deps** | ✅ Supported | ✅ Supported | ⚠️ Requires `pip install -e` |
| **PYPI maturity** | Recent; actively developed | Stable; 7+ years | Ancient; minimal maintenance |
| **Team adoption** | Growing (2025) | Established | Legacy projects only |
| **Python version support** | 3.8+ (uv), 3.13+ (uv binary) | 3.7+ | 2.7+ |
| **Ecosystem integration** | Emerging | Widespread | Everywhere |
| **Recommendation** | ✅ **Best for new projects** | ✅ If team mandates | ❌ Only for legacy |

### Local Path Dependency (Key for MIO)

`examples/echo-consumer` will import `sdk-py` locally during development. This requires:

**uv**:
```toml
[project]
dependencies = ["mio-sdk @ file://../sdk-py"]
```

**poetry**:
```toml
[tool.poetry.dependencies]
mio-sdk = {path = "../sdk-py", develop = true}
```

**setuptools**:
```bash
pip install -e ../sdk-py
```

All three work. `uv` is cleanest (no extra `pip install -e` ceremony).

### Project Config File

**uv** (`pyproject.toml` only):
```toml
[project]
name = "mio-sdk"
version = "0.1.0"
dependencies = ["nats-py", "prometheus-client", "opentelemetry-api"]

[build-system]
requires = ["hatchling"]  # or uv_build
build-backend = "hatchling.build"

[tool.uv]
# Minimal config needed
```

**poetry** (`pyproject.toml` + `poetry.lock`):
```toml
[tool.poetry]
name = "mio-sdk"
version = "0.1.0"

[tool.poetry.dependencies]
python = "^3.8"
nats-py = "^2.0"
prometheus-client = "^0.20"
opentelemetry-api = "^1.0"

[build-system]
requires = ["poetry-core"]
build-backend = "poetry.core.masonry.api"
```

**setuptools** (`pyproject.toml` + `setup.py`):
```toml
[build-system]
requires = ["setuptools", "wheel"]
build-backend = "setuptools.build_meta"
```

### Recommendation for MIO

**Use `uv`.** Rationale:
1. **Fast CI**: uv's subsecond install speeds up GitHub Actions.
2. **Single tool**: Replace pip, venv, pip-tools, etc. Lower cognitive load.
3. **Modern**: Actively developed (2025+); future-proof for 5+ years.
4. **Local deps**: Works seamlessly with `examples/echo-consumer`.
5. **New project**: No legacy constraints; no reason to use `poetry` unless team mandates.

**Fallback**: If team has a strong `poetry` preference, use poetry. The SDK code doesn't change; only the build config differs.

### Sources & Justification

- [Python Build Backends in 2025 (Medium, Chris Evans)](https://medium.com/@dynamicy/python-build-backends-in-2025-what-to-use-and-why-uv-build-vs-hatchling-vs-poetry-core-94dd6b92248f) — Latest comparison.
- [Poetry vs UV (Medium, Hitoruna)](https://medium.com/@hitorunajp/poetry-vs-uv-which-python-package-manager-should-you-use-in-2025-4212cb5e0a14) — Detailed trade-offs.
- [GitHub astral-sh/uv](https://github.com/astral-sh/uv) — Official repo; active development.
- [Poetry documentation](https://python-poetry.org/) — Stable; 7+ years proven.

### Adoption Risk

**Very low for `uv`.** Mature (2025), but adoption is still growing. Switching from `poetry` to `uv` later is a one-time config change (no code changes).

### Decision

**Use `uv init` to scaffold `sdk-py/pyproject.toml`.** Pin deps to `prometheus-client`, `opentelemetry-api`, `opentelemetry-sdk`, `nats-py`. Support `uv` for local dev, accept `poetry` lock files in CI if team preference changes later.

---

## Alignment with P2 Plan

All 12 research conclusions align with the existing P2 plan:

1. ✅ New `jetstream` package (Go) — matches plan's Step 1.
2. ✅ `nats-py` async-only (Python) — matches plan's Step 1, implicitly async.
3. ✅ `Nats-Msg-Id` idempotency with namespace — matches plan §7 (defense-in-depth with Postgres).
4. ✅ OTel via headers — matches plan's integration tests (Step 8).
5. ✅ Cardinality discipline — matches plan's success criteria (no account_id labels).
6. ✅ Pull consumer ergonomics (`<-chan` Go, async iterator Python) — matches plan §6.
7. ✅ Schema-version checks on publish only — matches plan's Verify() step.
8. ✅ Subject builder safety — matches plan's Step 2.
9. ✅ API surface parity — matches plan's "mirror surface" goal.
10. ✅ Codegen from `proto/channels.yaml` — matches plan's Step 7 and the channel_type registry.
11. ✅ Integration testing against `playground/nats/` — matches plan's Step 8.
12. ✅ `uv` for Python packaging — supports rapid development (new project, no legacy constraints).

---

## Open Questions

1. **OTel span attributes**: Should the SDK set standard attributes like `messaging.system`, `messaging.destination`, `messaging.message_id` on spans? (Answer: yes, per OTel messaging conventions; add to P2 implementation checklist.)

2. **Metric histogram buckets for Go**: Should `prometheus/client_golang` use the same buckets as `prometheus-client` in Python? (Answer: yes, `[0.001, 0.01, 0.1, 1, 10]` for consistency.)

3. **MaxDeliver and dead-letter handling**: Should the SDK accept a `WithMaxDeliver()` option? If a message exceeds MaxDeliver, where does it go—NATS advisory topic or user callback? (Answer: expose as option; DLQ strategy deferred to P5.)

4. **Async context propagation in Python**: Should `ConsumeInbound()` accept a context parameter (like Go's `ctx`), or rely on `asyncio.create_task()` context inheritance? (Answer: accept optional `ctx` for clarity; Python's implicit async context is error-prone.)

5. **Backward compatibility for channel registry**: If a channel is removed from `proto/channels.yaml` (e.g., marking as `inactive`), should existing SDKs reject publishes to that channel? (Answer: yes; forces explicit migration when decommissioning a channel.)

---

## Summary Table: Decisions by Question

| Q | Topic | Decision | Risk | Citation |
|---|---|---|---|---|
| 1 | Go JetStream API | Use `jetstream` package (v2) | Very low | pkg.go.dev, natsbyexample.com |
| 2 | Python client | `nats-py` (async-only) | Very low | nats-py docs, PyPI |
| 3 | Idempotency | `Nats-Msg-Id` with `inb:acct:source_id` | Very low | NATS dedup docs, system-arch |
| 4 | OTel tracing | W3C `traceparent` header | Very low | OTel spec, NATS header constraints |
| 5 | Cardinality | Safe: `channel_type`, `direction`, `outcome`; reject IDs | Very low | Prometheus best practices |
| 6 | Pull consumers | Go: `<-chan Delivery`; Python: async iterator | Very low | NATS docs, lang standards |
| 7 | Schema checks | Publish-side hard reject; consume-side no-op | Very low | Protobuf evolution, system-arch |
| 8 | Subject safety | Validate tokens, check registry at publish | Very low | NATS subject rules, plan |
| 9 | API parity | Go: options pattern; Python: kwargs | Very low | Lang standards |
| 10 | Codegen | `tools/genchanneltypes/` from YAML, regen on `make proto-gen` | Very low | Plan's code-gen pattern |
| 11 | Integration tests | 8 scenarios via `docker-compose` + `playground/nats/` | Very low | NATS testing guides |
| 12 | Python packaging | **`uv`** (fast, zero-config, future-proof) | Very low | 2025 packaging landscape |

---

## Recommendations for Implementation

**Priority order for P2 implementation:**

1. **Scaffolding** (1h): `go mod init`, `uv init`, create directory structure.
2. **Subject builders + validation** (1h): `subjects.go`, `subjects.py`; test with known/bad inputs.
3. **Client struct + options** (1h): `client.go`, `client.py`; connection mgmt, lifecycle.
4. **Publish APIs** (2h): `publish_inbound.go`, `publish_outbound.go`, Python mirrors; idempotency, OTel headers.
5. **Consume APIs** (2h): `consume_inbound.go`, `consume_outbound.go`, Python mirrors; pull-fetch helpers.
6. **Metrics** (1h): `metrics.go`, `metrics.py`; register Prometheus collectors; test cardinality.
7. **Tracing** (1h): `tracing.go`, `tracing.py`; OTel propagator setup; extract/inject headers.
8. **Schema verification** (1h): `version.go`, `version.py`; `Verify()` function, channel_type registry.
9. **Codegen** (1h): `tools/genchanneltypes/main.go`; integrate into Makefile.
10. **Integration tests** (3h): 8 test scenarios; `docker-compose` setup; both SDKs.
11. **Documentation** (1h): README, CONTRIBUTING.md, inline code comments.

**Total estimate**: ~14–15h (fits within P2's "1d" budget, with margin for debugging).

---

## References

### NATS & JetStream

- [NATS `jetstream` package](https://pkg.go.dev/github.com/nats-io/nats.go/jetstream) — Go client v2 API.
- [NATS Consumer Details](https://docs.nats.io/nats-concepts/jetstream/consumers) — Consumer config semantics.
- [NATS JetStream Model Deep Dive](https://docs.nats.io/using-nats/developer/develop_jetstream/model_deep_dive) — Dedup, retention, ordering.
- [NATS Docker & Compose](https://nats.io/blog/docker-compose-plus-nats/) — Integration test setup.
- [NATS by Example](https://natsbyexample.com/) — Practical examples.

### Python NATS

- [nats-py on PyPI](https://pypi.org/project/nats-py/) — Official package.
- [nats-py documentation](https://nats-io.github.io/nats.py/) — Full API reference.

### OpenTelemetry

- [OTel Context Propagation](https://opentelemetry.io/docs/concepts/context-propagation/) — Spec.
- [OTel Messaging Semantics](https://opentelemetry.io/docs/specs/semconv/messaging/messaging-spans/) — Span kinds.
- [W3C TraceContext](https://w3.org/TR/trace-context/) — Spec for `traceparent` header.

### Prometheus

- [Prometheus Naming Practices](https://prometheus.io/docs/practices/naming/) — Label design.
- [High-Cardinality Metrics (Last9)](https://last9.io/blog/how-to-manage-high-cardinality-metrics-in-prometheus/) — Cardinality-bomb cases.

### Python Packaging (2025)

- [Python Build Backends Comparison (2025)](https://medium.com/@dynamicy/python-build-backends-in-2025-what-to-use-and-why-uv-build-vs-hatchling-vs-poetry-core-94dd6b92248f) — Latest landscape.
- [Poetry vs UV](https://medium.com/@hitorunajp/poetry-vs-uv-which-python-package-manager-should-you-use-in-2025-4212cb5e0a14) — Trade-offs.
- [astral-sh/uv GitHub](https://github.com/astral-sh/uv) — Official repo.

### Protobuf

- [Protobuf Best Practices](https://protobuf.dev/best-practices/dos-donts/) — Schema evolution.
- [Protovalidate](https://github.com/bufbuild/protovalidate) — Validation reference.

---

**Report completed**: 2026-05-08 10:56 UTC
**Researcher**: Claude Agent (Technical Analysis Mode)
**Status**: Ready for P2 implementation planning
