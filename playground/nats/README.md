# NATS JetStream playground

Hands-on rig for validating MIO's bus design before any of it ships.
Maps directly onto §"Key design rules" in `../../plan.md` so each rule has
a button you can press.

## What you get

- **NATS 2.10** with JetStream, file storage, monitoring on `:8222`
- **`nats-box`** CLI in a sidecar so the host doesn't need `nats` installed
- **Two streams** matching the MIO design:
  - `MESSAGES_INBOUND` — `mio.inbound.>` · limits retention · 7d · replayable
  - `MESSAGES_OUTBOUND` — `mio.outbound.>` · workqueue retention · 23h ceiling
- **Two durable consumers**:
  - `ai-consumer` on inbound · `MaxAckPending=1` (per-thread ordering)
  - `sender-pool` on outbound · `MaxAckPending=32` (parallel drain)

## Subject naming convention

```
mio.<direction>.<channel>.<workspace_id>.<thread_id>
       ▲           ▲           ▲              ▲
       │           │           │              └─ enables per-thread ordering filters
       │           │           └─ per-workspace rate limits / multi-tenant scoping
       │           └─ adapter (zoho-cliq, slack, telegram, discord)
       └─ inbound | outbound
```

Examples:

```
mio.inbound.zoho-cliq.workspace-1.thread-42
mio.outbound.slack.acme-corp.C0123ABC.1700000000-123456
```

## Quickstart

```bash
make up              # start NATS
make bootstrap       # create both streams + both consumers
make publish-inbound TEXT='hello from cliq'
make sub-ai          # pull-fetch as ai-consumer (Ctrl-C to stop)
make info            # snapshot of streams + consumers
```

## The rule-by-rule drills

These map 1:1 to the §"Key design rules" in `plan.md`. Run them in order;
each one is a falsification test. If a drill behaves unexpectedly, the
corresponding rule needs revisiting **before** it's codified in code.

### Rule 3 — idempotency at the edge

Goal: prove the bus doesn't dedupe by content, so the gateway must own
the `(channel, source_message_id)` unique constraint.

```bash
make publish-inbound TEXT='same payload'
make publish-inbound TEXT='same payload'
make peek-inbound    # → two messages, two seq numbers
```

Takeaway: NATS will happily accept duplicates; dedupe is the gateway's job
(Postgres unique index, or the JetStream `Nats-Msg-Id` header + the stream's
`duplicate_window`, which is set to 2min in our config).

Bonus drill — header-based dedupe inside the 2min window:

```bash
docker compose run --rm cli pub mio.inbound.zoho-cliq.workspace-1.thread-42 \
  'idempotent payload' -H 'Nats-Msg-Id:abc-123'
docker compose run --rm cli pub mio.inbound.zoho-cliq.workspace-1.thread-42 \
  'idempotent payload' -H 'Nats-Msg-Id:abc-123'
make peek-inbound    # → only one message lands
```

### Rule 5 — per-thread ordering via MaxAckPending=1

Goal: prove the AI consumer sees messages strictly in order on a given
thread, even with several queued.

```bash
for i in 1 2 3 4 5; do make publish-inbound TEXT="msg-$i"; done
make sub-ai          # → fetches one at a time; next only after ack
```

Open a second terminal and run `make sub-ai` again — it blocks on the lone
in-flight delivery until the first session acks. That's `MaxAckPending=1`
working: the durable consumer is effectively single-flight.

To graduate later: bump `max_ack_pending` in `consumers/inbound-ai-consumer.json`
and rebuild. Don't graduate until throughput demands it.

### Replayability — the "limits retention" promise

Goal: prove a fresh consumer can read history from seq=1 (prompt iteration,
training data, debugging).

```bash
for i in a b c d; do make publish-inbound TEXT="seed-$i"; done
make replay          # adds a fresh ephemeral consumer at seq=1, drains, removes
```

If `MESSAGES_INBOUND` were workqueue retention instead, this drill would
return zero messages — workqueue deletes on ack. That's why outbound is
workqueue (we *want* drain semantics) and inbound is limits (we *want* replay).

### Outbound is a workqueue (Rule for the sender pool)

Goal: prove only one sender consumer instance picks up each outbound command.

```bash
make publish-outbound TEXT='reply-1'
make publish-outbound TEXT='reply-2'
make sub-sender      # drains both
make peek-outbound   # → empty after drain
```

Workqueue retention deletes acked messages; that's the bus telling us
"this work is done." The 23h `max_age` is the safety net for stuck commands.

## Subject design — why these dimensions

| Dimension | Why it lives in the subject |
|---|---|
| `direction` (inbound/outbound) | One stream per direction; subject prefix lets a single filter scope a consumer cleanly. |
| `channel` (zoho-cliq, slack, …) | Per-channel sender pools, per-channel rate-limit buckets, per-channel sinks. |
| `workspace_id` | Per-workspace rate limits (Rule 4 — one chatty tenant must not starve others). Filter `mio.outbound.zoho-cliq.workspace-1.>` for that tenant's bucket. |
| `thread_id` | Future-proofing for per-thread consumers when we graduate from `MaxAckPending=1` globally to `MaxAckPending=1` per thread (NATS doesn't ship that natively — partition by subject when we get there). |

## Lifecycle in the cluster

This playground uses `num_replicas: 1` (single node). The production
config in `deploy/charts/mio-nats/` will use `num_replicas: 3` on a
3-node JetStream cluster (pd-ssd PVCs, zone-spread). Same stream/subject
shape, different durability posture.

## Files

```
playground/nats/
├── compose.yaml                              # NATS + nats-box CLI sidecar
├── Makefile                                  # all the drills
├── streams/
│   ├── messages-inbound.json                 # limits, 7d, replayable
│   └── messages-outbound.json                # workqueue, 23h ceiling
├── consumers/
│   ├── inbound-ai-consumer.json              # MaxAckPending=1
│   └── outbound-sender-consumer.json         # MaxAckPending=32
└── README.md                                 # this file
```

## Cleanup

```bash
make down            # stop, keep data
make reset           # stop + wipe jetstream volume
```

## What this playground is NOT

- A test suite. It's a *kicking-the-tires* rig — you run it, observe, learn.
- A production config. Single replica, no auth, no TLS, no Workload Identity.
- A substitute for the SDK. SDKs (P2) bake idempotency, OTel, schema-version
  checks. The CLI here is for proving the **bus**, not the **conventions**.
