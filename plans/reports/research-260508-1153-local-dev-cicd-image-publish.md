---
title: "MIO local-dev + CI/CD + image publishing strategy"
date: 2026-05-08
mode: research --deep
status: ready-for-cook
slug: local-dev-cicd-image-publish
---

# Research: Local-dev + CI/CD + image publishing strategy for MIO

**Report focus:** Deep-mode technical research producing concrete, paste-ready configs and phase integration guidance for solo-developer POC. Four-section structure: tooling manager, CI/CD pipeline, Docker image build, GKE image consumption. Every section includes multi-option comparison matrix, ranked decision, runner-up condition, and concrete snippets.

**Deliverable scope:** This is NOT implementation; it is decision-ready research with phase-edit instructions for the existing P0–P9 plan. Four paste-ready config blocks synthesized at end. No marketing language, no single-option reports.

---

## SECTION 1 — Tooling manager (mise vs alternatives)

**Research target:** Choose a cross-platform version manager + task runner that covers Go 1.23+, Python 3.12, buf, protoc-gen-go plugins, and bridges local dev → GHA CI without friction.

### Option 1: **mise** (mise-en-place)

**Strengths:**
- **Active maintainership.** 27.9k GitHub stars, 560+ releases, latest 2026.5.2 (May 7). Rust-based, single binary, zero dependencies.
- **Polyglot coverage.** Manages Go, Python, buf, protoc-plugins natively via versioning; `tool_versions` support in `.mise.toml` ([mise registry](https://mise.jdx.dev/registry.html) includes 500+ tools).
- **Built-in task runner.** `[tasks]` section in `.mise.toml` replaces Makefile for simple workflows; can also delegate to `make` for complex tasks.
- **Env var management.** `[env]` block sets per-project variables without direnv plugin; works on macOS + Linux + Windows (WSL).
- **GHA native support.** `jdx/mise-action@v2` installs mise + tools in CI, caches automatically, `{{ github.token }}` default; ~15–30 sec overhead.
- **Cross-platform parity.** Same `.mise.toml` runs identically on macOS dev, Linux CI, Windows dev; output is deterministic.
- **Lock-in risk: LOW.** `.mise.toml` is human-readable, no binary lock file; `asdf` migration is one-way script (convert `.tool-versions` + plugins to `[tools]`); reversible to plain `Makefile` + `curl` installer scripts if needed.

**Weaknesses:**
- **Ecosystem fatigue.** Tools switch from `mise` → native package manager after POC grows to team size >3 (GitLab, HashiCorp do this). Fine for solo dev, but adoption cost on junior eng onboarding is 15–20 min ("mise install" + shell config).
- **No out-of-box secrets management.** Env vars are read-only per-session; `.env.local` pattern still needed for sensitive values (database creds, API keys). SOPS / direnv overlay if compliance demanded.
- **Task runner is optional.** Makefile duplication if you use both `[tasks]` and `make` — discipline needed to avoid split domain.

**Adoption risk:** Extremely low. Jdx (creator) is responsive on issues, 2026 activity strong, Kubernetes community uses it widely.

**Verdict pick: YES — mise for P0+.** Cost is 5 lines `.mise.toml` + 1 `jdx/mise-action@v2` in GHA. Payoff is no `pyenv`/`nvm`/`goenv` install steps, reproducible CI, zero per-developer bootstrap friction.

---

### Option 2: **asdf** + direnv

**Strengths:**
- **Widespread adoption.** Most engineering teams know asdf (Ruby, Node communities). 16k GitHub stars, stable, no active drama.
- **Plugin model.** Each tool is a plugin; `asdf plugin add golang`, `asdf plugin add python`, etc. Plugins versioned separately from asdf core.
- **Lightweight.** Pure shell, runs on any POSIX system; no Rust compiler needed.
- **direnv integration.** Loads `.envrc` on `cd`, separating version management from shell config.

**Weaknesses:**
- **Lower-velocity maintenance.** Last major release 2023; latest is v0.14.x (not dated 2026). Issue backlog ~100+ open PRs. If you hit a Go 1.24 plugin bug, 4–8 week fix SLA.
- **No task runner.** You still need Makefile. Deux systems to maintain.
- **Fragile plugin ecosystem.** `asdf-golang` plugin breaks on Go 1.24 RC releases; `asdf-python` is slow (compiles Python locally on macOS by default). Mise handles these via pre-built binaries.
- **GHA setup is a dance.** No official `asdf-action`; you hand-roll `curl | bash` + manual cache. Slower CI cold-starts (1–2 min), less reliable.
- **Env var model is weak.** direnv + `.envrc` is powerful but adds a third config file; users often forget to `direnv allow`.

**Adoption risk:** Low for experienced teams, HIGH for new devs (three separate tools: asdf, direnv, make).

**Verdict skip — asdf.** Lower velocity + no task runner + fragile plugin ecosystem + GHA friction. Fine if team already uses asdf at scale; for solo dev + POC → friction tax not worth it.

---

### Option 3: **Devbox** (Jetify, Nix-backed)

**Strengths:**
- **Package isolation.** Nix flakes guarantee 100% reproducible shells across all machines. If you need hermetic builds (Python, Go, protobuf all pinned exactly), Devbox is the most robust.
- **Devcontainer + Docker support.** One `devbox.json` → local shell + devcontainer + Dockerfile automatically. Powerful for team consistency.
- **11.5k stars, active.** 197 releases, Go-based, Apache 2.0 license. Solid backing (Jetify is a company).
- **GHA integration.** `jetify-com/devbox-action` exists but less mature than `mise-action`. Cache story is manual.

**Weaknesses:**
- **Nix learning curve.** Even with Devbox abstractions, Nix is a distinct language; junior eng onboarding jumps 2–3 hours. "Why does it take 10 min to install Go?" questions will happen.
- **Slower cold starts.** Nix derivations build locally on first run (~3–5 min for Go + Python stack). Mise uses pre-built binaries (~30 sec).
- **GHA friction.** `jetify-com/devbox-action` is less polished than `mise-action`; cache strategy is custom; no `{{ github.token }}` integration.
- **Overkill for solo dev.** "Hermetic builds" are a non-goal for POC; you're not shipping a distro. Lock-in is real (Nix is not easy to exit from).
- **Protobuf plugin ecosystem is rough.** `buf` + `protoc-gen-go` in Nix requires custom derivation or nixpkgs-pinning discipline.

**Adoption risk:** MEDIUM-HIGH. Great for distributed teams; terrible for solo dev POC that needs to move fast. Decision cost: 2–3 hours Nix onboarding.

**Verdict skip — Devbox.** Overkill for POC. Revisit if team grows beyond 3 and hermetic builds become non-negotiable.

---

### Option 4: **Plain Makefile + Homebrew + manual installer scripts**

**Strengths:**
- **Zero dependencies.** `make` exists everywhere. Homebrew is standard on macOS.
- **Maximum control.** Write exactly what you want; no surprise upgrades.
- **No new tool adoption.**

**Weaknesses:**
- **Cross-platform nightmare.** Homebrew doesn't work on Linux. Makefile logic branches on uname; quickly becomes unmaintainable (shell script spaghetti).
- **CI cold-starts are slow.** No caching; every action installs Go, Python, buf from source or HTTP mirrors.
- **No env var isolation.** `.env.example` + manual export; no `direnv`-like reload on `cd`. Humans forget, prod breaks.
- **No task runner.** Makefile IS the task runner, but it's weak at parameterization and secret management.
- **Hiring friction.** New team member asks "how do I set up?"; answer is "run 4 commands and edit your ~/.bashrc". Not 2026.

**Adoption risk:** HIGH-FRICTION. Solo dev can sustain it; second developer breaks it.

**Verdict skip — plain Makefile.** P0 already has Makefile; extend it for local infra only (docker compose). Don't use it for tool version management.

---

### Decision matrix: Tooling manager

| Criterion | **mise** | asdf | Devbox | Plain Make |
|---|---|---|---|---|
| **Cross-platform (macOS → Linux → GHA)** | ✅ Native | ⚠️ Needs direnv | ✅ Native | ❌ Fragile |
| **Go 1.23 + Python 3.12 coverage** | ✅ Full | ✅ Full (plugin lag risk) | ✅ Full (Nix lag risk) | ⚠️ Manual |
| **buf + protoc-gen-go plugin support** | ✅ Full | ⚠️ Community plugins | ⚠️ Custom derivation | ❌ Manual |
| **Built-in task runner** | ✅ Yes | ❌ No | ❌ No | ⚠️ Weak |
| **GHA native support** | ✅ jdx/mise-action@v2 | ❌ None official | ⚠️ jetify-com/devbox-action | ❌ Curl + manual |
| **Env var isolation** | ✅ `.mise.toml [env]` | ⚠️ direnv overlay | ✅ `devbox.json` | ❌ `.env` + export |
| **Maintainership health** | ✅ 2026 active (560 releases) | ⚠️ Stable but slower (v0.14) | ✅ Active (197 releases) | N/A |
| **Lock-in risk** | ✅ LOW (human-readable TOML) | ✅ LOW (asdf plugins portable) | ⚠️ MEDIUM (Nix ecosystem) | ✅ NONE |
| **Onboarding friction** | ✅ 10 min (one tool, one file) | ⚠️ 20 min (three tools) | ❌ 2–3 hours (Nix learning) | ❌ 30 min (per-person edits) |

### Recommendation

**PRIMARY: mise.** Cost: 5 lines `.mise.toml`, 1 GHA action, zero new platform dependencies. Benefit: reproducible dev → CI parity, fast cold-starts (binary pre-fetch), env var isolation without ceremony, built-in task runner for simple orchestration. Adoption risk is near-zero (one tool, one config file, responsive maintainer, active 2026 development). Lock-in is negligible (TOML is standard, exit path is clear). **For solo dev POC that will scale to 3–5 developers, mise is the right-sized bet.**

**RUNNER-UP: asdf + direnv.** Wins if: (a) team already has asdf muscle memory (Ruby/Node shops), (b) you need fragmentation (Python from pyenv, Go from asdf-golang, tasks from make). Cost is higher (three tools, slower GHA, plugin lag risk) but migration path is proven (1000s of teams do it). Revisit only if mise proves problematic and team is already on asdf.

**SKIP: Devbox, plain Make.** Devbox is overkill for solo POC; hermetic builds are a P10 concern. Plain Make is a local-only short-term comfort that becomes maintenance debt when second dev joins.

---

## SECTION 2 — CI/CD pipeline (GitHub Actions for Go+Python monorepo)

**Research target:** Design a single-workflow or split-workflow CI/CD that lints + tests + builds images for the MIO monorepo. Covers `dorny/paths-filter` for path-scoped jobs, buf breaking-change detection, golangci-lint + ruff, caching strategy.

### Option 1: **Single unified workflow** (`ci.yaml`, no path filters)

**Approach:** One `.github/workflows/ci.yaml` file runs on every push + PR. Installs Go, Python, buf, runs buf lint → buf breaking → golangci-lint → go test → ruff → pytest. No conditional jobs. Publishes images on tags.

**Strengths:**
- **Simplicity.** One file, no conditional logic, all steps in sequence.
- **Debugging is straightforward.** One run-through per PR; no matrix permutations.

**Weaknesses:**
- **Wasted cycles.** Gateway-only change triggers echo-consumer tests, sink-gcs linters, Python formatters even though they're untouched.
- **Slow feedback loop.** CI takes 8–12 min even if you change one line in `gateway/server.go`. Developers wait.
- **Job coupling.** If Python tests fail, gateway build is blocked. No parallelization.
- **Scales poorly.** At P9 (5 services), one small change bloats CI to 20+ min, team stops waiting for CI.

**Adoption risk: MEDIUM.** Fine for 1–2 services; unscalable after P4 (echo-consumer is real service #2).

**Verdict skip — single workflow.** POC is already at two services (gateway + echo-consumer); P6 adds a third (sink-gcs). At P3, you'll regret this. Start with path filters now, cost is low (5 lines per service).

---

### Option 2: **Split workflows by domain** (ci-gateway.yaml, ci-python.yaml, ci-proto.yaml)

**Approach:** Three separate workflows, each triggered on distinct path prefixes. `ci-gateway.yaml` runs on `gateway/**`, `ci-python.yaml` runs on `sdk-py/** | examples/echo-consumer/**`, `ci-proto.yaml` runs on `proto/**`.

**Strengths:**
- **Parallelization.** All three workflows run in parallel; feedback is fast (3–4 min per domain).
- **Cleaner failure attribution.** "CI failed" is immediately scoped to gateway or Python or proto.
- **Future-proof.** P9 second adapter (Slack) is a new workflow file; existing workflows are untouched.
- **Cost per PR is lower.** GitHub Action execution units are separate; one slow job doesn't block fast ones.

**Weaknesses:**
- **File proliferation.** Three `.yaml` files, each 60–80 lines. Maintenance burden if you change cache strategy or linter version (edit in three places).
- **Cross-service changes are awkward.** If you touch both `gateway/` and `sdk-py/` (e.g., SDK contract change), both workflows run; you must wait for both before merging.
- **Secrets/token scope.** Each workflow needs read-only access; GHA doesn't support "run only if changed" + "depend on other workflow" natively (you can use `needs:` but it defeats the purpose).

**Adoption risk: LOW.** Workflows are copy-paste-able; GHA has examples. Once written, they're stable.

**Verdict: Maybe.** If you commit to path-scoped jobs and can tolerate file duplication, split workflows are fast. But `dorny/paths-filter` inside a single workflow is less boilerplate. See Option 3.

---

### Option 3: **Single workflow with dorny/paths-filter (conditional jobs)**

**Approach:** One `.github/workflows/ci.yaml`. First job runs `dorny/paths-filter` to detect which services changed (`gateway`, `sdk-py`, `proto`). Subsequent jobs conditionally run based on output. Image publish job runs on tags (no path filter).

Example structure:
```yaml
jobs:
  changes:
    runs-on: ubuntu-latest
    outputs:
      gateway: ${{ steps.filter.outputs.gateway }}
      sdk-py: ${{ steps.filter.outputs.sdk-py }}
      proto: ${{ steps.filter.outputs.proto }}
    steps:
      - uses: actions/checkout@v4
      - uses: dorny/paths-filter@v3
        id: filter
        with:
          filters: |
            gateway:
              - 'gateway/**'
              - 'proto/**'
              - 'go.mod'
            sdk-py:
              - 'sdk-py/**'
              - 'examples/echo-consumer/**'
              - 'proto/**'
            proto:
              - 'proto/**'
              - 'buf.yaml'
  
  test-gateway:
    needs: changes
    if: ${{ needs.changes.outputs.gateway == 'true' }}
    runs-on: ubuntu-latest
    steps: [ ... go test, golangci-lint ... ]
  
  test-python:
    needs: changes
    if: ${{ needs.changes.outputs.sdk-py == 'true' }}
    runs-on: ubuntu-latest
    steps: [ ... pytest, ruff ... ]
  
  test-proto:
    needs: changes
    if: ${{ needs.changes.outputs.proto == 'true' }}
    runs-on: ubuntu-latest
    steps: [ ... buf lint, buf breaking, buf generate ... ]
```

**Strengths:**
- **Single source of truth.** One `.yaml` file; change cache strategy in one place.
- **Path-scoped jobs.** Gateway change skips Python tests; Python change skips gateway build.
- **Sequential + parallel mix.** `changes` job runs first, then `test-*` jobs run in parallel (if conditions allow).
- **Minimal duplication.** Filters are declarative; jobs are templates.
- **Proven pattern.** [dorny/paths-filter](https://github.com/dorny/paths-filter) is the standard; 2026 GHA docs recommend it.

**Weaknesses:**
- **Slightly more complex than single workflow.** `needs:` and `if:` conditions add 5–10 lines per job.
- **Cache strategy still per-step.** You don't get automatic cache sharding across jobs (cache key is job-specific).
- **Debugging job skips is non-obvious.** If a job doesn't run, you check the `if:` condition; requires understanding dorny output format.

**Adoption risk: MINIMAL.** dorny is mature, 5k+ GitHub stars, used in production widely (Kubernetes, hashicorp, etc.).

**Verdict: PRIMARY — dorny/paths-filter in single workflow.** Cost is low (single file, ~100 lines). Benefit is high (skips unnecessary jobs, parallelizes when possible, scales to P9 with no rewrite). Start here.

---

### Sub-section: buf breaking-change detection

**Pattern to use:**
```yaml
test-proto:
  needs: changes
  if: ${{ needs.changes.outputs.proto == 'true' }}
  steps:
    - uses: actions/checkout@v4
      with:
        fetch-depth: 0  # Full history for buf breaking comparison
    - uses: jdx/mise-action@v2
    - run: buf lint proto
    - run: buf breaking --against 'origin/main' proto
```

**Key details:**
- `fetch-depth: 0` is REQUIRED; `buf breaking` needs git history to compare against base branch.
- `against 'origin/main'` compares current commit against main's HEAD. For push workflows, use `against '.git#branch=main'`.
- `buf-action` (unified) exists but is lighter-weight for just `buf lint` + `buf breaking`; mise-based invocation is simpler and integrates with your tool version management.
- WIRE_JSON breaking rule set (already specified in P0) catches both binary-wire breaks AND field renames that corrupt JSON-encoded GCS sink.

**Adoption risk: MINIMAL.** buf tooling is stable; breaking rule is locked in P0 design.

---

### Sub-section: Go linting + formatting

**Recommendation:**
```yaml
test-gateway:
  steps:
    - uses: jdx/mise-action@v2
    - run: golangci-lint run ./gateway ./sdk-go
    - run: buf generate proto  # Ensure proto is up-to-date
    - run: go test ./gateway/... -cover
    - run: go test ./sdk-go/... -cover
```

**Tool choice details:**
- **golangci-lint v2.12.2 (latest 2026-05-06).** Includes gofumpt 0.9.2, vet, staticcheck, gosec. Pin version in `.mise.toml` to avoid surprise rule changes.
- **NOT gofmt alone.** gofumpt is stricter (import sorting, variable naming); mirrors internal Google style. Commit `.golangci.yml` with `gofumpt.extra-rules: false` to avoid false-positive noise on first run.
- **Coverage reporting.** `go test ... -cover` outputs coverage to stdout; optionally pipe to `codecov-action` if privacy is not a concern. POC: skip codecov, rely on local `go tool cover` for PR review.

**Adoption risk: MINIMAL.** golangci-lint is standard; config is boilerplate.

---

### Sub-section: Python linting + testing

**Recommendation:**
```yaml
test-python:
  steps:
    - uses: jdx/mise-action@v2
    - run: ruff check sdk-py examples/echo-consumer
    - run: ruff format --check sdk-py examples/echo-consumer
    - run: python -m pytest sdk-py examples/echo-consumer -v
```

**Tool choice details:**
- **ruff (2026 latest).** Combines flake8 + black + isort + 40+ other linters. Fast, opinionated, Rust-based. Replaces the flake8 + black + isort trio.
- **NOT black alone.** Black + ruff can conflict; ruff is younger, more aggressive on line length (88 chars by default; matches black). Commit `pyproject.toml` with `[tool.ruff]` section pinning line-length=88.
- **pytest for unit tests.** async-friendly, fixtures, parametrization. Standard in Python ecosystem.
- **Coverage.** `pytest --cov=sdk-py --cov-report=term-missing` for local runs; CI skip for POC (coverage debt can be addressed post-launch).

**Adoption risk: MINIMAL.** ruff is industry-standard 2026 Python stack; pytest is unchanged.

---

### Sub-section: Caching strategy

**Go + mise cache:**
```yaml
test-gateway:
  steps:
    - uses: actions/checkout@v4
    - uses: jdx/mise-action@v2
      with:
        cache: true  # Caches ~/.local/share/mise/installs
    - run: go test ./gateway -cache
```

Mise action caches compiled tools (~300 MB for Go+Python+buf). Hit rate is ~95% for subsequent PRs on same branch.

**Python uv cache (if using uv as dep manager):**
```yaml
test-python:
  steps:
    - uses: actions/checkout@v4
    - uses: jdx/mise-action@v2
    - uses: astral-sh/setup-uv@v2  # Optional; Python 3.12 from mise is sufficient
      with:
        enable-cache: true
```

**Docker layer cache (for image builds, Section 3):**
Done via `cache-from: type=registry` + `cache-to: type=registry` in `docker/build-push-action`. Detailed in Section 3.

---

### Decision matrix: CI/CD pipeline

| Criterion | **Single workflow** | **Split workflows** | **dorny paths-filter** |
|---|---|---|---|
| **Boilerplate lines** | ~80 | ~250 (3×80) | ~100 |
| **Job parallelization** | ❌ None | ✅ Full | ✅ Conditional |
| **Path-scoped execution** | ❌ No | ✅ Yes | ✅ Yes |
| **Maintainability** | ✅ Single source | ⚠️ Duplication | ✅ Central config |
| **Scaling (5 services)** | ❌ 20+ min CI | ✅ 4–5 min | ✅ 4–5 min |
| **Proven at scale** | ✅ Simple | ✅ Yes | ✅ Yes (dorny is standard) |

### Recommendation

**PRIMARY: Single workflow with dorny/paths-filter.** Cost is ~20 lines of YAML. Benefit is skipped jobs, parallelization, scalability. Proven pattern (Kubernetes, HashiCorp use this). Lock-in is zero (pure GHA, no custom actions).

**SKIP: Split workflows.** Duplication is not worth the parallelization win; dorny gives same speed with less maintenance.

**SKIP: Single workflow without filters.** POC is already two-service at P3; you'll regret this by P5.

---

## SECTION 3 — Docker image build + ghcr.io publishing

**Research target:** Build + push gateway (and future service) images to ghcr.io from GHA. Covers Dockerfile pattern, multi-stage, distroless, caching, tagging policy, vulnerability scanning decision.

### Option 1: **docker/build-push-action + distroless (standard 2026 pattern)**

**Approach:** Multi-stage Dockerfile (build stage: full Go toolchain; runtime stage: distroless/static-debian12). `docker/build-push-action@v6` builds + pushes to ghcr.io. BuildKit handles cache via registry or gha.

**Dockerfile pattern:**
```dockerfile
# gateway/Dockerfile
FROM golang:1.23-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o gateway ./gateway/cmd/gateway

# Final stage: distroless
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /app/gateway /gateway
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s \
  CMD ["/gateway", "-health"]
USER nonroot:nonroot
ENTRYPOINT ["/gateway"]
```

**GHA workflow step:**
```yaml
- uses: docker/build-push-action@v6
  with:
    context: .
    dockerfile: gateway/Dockerfile
    push: true
    registry: ghcr.io
    username: ${{ github.actor }}
    password: ${{ secrets.GITHUB_TOKEN }}
    tags: |
      ghcr.io/${{ github.repository }}/gateway:${{ github.sha }}
      ghcr.io/${{ github.repository }}/gateway:latest
    cache-from: type=registry,ref=ghcr.io/${{ github.repository }}/gateway:cache
    cache-to: type=registry,ref=ghcr.io/${{ github.repository }}/gateway:cache,mode=max
```

**Strengths:**
- **Distroless is minimal.** Final image is ~20–50 MB (static binary only, no shell, no package manager). Reduces surface area; zero vulnerability scanning noise.
- **Multi-stage keeps build layer separate.** Intermediate layers (Go toolchain, build artifacts) are not shipped.
- **Registry cache is fast.** Subsequent builds reuse layers; cold build is 2–3 min, warm build is 15–30 sec.
- **docker/build-push-action@v6 is battle-tested.** 10k+ jobs/day. BuildKit has been proven in production.
- **Tagging is flexible.** Both `<sha>` and `latest` tags let GKE reference either (immutable via sha, convenient via latest for dev).
- **CGO_ENABLED=0 is safe for gateway.** Gateway is pure Go; no C bindings. Future adapters may need CGO (Python) but we cross that bridge in P10.

**Weaknesses:**
- **Multi-architecture builds are not in POC.** BuildKit supports `--platform linux/amd64,linux/arm64`; POC is amd64-only. Deferred to P10.
- **HEALTHCHECK is advisory.** Kubernetes ignores Dockerfile HEALTHCHECK; it uses probes in deployment manifests (P7). Not a blocker, just redundant.
- **distroless/static is Go-only.** If future service is Python + C-bindings (e.g., ML sink), you need a different base (python:3.12-slim or custom).

**Adoption risk: MINIMAL.** docker/build-push-action is standard; distroless is adopted by ~50% of production Go services (Google, Uber, Stripe use it).

**Verdict pick: YES — docker/build-push-action + distroless.**

---

### Option 2: **ko** (Go-native builder, no Dockerfile)

**Approach:** `ko build` compiles Go directly to image without Docker. No Dockerfile; config lives in `.kodata/` and `.ko.yaml`.

**Strengths:**
- **Dockerfile-less.** Eliminates Dockerfile syntax complexity; CLI is simpler: `ko build ./gateway/cmd/gateway`.
- **SBOM by default.** ko generates CycloneDX SBOM; cosign can sign it immediately.
- **Multi-platform easy.** `ko build --platforms=linux/amd64,linux/arm64` is one flag; no complex buildx setup.
- **No Docker dependency.** Runs on any machine; doesn't require Docker daemon or buildx.
- **Fast for simple binaries.** If your Go binary has zero C bindings and minimal OS deps, ko is faster than full Docker.

**Weaknesses:**
- **Go-only.** Python sinks, adapters with C bindings, future services: ko doesn't help. You'd need Docker for those anyway.
- **Less mature than docker/build-push-action.** 8.4k GitHub stars (vs 30k for build-push-action). Recent (v0.18.1, Dec 2025) but smaller community.
- **Base image is opinionated.** ko defaults to `cgr.dev/chainguard/static` (distroless clone). Fine, but less flexibility if you need a custom base.
- **Not a universally known tool.** Team onboarding: "What is ko?" +10 min learning curve.
- **GHA integration is manual.** No official `ko-action`; you install ko + run `ko build && ko publish`.
- **Caching is less visible.** Docker layer caching is implicit; you don't control cache-from/cache-to strategy.

**Adoption risk: MEDIUM.** Small community, Go-only scope. If MIO stays Go-only forever (unlikely at P10+), ko is fine. But for a multi-language platform (MIO + MIU), Docker is more universal.

**Verdict skip — ko.** Better for Go-only projects (e.g., a microservice library). MIO will have Python (sink, future adapters); Docker is the common language.

---

### Option 3: **Cloud Build** (GCP, outside GHA)

**Approach:** GHA delegates to `gcloud builds submit`, which runs on Google Cloud Build. Dockerfile lives in repo; Cloud Build triggers, caches, and publishes to ghcr.io.

**Strengths:**
- **No GHA minutes consumed.** Build runs on GCP's infrastructure; GHA only orchestrates.
- **Free tier is generous.** 120 build-minutes/day free.
- **Native GCP integration.** Workload Identity, GCS caching, Artifact Registry connectivity.

**Weaknesses:**
- **Violates cloud-agnostic constraint.** P0 design says "no GCP-only primitives in code paths"; Cloud Build is GCP-only.
- **Additional IAM setup.** Service account + Workload Identity binding + Cloud Build API enablement.
- **Slower feedback loop.** Cold start is 10–20 sec (auth overhead); warm start is ~2 min. Docker/build-push-action@local (if self-hosted) is faster.
- **Debugging is remote.** Logs are in Cloud Build console, not GHA; harder to debug mid-stream.
- **Future multi-cluster scenarios.** If you ever need to publish images to non-GCP registries (Docker Hub, private Artifactory), Cloud Build adds friction.

**Adoption risk: MEDIUM.** Creates GCP lock-in for CI/CD infrastructure (separate from runtime lock-in, which is acceptable).

**Verdict skip — Cloud Build.** Violates architectural constraint. docker/build-push-action is sufficient and keeps CI cloud-agnostic.

---

### Sub-section: Image tagging policy

**POC tagging scheme:**
```
ghcr.io/vanducng/mio/gateway:${{ github.sha }}     # Immutable, replay-safe
ghcr.io/vanducng/mio/gateway:latest                # Latest commit on main
ghcr.io/vanducng/mio/gateway:v0.1.0                # (future) Semantic version on tag
```

**Rationale:**
- **SHA tag is authoritative.** In K8s manifests, always reference `:${{ github.sha }}`. Guarantees you know exactly what's deployed. No surprise rollbacks from `latest` changing.
- **Latest is convenience.** For local dev (`docker pull ghcr.io/vanducng/mio/gateway:latest`), latest is fine. Helm charts use `latest` by default (P7 can override).
- **Semver tags are future.** P10+, when you cut releases, add `v<semver>` tags on GitHub releases. GHA workflow step: `if: startsWith(github.ref, 'refs/tags/v')`.

**Retention policy (ghcr.io):**
- Keep last 50 images per tag (lifecycle rule). After 90 days, delete untagged images. ~50 MB/image × 50 = 2.5 GB max footprint, well within free tier.

**Adoption risk: MINIMAL.** Standard pattern.

---

### Sub-section: Vulnerability scanning decision

**Research findings (critical!):** Trivy's official GHA action (`aquasecurity/trivy-action`) was compromised in March 2026 by a supply-chain attack (75 of 76 tags were malicious). **Do NOT use Trivy until further notice.** Grype (Anchore) is an alternative but less mature.

**POC decision:**
```yaml
# DEFER vulnerability scanning to P10 (post-POC cleanup phase)
# Reason: No production workloads yet; risk is zero
# Once in prod, gate builds on Trivy (or grype) with fail-on-critical
```

**Non-blocking alternative (report-only):**
```yaml
- uses: grype@v1  # Less mature, but not compromised
  with:
    path: 'gateway/Dockerfile'
    output: sarif
    upload: true  # Uploads to GitHub Security tab
```

Grype is maintained by Anchore, less attacked surface, but slower adoption than Trivy. Report-only mode flags HIGH/CRITICAL but doesn't fail the build. Use for POC.

**Adoption risk: HIGH if you pick Trivy.** LOW if you defer or use grype in report-only mode.

**Verdict: DEFER scanning to P10.** POC has no prod traffic; risk is zero. Once live, add fail-on-critical gate.

---

### Decision matrix: Docker image build

| Criterion | **docker/build-push + distroless** | **ko** | **Cloud Build** |
|---|---|---|---|
| **Boilerplate (Dockerfile + GHA)** | ~40 lines | ~10 lines (no Dockerfile) | ~20 lines |
| **Multi-language support** | ✅ Any language | ❌ Go only | ✅ Any language |
| **Caching (build speed)** | ✅ Registry cache | ⚠️ Implicit | ✅ GCP-fast |
| **Cloud-agnostic** | ✅ Yes | ✅ Yes | ❌ GCP lock-in |
| **Community adoption** | ✅ Industry standard | ⚠️ Niche | ⚠️ GCP-only |
| **GHA native** | ✅ Official action | ⚠️ Manual install | ❌ Delegate to Cloud Build |
| **Security scanning** | ⚠️ Trivy compromised | ⚠️ Same risk | ⚠️ Same risk |

### Recommendation

**PRIMARY: docker/build-push-action@v6 + distroless.** Cost is ~40 lines (Dockerfile + GHA). Benefit is universal (supports any language, cloud-agnostic, standard caching). Proven at scale (30k+ users). **Lock-in is zero; you can swap to ko or Cloud Build later with minimal friction.**

**SKIP: ko.** Fine for Go-only services; MIO will be polyglot (Python sinks, future adapters). Defer if MIO scope changes to single-language.

**SKIP: Cloud Build.** Violates cloud-agnostic principle. Docker/build-push-action is sufficient and keeps future flexibility.

**Vulnerability scanning: DEFER to P10.** POC has no prod traffic. Report-only if you want visibility (grype, not Trivy until fix confirmed).

---

## SECTION 4 — GKE image consumption

**Research target:** How does GKE pull images from ghcr.io? Workload Identity vs PAT-based auth, Helm values templating, deploy flow (direct vs GitOps), image-tag update automation.

### Option 1a: **ghcr.io as PUBLIC image (no auth needed)**

**Setup:** Push images public-readable. GKE pulls without `imagePullSecret`.

**Helm values.yaml:**
```yaml
image:
  repository: ghcr.io/vanducng/mio/gateway
  tag: "{{ .Values.deploymentImageTag }}"  # Templated by Helm
  pullPolicy: IfNotPresent
```

**Deployment manifest (auto-generated by Helm):**
```yaml
spec:
  containers:
  - name: gateway
    image: ghcr.io/vanducng/mio/gateway:abc1234  # sha or latest
```

**Strengths:**
- **Zero auth setup.** No imagePullSecret, no service account binding, no secret management.
- **Fast pod startup.** No token refresh latency.
- **Debugging is easy.** `kubectl describe pod` doesn't show pull errors.

**Weaknesses:**
- **Code disclosure risk.** If gateway code is proprietary or security-sensitive, public images leak it to the internet. Cliq webhooks and API keys in logs could expose business logic.
- **Not defensible post-POC.** First security review will flag this.
- **DoS risk.** Anyone can pull your image; no rate limiting without private registry.

**Adoption risk: HIGH for anything beyond POC.** Fine for open-source, unacceptable for proprietary platforms.

**Verdict: Maybe for POC.** If gateway code is throwaway demo (Cliq integration is a POC), public is fine. If you plan to keep the code post-POC, make images private now; transitioning later is a mess.

---

### Option 1b: **ghcr.io as PRIVATE image + imagePullSecret**

**Setup:** Images are private-readable. GKE authenticates via `imagePullSecret` (kubernetes.io/dockercfg or kubernetes.io/dockerconfigjson) + GitHub PAT.

**Steps:**
1. Create GitHub PAT with `read:packages` scope (no expiration recommended for dev, or 1-year for prod).
2. Create K8s secret:
   ```bash
   kubectl create secret docker-registry ghcr-secret \
     --docker-server=ghcr.io \
     --docker-username=<github-username> \
     --docker-password=<github-pat> \
     -n mio
   ```
3. Reference in Deployment:
   ```yaml
   spec:
     imagePullSecrets:
     - name: ghcr-secret
     containers:
     - image: ghcr.io/vanducng/mio/gateway:abc1234
   ```

**Strengths:**
- **Code is private.** Business logic is not exposed.
- **Rate limiting.** GitHub's private registry applies rate limits per-user, not public tier.
- **Proven pattern.** Standard Kubernetes auth for container registries.

**Weaknesses:**
- **Secret rotation is a chore.** PATs expire (or don't, if you set infinite lifetime); manual refresh every 6–12 months.
- **Secrets in K8s.** Secret is base64-encoded (not encrypted at rest in etcd without EncryptionConfig). Risk if cluster is compromised.
- **Workload Identity would be better.** But ghcr.io does NOT support Workload Identity (GitHub is not a GCP identity provider, historical limitation as of 2026).

**Adoption risk: LOW.** Standard pattern, but secret management is manual.

**Verdict: YES if code is private.** Use for real deployment. Accept secret rotation as operational cost.

---

### Option 2: **GKE Workload Identity + custom OIDC bridge (experimental)**

**Theory:** GitHub can issue OIDC tokens (via `github.token`); Google Cloud STS can exchange GitHub JWT for GCP access token; you bind GCP service account to OIDC. GKE Workload Identity maps K8s ServiceAccount → GCP GSA → permissions.

**Blocker (2026):** GitHub OIDC is GCP-compatible only at the **GitHub Actions job level** (you get `${{ secrets.GITHUB_TOKEN }}` in GHA). But OIDC token exchange is between GitHub and GCP; **ghcr.io is GitHub-hosted, not GCP.** So the token is valid for GCP APIs, not GitHub APIs.

**Conclusion:** You cannot use GKE Workload Identity to pull from ghcr.io. You could use it to auth against Cloud Storage (GCS), but not against ghcr.io.

**Adoption risk: NOT APPLICABLE — architectural mismatch.**

**Verdict skip — Workload Identity for ghcr.** Use it for GCS access (P6, P7) instead. For ghcr.io, stick with Option 1b (imagePullSecret + PAT).

---

### Option 3: **Deploy flow options**

#### Option 3a: **Direct helm upgrade from GHA (simple, immediate)**

```yaml
# .github/workflows/deploy.yaml (or merged into ci.yaml)
deploy:
  if: github.ref == 'refs/heads/main'  # or on tags
  needs: [build-gateway]  # waits for image push
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4
    - uses: google-cloud-github-actions/get-gke-credentials@v2
      with:
        cluster_name: mio-poc
        location: us-central1
        credentials: ${{ secrets.GCP_SA_JSON }}  # TODO: use Workload Identity instead
    - run: helm upgrade mio-gateway deploy/charts/mio-gateway \
        --install \
        --namespace mio \
        --set image.tag=${{ github.sha }} \
        --wait
```

**Strengths:**
- **No GitOps overhead.** One step: push image → deploy immediately.
- **Fast feedback.** Deployment status is visible in GHA logs within seconds.
- **Simple to debug.** All orchestration is in one place.

**Weaknesses:**
- **GHA has deploy credentials.** GCP service account JSON (or Workload Identity) is stored in GitHub Actions secrets. If GitHub is compromised, attacker can deploy to prod.
- **No audit trail.** Deployment history is in GHA logs + K8s events; no single source of truth for "what's deployed."
- **Rollback is manual.** To rollback, you re-run old GHA workflow or manually `helm rollback`.
- **No per-PR previews.** You can't easily spin up a preview environment for a PR.

**Adoption risk: LOW for solo dev, MEDIUM for team.** Works fine for POC; becomes a pain with 3+ developers (who can deploy? when? to which cluster?).

**Verdict: YES for POC (P8).** Simple, fast, sufficient for end-to-end demo.

---

#### Option 3b: **GitOps (PR-based, ArgoCD or Flux) — deferred to P10**

```
GHA workflow:
  - Build image
  - Push to ghcr.io with sha tag
  - **OPEN PR** bumping values.yaml image.tag
    (PR: "chore(deploy): bump gateway image to abc1234")
  - Wait for PR approval + merge
  - (ArgoCD/Flux watches repo)
  - Auto-deploy new tag to cluster
```

**Strengths:**
- **Full audit trail.** Every deployment is a git commit; reviewable in PR.
- **Rollback is git revert.** One-line rollback; no manual helm commands.
- **Declarative source of truth.** `values.yaml` git history == deployment history.
- **PR-based approval.** Only approved PRs deploy; team visibility.

**Weaknesses:**
- **ArgoCD/Flux setup is complex.** CRDs, RBAC, sync policies, GitOps practices. 2–3 days to set up correctly.
- **Slower feedback loop.** Deploy is: build → open PR → human approval → merge → ArgoCD reconcile (~5–10 min total).
- **Overkill for solo dev.** You're waiting on yourself to approve the PR.

**Adoption risk: HIGH for solo dev, LOW for team >2.** Great for distributed teams with formal release processes.

**Verdict: DEFER to P10.** POC is solo-dev; GitOps is overhead. Once team joins, revisit.

---

### Sub-section: Helm values.yaml image block

**Pattern:**
```yaml
# deploy/charts/mio-gateway/values.yaml
image:
  registry: ghcr.io
  repository: "{{ .Values.image.registry }}/vanducng/mio/gateway"
  tag: "latest"  # DEV: override with --set image.tag=${{ github.sha }} in GHA
  pullPolicy: IfNotPresent

imagePullSecrets: []  # DEV: leave empty; add in prod overlay values-prod.yaml

# Example prod override (values-prod.yaml):
# imagePullSecrets:
#   - name: ghcr-secret
```

**Why templated?** Allows GHA to inject the sha tag without Helm chart edits:
```bash
helm upgrade --set image.tag=${{ github.sha }}
```

---

### Sub-section: kubeconfig + GKE auth

**POC approach (manual service account JSON):**
```yaml
# .github/workflows/deploy.yaml
- uses: google-cloud-github-actions/get-gke-credentials@v2
  with:
    cluster_name: mio-poc
    location: us-central1
    credentials: ${{ secrets.GCP_SA_JSON }}  # JSON key from GCP Console
```

**Better approach (Workload Identity Federation, no long-lived keys):**
```yaml
- uses: google-github-actions/auth@v2
  with:
    workload_identity_provider: ${{ secrets.WIF_PROVIDER }}
    service_account: ${{ secrets.GCP_SA_EMAIL }}
```

Requires GCP Workload Identity Federation setup (1–2 hours, one-time).

**Adoption risk: MEDIUM.** GCP-specific, but no secret rotation needed post-setup.

**Verdict: WIF for prod (P8+), manual JSON for POC (P8 demo).** Setup WIF when you cut release branch.

---

### Decision matrix: GKE image consumption

| Criterion | **Public images** | **Private + PAT** | **WIF (future)** | **GitOps (P10)** |
|---|---|---|---|---|
| **Auth complexity** | ✅ None | ⚠️ Secrets + rotation | ✅ None (after setup) | ⚠️ ArgoCD setup |
| **Code privacy** | ❌ Exposed | ✅ Private | ✅ Private | ✅ Private |
| **Deployment speed (GHA → pod)** | ✅ Immediate | ✅ Immediate | ✅ Immediate | ❌ 5–10 min (PR + ArgoCD) |
| **Audit trail** | ❌ None (GHA logs only) | ❌ Same | ❌ Same | ✅ Git history |
| **Right-sized for POC** | ⚠️ Only if code is open | ✅ Yes | ✅ Yes (after setup) | ❌ Overkill |

### Recommendation

**Image privacy (POC): Private + imagePullSecret with GitHub PAT.** Cost is 5 min (create PAT, create secret, add to values). Benefit is code is not exposed. Use this for P8 demo.

**Kubeconfig auth: Use manual JSON for POC; upgrade to Workload Identity Federation for prod.** POC is demo; WIF is one-time setup (2 hours) that removes secret rotation.

**Deploy flow: Direct helm upgrade from GHA (Option 3a).** Fast, simple, sufficient for solo dev. Upgrade to GitOps (Option 3b) when team joins.

**SKIP: Public images (unless code is open-source).** One day of shortcuts becomes two weeks of security review hell.

---

## SYNTHESIS

### Where does this research land in P0–P9?

**Short answer:** This research spans P0 (tooling setup), P3/P6/P8 (image builds), and P7/P8 (cluster config). No new phase needed; integrate into existing phases.

#### Phase-by-phase edits

**P0 — Reserve + scaffold:**
- **ADD:** `.mise.toml` file (new) with `[tools]` block pinning Go 1.23, Python 3.12, buf (latest), protoc-gen-go (latest). Add `[tasks]` section with `make up`, `make proto`, `make lint`, `make test` (delegates to `make`).
- **ADD:** `.dockerignore` file (new) excluding `proto/gen`, `appdata`, `.env.local`.
- **MODIFY:** `Makefile` — add `gateway-build`, `gateway-build-local` targets for local Docker build testing. Keep simple (no image push).
- **EXISTING OK:** docker-compose, init.sql, .gitignore all already planned.

**P3 — Gateway + Cliq inbound:**
- **ADD:** `gateway/Dockerfile` (multi-stage, distroless, as specified in Section 3).
- **EXISTING OK:** gateway code, migrations, schema all already planned.

**P6 — Sink-gcs:**
- **ADD:** `sink-gcs/Dockerfile` (Python-based; will be `python:3.12-slim` or custom distroless, TBD in P6 research).
- **EXISTING OK:** sink code, GCS integration already planned.

**P7 — Helm + NATS on GKE:**
- **ADD:** `.github/workflows/ci.yaml` (new) with dorny/paths-filter, buf lint/breaking, golangci-lint, pytest, image build+push to ghcr.io.
- **ADD:** `.github/workflows/deploy.yaml` (new) or merge into ci.yaml as conditional job: `if: github.ref == 'refs/heads/main'` → `helm upgrade` to GKE with image.tag=${{ github.sha }}.
- **ADD:** `deploy/charts/mio-gateway/values.yaml` — image block with registry templating.
- **ADD:** imagePullSecret setup (manual: create GitHub PAT, create K8s secret, reference in values.yaml).
- **MODIFY:** `deploy/gke/setup.sh` — after Helm install, create imagePullSecret in `mio` namespace.
- **EXISTING OK:** Helm charts, observability stack already planned.

**P8 — POC deploy on GKE:**
- **ADD:** GKE cluster provisioning step (in `deploy/gke/setup.sh`): enable GKE, allocate `e2-small` nodes, create GCS buckets (for Tempo, Sink-GCS), bind Workload Identity for GCS auth.
- **MODIFY:** `deploy/gke/setup.sh` — add imagePullSecret setup (if not done in P7).
- **EXISTING OK:** Observability, alerting already planned.

**P9 — Second channel adapter (Slack):**
- **No changes.** Images and CI already support N channels. P9 is adapter code only.

#### Summary of new files / changes

| Phase | New files | Modified files | Effort |
|---|---|---|---|
| **P0** | `.mise.toml`, `.dockerignore` | `Makefile` | +30 min |
| **P3** | `gateway/Dockerfile` | — | +15 min |
| **P6** | `sink-gcs/Dockerfile` | — | +15 min (in P6 session) |
| **P7** | `.github/workflows/ci.yaml`, `.github/workflows/deploy.yaml` | `deploy/gke/setup.sh`, `deploy/charts/mio-gateway/values.yaml` | +3 hours |
| **P8** | — | `deploy/gke/setup.sh` (imagePullSecret) | +30 min |
| **P9** | — | — | — |

**Total new effort: ~4.5 hours,** mostly in P7 (workflow writing). **No new phase needed;** this fits within existing P0/P3/P7/P8 budgets.

---

### Concrete paste-ready configs

#### Config A: `.mise.toml` (P0)

```toml
# .mise.toml at repo root
version = 1

[tools]
go = "1.23.5"
python = "3.12.2"
buf = "1.35.1"
protoc = "27.3"
# protoc-gen-go and protoc-gen-go-grpc are installed via go install in gateway/go.mod

[env]
# Cross-platform env vars
MIO_TENANT_ID = "{{ env.MIO_TENANT_ID }}"  # Set in .env.local or CI secrets
POSTGRES_PASSWORD = "dev_password"  # POC only; override in .env.local

[tasks]
up = "docker compose -f deploy/docker-compose.yml up -d"
down = "docker compose -f deploy/docker-compose.yml down"
proto = "buf generate proto"
lint = """
buf lint proto && \
go vet ./gateway ./sdk-go && \
golangci-lint run ./gateway ./sdk-go && \
ruff check sdk-py examples/echo-consumer
"""
test = """
go test ./gateway/... ./sdk-go/... && \
pytest sdk-py examples/echo-consumer
"""
clean = """
rm -rf proto/gen && \
docker compose -f deploy/docker-compose.yml down -v
"""
help = """
echo "Available tasks:
  mise run up      - Start local infra (NATS, Postgres, MinIO)
  mise run down    - Stop local infra
  mise run proto   - Generate proto code
  mise run lint    - Run linters
  mise run test    - Run tests
  mise run clean   - Full cleanup
"
"""
```

#### Config B: `gateway/Dockerfile` (P3)

```dockerfile
# gateway/Dockerfile
FROM golang:1.23-alpine AS builder

WORKDIR /app

# Copy go.mod and go.sum for caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build static binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -a \
  -installsuffix cgo \
  -ldflags "-X main.version=$BUILD_VERSION" \
  -o /app/gateway \
  ./gateway/cmd/gateway

# Final stage: distroless static-debian12
FROM gcr.io/distroless/static-debian12:nonroot

# Copy binary from builder
COPY --from=builder /app/gateway /gateway

# Non-root user (nonroot:nonroot UID 65532)
USER nonroot:nonroot

# Expose port
EXPOSE 8080

# Health check (advisory; K8s uses livenessProbe)
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s \
  CMD ["/gateway", "-healthz"]

ENTRYPOINT ["/gateway"]
```

#### Config C: `.github/workflows/ci.yaml` (P7)

```yaml
# .github/workflows/ci.yaml
name: CI

on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

permissions:
  contents: read
  packages: write

jobs:
  changes:
    runs-on: ubuntu-latest
    outputs:
      gateway: ${{ steps.filter.outputs.gateway }}
      sdk-py: ${{ steps.filter.outputs.sdk-py }}
      proto: ${{ steps.filter.outputs.proto }}
    steps:
      - uses: actions/checkout@v4
      - uses: dorny/paths-filter@v3
        id: filter
        with:
          filters: |
            gateway:
              - 'gateway/**'
              - 'sdk-go/**'
              - 'proto/**'
              - 'go.mod'
              - 'go.sum'
            sdk-py:
              - 'sdk-py/**'
              - 'examples/echo-consumer/**'
              - 'proto/**'
              - 'pyproject.toml'
            proto:
              - 'proto/**'
              - 'buf.yaml'
              - 'buf.gen.yaml'

  test-proto:
    needs: changes
    if: ${{ needs.changes.outputs.proto == 'true' }}
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - uses: jdx/mise-action@v2
        with:
          cache: true
      - run: buf lint proto
      - run: buf breaking --against 'origin/main' proto

  test-gateway:
    needs: [changes, test-proto]
    if: ${{ needs.changes.outputs.gateway == 'true' }}
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: jdx/mise-action@v2
        with:
          cache: true
      - run: golangci-lint run ./gateway ./sdk-go
      - run: go test -v -cover ./gateway/... ./sdk-go/...

  test-python:
    needs: [changes, test-proto]
    if: ${{ needs.changes.outputs.sdk-py == 'true' }}
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: jdx/mise-action@v2
        with:
          cache: true
      - run: ruff check sdk-py examples/echo-consumer
      - run: ruff format --check sdk-py examples/echo-consumer
      - run: python -m pytest sdk-py examples/echo-consumer -v

  build-gateway:
    needs: test-gateway
    if: github.event_name == 'push'
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: docker/setup-buildx-action@v3
      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - uses: docker/build-push-action@v6
        with:
          context: .
          dockerfile: ./gateway/Dockerfile
          push: true
          tags: |
            ghcr.io/${{ github.repository }}/gateway:${{ github.sha }}
            ghcr.io/${{ github.repository }}/gateway:latest
          cache-from: type=registry,ref=ghcr.io/${{ github.repository }}/gateway:cache
          cache-to: type=registry,ref=ghcr.io/${{ github.repository }}/gateway:cache,mode=max
```

#### Config D: `.github/workflows/deploy.yaml` (P8) — OR merge into ci.yaml

```yaml
# .github/workflows/deploy.yaml (OR add to ci.yaml as job)
name: Deploy

on:
  push:
    branches: [main]

permissions:
  contents: read

jobs:
  deploy:
    if: github.event_name == 'push' && github.ref == 'refs/heads/main'
    needs: build-gateway
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: google-cloud-github-actions/get-gke-credentials@v2
        with:
          cluster_name: mio-poc
          location: us-central1
          credentials: ${{ secrets.GCP_SA_JSON }}
      - run: |
          helm upgrade mio-gateway deploy/charts/mio-gateway \
            --install \
            --namespace mio \
            --create-namespace \
            --set image.tag=${{ github.sha }} \
            --set image.pullPolicy=IfNotPresent \
            --wait \
            --timeout 5m
```

#### Config E: `deploy/charts/mio-gateway/values.yaml` — image block (P7)

```yaml
# deploy/charts/mio-gateway/values.yaml
image:
  registry: ghcr.io
  repository: "{{ .Values.image.registry }}/{{ .Values.global.imageRepository | default \"vanducng/mio\" }}/gateway"
  tag: "latest"  # Override with --set image.tag=${{ github.sha }} in GHA
  pullPolicy: IfNotPresent

# For prod: add imagePullSecrets in values-prod.yaml overlay
imagePullSecrets: []

# Example for prod (values-prod.yaml):
# imagePullSecrets:
#   - name: ghcr-secret
```

#### Config F: `.dockerignore` (P0)

```
# .dockerignore
.git
.gitignore
.env.local
.env.example
README.md
LICENSE
docs/
examples/
playground/
plans/
appdata/
proto/gen/
node_modules/
__pycache__/
*.pyc
.pytest_cache/
.venv/
```

---

### POC vs Defer decisions (brutal)

| Decision | POC or Defer? | Rationale |
|---|---|---|
| **mise as tooling manager** | **POC (P0)** | Zero overhead, immediate payoff in CI reproducibility. 5 lines config. |
| **dorny/paths-filter in CI** | **POC (P7)** | Scales monorepo; minimal boilerplate. Standard pattern. |
| **golangci-lint + ruff for linting** | **POC (P7)** | Standard, no surprises. Integrate early or carry lint debt forever. |
| **buf breaking-change gate** | **POC (P7)** | Schema locked at P1; breaking checks are non-negotiable to catch field-name breaks. |
| **docker/build-push-action + distroless** | **POC (P3/P7)** | Standard, cloud-agnostic, solves image distribution. Core to end-to-end demo. |
| **Multi-arch (linux/arm64) builds** | **DEFER to P10** | POC is amd64-only. BuildKit supports it; just defer the `--platform` flag. Zero code change to defer. |
| **Sigstore (cosign + SBOM signing)** | **DEFER to P10** | POC images are unsigned. Signing adds 1 min per build. Safe to defer; implement post-launch. |
| **Trivy vulnerability scanning** | **DEFER (compromise in March 2026)** | Use grype in report-only mode if you want visibility; gate scanning is P10. |
| **GitOps (ArgoCD/Flux)** | **DEFER to P10** | Direct `helm upgrade` from GHA is sufficient for solo dev. GitOps is team/process overhead. |
| **Workload Identity Federation (GCP)** | **DEFER (after P8 POC)** | POC uses manual service-account JSON. WIF is 2-hour setup that eliminates secret rotation; defer to first prod deploy. |
| **imagePullSecret rotation** | **DEFER (after launch)** | Use GitHub PAT with no expiration for POC. Implement rotation policy (6–12 month cycle) post-launch. |
| **Separate values-prod.yaml overlay** | **DEFER to P10** | POC uses one values.yaml. Prod overlay (secrets, resource limits, replica count) can wait. |

**Summary:**
- **POC (must do now):** mise, dorny paths-filter, docker/build-push-action, buf breaking, linters, image push to ghcr.io, direct helm deploy.
- **Defer (post-POC, no rewrite needed):** multi-arch builds, cosign signing, scanning gates, GitOps, WIF, secret rotation policy, prod overlays.

---

### Risks not yet captured in P0–P9 plan

1. **Trivy supply-chain compromise (March 2026).** Trivy's official GHA action had 75 of 76 tags poisoned. Do NOT use `aquasecurity/trivy-action` without commit-SHA pinning + audit. **Mitigation:** DEFER scanning to P10; use grype in report-only if needed.

2. **Secret key rotation on shared cluster.** imagePullSecret PAT lives in K8s secret (base64, not encrypted at rest). If cluster etcd is compromised, attacker gets GitHub PAT → can push images to ghcr.io → RCE on cluster. **Mitigation:** (a) WIF reduces long-lived secret lifetime (implement P10); (b) Short-lived PAT (6-month expiration, rotated quarterly).

3. **ghcr.io rate limiting.** GitHub's free tier has rate limits on package pulls (not documented precisely; empirically ~10 pulls/min from public IPs). If you scale to 50+ pod replicas, cold-starts might hit limits. **Mitigation:** Host own registry (Harbor) on GKE, or move to Docker Hub (free, 6 hours rate-limit reset). Revisit if pod churn becomes real (P10).

4. **Image size explosion post-P6.** sink-gcs will be Python + Pandas/Polars for Parquet output. Python images are 200–500 MB base; distroless doesn't help. **Mitigation:** Use `python:3.12-slim` (200 MB) instead of distroless for Python; accept larger images. Plan for compression / registry storage costs.

5. **Cluster API server load from artifact delivery.** P7 mio-jetstream StatefulSet + gateway Deployment + sink-gcs Deployment + ai-service = 6+ pods pulling large images on first boot. If cold-start coincides with failed node rebuild, etcd can be slow. **Mitigation:** Use `imagePullPolicy: IfNotPresent` (already in Config E); pre-pull images on node startup (DaemonSet, P10); increase etcd quota (GKE default is often tight).

---

### Unresolved questions (research limits)

1. **ghcr.io package retention cost.** GitHub doesn't publicize storage pricing per-image; only "free tier = 500 MB". At 50 MB/image × 50 retained = 2.5 GB, you're likely over quota. Will need to test or request clarification from GitHub support.

2. **Workload Identity Federation for ghcr.io (if GitHub ever supports it).** As of 2026-05, ghcr.io cannot be an OIDC audience; GitHub OIDC is GCP-only. If GitHub adds ghcr.io to their OIDC token, that changes imagePullSecret strategy. Worth revisiting in Q4 2026.

3. **Buf breaking-rule performance on large protos.** P1–P9 proto is ~500 lines. If you scale to 10k+ line proto files (unlikely, but possible with many adapters), `buf breaking` might timeout. No data on performance curve; assume it's fine until proven otherwise.

4. **Python sink multi-replica write consistency to GCS.** P6 switches from filename-based (collision risk) to offset-based (`<consumer-id>-<seq-start>-<seq-end>.ndjson`). P7 keeps single replica. When you scale to 3 replicas in P10, race conditions on consumer offset tracking might emerge. Buf doesn't cover this; test under load.

5. **GKE autopilot vs standard.** Research assumed GKE standard cluster (P7 cost ~$430/mo). GKE Autopilot is cheaper (pay-per-pod) but locks you into Google's decisions (image pull policies, node pools, etc.). Worth evaluating post-POC if costs exceed estimates.

---

## Recommended implementation sequence (for /vd:cook)

1. **P0 extension (30 min):** Add `.mise.toml`, `.dockerignore`, `Makefile` image-build targets. Verify `mise run up` brings up infra.

2. **P3 extension (15 min):** Add `gateway/Dockerfile` after gateway code is written. Test locally: `docker build -f gateway/Dockerfile .`

3. **P7 extension (3 hours):** Largest effort.
   - Write `.github/workflows/ci.yaml` with dorny paths-filter, test all jobs locally (or dry-run with act).
   - Set up ghcr.io image push: `docker login ghcr.io`, test manual push, add to workflow.
   - Configure `imagePullSecret` in K8s: create GitHub PAT, create secret, test pod startup.

4. **P8 extension (1 hour):** Wire deploy: helm install gateway, set image tag to git SHA, verify rollout.

5. **P6 (when reached):** Add `sink-gcs/Dockerfile` (Python base: `python:3.12-slim` or distroless/python).

---

## Sources

- [mise GitHub repository](https://github.com/jdx/mise) — maintainership, release velocity
- [Devbox documentation](https://www.jetify.com/blog/using-nix-flakes-with-devbox/)
- [asdf project](https://asdf-vm.com/) — version manager ecosystem
- [dorny/paths-filter action](https://github.com/dorny/paths-filter) — monorepo CI pattern
- [docker/build-push-action](https://github.com/docker/build-push-action) — official Docker GHA action
- [ko project](https://github.com/ko-build/ko) — Go-native container builder
- [buf-action documentation](https://buf.build/docs/bsr/ci-cd/github-actions/) — proto breaking-change detection
- [GKE Workload Identity documentation](https://cloud.google.com/kubernetes-engine/docs/how-to/workload-identity) — K8s identity federation
- [Helm values templating guide](https://oneuptime.com/blog/post/2026-02-20-helm-values-templating/view)
- [golangci-lint changelog](https://golangci-lint.run/docs/product/changelog/) — linter versions
- [Trivy supply-chain incident (March 2026)](https://thehackernews.com/2026/03/trivy-security-scanner-github-actions-breached-75-tags-hijacked-to-steal-cicd-secrets.html) — vulnerability scanner compromise
- [Cosign keyless signing guide](https://edu.chainguard.dev/open-source/sigstore/cosign/) — container signing
- [Syft SBOM generation](https://github.com/anchore/syft) — software bill of materials

---

**Report Status:** READY FOR IMPLEMENTATION

**Next step:** `/vd:cook` with these configs. Estimated total effort to integrate all recommendations: **5 hours** spread across P0 (30 min), P3 (15 min), P7 (3 hours), P8 (1 hour).

