---
type: review
date: 2026-05-08 12:13
scope: plans/260507-0904-mio-bootstrap (P0–P9) + docs/system-architecture.md
predecessor: review-260508-1133-bootstrap-plan-audit.md
mode: pre-cook readiness, fixes applied in place
---

# MIO Bootstrap — Pre-Cook Readiness Audit

## TL;DR

**Verdict: ready-to-cook after this round of fixes.** The 11:39 audit caught the cross-phase invariant drifts. This pass found 7 more gaps the prior audit missed — **3 are P0/P1/P3 cook-blockers** (init.sql non-idempotent, .mise.toml invalid TOML, tools/proto-roundtrip module isolation, P3 inbound subject grammar mismatch). All 7 fixed in place. Headline: **P3 was about to publish a 5-token inbound subject (`...<conv>.<msg_id>`) but the P2 SDK exposes a 4-token-only `Inbound(channelType, accountID, conversationID)` builder** — that would have failed at compile, not at smoke-test. Other patches: 4 stale spots in `docs/system-architecture.md` the prior audit missed (line 105 diagram + line 231 unique-constraint + line 274 partition + lines 337-341 metric labels), P4 schema-Verify-on-consume contradiction with P2, P5 missing `Dispatcher` type definition.

## Gaps found (and fixed)

### Cook-blockers

| # | Phase | Gap | Why it would bite |
|---|---|---|---|
| 1 | P0 | `init.sql` had `CREATE ROLE mio_app … ; CREATE DATABASE mio …` | Postgres entrypoint creates role + DB from `POSTGRES_USER`/`POSTGRES_DB` **before** `/docker-entrypoint-initdb.d/*.sql` runs. Statements would error on first cold boot (`role "mio_app" already exists`). No `IF NOT EXISTS` syntax exists for those. P0 step 13 smoke check (`make down -v && make up`) would fail. |
| 2 | P0 | `.mise.toml` step 9 used `[tasks.up] = { run = "make up", ... }` | Invalid TOML — table headers cannot have `=`. `mise install` / `mise tasks ls` would refuse to parse the file before any other P0 work could land. |
| 3 | P1 | `tools/proto-roundtrip/go.mod` as a separate module | Tool needs to import `github.com/vanducng/mio/proto/gen/go/mio/v1`. Separate module sees the root only via a `replace` directive **or** a `go.work`, but `go.work` is gitignored (P0 step 2). CI would build it from the separate module, fail to resolve, exit non-zero. |
| 4 | P3 | Inbound publish subject was `mio.inbound.<ct>.<acct>.<conv>.<message_id>` (5 tokens) | P2 SDK `subjects.Inbound(channelType, accountID, conversationID)` only takes 3 args → 4-token subject. Arch-doc §5 reserves the 5th `.<message_id>` segment for outbound edit/delete only. Gateway code would not compile against the SDK; if someone hand-built the subject to bypass, P9's `mio.inbound.slack.>` filter still works (`>` matches any depth), but two channels would have inconsistent token counts and the architecture promise breaks. |

### Plan / doc drifts (non-blocking but corrosive)

| # | Where | Gap |
|---|---|---|
| 5 | `docs/system-architecture.md` | The 11:39 audit (review-260508-1133) said §5 was patched, but **4 other spots in the same doc still carried the pre-realignment terms** — line 105 sequence diagram (`(channel, source_message_id)`), line 231 unique-constraint prose (same), line 274 GCS partition (`channel=<channel>`), and lines 337-341 metric labels (`{channel,outcome}` / `{channel,workspace,outcome}` — the `workspace` label is a forbidden cardinality bomb per P2:121, P3:336, P5:205). Plus minor terminology drift: pd-ssd vs P7's pd-balanced, per-workspace vs P5's per-`account_id`, thread_id vs P3:147's per-conversation graduation shard. |
| 6 | P4 | Skeleton comment + Risks + Research-backing all said "sdk-py Verify guards on consume" / "SDK Verify rejects before `handle()` is called". P2 explicitly locks Verify as **publish-only** (P2:140, 200, 217 — consume-side passes through for forward-compat). Result: the success criterion accidentally tested the publish path while the comment claimed consume-side rejection — readers would pick up the wrong contract during cook and try to add consume-side Verify to the SDK. |
| 7 | P5 | Step 2 worker code: `dispatcher.ForCommand(cmd).Send/Edit`. Nowhere in P5 was a `Dispatcher` type defined — Files only listed `dispatch.go` "registry-backed lookup" and `registry.go` `RegisterAdapter`/`RegisteredAdapters`. A coder following the plan would not know the constructor signature, where to wire it from `main.go`, or what to do with an unregistered `channel_type`. |

## Fixes applied (concrete edits)

- **`p0-reserve-and-scaffold/plan.md` step 7** — `init.sql` rewritten as comment-only placeholder (with explicit "do NOT add CREATE ROLE / CREATE DATABASE here" warning + DO-block recipe for any future additional roles).
- **`p0-reserve-and-scaffold/plan.md` step 9** — `.mise.toml` example uses `[tasks.up]` section headers; called out invalid `[tasks.up] = {…}` form so it doesn't reappear.
- **`p1-proto-v1-envelope/plan.md` Files + step 6 + Success Criterion** — round-trip Go half lives in the root module; Python half via `uv run` against `sdk-py`'s pinned protobuf; protobuf pinned once in root `go.mod`.
- **`p3-gateway-cliq-inbound/plan.md` step 13.6 + Success Criterion** — inbound subject corrected to 4-token `mio.inbound.<ct>.<acct>.<conv>`; explicit note that the optional 5th `.<message_id>` is outbound-only and the SDK builder is the gate.
- **`docs/system-architecture.md`** — 4 spots fixed (line 105 diagram, line 231 unique-constraint, line 274 partition, lines 337-341 metric-label table) + cardinality-discipline note added before the metrics table; pd-ssd → pd-balanced (×3 in node labels); per-workspace → per-`account_id` rate-limit prose; thread_id → conversation_id graduation prose.
- **`p4-echo-consumer/plan.md` skeleton + Risks + Research-backing + Success Criterion** — schema-Verify asymmetry made explicit (publish-side only; consume passes through; defense-in-depth consume-Verify is P5 not P4).
- **`p5-outbound-path-cliq/plan.md` Files + step 2** — `dispatch.go` defines `type Dispatcher struct { byChannel map[string]Adapter }` + `New(adapters []Adapter) *Dispatcher` (panics on dup slug) + `ForCommand(cmd) Adapter`; `main.go` builds it once after all `init()`s via `sender.RegisteredAdapters()`; unregistered `channel_type` → `Term` `reason="other"`.
- **`master.md` Progress Log + Revisions table** — full record of this round.

## Cross-phase invariants — re-checked

| # | Invariant | Status now |
|---|---|---|
| 1 | Subject grammar `mio.<dir>.<channel_type>.<account_id>.<conversation_id>[.<message_id>]` (msg_id outbound-only) | ✅ — prior arch-doc §5 patch held; **P3 inbound subject corrected to 4-token here** (was the missing piece) |
| 2 | Idempotency address `(account_id, source_message_id)` | ✅ — **arch-doc line 105 + 231 patched here** (prior audit missed) |
| 3 | Metric labels `{channel_type, direction, outcome}` only | ✅ — **arch-doc §10 metric table patched here** (`workspace` was the leftover cardinality bomb) |
| 4 | Schema-version: enforce on publish, skip on consume | ✅ — **P4 reconciled here** (was contradicting itself) |
| 5 | Stream/consumer provisioning: gateway authoritative | ✅ — held |
| 6 | Adapter self-registration via `init()` (`sender.RegisterAdapter`) | ✅ — held; **P5 `Dispatcher` type wired here** |
| 7 | Sink filename offset-based | ✅ — held |
| 8 | ConversationKind: boolean flags, NOT prefixes | ✅ — held |
| 9 | Cliq event id derivation (P3 Step 0) | ✅ — held |
| 10 | P3 Step 0 gate intact | ✅ — held |
| 11 | Out-of-scope creep | ✅ — held |
| 12 | Three deferred risks tracked | ✅ — held |

## Cook-readiness note for P0

After this round, P0 is independently executable end-to-end without judgement calls:
- All file contents specified down to TOML / SQL syntax.
- Smoke-verify (step 13) is now achievable (init.sql idempotent, .mise.toml parses).
- `make up` → `docker compose ps` → 3× `(healthy)` works on a fresh clone.
- Pushing to `vanducng/mio` lands a clean repo skeleton.

## Open questions (none new — carry-overs only)

- P5:160 — `attributes["replaces_send_id"]` correlator authoring (AI vs SDK auto-fill). Same as 11:39 audit; design choice deferred to P5 cook-time decision.
- P9:332 — Slack outbound subject example shows only the 5-token form. Non-edit Slack outbound builds via `subjects.Outbound(...)` with no `messageID` arg → 4-token. Plan does not call this out; not blocking.
- P7:202 — kube-prometheus-stack defer to P8. P8 brings up a separate tainted obs-pool; the original premise (3× e2-small RAM tight) still holds, but P7's cluster-spec doesn't pre-declare obs-pool. P8 step 5.1 creates it. Not blocking, mild surprise during P8 cook.

**Status:** DONE
**Summary:** 7 gaps fixed in place (3 cook-blockers + 4 drift/contradiction). All cross-phase invariants now hold across plan + design doc. P0 independently cookable.
**Concerns:** None ship-blocking. Three carry-over open questions resurface during their own phase cook; none affect P0.
