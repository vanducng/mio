---
phase: 2
title: "SDKs (sdk-go, sdk-py)"
status: pending
priority: P1
effort: "1d"
depends_on: [1]
---

# P2 — SDKs (sdk-go + sdk-py)

## Overview

Thin NATS wrappers that bake in the conventions: idempotency keys, OTel,
Prometheus metrics, schema-version checks. Convention enforcers, not
NATS replacements. After P2, gateway + AI service both consume the same
typed surface.

## Goal & Outcome

**Goal:** Publish/consume APIs typed against `mio.v1`, with idempotency, tracing, metrics built in. One Go package, one Python package, mirror surface.

**Outcome:** Gateway code (P3) imports `sdk-go` for publish; echo-consumer (P4) imports `sdk-py` for consume. No code in either calls `nats-io/nats.go` or `nats.aio` directly.

## Files

### Go (`sdk-go/`)

- `sdk-go/go.mod` — module `github.com/vanducng/mio/sdk-go`; deps: `github.com/nats-io/nats.go` (uses the v2 `jetstream` sub-package, **not** the legacy `JetStreamContext`), `github.com/prometheus/client_golang`, `go.opentelemetry.io/otel`.
- `sdk-go/client.go` — `Client` wraps `*nats.Conn` + `jetstream.JetStream` (from `github.com/nats-io/nats.go/jetstream`).
- `sdk-go/publish_inbound.go` — `PublishInbound(ctx, *pb.Message) error`.
- `sdk-go/publish_outbound.go` — `PublishOutbound(ctx, *pb.SendCommand) error`.
- `sdk-go/consume_inbound.go` — pull-consumer helper for `ai-consumer`; returns `<-chan Delivery`.
- `sdk-go/consume_outbound.go` — pull-consumer helper for `sender-pool`; returns `<-chan Delivery`.
- `sdk-go/subjects.go` — subject builder + token validators.
- `sdk-go/metrics.go` — Prometheus collectors (counters + latency histograms with fixed buckets).
- `sdk-go/tracing.go` — W3C `traceparent` injection/extraction over NATS headers.
- `sdk-go/version.go` — `const SchemaVersion = 1` + `Verify(*pb.Message) error`.
- `sdk-go/channeltypes.go` — **generated** by `tools/genchanneltypes/`; contains `var Known = map[string]bool{...}` (only entries with `status: active`).
- `sdk-go/client_test.go` — integration test (build tag `integration`) against `playground/nats/`.

### Python (`sdk-py/`)

- `sdk-py/pyproject.toml` — **`uv`-flavored** (`[project]` table, `hatchling` backend, `[tool.uv]`); package `mio-sdk`; deps: `nats-py`, `protobuf`, `prometheus-client`, `opentelemetry-api`, `opentelemetry-sdk`. **No** `[tool.poetry]` / `setup.py`.
- `sdk-py/uv.lock` — committed lockfile.
- `sdk-py/mio/__init__.py`
- `sdk-py/mio/client.py` — **async-only** `Client` (`nats-py` has no sync API; this is a deliberate constraint, documented).
- `sdk-py/mio/subjects.py`
- `sdk-py/mio/metrics.py` (`prometheus-client`; same buckets as Go).
- `sdk-py/mio/tracing.py` (`opentelemetry.propagators.tracecontext`).
- `sdk-py/mio/version.py` — `SCHEMA_VERSION = 1` + `verify(msg) -> None`.
- `sdk-py/mio/channeltypes.py` — **generated**; contains `KNOWN: set[str] = {...}`.
- `sdk-py/tests/test_client.py` — pytest + `pytest-asyncio`, integration-marked.

### Codegen (`tools/genchanneltypes/`)

- `tools/genchanneltypes/main.go` — reads `proto/channels.yaml`, emits `sdk-go/channeltypes.go` and `sdk-py/mio/channeltypes.py`. Wired into `make proto-gen`. CI `git diff --exit-code` gate prevents drift.

## Subject grammar (locked from research)

```
mio.<direction>.<channel_type>.<account_id>.<conversation_id>[.<message_id>]
```

Replaces the earlier `mio.<dir>.<channel>.<workspace_id>.<thread_id>` draft
in `docs/system-architecture.md` §5 — aligns wire segments with the proto
foundation (channel_type from registry, account_id = mio UUID, conversation_id
covers DM/group/channel/thread uniformly). Architecture doc §5 to be patched
when the SDK lands.

Examples:
```
mio.inbound.zoho_cliq.<acct-uuid>.<conv-uuid>
mio.outbound.zoho_cliq.<acct-uuid>.<conv-uuid>.<msg-ulid>     # edit/delete commands
mio.outbound.slack.<acct-uuid>.<conv-uuid>
```

Use UUIDs (not slugs) so the subject stays stable across rename/display-name
changes. Subject tokens may not contain `.`; UUIDs and ULIDs are safe.

## Steps (Go)

1. **Module bootstrap.**
   - [ ] `cd sdk-go && go mod init github.com/vanducng/mio/sdk-go`.
   - [ ] Add deps: `github.com/nats-io/nats.go` (we will import the v2 sub-package `github.com/nats-io/nats.go/jetstream` — **do not** use `nc.JetStream()` legacy `JetStreamContext`), `github.com/prometheus/client_golang/prometheus`, `go.opentelemetry.io/otel`, `go.opentelemetry.io/otel/propagation`.

2. **Codegen first** (`tools/genchanneltypes/`).
   - [ ] Implement `tools/genchanneltypes/main.go`: parse `proto/channels.yaml`, emit `sdk-go/channeltypes.go` (`var Known = map[string]bool{...}`, only `status: active`) and `sdk-py/mio/channeltypes.py` (`KNOWN: set[str] = {...}`).
   - [ ] Wire into `Makefile` under `proto-gen` so every `make proto-gen` regenerates both files.
   - [ ] Add CI step `make proto-gen && git diff --exit-code sdk-go/channeltypes.go sdk-py/mio/channeltypes.py` — fail on drift.

3. **`subjects.go` — subject builder + validators.**
   - [ ] `Inbound(channelType, accountID, conversationID string) (string, error)` → `mio.inbound.<ct>.<aid>.<cid>`.
   - [ ] `Outbound(channelType, accountID, conversationID string, messageID ...string) (string, error)` → optional 6th segment.
   - [ ] Internal `validateToken(t string) error`: reject empty, reject any token containing `.`.
   - [ ] At publish time, reject `channelType` not present in `Known` (only `status: active` entries pass).
   - [ ] Unit test: known good inputs → expected strings; bad tokens (`.`, empty, unknown channel_type) → typed errors.

4. **`client.go` — connection + options.**
   - [ ] `New(url string, opts ...Option) (*Client, error)`: connect via `nats.Connect`, then `jetstream.New(nc)` to get the v2 `JetStream` handle. Store both on `Client`.
   - [ ] Functional options: `WithName`, `WithCreds`, `WithTracerProvider`, `WithMetricsRegistry`, `WithMaxAckPending(int)` (default 1), `WithAckWait(time.Duration)` (default 30s).
   - [ ] `Close()` drains and closes the underlying connection.
   - [ ] Note: SDK does **not** create streams or consumers — gateway startup (P3) is authoritative for provisioning.

5. **`version.go` — schema verification (publish-only).**
   - [ ] `const SchemaVersion = 1`.
   - [ ] `Verify(msg *pb.Message) error`: reject if `SchemaVersion != 1`, reject if any of `TenantId`/`AccountId`/`ChannelType`/`ConversationId` is empty, reject if `!Known[ChannelType]`.
   - [ ] Document the asymmetry inline: **publish enforces; consume does not** (consumers must tolerate forward-compatible additions).

6. **`tracing.go` — W3C traceparent over NATS headers.**
   - [ ] Use `otel.GetTextMapPropagator()` (default tracecontext); inject on publish, extract on consume.
   - [ ] Carrier wraps `nats.Header` via a `TextMapCarrier` adapter (Get/Set/Keys).
   - [ ] On publish: start span with `trace.SpanKindProducer`; set attributes `messaging.system=nats`, `messaging.destination=<subject>`, `messaging.message_id=<Nats-Msg-Id>`.
   - [ ] On consume: extract context, start span with `trace.SpanKindConsumer`, link to extracted context.

7. **`metrics.go` — bounded-cardinality Prometheus.**
   - [ ] Counter `mio_sdk_publish_total{channel_type, direction, outcome}`.
   - [ ] Counter `mio_sdk_consume_total{channel_type, direction, outcome}`.
   - [ ] Histogram `mio_sdk_publish_latency_seconds{channel_type, direction}` with buckets `[0.001, 0.005, 0.010, 0.050, 0.100, 0.500, 1.0]`.
   - [ ] Histogram `mio_sdk_consume_latency_seconds{channel_type, direction}` with the same buckets.
   - [ ] Hard rule (enforced via comment + code review): **only** the three labels `channel_type`, `direction`, `outcome`. No `account_id`, `tenant_id`, `conversation_id`, `message_id`.
   - [ ] `outcome` enum: `success`, `error`, `dedup`, `timeout`, `invalid`.

8. **`publish_inbound.go` / `publish_outbound.go`.**
   - [ ] Call `Verify(msg)` first; on error return immediately with `outcome=invalid` recorded.
   - [ ] Build `Nats-Msg-Id` header:
     - Inbound: `inb:<account_id>:<source_message_id>` (namespace by `account_id` so two installs of the same channel cannot collide).
     - Outbound: `out:<send_command.id>` (ULID is globally unique; no extra namespacing needed).
   - [ ] Inject `traceparent` via the tracecontext propagator (step 6).
   - [ ] Build subject via `subjects.go` (step 3); never accept a raw subject from the caller.
   - [ ] Marshal `msg` (proto), call `js.PublishMsg(ctx, &nats.Msg{Subject, Header, Data})` on the **v2** `jetstream.JetStream`. Observe latency histogram around the call.
   - [ ] On `ErrDuplicateID` / dup-detection ack flag: record `outcome=dedup`, return nil (idempotent success).

9. **`consume_inbound.go` / `consume_outbound.go`.**
   - [ ] Signature: `ConsumeInbound(ctx context.Context, durable string) (<-chan Delivery, error)`. **Caller invents `durable`**; SDK never auto-generates one.
   - [ ] Look up the consumer via `js.Consumer(ctx, stream, durable)` (v2 API); if it doesn't exist, return error — gateway/provisioner owns creation.
   - [ ] Use `consumer.Consume(...)` (v2) to drive a goroutine that pushes typed `Delivery` values onto the returned channel; channel buffer size = configured `MaxAckPending` (default 1).
   - [ ] `Delivery` exposes `.Msg() *pb.Message`, `.Ack() error`, `.Nak(delay time.Duration) error`, `.Term() error`.
   - [ ] On ctx cancellation: stop the consume context, close the channel, return.
   - [ ] **Skip schema verification** on consume side (asymmetry per step 5).
   - [ ] Extract OTel context via tracing.go before yielding the delivery.

10. **Integration test (`client_test.go`, build tag `integration`).**
    - [ ] Bring up `playground/nats/` via Make target.
    - [ ] Round-trip: publish inbound → consume → ack; assert subject, payload, and `traceparent` survive.
    - [ ] Idempotency: publish same `(account_id, source_message_id)` twice within the dedup window → assert exactly one delivery.
    - [ ] Schema: publish with `schema_version=2` → assert `Verify` rejects.
    - [ ] Subject builder: bad tokens (`.`, empty, unknown channel_type) → typed errors.
    - [ ] Metrics: scrape registry, assert label set is exactly `{channel_type, direction, outcome}`.
    - [ ] Run: `make test-integration` → `go test ./sdk-go -tags=integration`.

## Steps (Python)

1. **Project bootstrap with `uv`.**
   - [ ] `cd sdk-py && uv init --package mio-sdk` (creates `pyproject.toml` with `[project]` table + `hatchling` backend).
   - [ ] `uv add nats-py protobuf prometheus-client opentelemetry-api opentelemetry-sdk`.
   - [ ] `uv add --dev pytest pytest-asyncio`.
   - [ ] Commit `uv.lock`.
   - [ ] **Do not** introduce `poetry` / `setup.py` / `requirements.txt`.

2. **`mio/subjects.py` — mirror of Go `subjects.go`.**
   - [ ] `inbound(channel_type, account_id, conversation_id) -> str`.
   - [ ] `outbound(channel_type, account_id, conversation_id, message_id=None) -> str`.
   - [ ] `_validate_token(t: str)`: raise `ValueError` on empty or `"." in t`.
   - [ ] Reject `channel_type` not in `KNOWN` (imported from generated `channeltypes.py`).

3. **`mio/version.py`.**
   - [ ] `SCHEMA_VERSION = 1`; `verify(msg) -> None` raising `ValueError` on schema mismatch, empty four-tier IDs, or unknown `channel_type`.
   - [ ] Same publish-only asymmetry note as Go.

4. **`mio/client.py` — async `Client`.**
   - [ ] `nats-py` is **async-only** — there is no sync surface. Document this in the module docstring.
   - [ ] `Client.connect(url: str, *, name=None, creds=None, tracer_provider=None, metrics_registry=None, max_ack_pending=1, ack_wait=30.0) -> Client` (async classmethod).
   - [ ] Internally holds `nats.NATS` connection + `JetStreamContext` from `nc.jetstream()`.
   - [ ] `aclose()` for graceful shutdown.

5. **`mio/tracing.py`.**
   - [ ] Use `opentelemetry.propagators.tracecontext.TraceContextTextMapPropagator`.
   - [ ] Carrier adapter over a `dict[str, str]` (NATS headers).
   - [ ] Inject on publish (PRODUCER span), extract on consume (CONSUMER span); same messaging attributes as Go.

6. **`mio/metrics.py`.**
   - [ ] Same metric names as Go: `mio_sdk_publish_total`, `mio_sdk_consume_total`, `mio_sdk_publish_latency_seconds`, `mio_sdk_consume_latency_seconds`.
   - [ ] Histogram buckets: `(0.001, 0.005, 0.010, 0.050, 0.100, 0.500, 1.0)`.
   - [ ] Labels strictly `("channel_type", "direction", "outcome")` for counters; `("channel_type", "direction")` for histograms.

7. **`mio/client.py` publish methods.**
   - [ ] `await client.publish_inbound(msg)` and `publish_outbound(cmd)`.
   - [ ] Run `verify(msg)` first; record `outcome=invalid` on failure.
   - [ ] Build `Nats-Msg-Id` exactly like Go (`inb:<aid>:<sid>` / `out:<cmd.id>`).
   - [ ] Inject `traceparent`; build subject via `subjects.py`; call `js.publish(subject, payload, headers=...)` on `nats-py` JetStream.
   - [ ] Catch `nats.js.errors.APIError` for dup detection → record `outcome=dedup`, return.

8. **`mio/client.py` consume methods (async + signal-friendly).**
   - [ ] Signature: `async def consume_inbound(self, durable: str) -> AsyncIterator[Delivery]`. Caller passes `durable`; SDK never auto-generates.
   - [ ] Use **pull subscription**: `psub = await js.pull_subscribe(subject, durable=durable)`.
   - [ ] Loop: `msgs = await psub.fetch(batch=1, timeout=5.0)` — **explicit 5-second timeout** so `KeyboardInterrupt` / SIGINT / `asyncio.CancelledError` can interrupt the wait. Do **not** use an unbounded `fetch()`.
   - [ ] On `nats.errors.TimeoutError`: continue loop (no messages this window).
   - [ ] Yield a `Delivery` wrapper exposing `.msg`, `.ack()`, `.nak(delay)`, `.term()`.
   - [ ] **Skip schema verification** on consume.
   - [ ] Extract OTel context before yielding.
   - [ ] Caller cancels by exiting the `async for` (e.g., via `asyncio.CancelledError`); SDK closes the pull subscription cleanly.

9. **Integration test (`tests/test_client.py`, marker `integration`).**
   - [ ] Mirror the Go scenarios: round-trip, idempotency, schema reject, bad-token reject, metric labels.
   - [ ] OTel propagation: assert the `traceparent` extracted on consume yields the same trace_id as injected on publish.
   - [ ] Run: `uv run pytest -m integration`.

## Success Criteria

- [ ] Go uses `github.com/nats-io/nats.go/jetstream` (v2). `JetStreamContext` does **not** appear anywhere in `sdk-go/`.
- [ ] Python uses `nats-py` async surface only. No sync wrapper, no thread pool inside the SDK.
- [ ] `sdk-py/pyproject.toml` is `uv`-flavored (`[project]` + `hatchling`); `uv.lock` is committed; `poetry` / `setup.py` are absent.
- [ ] `make test-integration` (Go) and `uv run pytest -m integration` (Python) both pass against `playground/nats/`.
- [ ] **Idempotency assert**: integration test publishes the same `(account_id, source_message_id)` twice within the 2-minute dedup window with `Nats-Msg-Id = inb:<account_id>:<source_message_id>` — exactly one stream message materializes; second publish is recorded as `outcome=dedup`.
- [ ] **OTel round-trip assert**: `traceparent` injected on publish is extracted on consume; resulting span's `trace_id` matches the publisher's; publisher span kind is `PRODUCER`, consumer span kind is `CONSUMER`.
- [ ] **Schema enforcement asymmetry**: publish-side `Verify` rejects `schema_version=2`, empty `tenant_id`/`account_id`/`channel_type`/`conversation_id`, and unknown `channel_type`. Consume side passes a `schema_version=2` message through untouched (intentional, documented).
- [ ] **Subject builder rejection**: every input with a `.`-containing token, empty token, or unknown `channel_type` returns a typed error in Go and raises `ValueError` in Python — covered by table-driven unit tests on both sides.
- [ ] **Metric label discipline**: scraping the registry shows exactly `channel_type`, `direction`, `outcome` on counters and `channel_type`, `direction` on histograms. A grep for `account_id`, `tenant_id`, `conversation_id`, `message_id` across `metrics.go` and `metrics.py` returns zero label uses.
- [ ] **Histogram buckets**: publish-latency histogram buckets are exactly `[0.001, 0.005, 0.010, 0.050, 0.100, 0.500, 1.0]` in both languages.
- [ ] **Caller-owned durable**: SDK consume APIs require a non-empty `durable` argument and never auto-generate one; passing empty returns an error / raises `ValueError`. Provisioning is delegated to gateway startup (P3).
- [ ] **Codegen lockstep**: `make proto-gen` regenerates `sdk-go/channeltypes.go` and `sdk-py/mio/channeltypes.py`. CI's `git diff --exit-code` gate fails if either drifts from `proto/channels.yaml`.
- [ ] **Python signal handling**: pull-fetch loop uses an explicit 5s timeout so SIGINT / `asyncio.CancelledError` interrupts cleanly within ≤5s; verified by an integration test that cancels mid-fetch.

## Risks

- **Legacy vs v2 JetStream API divergence (Go).** Examples and blog posts still reference `nc.JetStream()` (`JetStreamContext`). Mitigation: lint rule / code-review checklist forbids importing the legacy surface; CI grep for `nc.JetStream(` in `sdk-go/` fails the build.
- **`nats-py` async-only constraint.** Any future sync caller (e.g., a CLI smoke-test) must spawn its own event loop — the SDK will not provide a sync facade. Mitigation: document explicitly in `sdk-py/README.md`; KISS.
- **Codegen drift.** If a developer edits `proto/channels.yaml` without running `make proto-gen`, the SDK silently rejects valid publishes (or accepts retired ones). Mitigation: CI `git diff --exit-code` gate on the generated files; pre-commit hook calls `make proto-gen` when `proto/channels.yaml` changes.
- **API surface drift Go ↔ Python.** Mitigation: write Go first, translate to Python; PR template includes a side-by-side method table.
- **Dedup window edge case.** A retry at exactly the 2-minute boundary can produce a duplicate stream message. Mitigation: rely on the Postgres `(account_id, source_message_id)` unique constraint as defense in depth (per `docs/system-architecture.md` §7); document in code comment near `Nats-Msg-Id` builder.
- **Metric cardinality explosion.** A future contributor adds `account_id` as a label "for debugging". Mitigation: comment block in `metrics.go` / `metrics.py`; CONTRIBUTING.md note; CI grep gate.
- **Subject token bypass.** Free-text channel-side identifiers (e.g., from a webhook payload) might contain `.`. Mitigation: SDK validators reject these at publish; gateway must map to UUID before calling the SDK.

## Research backing

[`plans/reports/research-260508-1056-p2-sdk-nats-jetstream-clients.md`](../../reports/research-260508-1056-p2-sdk-nats-jetstream-clients.md)

Validated deltas to fold during execution:
- **Use the new `jetstream` package** (`github.com/nats-io/nats.go/jetstream`, v2 API) over the legacy `JetStreamContext`. Maintainer-recommended; better pull-consumer ergonomics; cleaner ack semantics.
- **Python**: `nats-py` async-only (no sync alternative needed). Use `uv` for Python packaging (faster than poetry, zero-config, future-proof).
- **Schema-version validation asymmetry confirmed**: validate on publish (hard reject), skip on consume (allows backward-compatible field additions). Document this contract.
- **Metric labels strictly bounded**: `channel_type`, `direction`, `outcome` only. Reject `account_id`/`tenant_id`/`conversation_id` as labels (cardinality bomb). Histogram buckets for publish latency: 1ms, 5ms, 10ms, 50ms, 100ms, 500ms, 1s.
- **Pull consumer ergonomics**: Go returns `<-chan Delivery` (caller-owned lifecycle); Python uses async iterator. Caller invents durable name; SDK does not.
- **OTel header**: W3C `traceparent` is ASCII-safe over NATS headers; PRODUCER/CONSUMER span kinds; extraction via standard tracecontext propagator.
- **Effort estimate**: 14–15h end-to-end (fits P2's 1d budget).
