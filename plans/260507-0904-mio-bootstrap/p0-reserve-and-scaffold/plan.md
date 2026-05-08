---
phase: 0
title: "Reserve + scaffold"
status: in-progress
priority: P1
effort: "1h"
depends_on: []
---

# P0 — Reserve + scaffold

## Overview

Lock down the names and the directory shape before any meaningful code lands.
Cheap now, painful later. After this phase, `git clone && make up` brings
up NATS + Postgres + MinIO locally.

## Goal & Outcome

**Goal:** Monorepo skeleton committed; local infra brought up by a single command; `buf` wired; module/package paths reserved.

**Outcome:** A new contributor can clone, run `make up`, see NATS streams via the CLI, and run `buf generate` without errors.

## Files

- **Create:**
  - `go.mod` — module `github.com/vanducng/mio` (single-module; Go 1.23+)
  - `.mise.toml` — toolchain pin (Go 1.23.x, Python 3.12.x, buf, protoc); `[env]` block for shared dev env; `[tasks]` minimal — delegates to Makefile, doesn't replace it (Makefile remains the canonical task runner; mise owns versions only). Cross-platform parity macOS dev → linux CI via `jdx/mise-action@v2`.
  - `.dockerignore` — repo-root level; excludes `.git`, `appdata/`, `proto/gen/`, `playground/`, `plans/`, `docs/`, `.env*`, `__pycache__`, `.venv`, `node_modules`, `*.pyc`, `.pytest_cache/`, IDE dirs. Keeps build context small for both `gateway/Dockerfile` (P3) and `sink-gcs/Dockerfile` (P6).
  - `Makefile` — top-level: `up`, `down`, `proto`, `lint`, `test`, `clean`, `help`, `gateway-build`, `gateway-build-local` (last two: local Docker image build for smoke-testing the Dockerfile pre-CI; no push)
  - `buf.yaml` (v2, STANDARD lint + WIRE_JSON breaking — see P1 for rationale), `buf.gen.yaml` at repo root
  - `proto/mio/v1/.gitkeep` (placeholder; envelope lands in P1)
  - `proto/channels.yaml` (placeholder; registry populated in P1)
  - `gateway/.gitkeep`, `sdk-go/.gitkeep`, `sdk-py/.gitkeep`, `sink-gcs/.gitkeep`, `examples/echo-consumer/.gitkeep`, `deploy/charts/.gitkeep`
  - `deploy/docker-compose.yml` — NATS + Postgres + MinIO with HTTP healthchecks
  - `deploy/postgres/init.sql` — bootstrap only (role + empty `mio` DB)
  - `.editorconfig` — polyglot rules (Go/proto tabs, Python 4sp, YAML 2sp, MD no-trim)
  - `.env.example` — port-override template (`POSTGRES_PORT`, `NATS_PORT`, `MINIO_API_PORT`, `MINIO_CONSOLE_PORT`)
- **Modify:**
  - `README.md` — Quickstart (`make up` → `make proto`), port-override note pointing at `.env.example`
  - `.gitignore` — verify Go/Python/local; **add** `go.work`, `go.work.sum`, `appdata/`, `proto/gen/`, `.env.local`

## Steps

1. **Init Go module.** `go mod init github.com/vanducng/mio` at repo root. Single-module layout (research Q1: solo dev + 3–4 Go services; multi-module deferred until P10 if adapters split).
2. **Update `.gitignore`.** Append `go.work`, `go.work.sum` (local-only workspace), `appdata/`, `proto/gen/`, `.env.local`. Verify Go + Python + IDE blocks already present.
3. **Write `buf.yaml` (v2).** `version: v2`; `lint.use: [STANDARD]`; `breaking.use: [WIRE_JSON]`. WIRE_JSON breaking is load-bearing: catches both binary-wire breaks AND field renames that would corrupt the JSON-encoded GCS sink (P6) + BigQuery external tables (P8) before P9 second-adapter ships. (`STANDARD` is NOT a valid `buf` v2 breaking rule set — only MINIMAL/PACKAGE/FILE/WIRE/WIRE_JSON exist; this corrects the earlier draft.)
4. **Write `buf.gen.yaml`.** Plugins: `buf.build/protocolbuffers/go` → `proto/gen/go`, `buf.build/protocolbuffers/python` → `proto/gen/py`. No gRPC plugin (envelope rides NATS JetStream, not gRPC). Paths-by-package.
5. **Write `deploy/docker-compose.yml`** with healthchecks:
   - `nats:2.10-alpine`, command `-js -sd=/data -m 8222`, ports `${NATS_PORT:-4222}:4222` + `8222:8222`, volume `./appdata/nats:/data`, healthcheck `wget --spider http://localhost:8222/healthz` (interval 10s, retries 5), `restart: unless-stopped`.
   - `postgres:16-alpine`, env `POSTGRES_DB=mio` / `POSTGRES_USER=mio_app` / `POSTGRES_PASSWORD=dev_password`, port `${POSTGRES_PORT:-5432}:5432`, volumes `./appdata/postgres:/var/lib/postgresql/data` + `./deploy/postgres/init.sql:/docker-entrypoint-initdb.d/init.sql:ro`, healthcheck `pg_isready -U mio_app -d mio`, `restart: unless-stopped`.
   - `minio/minio:latest`, command `server /data --console-address :9001`, ports `${MINIO_API_PORT:-9000}:9000` + `${MINIO_CONSOLE_PORT:-9001}:9001`, env `MINIO_ROOT_USER=minioadmin` / `MINIO_ROOT_PASSWORD=minioadmin`, volume `./appdata/minio:/data`, healthcheck `curl -f http://localhost:9000/minio/health/live`, `restart: unless-stopped`.
   - No `depends_on: service_healthy` wiring yet — gateway/echo-consumer hook those in P3/P4. (Cross-phase: gateway startup is authoritative for stream/consumer provisioning; bootstrap Job in P7 is verification-only.)
6. **Write `.env.example`** with default ports + a comment block documenting the override pattern (Postgres 5432 collision is the most common; nudge devs to `cp .env.example .env.local && export $(cat .env.local)`).
7. **Write `deploy/postgres/init.sql`** — comment-only placeholder; the
   `postgres:16-alpine` entrypoint creates the role + database from the
   `POSTGRES_USER` / `POSTGRES_PASSWORD` / `POSTGRES_DB` env vars **before**
   running anything in `/docker-entrypoint-initdb.d/`. A `CREATE ROLE mio_app`
   here would error (`role "mio_app" already exists`) and break first cold
   boot — Postgres has no `CREATE ROLE IF NOT EXISTS` syntax.
   ```sql
   -- P0 bootstrap. Role + database are created by the postgres entrypoint
   -- from POSTGRES_USER / POSTGRES_PASSWORD / POSTGRES_DB. Do NOT add
   -- CREATE ROLE / CREATE DATABASE here — they will fail on cold boot.
   --
   -- Schema migrations are owned by gateway/migrations/ from P3.
   -- Foundation invariants (enforced by P3 migrations, not here):
   --   - tenant_id, account_id NOT NULL from row 1
   --   - idempotency address: (account_id, source_message_id)
   --
   -- If future setup needs an extension or a second role, wrap in a
   -- DO $$ BEGIN ... EXCEPTION WHEN duplicate_object THEN NULL; END $$;
   -- block so re-runs against an already-initialized volume stay clean.
   ```
   No tables, no role, no DB statements. Putting schema here forces a
   `appdata/postgres/` wipe on every migration change.
8. **Write `.editorconfig`** — `root=true`; `[*]` utf-8 + lf + final-newline + trim-trailing; `[*.{go,proto}]` tab indent_size 4; `[*.py]` space 4; `[*.{yml,yaml}]` space 2; `[*.md]` `trim_trailing_whitespace=false`.
9. **Write `.mise.toml`** (toolchain pin only; tasks delegate to Makefile).
   Use proper TOML section headers for tasks — `[tasks.up] = { ... }` is
   invalid TOML (table headers cannot have an `=` after them) and mise
   refuses to parse the file:
   ```toml
   [tools]
   go = "1.23"
   python = "3.12"
   "ubi:bufbuild/buf" = "latest"
   protoc = "27"

   [env]
   MIO_TENANT_ID = "tenant-dev"
   _.file = ".env.local"   # auto-loads .env.local if present (gitignored)

   [tasks.up]
   run = "make up"
   description = "Start local infra"

   [tasks.down]
   run = "make down"

   [tasks.proto]
   run = "make proto"

   [tasks.lint]
   run = "make lint"

   [tasks.test]
   run = "make test"
   ```
   Single source of truth for tool versions across macOS dev + linux CI; `mise install` reproduces the toolchain. Tasks are thin wrappers — Makefile stays canonical. (Research Section 1: mise wins over asdf/devbox on cross-platform parity, GHA support via `jdx/mise-action@v2`, and zero lock-in — `.tool-versions` migration path back to asdf in 5 min.)
10. **Write `.dockerignore`** at repo root — excludes `.git`, `appdata/`, `proto/gen/`, `playground/`, `plans/`, `docs/`, `.env*` (except `.env.example`), `__pycache__`, `.venv`, `node_modules`, `*.pyc`, `.pytest_cache/`, `.idea/`, `.vscode/`. Keeps build context small for P3 `gateway/Dockerfile` and P6 `sink-gcs/Dockerfile`.
11. **Write top-level `Makefile`** with `.PHONY` and `help` as first target:
   - `up` → `docker compose -f deploy/docker-compose.yml up -d`
   - `down` → `docker compose -f deploy/docker-compose.yml down`
   - `proto` → `buf generate`
   - `lint` → `buf lint && go vet ./...`
   - `test` → `go test ./...`
   - `clean` → `rm -rf proto/gen && docker compose -f deploy/docker-compose.yml down -v`
   - `gateway-build-local` → `docker build -f gateway/Dockerfile -t mio/gateway:dev .` (smoke-test Dockerfile locally; no push; build context is repo root so go.work-free single-module copy works)
   - `gateway-build` → same as above with `--build-arg BUILD_VERSION=$(shell git describe --always --dirty)` for version embedding
12. **Update `README.md` Quickstart.** Four lines: clone, `mise install`, `make up`, `make proto`. Add a "Port collisions" subsection pointing at `.env.example` (default ports: Postgres 5432, NATS 4222 + 8222, MinIO 9000 + 9001).
13. **Smoke-verify locally.** `mise install` (no errors); `make up` → `docker compose ps` shows all three `(healthy)`; `curl localhost:8222/healthz` returns 200; `pg_isready -h localhost -U mio_app` OK; `curl localhost:9000/minio/health/live` returns 200; `buf lint` passes on empty `proto/`; `go build ./...` no-ops cleanly.
14. **Commit + push** to `vanducng/mio`.

## Success Criteria

- [ ] `mise install` resolves Go 1.23, Python 3.12, buf, protoc cleanly; `mise current` lists all four
- [ ] `.dockerignore` committed; `docker build -f gateway/Dockerfile .` from a fresh clone (after P3 lands the Dockerfile) sees a small context (verify with `du -sh` on `.docker-build` if needed)
- [ ] `make up` brings up NATS + Postgres + MinIO; all three report `(healthy)` in `docker compose ps`
- [ ] Healthcheck endpoints respond: `http://localhost:8222/healthz` (NATS) → 200, `pg_isready` (Postgres) → OK, `http://localhost:9000/minio/health/live` (MinIO) → 200
- [ ] `nats` CLI (or `docker compose exec nats nats stream ls`) reports no streams — clean state, gateway will provision in P3 (cross-phase: gateway is authoritative)
- [ ] `buf lint` (STANDARD) and `buf breaking --against '.git#branch=main'` (WIRE_JSON) both pass on empty proto dir; `buf config ls-breaking-rules` lists WIRE_JSON rules (not STANDARD/WIRE)
- [ ] `go build ./...` succeeds (module valid; no sources yet)
- [ ] `.gitignore` excludes `go.work`, `go.work.sum`, `appdata/`, `proto/gen/`, `.env.local` (verified via `git check-ignore`)
- [ ] `.env.example` committed; README documents port-override pattern
- [ ] `init.sql` is bootstrap-only (no `CREATE TABLE`); re-running `make down -v && make up` succeeds without error
- [ ] Repo pushed to `vanducng/mio` on GitHub

## Risks

- **Go module rename pain.** `github.com/vanducng/mio` is final; rename forces every SDK consumer to reimport. *Mitigation:* lock now; if P10 needs per-adapter autonomy, split into sibling modules (`mio-adapter-*`) — root SDK imports stay stable.
- **Docker port collisions (esp. Postgres 5432).** *Mitigation:* `.env.example` + README override section; defaults documented; devs `cp .env.example .env.local`.
- **Schema leaking into `init.sql`.** Tables here force `appdata/postgres/` wipes whenever migrations evolve. *Mitigation:* `init.sql` is bootstrap-only by contract; P3+ owns schema via `goose`/`golang-migrate` (research Q4: tool choice deferred to P3).
- **Healthcheck flakiness on first boot.** Postgres init can take 5–10s on cold volume. *Mitigation:* healthcheck `interval=10s retries=5` ≈ 50s window; downstream services use `condition: service_healthy` from P3.
- **`go.work` accidentally committed.** Local-only workspace; if pushed, CI module resolution diverges from dev. *Mitigation:* `.gitignore` entry verified via Step 2 + Success Criteria check.
- **Buf STANDARD-lint false-positive on first proto.** Strict rules can flag stylistic issues in P1. *Mitigation:* fix the proto, not the rule. WIRE_JSON breaking permits forward-compatible field additions; only removals/renames/reorders trip it (which is exactly what we want pre-P9).
- **Out-of-scope creep.** Resist adding observability stack, traces, or schema to P0. Cross-phase invariants (label cardinality `channel_type`/`direction`/`outcome` only; subject grammar `mio.<dir>.<channel_type>.<account_id>.<conversation_id>[.<message_id>]`; schema-version validation on publish only) land in P1–P3, not here.

## Notes

Some of P0 already done in the morning sessions:
- [x] GitHub repo created (`vanducng/mio`)
- [x] README, LICENSE, .gitignore committed
- [x] `docs/system-architecture.md` committed (slot 2 lock-in)
- [ ] Go module init
- [ ] `buf.yaml`, `buf.gen.yaml`
- [ ] `deploy/docker-compose.yml` + `init.sql` (DB only; no schema)
- [ ] Top-level `Makefile`
- [ ] Directory placeholders + `proto/channels.yaml` placeholder

## Foundation alignment

P0 doesn't bake in any schema decisions; it only reserves directories and
brings local infra up. The locked-in design (four-tier addressing,
`channel_type` registry, `ConversationKind`, idempotent
`(account_id, source_message_id)`) lands in P1 (proto) and P3 (DB schema).
Keeping P0 schema-free avoids retrofitting if P1/P3 nudge the model.

## Research backing

[`plans/reports/research-260508-1056-p0-scaffold-monorepo-infra.md`](../../reports/research-260508-1056-p0-scaffold-monorepo-infra.md)

Confirmed deltas to fold during execution:
- Single Go module (`github.com/vanducng/mio`); add `go.work` + `go.work.sum` to `.gitignore` (local-only workspace).
- `buf.yaml` use STANDARD lint + **WIRE_JSON** breaking (research Q10; catches both binary-wire and JSON field-name breaks). Note: earlier "STANDARD breaking" draft was incorrect — `STANDARD` is not a valid v2 breaking rule set.
- Add HTTP healthchecks to docker-compose for `nats` (`8222/healthz`) + `postgres` (`pg_isready`) + `minio` (`/minio/health/live`); echo-consumer and gateway should `depends_on { condition: service_healthy }` later.
- `init.sql` stays bootstrap-only (DB + role); migrations own schema. Recommend `goose` for migration tool in P3 (research compared `golang-migrate` vs `goose` vs `Atlas` — both viable; P3 plan currently picks `golang-migrate`, fine to keep).
- Document docker-compose `.env` port-override pattern in README for Postgres 5432 collisions.
