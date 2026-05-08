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

```python
import asyncio, signal
from ulid import ULID
import mio                                 # sdk-py
from mio.gen.mio.v1 import Message, SendCommand

async def handle(msg: Message, client: mio.Client) -> SendCommand:
    cmd = SendCommand(
        id=str(ULID()),
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
        # this is a fresh send, not an edit
        edit_of_message_id="",
        edit_of_external_id="",
        attributes={"replied_to": msg.id},
    )
    await client.publish_outbound(cmd)
    return cmd

async def main():
    stop = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, stop.set)

    async with mio.Client.connect(url=NATS_URL, name="echo-consumer") as client:
        async for delivery in client.consume_inbound(durable="ai-consumer"):
            try:
                await handle(delivery.message, client)
                await delivery.ack()
            except Exception:
                await delivery.nak(delay=5)        # retried up to max_deliver
            if stop.is_set():
                break

if __name__ == "__main__":
    asyncio.run(main())
```

Note: every field on `SendCommand` is a string except for `attachments`
(empty here) and `attributes` — all proto-generated. `mio.Client` enforces
`schema_version=1` and the `channel_type ∈ Known` registry check.

## Steps

1. `pyproject.toml`: depends on `mio-sdk` (local path in dev, PyPI later), `python-ulid`. Python 3.12.
2. `echo.py` per skeleton above. Single-file; no extra modules.
3. Dockerfile: `python:3.12-slim`, `pip install .`, `CMD ["python", "echo.py"]`.
4. docker-compose service: depends on `nats` healthy + `gateway` started; `restart: on-failure`; env `NATS_URL=nats://nats:4222`.
5. README: how to run, where to point gateway, what to look for in `nats stream view MESSAGES_OUTBOUND`.
6. Smoke check: `make echo-up`, `curl -d @gateway/integration_test/fixtures/cliq-message.json http://localhost:8080/webhooks/zoho-cliq`, observe logs + outbound stream.

## Success Criteria

- [ ] `make echo-up` brings up consumer; logs show `subscribed durable=ai-consumer subject=mio.inbound.>`
- [ ] Webhook → gateway → echo-consumer → `MESSAGES_OUTBOUND` round-trip <1s on `docker compose`
- [ ] Outbound `SendCommand` carries non-empty `tenant_id`, `account_id`, `channel_type`, `conversation_id`, `conversation_external_id`
- [ ] When inbound has `thread_root_message_id` set, outbound `SendCommand.thread_root_message_id` equals it (reply-in-thread preserved); otherwise fall back to `source_message_id` so a fresh thread roots cleanly
- [ ] Replay same inbound 5× → 5 outbound `SendCommand`s (each has a fresh `id`; gateway sender will handle outbound dedup separately at the platform level)
- [ ] Killing the consumer mid-flight → message redelivered after `ack_wait=30s`
- [ ] Schema-version mismatch (set `schema_version=2` in a hand-crafted publish) → consumer rejects + nak's

## Risks

- **Schema-version mismatch** — `sdk-py` Verify guards on consume; fail fast on unknown major.
- **Async-context lifecycle** — `nats-py` JetStream pull subscriptions need explicit close on shutdown signal; the `async with` + signal-handler pattern above handles it.
- **Acks under exception** — wrap handler in try/except; on error, `nak` with delay so MaxDeliver counts work and poison messages get terminated.
- **`channel_type` registry drift** — if the consumer image is older than the gateway and a new `channel_type` lands, the consumer rejects publishes for it. Mitigation: `mio-sdk` consumer-side does NOT validate `channel_type` on consume (only on publish); document this asymmetry.
- **Thread fallback for fresh DMs** — using `source_message_id` as `thread_root_message_id` for non-threaded messages produces a virtual thread per DM. P5 outbound must treat empty thread root identically; verify before locking.

## Out (deferred)

- Retry budget per conversation — currently per-message via `max_deliver`; cross-message backoff is a P5+ concern.
- Real AI logic — lives in MIU.
