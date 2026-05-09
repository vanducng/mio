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
- [ ] **GCS archive populated** — `gs://mio-messages/channel_type=zoho_cliq/date=YYYY-MM-DD/` has the expected payloads
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

- 2026-05-08 14:?? — **P7 DONE_WITH_CONCERNS** (cook-260508-1418-p7-helm-and-nats-gke). Local commit `de15bc8`. 46 files / 2186+ lines. 4 Helm charts lint-clean: `mio-nats` (3-replica JS, pd-balanced 10Gi PVC, PDB minAvailable=2, zone topology spread), `mio-jetstream-bootstrap` (verify-only post-install Job — never calls `nats stream add`, gateway-startup remains SoT), `mio-gateway` (sequential rollout `maxSurge=0/maxUnavailable=1`, migration pre-hook, HPA, ServiceMonitor), `mio-sink-gcs` (WI SA annotation, 1 replica POC). CI/CD: single `ci.yaml` with `dorny/paths-filter@v3`; `build-gateway` + `build-sink-gcs` push `<sha>+main` tags to GHCR on main push (no `:latest`); `deploy.yaml` via `workflow_run` + `GCP_SA_JSON`; `dependabot.yml`; `.golangci.yml`. **P3 fixture-path concern RESOLVED** — 6 PII-scrubbed fixtures landed in `gateway/internal/channels/zohocliq/testdata/`; `normalize_test.go` updated; all 22 gateway tests green. Deferred to P8: `make kind-smoke` full in-cluster test (kind binary not installed; `brew install kind` to unblock), `ct lint`, bootstrap Job negative test, all GKE-live success criteria.
- 2026-05-08 14:?? — **P5 DONE_WITH_CONCERNS** (cook-260508-1356-p5-outbound-path-cliq). Local commit `0616eb4`. 21 files / 1967+ lines; all unit tests green (7 packages), fairness bench B p99=52ms while A bursts (per-account rate limit isolation verified), distroless image builds. Adapter pattern locked: `sender.RegisterAdapter` instance form (P9-litmus clean via `make gateway-dispatch-lint`); Dispatcher zero-branch lookup; per-account token bucket with TTL evict + `buckets_active`/`evicted_total` metrics; OutboundState LRU (10k/10m) for two-step edit UX; sender pool `MaxAckPending=32` with Ack/Nak/Term + 429 Retry-After handling. Concerns: (i) plan assumed `errors.As` on zohocliq HTTPError from pool but creates import cycle — resolved with `sender.DeliveryError` interface (cleaner, no behavior change); (ii) live M2 loop not verified (needs Cliq tunnel — that's P8 deployment check); (iii) **P3 fixture-path issue still open** — must move `playground/cliq/captures/*.json` payloads to `gateway/internal/channels/zohocliq/testdata/` BEFORE P7 CI workflows land or CI breaks immediately.
- 2026-05-08 13:?? — **P4 DONE_WITH_CONCERNS** (cook-260508-1314-p4-echo-consumer). Local commit `2afaef0`. 10 files / 823+ lines; 11/11 echo unit + 35/35 sdk-py tests green; Docker image builds; SIGTERM exits ≤1s. Live NATS integration: SendCommand lands on `mio.outbound.zoho_cliq.<acct>.<conv>` with full 4-tier scope, echo text, `thread_root_message_id` fallback, `attributes.replied_to`. Concerns (P2 follow-up tech debt): (i) **out-of-bounds touch** on `sdk-py/mio/tracing.py` — OTel 1.41 dropped `trace.use_span()` and changed `_DictCarrier.get()` signature; backward-compat hotfix applied (35/35 sdk-py still green) — needs proper P2 fix + OTel-integration test in P7; (ii) proto namespace collision `proto/gen/py/mio/` shadows `sdk-py.mio` — runtime `sys.modules` grafting workaround, real fix is relocating gen into sdk-py package; (iii) burst backpressure metric test deferred to P5 e2e.
- 2026-05-08 13:?? — **P6 DONE** (cook-260508-1245-p6-sink-gcs). Local commit `f8255f4`. 21 new files; 24/24 unit + 3/3 MinIO integration tests green; distroless image builds. Offset-based filenames, partition `channel_type=zoho_cliq/date=YYYY-MM-DD/`, idempotent restart + 2-pod non-overlap verified. Dockerfile bumped to `golang:1.25-alpine` (consistent with P3) for nats.go v1.52 dep. Deferred: BQ count-match (needs live stack), GCS lifecycle rules (P7 Terraform), `/healthz` (P7), Parquet upgrade.
- 2026-05-08 13:?? — **P3 DONE_WITH_CONCERNS** (cook-260508-1245-p3-gateway-cliq-inbound). Local commit `630bb5a`. **Step 0 live Cliq gate PASSED** — 6 captured fixtures + FINDINGS.md in `playground/cliq/captures/` (gitignored). 29 files, 2452+ lines; 19/19 tests green (sig verify, replay-5×→1+4dedups, DM/CHANNEL_PUBLIC/THREAD normalize); distroless image builds; compose validates. Dockerfile auto-bumped to `golang:1.25-alpine` (sdk-go go.mod requires 1.25 from nats.go v1.52). `.dockerignore` adds `!proto/gen/go/` (gateway needs gen code in build context). Concerns: (i) **CI fixture path: `normalize_test.go` reads `../../../../playground/cliq/captures/` (gitignored) — must move payloads to `gateway/internal/channels/zohocliq/testdata/` BEFORE P7 CI lands**; (ii) Cliq Participation Handler has no `is_private` field — all channels default CHANNEL_PUBLIC, defer to P5; (iii) thread root UUID FK passed nil, full conversation-lookup defer to P5.
- 2026-05-08 12:?? — **P2 DONE_WITH_CONCERNS** (cook-260508-1244-p2). Single local commit `d6b4295`. sdk-go (12 files, 20/20 tests green), sdk-py (8 files, 35/35 tests green); jetstream v2 API; codegen tool (`tools/genchanneltypes`) emits typed channeltypes from `channels.yaml`; subject builders 4-token Inbound / optional 5-token Outbound; Verify publish-only asymmetry enforced; `Nats-Msg-Id = inb:<account_id>:<source_message_id>`; metrics labels `{channel_type, direction, outcome}`; W3C traceparent inject/extract. Concerns (acceptable): (i) `sdk-go/go.mod` auto-bumped to go 1.25.0 (nats.go v1.52.0 dep) — root stays 1.23, mise GOTOOLCHAIN=auto handles; (ii) `sdk-go/go.mod` carries `replace github.com/vanducng/mio => ..` to import root proto types — root go.mod still clean (invariant intact); (iii) integration tests deferred to P3/P4 with live NATS.
- 2026-05-08 12:?? — **P1 DONE_WITH_CONCERNS** (cook-260508-1241-p1). Single local commit `558a753`. 5 proto files under `mio.v1`; `channels.yaml` with `zoho_cliq` active; round-trip Go↔Python green; `buf lint` + `buf breaking` (WIRE_JSON, 65 rules) clean. Self-corrected drifts vs plan: (i) Python protobuf pin `>=7.34.0,<8.0.0` (plan's `>=4.27.0,<5.0.0` is an unpublished range; buf py plugin emits 7.34.1 gencode); (ii) `paths=source_relative` removed from py plugin in `buf.gen.yaml` (unsupported); (iii) `attachment.proto` enum values prefixed (`KIND_IMAGE`) per STANDARD lint. Wire numbers untouched.
- 2026-05-08 12:?? — **P0 DONE** (cook-260508-1224-p0). 22 files in one local commit `f082000`; infra services healthy; all P0 Success Criteria green. Decisions: `buf.yaml` at repo root with `modules: [{path: proto}]` (buf v2 workspace); `proto/mio/v1/placeholder.proto` to satisfy buf v2 (P1 replaces); `NATS_MON_PORT` overridable. Deferred: GitHub push, `gateway-build` target (P3), `tools/proto-roundtrip` Go source (P1 root module per pre-cook (c)).
- 2026-05-08 12:13 — Pre-cook readiness audit (review-260508-1213) found 7 unresolved gaps and fixed all in place. P0 cook-blockers: (a) `init.sql` had `CREATE ROLE mio_app` / `CREATE DATABASE mio` — Postgres entrypoint already creates both from `POSTGRES_USER`/`POSTGRES_DB` before init.sql runs, so the statements would fail on cold boot (no `IF NOT EXISTS` syntax exists for these); rewrote init.sql as comment-only placeholder. (b) `.mise.toml` task block used `[tasks.up] = { ... }` which is invalid TOML (table headers cannot have `=`); rewrote with `[tasks.up]` section headers. P1 cook-blocker: (c) `tools/proto-roundtrip/go.mod` would need a `replace` directive to import generated proto types, and CI doesn't carry the gitignored `go.work` — moved Go round-trip into the root module; pinned `google.golang.org/protobuf` once in root `go.mod`; Python half runs via `uv run` against `sdk-py`'s pin. P3 cook-blocker: (d) inbound publish subject was 5-token `mio.inbound.<ct>.<acct>.<conv>.<msg_id>` but P2 SDK exposes `Inbound(channelType, accountID, conversationID)` (4 tokens) and arch-doc §5 says `[.<message_id>]` is outbound-only; corrected P3 step 13 + success criterion to 4-token form. Doc drifts: (e) `docs/system-architecture.md` had 4 stale references the prior 11:39 audit missed — line 105 sequence diagram `(channel, source_message_id)` → `(account_id, source_message_id)`; line 231 unique-constraint same fix; line 274 GCS partition `channel=<channel>` → `channel_type=<channel_type>`; lines 337-341 metric labels `{channel,outcome}` / `{channel,workspace,outcome}` → `{channel_type, direction, outcome}` (workspace label was a forbidden cardinality bomb); plus terminology cleanups (per-workspace → per-account, thread_id → conversation_id graduation key, pd-ssd → pd-balanced for NATS PVC). Plan-internal contradictions: (f) P4 said "sdk-py Verify guards on consume" / "rejects bad versions before handle()" — contradicted P2's locked publish-only Verify asymmetry; rewrote P4 skeleton comment + Risks + Success Criterion to make the asymmetry explicit (consume-side intentionally passes v2 through; rejection happens at the publisher). (g) P5 referenced `dispatcher.ForCommand(cmd).Send/Edit` with no `Dispatcher` type defined — clarified `dispatch.go` defines `type Dispatcher` with `New(adapters)` constructor (built once in `main.go` from `sender.RegisteredAdapters()`) and `ForCommand` lookup; nil → `Term` with `reason="other"`.
- 2026-05-08 11:53 — Deep research dispatched on local-dev tooling + CI/CD + image-publish strategy ([research-260508-1153](../reports/research-260508-1153-local-dev-cicd-image-publish.md), 1211 lines, --deep mode). Picks: **mise** (single-file `.mise.toml` for Go 1.23 + Python 3.12 + buf + protoc; tasks delegate to Makefile, no replacement); **single GHA workflow** with `dorny/paths-filter@v3` (gateway/sdk-py/proto path-scoped jobs); **`docker/build-push-action@v6` + `gcr.io/distroless/static-debian12:nonroot`** for the gateway Go binary (multi-stage, registry cache, sha + version tags, no `latest`); **direct `helm upgrade --set image.tag=<sha>`** from GHA (GitOps via ArgoCD/Flux deferred). Phase integration: no new phase — folds into P0 (`.mise.toml`, `.dockerignore`, Makefile targets), P3 (`gateway/Dockerfile`), P6 (`sink-gcs/Dockerfile`), P7 (`.github/workflows/ci.yaml` + `deploy.yaml`, image block in `values.yaml`, imagePullSecret), P8 (GHA→GKE auth via GCP service-account JSON for POC; WIF deferred). Defer: cosign signing, SBOM, multi-arch, Trivy gate (Trivy GHA action was supply-chain-compromised March 2026 — pin SHAs if used).
- 2026-05-08 11:39 — Cross-phase audit (review-260508-1133) drift fixes applied: (1) P9 adapter self-registration aligned to P5's locked `sender.RegisterAdapter(adapter)` instance API (was `dispatcher.Register(slug, factory)` — wrong package, wrong signature, contradicted "zero edits to dispatch.go"); (2) P3 ai-consumer `MaxAckPending=1` clarified as **global** with subject-shard graduation path (was "per account_id+conversation_id" — that's the graduation, not POC); (3) `master.md` Success Criterion partition path corrected `channel=zoho-cliq` → `channel_type=zoho_cliq`; (4) `docs/system-architecture.md` §5 subject grammar block + examples updated to realigned `mio.<dir>.<channel_type>.<account_id>.<conversation_id>` form (P2:67 carry-over patched); (5) webhook URL slug locked to **hyphen** per web URL convention (`/webhooks/zoho-cliq`); registry slug / NATS subject / metric label / GCS partition stay underscore (`zoho_cliq`); router maps URL hyphen → registry underscore in `server.go`; (6) P7 + P8 gateway rollout strategy flipped `maxSurge:1, maxUnavailable:0` → `maxSurge:0, maxUnavailable:1` — sequential rollout enforces single-writer for `AddOrUpdateStream` until split-brain risk resolved; (7) P9 Risks block surfaced thread-root backfill as a master-level deferred item (was buried in step-7 prose only).
- 2026-05-08 11:30 — All 10 phase plans rewritten in place to integrate research findings as concrete Steps/Files/Success Criteria/Risks (vs the earlier append-only "Research backing" summaries). Foundation review + red-team round complete (P0–P3). Three foundation fixes applied: (1) P0 `buf` `breaking.use` corrected from invalid `STANDARD` to `WIRE_JSON` (matches P1; STANDARD isn't a valid v2 breaking ruleset); (2) P1 `lint.use` aligned to STANDARD (v1 alias `DEFAULT` retired); (3) P4 ↔ P2 SDK consume surface reconciled — P4 now uses P2's `consume_inbound(durable) → AsyncIterator[Delivery]` (which owns the 5s pull-fetch internally), not the non-existent `pull_subscribe_inbound`. Three deferred risks added: P3 inbound bad-signature DDoS brake at ingress (before P9), stream-config split-brain across gateway replicas (single-replica until P7), P9 thread-root backfill mechanism (defer to P10 MessageRelation).
- 2026-05-08 11:06 — Deep research dispatch complete. 10 phase-backing reports landed in `plans/reports/research-260508-1056-pN-*.md` (one per P0–P9). Notable findings to fold during /vd:cook: P0 add health checks + `go.work` to .gitignore; P2 use new `jetstream` v2 API + `uv` for Python; P3 needs live Cliq verify (DM signal, deadline, response shape); P5 adapter self-register via `init()` confirms P9 zero-edit litmus; P6 filename collision on pod restart — switch to offset-based naming before multi-pod; P7 single source of truth for stream provisioning is gateway startup (bootstrap Job is logs-only); P8 use Tempo+GCS, ad-hoc kubectl/toxiproxy injection, ~$430/mo; P9 use conversation-object boolean flags (`is_im`/`is_mpim`/`is_private`) not channel-id prefixes for kind detection — litmus PASSES.
- 2026-05-07 11:39 — All phases (P0–P9) realigned to locked-in foundation: `channel_type` registry slug (underscore), per-account rate limit, four-tier scope on every wire+SDK+adapter ref, JS stream/consumer defs locked in P3+P7, P4 echo rewritten, P9 litmus made concrete (channels.yaml flip + adapter package; no proto edits)
- 2026-05-07 09:53 — Master plan revised: slot column synced to 09:10 start, P0 reset to `pending`, template progress entries cleared
- 2026-05-07 09:10 — Morning session resumed (NATS playground slot 09:10–09:55)
- 2026-05-07 09:04 — Plan dir created via `/vd:plan`; phase files P0–P9 scaffolded

## Revisions (post-research red-team)

| When | Who | Phase | Change |
|---|---|---|---|
| 2026-05-08 12:13 | pre-cook | P0 | `init.sql` rewritten as comment-only placeholder — Postgres entrypoint already creates role + DB from env vars before running init.sql; `CREATE ROLE` / `CREATE DATABASE` would fail on cold boot (no `IF NOT EXISTS` for those statements). Cook-blocker. |
| 2026-05-08 12:13 | pre-cook | P0 | `.mise.toml` task block syntax fixed — `[tasks.up] = { ... }` is invalid TOML (table headers cannot have `=`); now uses `[tasks.up]` section headers. Cook-blocker (mise refuses to parse). |
| 2026-05-08 12:13 | pre-cook | P1 | `tools/proto-roundtrip/` no longer carries its own `go.mod` / `requirements.txt`; Go half lives in root module (imports `proto/gen/go/mio/v1` directly), Python half runs via `uv run` against `sdk-py`'s pinned protobuf. Removes need for `replace` directive (gitignored `go.work` makes that brittle in CI). |
| 2026-05-08 12:13 | pre-cook | P3 | Inbound publish subject corrected from 5-token `mio.inbound.<ct>.<acct>.<conv>.<msg_id>` to 4-token `mio.inbound.<ct>.<acct>.<conv>`. P2 SDK exposes `Inbound(channelType, accountID, conversationID)` (3 args, 4 tokens); arch-doc §5 reserves the 5th `.<message_id>` segment for outbound edit/delete commands only. Cook-blocker (would not compile). |
| 2026-05-08 12:13 | pre-cook | docs | `docs/system-architecture.md` patched 4 stale spots the 11:39 audit missed: idempotency `(channel, source_message_id)` → `(account_id, source_message_id)` (line 105 sequence diagram + line 231 unique-constraint prose); GCS partition `channel=<channel>` → `channel_type=<channel_type>` (line 274); §10 metric labels normalized to `{channel_type, direction, outcome}` and the explicit cardinality-discipline note added (lines 337-341 had `{channel,workspace,outcome}` — `workspace` is a forbidden cardinality bomb). Plus pd-ssd → pd-balanced (P7 NATS PVC), per-workspace → per-`account_id` (P5 rate-limit key), thread_id → conversation_id (graduation shard key per P3:147). |
| 2026-05-08 12:13 | pre-cook | P4 | Schema-version Verify language reconciled with P2's locked publish-only asymmetry. P4 had said "sdk-py Verify guards on consume" / "rejects before handle()" — that contradicts P2:140/200/217 which intentionally skips Verify on consume. P4 skeleton comment, Risks, Research-backing, and Success Criterion now state: rejection is publish-side; consume-side passes v2 through to handle() by design (forward-compat); defense-in-depth consume Verify is a P5 (gateway-as-outbound-consumer) concern, not P4. |
| 2026-05-08 12:13 | pre-cook | P5 | `Dispatcher` type made explicit. P5 step 2 referenced `dispatcher.ForCommand(cmd).Send/Edit` with no type defined; now `dispatch.go` declares `type Dispatcher struct { byChannel map[string]Adapter }` + `New(adapters []Adapter) *Dispatcher` (panics on duplicate slug) + `ForCommand(cmd) Adapter`. `main.go` builds the dispatcher once after `init()` blocks via `sender.RegisteredAdapters()`. Unregistered `channel_type` → `Term` with `reason="other"`. |
| 2026-05-08 11:39 | audit | P9 | Adapter `init()` aligned to P5's locked `sender.RegisterAdapter(adapter)` (instance form, `sender/registry.go`) — replaces the incorrect `dispatcher.Register(slug, factory)` form (wrong package, wrong signature, self-contradicted "zero edits to `dispatch.go`") |
| 2026-05-08 11:39 | audit | P3 | `ai-consumer` MaxAckPending wording fixed: "MaxAckPending=1 globally (graduation: subject-shard …)" — was "per account_id+conversation_id pair" which conflated POC with graduation path; now matches P4/P7/arch-doc |
| 2026-05-08 11:39 | audit | master + arch-doc | Stale subject grammar / partition path patched: `master.md:60` `channel=zoho-cliq` → `channel_type=zoho_cliq`; `docs/system-architecture.md` §5 block + examples updated to realigned `mio.<dir>.<channel_type>.<account_id>.<conversation_id>` (carry-over flagged in P2:67) |
| 2026-05-08 11:45 | decision | P3 + P4 + P7 + P8 | Cliq webhook URL slug locked to **hyphen** per web URL convention (RFC 3986 / Google guidelines): `/webhooks/zoho-cliq`. Registry slug, NATS subject token, metric label, GCS partition, code identifier stay **underscore** (`zoho_cliq`). Router maps URL hyphen → registry underscore via `strings.ReplaceAll(slug, "-", "_")` in `server.go`. Two slugs by design: URLs follow web standard, internal identifiers follow code/subject standard. |
| 2026-05-08 11:53 | research | P0 | Added `.mise.toml` (Go 1.23 + Python 3.12 + buf + protoc), `.dockerignore`, and `gateway-build` / `gateway-build-local` Makefile targets to scope. Tooling baseline locked before P3 Dockerfile lands. |
| 2026-05-08 11:53 | research | P3 | `gateway/Dockerfile` line item expanded: multi-stage (`golang:1.23-alpine` builder → `gcr.io/distroless/static-debian12:nonroot` runtime), `CGO_ENABLED=0` static binary, USER 65532, EXPOSE 8080. Build context is repo root (not `gateway/`) so `go.work`-free single-module copy works. |
| 2026-05-08 11:53 | research | P6 | `sink-gcs/Dockerfile` line item expanded with same distroless pattern as P3 (Go binary; research's "Python sink" suggestion contradicts P6 plan which is Go — Go pattern stays). |
| 2026-05-08 11:53 | research | P7 | New section "8. CI/CD + image publish" added: `.github/workflows/ci.yaml` (single workflow, `dorny/paths-filter@v3`, mise-bootstrapped, buf lint + breaking + golangci-lint + ruff + go test + pytest + image build/push to ghcr.io with sha + branch tags, registry cache); `.github/workflows/deploy.yaml` runs on `push: main` → `helm upgrade --set image.tag=<sha>` against GKE; `deploy/charts/mio-gateway/values.yaml` image block uses `ghcr.io/vanducng/mio/gateway` repo + tag override; imagePullSecret created in `mio` namespace via `setup.sh`. |
| 2026-05-08 11:53 | research | P8 | Added GHA→GKE auth via static GCP service-account JSON in `secrets.GCP_SA_JSON` (Workload Identity Federation deferred to P10); imagePullSecret bootstrap step added before first `helm upgrade`. |
| 2026-05-08 11:39 | audit | P7 + P8 | Gateway Deployment rollout strategy flipped `maxSurge:1, maxUnavailable:0` → `maxSurge:0, maxUnavailable:1` — sequential rollout enforces single-writer for `AddOrUpdateStream` until split-brain risk resolved; LB drain still works via `connection_draining_timeout_sec:300` on surviving replica |
| 2026-05-08 11:39 | audit | P9 | Risks block surfaces thread-root backfill as master-level deferred item (was buried in step-7 prose only); now matches the three deferred-risk roster |
| 2026-05-08 11:30 | red-team | P0 | `buf breaking.use` corrected `STANDARD` → `WIRE_JSON`; `STANDARD` is not a valid v2 breaking ruleset |
| 2026-05-08 11:30 | red-team | P1 | `buf lint.use` aligned `DEFAULT` → `STANDARD` (v2 canonical name) |
| 2026-05-08 11:30 | red-team | P4 | Skeleton uses P2's `consume_inbound()` async iterator (the actual SDK surface), removing the fictitious `pull_subscribe_inbound` call that drifted the consume contract from sdk-py |
| 2026-05-08 11:30 | red-team | P3 | `Out (deferred)` block grew: inbound bad-signature DDoS brake at ingress (NGINX/Cloud Armor) before P9, and stream-config split-brain assumption documented for >1 gateway replica |

## References

- Design doc: `docs/system-architecture.md`
- Narrative + roadmap: `plans/plan.md` (local-only)
- NATS hands-on rig: `playground/nats/` (local-only)
- Cliq POC carry-over: `playground/cliq/` (local-only)

### Research backing (deep, --deep mode, 2026-05-08)

| Phase | Report |
|---|---|
| Foundation (channels data model) | [research-260507-1102-channels-data-model.md](../reports/research-260507-1102-channels-data-model.md) |
| P0 — Reserve + scaffold | [research-260508-1056-p0-scaffold-monorepo-infra.md](../reports/research-260508-1056-p0-scaffold-monorepo-infra.md) |
| P1 — Proto v1 envelope | [research-260508-1056-p1-proto-envelope-design.md](../reports/research-260508-1056-p1-proto-envelope-design.md) |
| P2 — SDKs (sdk-go + sdk-py) | [research-260508-1056-p2-sdk-nats-jetstream-clients.md](../reports/research-260508-1056-p2-sdk-nats-jetstream-clients.md) |
| P3 — Gateway + Cliq inbound | [research-260508-1056-p3-gateway-cliq-inbound-webhook.md](../reports/research-260508-1056-p3-gateway-cliq-inbound-webhook.md) |
| P4 — Echo consumer | [research-260508-1056-p4-echo-consumer-jetstream-pull.md](../reports/research-260508-1056-p4-echo-consumer-jetstream-pull.md) |
| P5 — Outbound path → Cliq | [research-260508-1056-p5-outbound-rate-limit-edit-ux.md](../reports/research-260508-1056-p5-outbound-rate-limit-edit-ux.md) |
| P6 — Sink-gcs | [research-260508-1056-p6-sink-gcs-archival-bigquery.md](../reports/research-260508-1056-p6-sink-gcs-archival-bigquery.md) |
| P7 — Helm + NATS on GKE | [research-260508-1056-p7-helm-nats-jetstream-gke.md](../reports/research-260508-1056-p7-helm-nats-jetstream-gke.md) |
| P8 — POC deploy on GKE | [research-260508-1056-p8-poc-deploy-observability-gke.md](../reports/research-260508-1056-p8-poc-deploy-observability-gke.md) |
| P9 — Second channel adapter | [research-260508-1056-p9-slack-adapter-second-channel.md](../reports/research-260508-1056-p9-slack-adapter-second-channel.md) |
| Cross-cutting — Local-dev tooling + CI/CD + image publishing | [research-260508-1153-local-dev-cicd-image-publish.md](../reports/research-260508-1153-local-dev-cicd-image-publish.md) |
