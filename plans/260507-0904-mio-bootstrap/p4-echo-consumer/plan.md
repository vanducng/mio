---
phase: 4
title: "examples/echo-consumer"
status: pending
priority: P1
effort: "2h"
depends_on: [2, 3]
---

# P4 — Echo consumer

## Overview

Tiny Python stub that consumes from `MESSAGES_INBOUND` and publishes back
to `MESSAGES_OUTBOUND` with the same text. Purpose: prove the loop end-to-end
locally before any real AI joins. Stays in this repo as a reference; the
real AI service lives in MIU.

The consumer is a **mirror of the four-tier scope** — every outbound
`SendCommand` carries the same `tenant_id`, `account_id`, `channel_type`,
`conversation_id` it received, so the gateway sender pool (P5) knows
exactly which platform install + which conversation to deliver to without
any DB lookup.

## Goal & Outcome

**Goal:** `examples/echo-consumer/` is a ~80-line Python script using `sdk-py` to consume one inbound and publish one outbound. End-to-end loop runs on `docker compose`.

**Outcome:** Run `make echo-up`, post the captured Cliq payload to local gateway, see `MESSAGES_OUTBOUND` get the echo `SendCommand` within 1s.

## Files

- **Create:**
  - `examples/echo-consumer/echo.py` — main script
  - `examples/echo-consumer/Dockerfile`
  - `examples/echo-consumer/pyproject.toml` (depends on `mio-sdk` from P2)
  - `examples/echo-consumer/README.md`
- **Modify:**
  - `deploy/docker-compose.yml` — add `echo-consumer` service depending on `nats`
  - `Makefile` — `echo-up`, `echo-logs`

## Consumer config

Durable consumer name: `ai-consumer` (the AI service in production will
reuse the same name; echo-consumer is the POC stand-in).

```python
# created on first connect; idempotent
consumer = await js.add_consumer(
    stream="MESSAGES_INBOUND",
    config=ConsumerConfig(
        durable_name="ai-consumer",
        deliver_policy="all",
        ack_policy="explicit",
        ack_wait=30,                # seconds
        max_ack_pending=1,          # per-conversation ordering at this stage
        max_deliver=5,
        replay_policy="instant",
        filter_subject="mio.inbound.>",
    ),
)
```

`MaxAckPending=1` is the throughput floor — graduation path is subject-shard
once load demands it (documented in master.md → Risks).

## Code skeleton

The consumer drives `mio.Client.consume_inbound(durable="ai-consumer")` —
the SDK's async iterator (P2 Step 8). Internally the SDK runs a 5-second
pull-fetch loop, so even when no messages arrive the iterator yields
control back to this loop every 5s. That gives SIGTERM a clean
shutdown granularity without `docker kill -9`.

**Why iterator, not raw `pull_subscribe + fetch`:** the SDK already owns
the pull-fetch loop, OTel context extraction, dedup-on-consume, and
backpressure metric emission (P2 contract). The consumer just consumes
the typed iterator. There is **no** `pull_subscribe_inbound` surface in
sdk-py; if a future workload needs raw fetch control, P2 grows that
surface — P4 doesn't reach around the SDK.

Signal handlers are registered **before** the iterator opens. Cancellation
is via the `stop` event checked between iterations (or `asyncio.CancelledError`
propagating through the iterator close path; both work — see notes).

```python
import asyncio, os, signal
from ulid import ULID
import mio                                 # sdk-py
from mio.gen.mio.v1 import Message, SendCommand

NATS_URL = os.environ["NATS_URL"]

async def handle(msg: Message, client: mio.Client) -> SendCommand:
    # No schema check here. P2 contract: SDK Verify is publish-side only.
    # The consume side intentionally tolerates forward-compatible additions,
    # so a v2 message *can* reach handle() — that's the asymmetry. We keep
    # this function pure and let the publisher (gateway) be the gate.
    # Defense-in-depth consume-side Verify is a P5 concern (gateway as
    # outbound consumer), not P4.
    cmd = SendCommand(
        id=str(ULID()),                     # idempotency address: out:<id>
        schema_version=1,
        # four-tier scope — preserved from inbound
        tenant_id=msg.tenant_id,
        account_id=msg.account_id,
        channel_type=msg.channel_type,
        # destination
        conversation_id=msg.conversation_id,
        conversation_external_id=msg.conversation_external_id,
        parent_conversation_id=msg.parent_conversation_id,
        thread_root_message_id=msg.thread_root_message_id or msg.source_message_id,
        # payload
        text=f"echo: {msg.text}",
        # fresh send, not an edit
        edit_of_message_id="",
        edit_of_external_id="",
        attributes={"replied_to": msg.id},
    )
    await client.publish_outbound(cmd)
    return cmd

async def main():
    # 1. Register signal handlers FIRST — before any long-running await.
    stop = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, stop.set)

    # 2. Connect + iterate. The SDK owns pull-fetch + 5s timeout internally,
    #    so the `stop.is_set()` check below runs at most ~5s after the signal.
    async with mio.Client.connect(url=NATS_URL, name="echo-consumer") as client:
        async for delivery in client.consume_inbound(durable="ai-consumer"):
            try:
                await handle(delivery.msg, client)     # P2 Delivery.msg
                await delivery.ack()
            except Exception:
                await delivery.nak(delay=5)            # retried up to max_deliver
            if stop.is_set():
                break                                  # exits iterator → SDK closes pull sub cleanly

if __name__ == "__main__":
    asyncio.run(main())
```

Notes:
- The P2 SDK's `consume_inbound()` returns `AsyncIterator[Delivery]` and
  internally fetches with a 5s timeout (P2 Step 8). Iterator exit (break,
  return, exception, or `asyncio.CancelledError`) closes the pull
  subscription cleanly — no manual `unsubscribe()` here.
- Every `SendCommand` field is a string except `attachments` (empty here) and
  `attributes` — all proto-generated.
- `mio.Client` (P2) enforces `schema_version=1` on **publish** via Verify.
  Consume-side does NOT validate (P2 publish/consume asymmetry).
- Backpressure metric `mio_consumer_pending` is emitted by the SDK from
  `consumer_info.num_pending`; alert at `pending > 10 for >30s`.
- Term policy for poison pills (e.g., schema v2) is deferred to **P5**;
  for now `nak(delay=5)` + `max_deliver=5` is the cap.

## Steps

1. **Package** — `pyproject.toml` depends on `mio-sdk` (local path in dev,
   PyPI later) and `python-ulid`. Python 3.12.
2. **Script** — `echo.py` per skeleton above. Single-file; no extra modules.
   Key invariants:
   - signal handlers registered before `async for delivery` opens
   - consume via `client.consume_inbound(durable=...)` async iterator (P2 surface; SDK owns the 5s pull-fetch)
   - exit on `stop.is_set()` between iterations; iterator close releases the pull subscription
   - `handle()` does **no** schema validation — that lives in SDK Verify (P2)
3. **Consumer config** — created idempotently on first run via
   `js.add_consumer(stream="MESSAGES_INBOUND", durable_name="ai-consumer", ...)`.
   Same name reused by production AI service in MIU. If config drifts
   (e.g. `MaxAckPending` change), NATS errors; manual reconciliation for now,
   automated by P7 bootstrap Job (see Risks).
4. **Dockerfile** — `python:3.12-slim`, `pip install --no-cache-dir -e .`,
   `CMD ["python", "echo.py"]`.
5. **docker-compose** — service depends on `nats` healthy + `gateway` started;
   `restart: on-failure`; env `NATS_URL=nats://nats:4222`.
6. **README** — how to run, where to point gateway, what to look for in
   `nats stream view MESSAGES_OUTBOUND`.
7. **Smoke check** — `make echo-up`, then
   `curl -d @gateway/integration_test/fixtures/cliq-message.json http://localhost:8080/webhooks/zoho-cliq`,
   observe logs + outbound stream.
8. **Shutdown check** — `docker compose kill -s SIGTERM echo-consumer`;
   container exits within ~5s (signal sets `stop`; next iteration tick exits the iterator;
   SDK closes the pull subscription cleanly — logs show `consumer closed durable=ai-consumer`).
9. **Burst check** — replay 100 inbound messages in <1s; verify
   `mio_consumer_pending` peaks <50 and drains to 0 within 5s.

## Success Criteria

- [ ] `make echo-up` brings up consumer; logs show `subscribed durable=ai-consumer subject=mio.inbound.>`
- [ ] Webhook → gateway → echo-consumer → `MESSAGES_OUTBOUND` round-trip <1s on `docker compose`
- [ ] Outbound `SendCommand` carries non-empty `tenant_id`, `account_id`, `channel_type`, `conversation_id`, `conversation_external_id`
- [ ] When inbound has `thread_root_message_id` set, outbound `SendCommand.thread_root_message_id` equals it (reply-in-thread preserved); otherwise fall back to `source_message_id` so a fresh thread roots cleanly
- [ ] Replay same inbound 5× → 5 outbound `SendCommand`s (each has a fresh `id`; gateway sender will handle outbound dedup separately at the platform level)
- [ ] Killing the consumer mid-flight → message redelivered after `ack_wait=30s`
- [ ] Schema-version mismatch on **publish** (set `schema_version=2` and call `client.publish_inbound`) → SDK `Verify` raises `ValueError` at the publisher; nothing reaches the stream; `handle()` is never called. (Consume-side does NOT validate per P2 asymmetry — a v2 message bypassed via raw `js.publish` would reach `handle()` untouched; that path is intentional, not a regression.)
- [ ] **SIGTERM produces clean shutdown ≤6s** (one 5s fetch tick + ack drain) — `docker compose kill -s SIGTERM` exits container; no `docker kill -9` needed
- [ ] **SDK closes pull subscription on iterator exit** — logs show `consumer closed durable=ai-consumer`; `nats consumer info MESSAGES_INBOUND ai-consumer` reports the durable still exists (durable is intentional) but no leaked ephemeral state from this consumer instance
- [ ] **Backpressure metric visible during burst** — replay 100 messages in <1s; `mio_consumer_pending` gauge peaks (visible via `/metrics`) then drains to 0 within 5s

## Risks

- **Schema-version mismatch** — guarded on **publish** only (P2 asymmetry: SDK Verify rejects unknown major at the producer; consume side intentionally passes through to allow forward-compatible additions). The echo handler is pure on purpose; `handle()` does no schema check. Defense-in-depth consume-side Verify is P5's job (gateway as outbound consumer), not P4's.
- **Async-iter shutdown hang** — a naive `async for delivery in sub.messages` with no internal timeout would hang on SIGTERM until the next message arrives. **Mitigated** at the SDK level: P2's `consume_inbound()` runs an internal 5s pull-fetch loop, so the iterator yields control every ~5s and `stop.is_set()` checks fire promptly.
- **Subscription leak on container restart** — without clean iterator shutdown, ephemeral pull state could leak. **Mitigated** by P2 contract: the iterator's `aclose()` (triggered by `break`/cancel/exit) closes the pull subscription. Durable consumer (`ai-consumer`) is intentionally persistent — that's the point of a durable.
- **Consumer config drift on redeploy** — `js.add_consumer()` is idempotent only when config matches; changing `MaxAckPending` or `ack_wait` makes NATS reject creation. **Mitigated** for P4 via manual reconciliation (`nats consumer del MESSAGES_INBOUND ai-consumer --force`); **P7** introduces a bootstrap Job that owns reconciliation.
- **Poison pill termination policy** — current loop `nak(delay=5)` + `max_deliver=5`, then NATS drops. Whether to `term()` immediately on schema-mismatch vs let `max_deliver` run out is **deferred to P5** (along with DLQ stream design).
- **Backpressure under burst** — `MaxAckPending=1` blocks all subsequent messages while a nak waits. SDK emits `mio_consumer_pending` gauge; alert at `pending > 10 for >30s`. Graduation path (subject sharding) documented in master.md.
- **Acks under exception** — `handle()` wrapped in try/except; on error, `nak(delay=5)` so MaxDeliver counts work.
- **`channel_type` registry drift** — `mio-sdk` consume-side does NOT validate `channel_type`; only publish-side does. Documented asymmetry — older consumer image still drains newer-typed inbound, won't reject on the consume edge.
- **Thread fallback for fresh DMs** — using `source_message_id` as `thread_root_message_id` for non-threaded messages produces a virtual thread per DM. P5 outbound must treat empty thread root identically; verify before locking (Cliq semantics need a P5 acceptance test).

## Out (deferred)

- Retry budget per conversation — currently per-message via `max_deliver`; cross-message backoff is a P5+ concern.
- Real AI logic — lives in MIU.

## Research backing

[`plans/reports/research-260508-1056-p4-echo-consumer-jetstream-pull.md`](../../reports/research-260508-1056-p4-echo-consumer-jetstream-pull.md)

Findings folded into Steps / skeleton / Risks above:
- **`fetch(timeout=5)` loop** chosen over `async for delivery` for clean SIGTERM behavior.
- **Signal handlers registered before subscription opens.**
- **Explicit `unsubscribe()` in `finally`** to avoid server-side state leak on container restart.
- **Schema rejection lives on the publish side of SDK Verify (P2 publish-only contract)** — handler is pure; consume side does not Verify.
- **Backpressure metric** `mio_consumer_pending` emitted by SDK; alert threshold `> 10 for >30s`.
- **Consumer config drift** mitigated manually for P4; automated reconciliation deferred to P7.
- **Poison-pill `term()` policy** deferred to P5 along with DLQ design.
