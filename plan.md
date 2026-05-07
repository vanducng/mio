# MIO Project

## What I'm building

MIO is the messaging I/O platform for MIU. It connects MIU's AI agents to the chat channels where customers actually live - Zoho Cliq first (POC), then Telegram, Slack, Discord, more later - and gives them a clean, channel-agnostic way to receive messages and respond back.

The core insight: **channels are messy, agents shouldn't care**. Zoho Cliq uses `chat_id` + `bot_unique_name`, Slack uses `thread_ts`, Discord uses `guild_id`, Telegram uses `chat_id`, etc.

MIO normalizes all of it into one canonical envelope so the AI service receives a `Message` and returns a `SendCommand` — without ever importing a Slack SDK or knowing what a thread looks like in Discord.

## Why decoupled

Every channel webhook has a hard ack deadline (Slack and Discord: 3 seconds). LLM calls take 2-30 seconds. Coupling them means dropped messages on the first slow agent run. So MIO sits in between: gateway acks fast, durably persists to a bus, AI service consumes on its own schedule. As bonus: replay-ability for prompt iteration, failure isolation, independent scaling, and the ability to add new consumers (analytics, archive, training data) without touching the receiver.

## Components

- **mio-gateway** — Go, stateless. One handler per channel for webhooks in; one consumer pool per channel for API calls out. Per-workspace rate limiting lives here.
- **mio-bus** — NATS JetStream, 3-replica cluster on GKE. The durable spine. Two streams: `MESSAGES_INBOUND` (limits retention, 7d, replayable) and `MESSAGES_OUTBOUND` (workqueue retention, 23h ceiling).
- **mio-proto** — Canonical Protobuf schema. `Message`, `SendCommand`, `Channel`, `User`, `Attachment`. Versioned via `buf`, breaking changes go to a new package.
- **mio-sdk-go / mio-sdk-py** — Shared client libraries. Wrap NATS, expose typed APIs, bake in idempotency, OpenTelemetry, Prometheus metrics, and schema version checks. Convention enforcers, not NATS replacements.
- **mio-sink-gcs** — Go consumer. Writes raw payloads to GCS, partitioned `gs://mio-messages/channel=zoho-cliq/date=YYYY-MM-DD/`. Long-term cold storage and replay source; doubles as the analytics substrate (BigQuery external tables can read it directly when an analyst actually asks).

The MIU AI service is *not* part of MIO — it imports `mio-sdk-py` and lives in MIU's repo.

## Stack decisions, locked in

- **Bus**: NATS JetStream. Cloud-agnostic, lightweight (~50MB RAM idle), runs on GKE today and any K8s tomorrow.
- **Languages**: Go for the gateway and consumers (Zoho Cliq via REST, slack-go, discordgo, grammY-equivalent, GCS SDK); Python for the AI service (LangGraph is Python-first).
- **Schema**: Protobuf via `buf`. JSON drift between Go and Python within a month otherwise.
- **Storage tiers**: Postgres + pgvector (operational, lives in AI service), GCS (raw archive + analytics source). Two lifetimes, two access patterns, never shared.
- **Workflow engine**: Hatchet (already running) wraps LangGraph runs with durable execution.
- **Platform**: GKE for the POC, packaged via Helm charts in-repo. Only K8s primitives — no managed cloud lock-in.
- **Local dev**: `docker compose` brings up NATS + Postgres + MinIO (stand-in for GCS), so the full loop runs offline.

## Key design rules I'm holding myself to

1. **Gateway is dumb.** No business logic, no AI calls. Validate signature, normalize, publish, ack. That's it.
2. **Consumers talk to NATS directly via the SDK.** No proxy service in front. The SDK handles convention; NATS handles transport.
3. **Idempotency at the edge.** `(channel, source_message_id)` is a unique constraint. Channels redeliver; we silently dedupe.
4. **Per-workspace rate limits, not global.** One chatty tenant must not starve others.
5. **Per-thread ordering** via single-replica AI consumer with `MaxAckPending=1` to start. Graduate only when throughput demands.
6. **Two-step UX for slow LLM calls.** Send "thinking…" immediately, edit-in-place when the real answer arrives.

## What I'm explicitly *not* building yet

- A UI. NATS CLI + NUI are enough for day-1 ops. First real UI need is workspace OAuth onboarding, and that goes in MIU's admin console, not a separate MIO app.
- A staging cluster. Solo dev scale — feature flags + fast rollback beat heavy gating.
- More than one channel adapter on day one. Zoho Cliq first (POC target, internal demand), then generalize to Slack/Telegram/Discord.
- An AI agent in this repo. MIU's agents live in MIU. We ship a tiny `examples/echo-consumer/` to prove the loop, nothing more.
- A dedicated BigQuery sink. GCS is the analytics substrate too; BQ external tables read it directly if/when needed.

## Proposed repo layout

```
mio/
├── proto/                    # Canonical Protobuf schema (buf-managed)
│   └── mio/v1/{message,send_command,channel,user,attachment}.proto
├── sdk-go/                   # Go client wrapping NATS + generated proto
├── sdk-py/                   # Python client wrapping NATS + generated proto
├── gateway/                  # mio-gateway: webhook handlers + outbound sender
│   └── internal/channels/zoho-cliq/   # first adapter
├── sink-gcs/                 # mio-sink-gcs: raw payload archiver
├── examples/
│   └── echo-consumer/        # Python stub: consume inbound, publish outbound
├── deploy/
│   ├── docker-compose.yml    # NATS + Postgres + MinIO + gateway + sink-gcs
│   └── charts/               # Helm charts for GKE
│       ├── mio-nats/
│       ├── mio-gateway/
│       └── mio-sink-gcs/
├── docs/
└── plan.md
```

Single Go module at root for gateway/sink/sdk-go, with `buf` generating into `proto/gen/{go,py}`. SDK packages publish independently.

## Phased roadmap

Sequenced so each phase produces a runnable artifact and the next phase has something concrete to build on. Phases are intentionally small — if one expands past its scope, the envelope is probably wrong; fix it before piling on.

| Phase | Title | Output | Notes |
|------:|-------|--------|-------|
| **P0** | Reserve + scaffold | GitHub repo, Go module path, Python package name reserved. Monorepo skeleton per layout. `buf` wired. `docker compose` brings up NATS + Postgres + MinIO. | Cheap now, painful later. |
| **P1** | Proto v1 envelope | `mio/v1/{message,send_command,channel,user,attachment}.proto` generating into `proto/gen/{go,py}`. | Get the envelope right before SDK or gateway code lands. |
| **P2** | SDKs (`sdk-go`, `sdk-py`) | Thin NATS wrappers, idempotency keys, OTel + Prometheus, schema-version checks. | Convention enforcers, not NATS replacements. |
| **P3** | `mio-gateway` + Zoho Cliq inbound | Webhook validates signature → normalize → publish to `MESSAGES_INBOUND` → ack inside Cliq's deadline. | Gateway stays dumb — no business logic. |
| **P4** | `examples/echo-consumer` | Python stub: consume inbound, publish outbound. End-to-end loop runs locally. | Proves the architecture before features. |
| **P5** | Outbound path → Cliq | Gateway consumer pool drains `MESSAGES_OUTBOUND`, calls Cliq REST, handles two-step "thinking…" UX. | Per-workspace rate limits live here. |
| **P6** | `mio-sink-gcs` | Consumer writes raw payloads to GCS, partitioned `channel=…/date=…`. Lifecycle: Standard → Nearline @ 30d → Coldline @ 90d. | MinIO locally; GCS + Workload Identity in cluster. |
| **P7** | Helm charts + NATS on GKE | 3-replica JetStream StatefulSet, pd-ssd PVCs, zone-spread, Prom exporter. Charts for `mio-gateway` and `mio-sink-gcs`. | Only K8s primitives — no managed lock-in. |
| **P8** | POC deploy on GKE | End-to-end Cliq loop running in-cluster, observable (metrics, traces, logs). | This is the "ship it" milestone. |
| **P9** | Second channel adapter | Slack or Telegram. If it took more than a day, the proto envelope is wrong — fix it before adding a third. | Litmus test for the abstraction. |

## Today's morning block (08:00–13:00, 2026-05-07)

Scoping reality: P7–P8 (GKE) are multi-day. This morning ends with **P0 done, P1 done, and a clean handoff to P2** — plus the conceptual groundwork that makes the rest cheap.

| Slot | Focus | Deliverable |
|------|-------|-------------|
| 08:00–08:45 | NATS JetStream hands-on in `playground/nats/` | Local NATS + `nats` CLI; both streams created; durable consumer + `MaxAckPending=1` + replay exercised. Validates §"Key design rules" before they're codified. |
| 08:45–09:30 | Architecture lock-in → `docs/system-architecture.md` | Promote this `plan.md` into `docs/` with two diagrams: data-flow (webhook → gateway → JetStream → consumer → outbound) and stream/subject naming table. |
| 09:30–09:45 | Break | — |
| 09:45–10:30 | Phased plan via `/vd:plan` | `plans/260507-0807-mio-bootstrap/` with phase files P0–P9 fleshed out (success criteria, files touched, risks). |
| 10:30–11:30 | **Execute P0** | `git init`, layout per §"Proposed repo layout", Go module, `buf.yaml` + `buf.gen.yaml`, `deploy/docker-compose.yml` (NATS + Postgres + MinIO), README, Makefile. `make up` lives; `nats` CLI sees the streams. Repo pushed to GitHub. |
| 11:30–12:30 | **Execute P1** | Full proto v1 pass: `message.proto`, `send_command.proto`, `channel.proto`, `user.proto`, `attachment.proto`. `buf lint` + `buf breaking` clean. `buf generate` into `proto/gen/{go,py}`. Smoke test: tiny Go file imports generated types and round-trips a `Message` through local NATS. Commit. |
| 12:30–13:00 | Lunch + wrap-up | Eat. Then 10–15min: journal entry (decisions made, surprises, what's queued), `git status` clean, P2 phase file polished so the next session opens to a ready brief. |

Cognitive modes get clean windows: *learn → decide → plan → execute → close*. Lumping them is how mornings disappear.

## What I'm explicitly *not* doing this morning

- **GKE anything.** No cluster, no Helm, no Workload Identity. P7–P8, later sessions. There's no binary to deploy yet.
- **Gateway code.** P3 — needs SDK first.
- **Channel adapter beyond proto sketch.** Zoho Cliq adapter logic is P3, not P1.
- **Sink-gcs.** P6.

Resist the urge.

## Why this matters for MIU

MIO is the *connective tissue*. Without it, every new MIU agent has to deal with channel SDKs, webhook signatures, rate limits, retries, and protocol quirks. With it, agents are pure intelligence — they receive context, decide, and respond. New channels become deployment work, not engineering work. New agents become prompt and tool work, not plumbing. Zoho Cliq is the POC — once that loop runs end-to-end on GKE, every additional channel is a copy-paste-tweak job. That asymmetry is what lets a solo dev build something that looks like an enterprise platform.

The cat-coded undertone of "MIO" alongside MIU is also worth more than it sounds — when you're naming things 100 times a day in code, in commits, in conversations with future hires, the name has to feel right. MIO does.
