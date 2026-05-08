---
type: review
date: 2026-05-08 11:33
scope: plans/260507-0904-mio-bootstrap (P0–P9)
---

# MIO Bootstrap Plan Audit

## TL;DR

**Verdict: needs-fixes** before /vd:cook. Plans are 90% coherent — invariants are well-internalized — but 5 concrete cross-phase drifts will bite during implementation. Headline: **P9's `dispatcher.Register("slack", factory)` API contradicts P5's `sender.RegisterAdapter(adapter)` self-registration contract**, and **ai-consumer `MaxAckPending` semantics contradict between P3 (per-key) and P4/P7 (global)**. Both are 1-line edits but ship-blocking. Top concrete fix: align P9 step 13 + line 73 to call `sender.RegisterAdapter(NewSender(...))` (no factory, no dispatcher symbol).

## Cross-phase invariants

| # | Invariant | Status | Evidence |
|---|---|---|---|
| 1 | Subject grammar `mio.<dir>.<channel_type>.<account_id>.<conv_id>` underscore slugs | ⚠ | Aligned in P1 (l.213), P2 (l.62), P3 (l.315), P5, P7 (l.91-95), P9 (l.408). **Drift: `docs/system-architecture.md:181` still shows old `mio.<dir>.<channel>.<workspace_id>.<thread_id>`; master.md:53 says doc to be patched, P2:67 says "Architecture doc §5 to be patched when SDK lands" — still unpatched.** |
| 2 | Idempotency address `(account_id, source_message_id)` | ✅ | P1:120 + l.243; P3:84,102 unique constraint; P5:213 outbound `out:<send_command.id>`; P9:227-233 enforces `event_id` over `client_msg_id`/`ts`. Consistent. |
| 3 | Metric labels `{channel_type, direction, outcome}` only | ✅ | P2:121,219; P3:332-336; P5:205,212; P6:177; P8:30; P9:404 — all explicitly forbid `account_id`/`tenant_id`/`conversation_id`. P5 adds bounded `http_status`/`reason` (acceptable). |
| 4 | Schema-version: enforce on publish, skip on consume | ✅ | P2:108,140,217 (asymmetry documented + tested); P3:Step5 publish via SDK; P4:149 consume skips Verify; P5:163 step 8 adds defense-in-depth consume-side Verify (extra, not contradictory). |
| 5 | Stream/consumer provisioning: gateway authoritative; P7 verify-only | ✅ | P3:114-122,340-345 + P5:211 + P7:24-27,148-157 + Negative test in P7:157,214. Strongest invariant in the plan. |
| 6 | Adapter self-registration via `init()` | ❌ | **P5:38,87,92,136 uses `sender.RegisterAdapter(Adapter)` (instance form). P9:73,93,252 uses `dispatcher.Register("slack", factory)` (string-keyed factory form).** Two different APIs. Also P9:104 says "no edits to `dispatch.go`" — but `dispatcher.Register` lives there. Self-contradicting. |
| 7 | Sink filename `<consumer-id>-<seq-start>-<seq-end>.ndjson` (offset-based) | ✅ | P6:53,65-82 makes this the contract; mandates pre-P7 multi-replica; P7:34,172 inherits and points back at P6. Two-pod test in P6:165 enforces. |
| 8 | ConversationKind: boolean flags, NOT prefixes | ✅ | P9:127-139,398-400 forbids `C…`/`D…`/`G…`; success criterion P9:309-311 enforces; risks P9:359-363. |
| 9 | Cliq event id derivation via P3 Step 0 | ✅ | P3:165-213 Step 0 explicit; `source_message_id` populated from Cliq message id (P3:276); idempotency UNIQUE on `(account_id, source_message_id)` (P3:102). Slack `event_id` semantically same role (P9:227). |
| 10 | P3 Step 0 gate intact | ✅ | P3:165-213 — capture rig (0.1), 6 questions (0.2), exit gate (0.3). All 6 questions in `## Risks` (P3:401-407) and master.md Progress Log line 133. |
| 11 | Out-of-scope creep (UI, staging, multi-region, BQ sink, agents, ch#3+) | ✅ | None observed. P8:209 explicitly defers second adapter; sink uses BQ external tables not dedicated sink (P6:42,145-156); no admin UI in any phase. Solid. |
| 12 | Three deferred risks (DDoS brake / split-brain / thread-root backfill) | ⚠ | DDoS brake: P3:391 Out-deferred ✅. Split-brain: P3:392 Out-deferred ✅. Thread-root backfill: **P9:227 mentions "raw `thread_ts` in attributes until P10 backfill" but no Out-deferred / Risks block in P9 calls it out as one of the three master-level deferred items.** Not blocking, but tracking surface is missing. |

## Drift list (phase plan ↔ research / cross-phase)

- **master.md:60 `channel=zoho-cliq` (old hyphen + key name)** — Success Criterion still references the pre-realignment partition path; P6:170 + P8:191 + P9:332 all use `channel_type=zoho_cliq/`. Master is the only stale spot.
- **docs/system-architecture.md:181** — subject grammar block still shows `mio.<dir>.<channel>.<workspace_id>.<thread_id>`; P2:67 acknowledged "to be patched when SDK lands"; still pending. Examples l.193-195 also stale (`zoho-cliq` hyphen, `workspace-1`, `thread-42`).
- **P3:145 vs P4:56 vs P7:110** — `ai-consumer` MaxAckPending semantics. P3 says **"MaxAckPending=1 per `account_id+conversation_id` pair (graduation: subject-shard if needed)"** but P4 + P7 + master.md:213 architecture-doc line 213 all describe **global MaxAckPending=1**. Per-key requires subject-bind shard consumers; that's the *graduation*, not the POC. P3 prose conflates the two.
- **P9:73,93,252 — `dispatcher.Register(slug, factory)` vs P5:87,92,136 — `sender.RegisterAdapter(adapter)`**. Different package (`dispatcher` vs `sender`), different signature (string+factory vs adapter-instance), different return (factory takes `sender.Deps`, P5 has no `Deps` type). Plus P9 requires "zero edits to `dispatch.go`" (P9:104,306) — but `dispatcher.Register` is by name a `dispatch` package function. Either the `Register` API moves out of `dispatch.go` into `registry.go` (P5 already places it there → P5:84 file `registry.go`), OR P9's litmus criterion is misstated.
- **Webhook URL slug**: P3:20,243 + P8:142 use `/webhooks/zoho-cliq` (hyphen). P7:219 uses `/webhooks/zoho_cliq` (underscore). URL-path slug is operator-facing and decoupled from registry slug, but inconsistency will surface as a Cliq config typo. P9:90,261 uses `/webhooks/slack` (single word — irrelevant to drift).
- **P3:392 split-brain note** says "POC runs single replica until P7"; P7 deploys **2 gateway replicas** (P7:161 `Deployment with 2 replicas (HPA min)`). The deferred-risk text predicts this, but P7 plan doesn't surface "rolling-upgrade-must-be-sequential" anywhere — should be added to P7's gateway-chart Steps or Risks.
- **P9:332 success criterion**: `metric label channel_type="slack", no underscore — single-word slug`. Comment is accurate but reads like an exception. Make it consistent: every channel uses its registry slug verbatim — `slack` and `zoho_cliq` are *both* registry slugs, the underscore in `zoho_cliq` is from multi-word, not a separate convention.

## Dependency graph sanity

Master.md:88-94 graph: `P0→P1→P2─┬─P3→P4→P5─┐` / `              └─P6────────────┘ ─→P7→P8→P9`.

Verified:
- P3 (l.7 `depends_on: [2]`) — needs P2 SDK ✅; doesn't reach for P5/P6 artifacts ✅.
- P4 (l.7 `depends_on: [2,3]`) ✅. Pulls inbound stream + SDK; doesn't reach P5.
- P5 (l.7 `depends_on: [3,4]`) ✅.
- P6 (l.7 `depends_on: [2]`) — parallelizable from P3 ✅; consumer name `gcs-archiver` provisions itself; no P3/P5 artifact required.
- P7 (l.7 `depends_on: [3,5,6]`) ✅ — needs all three services to ship charts.
- P8 (l.7 `depends_on: [7]`) ✅.
- P9 (l.7 `depends_on: [8]`) ✅ — explicitly the litmus, runs after full POC.

No backwards edges. Graph is buildable.

## Out-of-scope creep

None. Out-of-scope items in master.md:64-71 (UI/staging/multi-region/dedicated BQ sink/agent intelligence/ch#3+) do not appear as Steps or Files in any phase plan. P8:206-211 even repeats deferral of "second channel adapter" itself (with reasoning), which is what P9 solves — clean separation.

## Top 3 risks not yet captured

1. **Two-replica gateway (P7) × stream provisioning** — P3:392 acknowledges split-brain risk for `AddOrUpdateStream` across replicas, but P7 ships 2 replicas without surfacing the "single-writer / sequential rolling upgrade" precondition in the chart Steps. If two pods boot near-simultaneously with disagreeing config (mid-upgrade), the second pod's `AddOrUpdateStream` could mutate config. Add a Step in P7 §3 making rollout explicitly serial (`maxSurge: 0, maxUnavailable: 1`) — currently P7:161 says `maxSurge: 1, maxUnavailable: 0` which is the *opposite* (allows two replicas of new version concurrently with old).
2. **`outbound_state` lost across the 2-replica gateway** (P5:157-161). The in-memory map lives in *one* gateway pod. AI publishes "thinking…" → routed to pod A (writes `outbound_state[A]=X`) → AI publishes "answer" → routed to pod B (`outbound_state` empty → falls back to Send, not Edit). With multi-replica gateway in P7/P8, the documented `restart-during-edit` failure mode becomes a steady-state failure mode. Either pin sender-pool consumer to one gateway replica, or accept the failure-mode metric will be non-zero from day one (and persist to Postgres earlier than "when metric goes non-zero").
3. **PDB `minAvailable: 2` × 3-replica NATS × autoscaler scale-down** (P7:142,232) — autoscaler with `--max-graceful-termination-sec=120` plus PDB `minAvailable:2` will indefinitely block scale-down if drain hits PDB. P7 mentions "stall scale-down" but doesn't quantify cost: a 3-zone idle cluster running unconfigured for a weekend. Set `--max-graceful-termination-sec=600` ceiling and document operator action ("manual cordon then `kubectl drain --grace-period=600`").

## Recommended fixes (concrete edits)

- **`master.md:60`** — change `gs://mio-messages/channel=zoho-cliq/date=YYYY-MM-DD/` to `gs://mio-messages/channel_type=zoho_cliq/date=YYYY-MM-DD/`. Reason: Success Criterion still uses pre-realignment slug; P6:170 / P8:191 already use the correct form.
- **`docs/system-architecture.md:181`** (and examples l.193-195) — replace `mio.<direction>.<channel>.<workspace_id>.<thread_id>` block with `mio.<direction>.<channel_type>.<account_id>.<conversation_id>[.<message_id>]`; replace examples with the P9-style `mio.inbound.zoho_cliq.<account-uuid>.<conv-uuid>`. Reason: P2:67 explicitly flagged this. Doc is the design source of truth; consumers (researchers, future contributors) shouldn't see two truths.
- **`p3-gateway-cliq-inbound/plan.md:145`** — change `MaxAckPending=1 per account_id+conversation_id pair (graduation: subject-shard if needed)` to `MaxAckPending=1 globally (graduation: subject-shard by account_id+conversation_id when load forces)`. Reason: aligns with P4:56,64 + P7:110 + architecture-doc:213; per-key requires sharded consumers, which is the graduation path.
- **`p9-second-channel-adapter/plan.md:73, 93, 252`** — change `dispatcher.Register("slack", func(deps sender.Deps) sender.Adapter { return NewSender(deps) })` to `sender.RegisterAdapter(NewSender(/* config from env */))`. Drop the `dispatcher` symbol everywhere. Reason: P5:87 already locked the API as `sender.RegisterAdapter(Adapter)` instance-form in `sender/registry.go`. Consistency + P9 success-criterion P9:306 ("zero adapter-specific edits to dispatch.go") becomes physically true.
- **`p9-second-channel-adapter/plan.md:73`** in the bullet list — replace "calls `dispatcher.Register("slack", NewSender)` per P5 self-registration contract" with "calls `sender.RegisterAdapter(NewSender(...))` per P5 self-registration contract". Same reason.
- **`p3-gateway-cliq-inbound/plan.md:20, 243`** + **`p8-poc-deploy-gke/plan.md:142`** — change `/webhooks/zoho-cliq` to `/webhooks/zoho_cliq` (or change P7:219 to hyphen). Reason: pick one slug for URL path; P7:219's underscore form aligns with the `channel_type` registry slug, simpler ("URL slug == registry slug"); recommend underscore everywhere.
- **`p7-helm-and-nats-gke/plan.md:161`** — change `maxSurge: 1, maxUnavailable: 0` (gateway Deployment) to `maxSurge: 0, maxUnavailable: 1` until stream-config split-brain is resolved (P3:392 deferred risk). Reason: current values allow two new-version pods running concurrently with old; if config changed across versions, AddOrUpdateStream race materializes. Sequential rollout enforces single-writer.
- **`p9-second-channel-adapter/plan.md` Risks block** — add: "**Thread-root backfill** — for Slack threads where the parent message arrived before the bot was added, `thread_root_message_id` resolution requires lookup. POC carries raw `thread_ts` in `attributes`; backfill mechanism deferred to P10 with `MessageRelation` arrival." Reason: master.md Progress Log:133 lists this as one of the three deferred risks but P9 plan doesn't surface it in Risks/Out (only in step 7 prose at l.227).

## Open questions

- P9:332 — Slack outbound subject `mio.outbound.slack.<account_id>.<conversation_id>.<message_id>` is shown only with the trailing message_id segment (edit/delete commands). Does P9 need a non-edit outbound subject too? Check against P5 dispatch logic which builds the subject (P2 SDK side).
- P5:160 — `attributes["replaces_send_id"]` correlator semantics: is it the AI publisher's responsibility to set this, or does the SDK auto-fill from `edit_of_message_id`? P5 says both forms are valid ("treats the `attributes["replaces_send_id"]` correlator as authoritative") — clarify which form the echo-consumer (P4) demonstrates so MIU integrators have one canonical example.
- P7:202 — defers `kube-prometheus-stack` to P8 due to RAM, but P8:147 uses a *separate* tainted obs-pool node (not the original 3× e2-small). Was the deferral premise still valid, or should P7's cluster spec call out the obs-pool node-pool from day one to avoid a P8 surprise resize?

**Status:** DONE_WITH_CONCERNS
**Summary:** 12 cross-phase invariants checked: 9 ✅, 3 ⚠/❌. Five concrete drifts (1 doc, 4 plan), all 1-line edits. Two ship-blocking: P9 dispatcher API mismatch with P5; P3 ai-consumer MaxAckPending semantic divergence. Other fixes are polish.
**Concerns:** Master.md:60 + system-architecture.md:181 are stale relative to the realigned subject grammar — fix before P0 cook to avoid rework. P9's `dispatcher.Register` symbol may indicate the plan author drifted from P5 mid-write; resolve before P5 cook (P5 author needs to know the final API name).
