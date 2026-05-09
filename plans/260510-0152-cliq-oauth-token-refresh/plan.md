---
title: "mio-gateway: Cliq OAuth token refresh"
status: pending
goal: "Gateway sender auto-refreshes Zoho Cliq access tokens — eliminates 1h-uptime 401 failures."
created: 2026-05-10
mode: quick
---

# mio-gateway: Cliq OAuth token refresh

## Goal

Replace the static `CLIQ_BOT_TOKEN` env model with a refresh-token-backed `tokenSource` so the gateway sender keeps a valid Zoho access token for the lifetime of the pod, instead of failing every echo after the 1-hour TTL.

## Approach

Add `tokenSource` (mutex-protected cache of `accessToken` + `expiresAt`) that exchanges a long-lived refresh token for a fresh access token via `https://accounts.zoho.com/oauth/v2/token`. Refresh proactively when ≤5 min remaining. On 401 from a cached token, invalidate cache and retry once (handles "Zoho rotated my token early" edge); second 401 surfaces to the pool as terminal `auth` failure.

Decision baseline: bug report `infra/plans/reports/debug-260510-0147-mio-cliq-sender-401-no-refresh.md` (root cause + design tradeoffs already settled).

## Success Criteria

- [ ] Pod uptime >2h: zero `sender: 4xx — terminating reason=auth` log lines
- [ ] `mio_gateway_cliq_token_refresh_total{result="ok"}` ≥1 increment per ~55 min window
- [ ] Unit test passes: `TokenSource_RefreshesNearExpiry`, `Send_SelfHealsOn401`, `Send_SecondConsecutive401Terminates`
- [ ] Existing tests still pass: `TestSend_HTTP401_NotRetryable` updated to reflect "second 401 = Term"
- [ ] Secret in cluster has `CLIQ_CLIENT_ID`, `CLIQ_CLIENT_SECRET`, `CLIQ_REFRESH_TOKEN`
- [ ] `mio-gateway` deployment running new image SHA, healthy for ≥10 min post-rollout

## Status note (2026-05-10)

Phases 1 + 2 complete: code merged in mio repo, all tests green with `-race`,
reviewer findings addressed. Phase 3 deliberately held — user opted not to
rotate secret in this session. Production gateway still runs old image
(`92800f3...`) with the static-token bug; deploy blocked until OAuth creds
are minted and added to `infra/fluxcd/apps/prod/mio/secrets.enc.yaml`.

Bypass plan if needed before deploy: existing `CLIQ_BOT_TOKEN` can be rotated
to a fresh access token via `playground/cliq/setup-zoho-cliq-oauth.sh` — buys
~60 min of working echo on the OLD image, while the proper fix waits.

## Out of Scope

- Multi-tenant per-account tokens — POC stays single-bot; deferred to multi-tenant epic
- DB-backed credential store — env vars suffice; revisit when adding more channels
- Pool-level `retry-once-with-refresh` hook — keep self-heal in adapter; lift later if Slack/Teams need it
- DLQ for terminal auth failures — POC accepts message loss on real auth break
- Other channel adapters — Cliq only

## Phases

| # | Phase | Status | Depends on | Effort |
|---|---|---|---|---|
| 1 | [add-token-source-with-refresh](#phase-1-add-token-source-with-refresh) | completed | — | ~2h |
| 2 | [wire-adapter-and-self-heal-on-401](#phase-2-wire-adapter-and-self-heal-on-401) | completed | 1 | ~2h |
| 3 | [rotate-secret-and-deploy](#phase-3-rotate-secret-and-deploy) | blocked | 2 | ~30m |

## Constraints

- Stdlib only (`net/http`, `sync.Mutex`, `encoding/json`) — no new dependencies
- Keep `cliqMaxDeliver=5` and pool 4xx-classifier semantics unchanged
- Adapter `init()` self-registration pattern preserved
- SOPS age key: `infra/.secrets/age-key.txt` (per `~/.claude/CLAUDE.md`)
- Image pin: `fluxcd/apps/prod/mio/release-gateway.yaml:25` (gateway) + `:52` (migration init)

## Risks

- **Refresh-endpoint outage** during request → first request after expiry stalls 10s then errors. *Mitigation:* refresh proactively at 5-min remaining (not on-demand at expiry); add timeout 5s on refresh HTTP call separate from the 10s send timeout.
- **Concurrent refresh stampede** if N workers all see "stale" simultaneously → N requests to Zoho. *Mitigation:* mutex around refresh; double-check `expiresAt` after acquiring lock (singleflight pattern).
- **Refresh token revoked** (Zoho admin disables it) → repeated 401 on refresh endpoint. *Mitigation:* surface refresh failures with distinct error type (`refreshError`) — pool will Term, but log line says `reason=refresh_failed` not `auth`, so on-call knows to rotate the refresh token, not just the access token.
- **Secret rotation race during rollout** — pod reads old `CLIQ_BOT_TOKEN` env (now ignored), no refresh creds present yet → 401 on first send. *Mitigation:* Phase 3 sequences secret update *before* image bump; flux reconciles secret first.

## References

- Bug report: `/Users/vanducng/git/work/ab-spectrum/infra/plans/reports/debug-260510-0147-mio-cliq-sender-401-no-refresh.md`
- Working pattern (Python): `playground/cliq/poc-capture-channel-messages.py:86-110, 160-166`
- Setup script (refresh token mint): `playground/cliq/setup-zoho-cliq-oauth.sh`
- Pool 4xx classifier: `gateway/internal/sender/pool.go:220-329`

---

## Phase 1: add-token-source-with-refresh

**Priority:** P1 (blocker for Phase 2)
**Effort:** ~2h
**Depends on:** —

### Overview

Add a self-contained `tokenSource` type that fetches Zoho access tokens from a refresh token, caches them, and refreshes near expiry. No Adapter changes yet — phase ships as standalone testable unit.

### Files

- **Create:**
  - `gateway/internal/channels/zohocliq/token.go` (~80 lines)
  - `gateway/internal/channels/zohocliq/token_test.go` (~150 lines)

### Steps

1. Add `token.go` with:
   - `type tokenSource struct { clientID, clientSecret, refreshToken string; httpClient *http.Client; oauthURL string; mu sync.Mutex; current string; expiresAt time.Time; logger *slog.Logger; refreshTotal *prometheus.CounterVec }`
   - `func newTokenSource(clientID, clientSecret, refreshToken string, opts ...) *tokenSource` — opts allow override of `oauthURL` (for tests) and `httpClient`
   - `func (t *tokenSource) Get(ctx context.Context) (string, error)` — fast path: if `t.current != "" && time.Until(t.expiresAt) > 5*time.Minute` return cached. Else `t.refresh(ctx)`.
   - `func (t *tokenSource) Invalidate()` — sets `current=""`, `expiresAt=zero`. For self-heal in adapter.
   - `func (t *tokenSource) refresh(ctx context.Context) (string, error)` — POST to `oauthURL` with form `grant_type=refresh_token&client_id=...&client_secret=...&refresh_token=...`, parse `{"access_token":"...","expires_in":3600}`, store with `expiresAt = now + expires_in*sec - 30s safety`. Holds `t.mu` start-to-finish; double-checks `t.current` after lock acquire to dedupe concurrent callers.
   - `type refreshError struct { Status int; Body string }` with `Error() string` — distinct type so adapter can label `reason=refresh_failed` vs `reason=auth`.
2. Add `oauthDefaultURL = "https://accounts.zoho.com/oauth/v2/token"` constant.
3. Add Prometheus counter registration in a new exported `RegisterMetrics(reg prometheus.Registerer)` helper or wire via existing pool metric registration — pick whichever pattern the package already uses (check `pool.go:120-130` for precedent).
4. Add `token_test.go`:
   - `TestTokenSource_GetCachesUntilExpiry` — first call hits stub OAuth server, second call within TTL does not (httptest counts requests).
   - `TestTokenSource_RefreshesNearExpiry` — seed `expiresAt` to `now+4min`, call `Get`, assert refresh fired.
   - `TestTokenSource_ConcurrentGetsDedupe` — 10 goroutines call `Get` on cold cache; assert OAuth server saw exactly 1 request.
   - `TestTokenSource_RefreshFailureReturnsRefreshError` — OAuth server returns 401 (revoked refresh token); assert `errors.As(err, &refreshError{})`.
   - `TestTokenSource_InvalidateForcesRefetch` — warm cache, `Invalidate()`, next `Get` hits server again.

### Tests

- [ ] `go test ./gateway/internal/channels/zohocliq/... -run TokenSource -v` passes
- [ ] No race detector failures: `go test -race ./gateway/internal/channels/zohocliq/...`

### Success Criteria

- [ ] `tokenSource` is exercised only by tests in this phase (no production caller yet)
- [ ] Concurrent stampede test proves singleflight-by-mutex works
- [ ] Refresh failure produces typed error distinct from regular HTTP errors

### Risks

- **Mutex held during HTTP call** blocks all senders for refresh duration (~200ms typical). *Acceptable:* refreshes happen ~once/hour. If it becomes a hotspot, switch to `golang.org/x/sync/singleflight` later.

---

## Phase 2: wire-adapter-and-self-heal-on-401

**Priority:** P1
**Effort:** ~2h
**Depends on:** Phase 1

### Overview

Switch `Adapter` to consume `tokenSource` instead of a raw env-string token. Add one-shot self-heal: if Cliq returns 401, invalidate cache, retry the request once with a freshly-minted token; if that also returns 401, surface the typed error so the pool Terms the message with `reason=auth`.

### Files

- **Modify:**
  - `gateway/internal/channels/zohocliq/sender.go` (~30-line diff)
  - `gateway/internal/channels/zohocliq/init.go` (~5-line diff — fail-fast env check)
  - `gateway/internal/channels/zohocliq/sender_test.go` (~50 lines added; existing `TestSend_HTTP401_NotRetryable` updated)

### Steps

1. In `sender.go`:
   - Replace `botToken string` field with `tokens *tokenSource`.
   - In `NewAdapter()`: read `CLIQ_CLIENT_ID`, `CLIQ_CLIENT_SECRET`, `CLIQ_REFRESH_TOKEN` from env. If any missing, panic with explicit message (fail-fast at boot — better than silent 401 storm). Keep `CLIQ_API_BASE_URL` override unchanged.
   - In `Send()`:
     - Replace inline header-set with: `tok, err := a.tokens.Get(ctx); if err != nil { return "", err }`. If `err` is a `refreshError`, wrap so `IsRetryable()=false` and pool labels reason via the existing classifier (extend `classify4xx` to return `"refresh_failed"` when error type matches — touches `pool.go:312` lightly OR add a new `Reason() string` method on the error type that classify4xx checks first).
     - Wrap the HTTP send + status check in a small loop: try once; if returned error has `StatusCode()==401`, call `a.tokens.Invalidate()`, re-fetch token via `Get`, retry once. Bound the loop to **2 iterations max** — never spin.
     - Add metric `mio_gateway_cliq_send_self_healed_total` incremented when the second attempt succeeds.
2. In `init.go`: keep self-registration; rely on `NewAdapter()` panic for the env check (no separate validation needed).
3. In `sender_test.go`:
   - **Update** `TestSend_HTTP401_NotRetryable` → rename to `TestSend_SecondConsecutive401Terminates`. Stub OAuth server returns valid token; stub Cliq server returns 401 always. Assert `Send` returns `*HTTPError` with `Code=401` and `IsRetryable()=false`.
   - **Add** `TestSend_SelfHealsOn401`: Cliq stub returns 401 on attempt 1, 200 on attempt 2; assert `Send` returns success and `Invalidate` was called exactly once (count via stub OAuth server hits = 2: initial Get + post-invalidate refetch).
   - **Add** `TestSend_RefreshFailureSurfacesAsRefreshFailed`: OAuth stub returns 400; assert error labels as `refresh_failed` per classifier.
   - **Add** `TestNewAdapter_PanicsOnMissingRefreshCreds`: t.Setenv with each of the 3 vars missing, assert `NewAdapter` panics.
4. Verify `go vet ./gateway/...` and `go build ./gateway/...` clean.

### Tests

- [ ] All `zohocliq` package tests pass with `-race`
- [ ] Existing pool tests (`gateway/internal/sender/...`) untouched and still pass
- [ ] Manual: `go run gateway/cmd/...` boots locally with valid env vars; integration test in `gateway/integration_test/` still green

### Success Criteria

- [ ] One round-trip 401 inside `Send` → recovered transparently, message delivered
- [ ] Two consecutive 401s → message Term'd with `reason=auth` (current behaviour preserved for genuine auth failures)
- [ ] Refresh-endpoint failure → message Term'd with `reason=refresh_failed` (new label, distinguishable in metrics)
- [ ] Missing env vars at boot → pod CrashLoopBackOff with clear log message (better than 401 storm)

### Risks

- **Self-heal loop bounded at 2** — if Zoho rotates tokens mid-request twice, we Term spuriously. *Acceptable:* rotation cadence is 1h, two rotations within one Send call is implausible; the pool's redelivery (`MaxDeliver=5`) covers any residual case.
- **Classifier extension** touches `pool.go` — keep change minimal (one switch case or a `Reason()` method check) to avoid scope creep into pool semantics.

---

## Phase 3: rotate-secret-and-deploy

**Priority:** P1
**Effort:** ~30m
**Depends on:** Phase 2

### Overview

Mint fresh OAuth credentials, update the SOPS-encrypted secret in the infra repo with the three new keys, bump the image tag in the HelmRelease, reconcile flux, verify rollout. Order matters: secret first, then image — otherwise new pod boots with old secret schema and panics on missing env vars.

### Files (in infra repo, not mio)

- **Modify:**
  - `infra/fluxcd/apps/prod/mio/secrets.enc.yaml` (SOPS-encrypted; add 3 keys, optionally remove `CLIQ_BOT_TOKEN` once new pod healthy)
  - `infra/fluxcd/apps/prod/mio/release-gateway.yaml:25` and `:52` (bump tag to new commit SHA from Phase 2 merge)

### Steps

1. **Mint refresh token + capture client creds:** in `mio` repo, run `playground/cliq/setup-zoho-cliq-oauth.sh` and copy `ZOHO_CLIQ_REFRESH_TOKEN`, `ZOHO_CLIQ_CLIENT_ID`, `ZOHO_CLIQ_CLIENT_SECRET` from `playground/cliq/secrets.env`.
2. **Edit secret in infra repo** (one commit — add new keys, remove old `CLIQ_BOT_TOKEN`):
   ```bash
   cd /Users/vanducng/git/work/ab-spectrum/infra
   SOPS_AGE_KEY_FILE=.secrets/age-key.txt sops fluxcd/apps/prod/mio/secrets.enc.yaml
   ```
   Replace `CLIQ_BOT_TOKEN` line with three new keys under `stringData:`:
   ```yaml
   CLIQ_CLIENT_ID: <from playground secrets.env>
   CLIQ_CLIENT_SECRET: <from playground secrets.env>
   CLIQ_REFRESH_TOKEN: <from playground secrets.env>
   ```
   **Why safe to remove `CLIQ_BOT_TOKEN` now:** existing pod has its env-var snapshot from boot — secret edits don't propagate to running pods. New pod (after image bump in step 4) reads only the new keys. No fallback retained.
3. **Push gateway image:** ensure Phase 2 PR is merged + CI built `ghcr.io/vanducng/mio/gateway:<new-sha>`. Confirm with `docker manifest inspect ghcr.io/vanducng/mio/gateway:<sha>`.
4. **Bump tag in `release-gateway.yaml`:** lines 25 (`image.tag`) and 52 (`migration.image.tag`) → new SHA.
5. **Commit infra changes** in two separate commits for blast-radius isolation:
   - `chore(mio): swap cliq bot token for oauth refresh credentials`
   - `chore(mio): bump gateway image to <short-sha> for oauth refresh`
6. **Reconcile flux:**
   ```bash
   flux reconcile source git flux-system
   flux reconcile kustomization apps-prod-mio
   flux reconcile helmrelease mio-gateway -n mio
   ```
7. **Verify rollout:**
   ```bash
   kubectl --context gke_dp-prod-7e26_us-central1-a_prod -n mio rollout status deploy/mio-gateway
   kubectl --context gke_dp-prod-7e26_us-central1-a_prod -n mio logs deploy/mio-gateway --tail=50 | grep -i 'token'
   ```
   Expect: pod ready in <90s, no panic, at least one `cliq: token refreshed` log within first send.
8. **Smoke test:** send a test message in the Cliq channel; verify echo arrives. Re-test 65 min later to confirm refresh kicked in.

### Tests

- [ ] `kubectl rollout status` returns success without restart loops
- [ ] First echo after rollout → 200, logged `cliq: token refreshed` once
- [ ] 65-min-later echo still works (proves refresh triggered)
- [ ] `mio_gateway_outbound_terminated_total{reason="auth"}` flat (no new increments)
- [ ] `mio_gateway_cliq_token_refresh_total{result="ok"}` ≥ 1 within first hour, ≥ 2 within 90 min

### Success Criteria

- [ ] Production gateway running new image, healthy >10 min
- [ ] Echo verified working at T+0 and T+65min
- [ ] Metrics confirm refresh path exercised
- [ ] No `oauthtoken_invalid` errors in logs after rollout

### Risks

- **Wrong-order rollout** (image bumped before secret) → new pod panics on missing env. *Mitigation:* commit/reconcile secret first, wait 30s for kustomization to apply, then bump image.
- **Refresh token mints with wrong scopes** → all sends fail post-refresh. *Mitigation:* `setup-zoho-cliq-oauth.sh` defaults to `ZohoCliq.Webhooks.CREATE,Channels.ALL,Messages.CREATE` — sufficient. Verify the granted scope in OAuth response includes `Messages.CREATE` before bumping image.

---

## Handoff

After Phase 3 verification window (90 min watching metrics), this plan is done. Follow-ups for separate plans:

- Multi-tenant credential store (when adding 2nd Cliq tenant or 1st Slack tenant)
- Pool-level retry-once-with-refresh hook (when 2nd OAuth-style channel arrives)
- DLQ for `reason=refresh_failed` (when refresh-token revocation becomes a real ops scenario)
