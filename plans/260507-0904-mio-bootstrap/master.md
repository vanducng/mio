---
title: "MIO Bootstrap — POC to GKE + second channel"
status: in-progress
goal: "Cliq message loop running end-to-end on GKE through JetStream, plus a second channel proving the abstraction."
created: 2026-05-07
mode: master+quick-phases
---

# MIO Bootstrap — Master Plan

> Master tracker for the MIO POC. Each phase has its own sub-directory with
> a tight `plan.md`. Update the **Status** column here every time a phase
> moves; that's the single source of progress truth.

## Goal

Ship the MIO POC: a Zoho Cliq message loop running end-to-end on GKE
(webhook → gateway → JetStream → echo-consumer → JetStream → Cliq REST),
then validate the abstraction by adding a second channel (Slack or Telegram)
in ≤1 day.

## Approach

Sequenced build per the roadmap in `plan.md` (local-only narrative). Each
phase produces a runnable artifact and the next phase has something concrete
to build on. Decoupled gateway + bus + AI consumer; Postgres + GCS for
storage; Helm charts on GKE. No managed-cloud lock-in.

Detailed component definitions, data flows, and design rules: see
`docs/system-architecture.md`.

## Design strategy — channels data model (locked)

Source: `plans/reports/research-260507-1102-channels-data-model.md` (deep
research across Slack/Discord/Mattermost/Rocket.Chat/Matrix/Zulip/Sendbird +
goclaw migration scars).

Five foundation choices, baked in at P1 because retrofitting them costs a
goclaw-style 30-table migration:

1. **Four-tier addressing**: `tenant_id → account_id → conversation_id → message_id`. Every wire message carries all four. Tenant and account are present from row 1, not retrofitted.
2. **`channel_type` is a string with a registry** (`proto/channels.yaml`), not a proto enum. Adding a channel = entry in YAML + adapter; no proto regen, no SDK redeploy.
3. **Single polymorphic `Conversation` with a `kind` discriminator** — DM, GROUP_DM, CHANNEL_PUBLIC, CHANNEL_PRIVATE, THREAD, FORUM_POST, BROADCAST. Maps cleanly to all 7 platforms surveyed. No per-channel-type tables.
4. **Idempotent address is `(account_id, source_message_id)`**, never `(channel_type, source_message_id)`. Survives one tenant running two Slack workspaces.
5. **`attributes map<string,string>` (proto) / `JSONB` (DB) is the only legal home for channel-specific data.** Promotion to a typed proto field requires ≥2 consumers / channels using it.

What's deliberately *not* in v1 (to keep POC simple):
- No `Account` or `Conversation` proto **messages** — those are DB tables; the wire envelope carries their IDs flat.
- No `MessageRelation` (edit/reaction/reply) — defer until P5 outbound edit semantics decision.
- No `channel_users` table — defer until cross-channel identity merge becomes real.
- No RLS, no member roster, no compaction — none of which break the wire format if added later.

Subject grammar realigned: `mio.<dir>.<channel_type>.<account_id>.<conversation_id>[.<message_id>]` (was `<channel>.<workspace>.<thread>`). See P2.

## Success Criteria

- [ ] **End-to-end loop in GKE** — Cliq webhook lands in cluster, AI echo-consumer sees the message, reply appears in the same Cliq thread within 5s
- [ ] **Per-thread ordering** — `MaxAckPending=1` consumer in cluster confirmed via load test
- [ ] **Idempotency** — replaying the same Cliq webhook 5× produces exactly one downstream publish
- [ ] **GCS archive populated** — `gs://mio-messages/channel=zoho-cliq/date=YYYY-MM-DD/` has the expected payloads
- [ ] **Per-workspace rate limit** — bursting one workspace doesn't delay another's outbound throughput
- [ ] **Second channel adapter ≤1 day** — Slack or Telegram inbound + outbound shipped in a single working day; no proto changes required

## Out of Scope (this plan)

- **UI / admin console** — workspace OAuth onboarding belongs in MIU's admin console, not MIO
- **Staging cluster** — feature flags + fast rollback only
- **Multi-region GKE** — single regional cluster
- **Dedicated BigQuery sink** — GCS + external tables instead
- **Agent intelligence** — LangGraph runs live in MIU, not here
- **Channel #3+** — abstraction validated at #2; further channels are deployment work, separate plans

## Phases

| # | Phase | Status | Depends on | Effort | Slot in plan.md |
|---|---|---|---|---|---|
| P0 | [Reserve + scaffold](p0-reserve-and-scaffold/plan.md) | pending | — | 1h | 11:25–12:25 today |
| P1 | [Proto v1 envelope](p1-proto-v1-envelope/plan.md) | pending | P0 | 1h | 12:25–13:25 today |
| P2 | [SDKs (sdk-go, sdk-py)](p2-sdks-go-and-py/plan.md) | pending | P1 | 1d | 17:00–20:30 today (evening overflow) |
| P3 | [Gateway + Cliq inbound](p3-gateway-cliq-inbound/plan.md) | pending | P2 | 1–2d | next session |
| P4 | [Echo consumer](p4-echo-consumer/plan.md) | pending | P2, P3 | 2h | |
| P5 | [Outbound path → Cliq](p5-outbound-path-cliq/plan.md) | pending | P3, P4 | 1d | |
| P6 | [Sink-gcs](p6-sink-gcs/plan.md) | pending | P2 (parallelizable from P3) | 1d | |
| P7 | [Helm charts + NATS on GKE](p7-helm-and-nats-gke/plan.md) | pending | P3, P5, P6 | 1–2d | |
| P8 | [POC deploy on GKE](p8-poc-deploy-gke/plan.md) | pending | P7 | 1d | |
| P9 | [Second channel adapter](p9-second-channel-adapter/plan.md) | pending | P8 | 1d (litmus) | |

### Dependency graph

```
P0 → P1 → P2 ─┬─ P3 → P4 → P5 ─┐
              │                 ├─ P7 → P8 → P9
              └─ P6 ────────────┘
```

P6 can run parallel to P3–P5 if I want a second front. Default: sequential.

## Milestones

| M | Hits when | What's true |
|---|---|---|
| **M1: Schema locked** | P1 done | `mio/v1` proto generates clean Go + Python; round-trip test passes |
| **M2: Local loop runs** | P5 done (locally) | Cliq webhook (via tunnel) → local docker compose → reply in Cliq |
| **M3: Archive working** | P6 done | GCS partitioned writes verifiable, replayable from cold storage |
| **M4: GKE up** | P7 done | 3-replica JetStream cluster healthy, gateway + sink running, observable |
| **M5: POC shipped** | P8 done | M2 loop runs in-cluster, traces + metrics + logs flowing |
| **M6: Abstraction validated** | P9 done | Second channel ships in ≤1 day; if not, the proto envelope is wrong — fix before adding #3 |

## Constraints

- Solo developer; phases sized for a single working session each (1h–1d)
- Cloud-agnostic by construction — no GCP-only primitives in code paths
- GKE is the POC target; Helm charts must work on any conformant K8s
- Schema breaking changes go to a new `mio.v2` package — never mutate `v1`
- Local dev must run offline (`docker compose` covers NATS + Postgres + MinIO)

## Risks

| Risk | Mitigation |
|---|---|
| Cliq webhook deadline (≤5s) breached on slow Postgres | Idempotency upsert is `INSERT … ON CONFLICT DO NOTHING` (cheap); pool warm; publish before secondary writes |
| Schema drift Go ↔ Python | `buf breaking` in CI; semantic version field on every message; SDK rejects unknown major |
| `MaxAckPending=1` becomes throughput floor | Documented graduation path: shard by subject. Only graduate when load-test forces it |
| Per-workspace rate-limit memory growth | TTL eviction on workspace buckets; cap total bucket count; metric on bucket count |
| GKE costs creep before there's traffic | Start with `e2-small` nodes; scale up on metric, not on hope |
| Second channel takes >1 day | Litmus test for the proto envelope. If hit, **stop and fix the envelope** before P10 |

## Progress Log

> Append a one-liner each time a phase status flips or a pre-phase slot completes. Newest at top.

- 2026-05-07 11:39 — All phases (P0–P9) realigned to locked-in foundation: `channel_type` registry slug (underscore), per-account rate limit, four-tier scope on every wire+SDK+adapter ref, JS stream/consumer defs locked in P3+P7, P4 echo rewritten, P9 litmus made concrete (channels.yaml flip + adapter package; no proto edits)
- 2026-05-07 09:53 — Master plan revised: slot column synced to 09:10 start, P0 reset to `pending`, template progress entries cleared
- 2026-05-07 09:10 — Morning session resumed (NATS playground slot 09:10–09:55)
- 2026-05-07 09:04 — Plan dir created via `/vd:plan`; phase files P0–P9 scaffolded

## References

- Design doc: `docs/system-architecture.md`
- Narrative + roadmap: `plans/plan.md` (local-only)
- NATS hands-on rig: `playground/nats/` (local-only)
- Cliq POC carry-over: `playground/cliq/` (local-only)
