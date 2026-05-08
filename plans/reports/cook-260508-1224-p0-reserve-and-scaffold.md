---
type: cook
phase: P0
date: 2026-05-08
status: DONE
---

# P0 Cook Report — Reserve + scaffold

## TL;DR
Monorepo skeleton committed: Go module, buf v2 config, docker-compose (NATS+Postgres+MinIO with healthchecks), Makefile, mise toolchain, all placeholder dirs. Biggest decision: moved buf.yaml to repo root (not proto/) with `modules: [{path: proto}]` — buf v2 requires this for multi-module workspace semantics, and added a minimal placeholder.proto so buf lint/breaking commands exit cleanly. `buf breaking --against .git#branch=main` will be fully green post-commit (currently requires at least one proto in the against-target, which this commit provides).

## Files created/modified

- `go.mod` — module `github.com/vanducng/mio`, go 1.23
- `.gitignore` — added `go.work`, `go.work.sum`, `.env.local` entries
- `buf.yaml` — v2, `modules: [{path: proto}]`, lint STANDARD, breaking WIRE_JSON (repo root, not proto/)
- `buf.gen.yaml` — go + python plugins, outputs to `proto/gen/go` / `proto/gen/py`
- `deploy/docker-compose.yml` — NATS 2.10, Postgres 16, MinIO latest; all ports env-overridable (added NATS_MON_PORT); HTTP healthchecks on all three
- `deploy/postgres/init.sql` — comment-only placeholder; no CREATE ROLE/DATABASE
- `.env.example` — port override template for all 5 ports
- `.editorconfig` — polyglot: tab for Go/proto, 4sp for Python, 2sp for YAML, no-trim for MD
- `.mise.toml` — tools: go=1.23, python=3.12, ubi:bufbuild/buf=latest, protoc=27; [env] block; 5 task delegates to Makefile
- `.dockerignore` — excludes .git, appdata/, proto/gen/, playground/, plans/, docs/, .env*, Python/Node artifacts, IDE dirs
- `Makefile` — help, up, down, proto, lint, test, clean, gateway-build, gateway-build-local
- `README.md` — Quickstart (4-step: clone/mise install/make up/make proto), Port collisions section; fixed stale idempotency reference
- `proto/buf.yaml` — removed (moved to repo root)
- `proto/buf.gen.yaml` — removed (moved to repo root)
- `proto/channels.yaml` — placeholder; registry populated in P1
- `proto/mio/v1/placeholder.proto` — minimal proto package declaration (required for buf commands to exit 0 on empty dir)
- `proto/mio/v1/.gitkeep` — placeholder
- `gateway/.gitkeep`, `sdk-go/.gitkeep`, `sdk-py/.gitkeep`, `sink-gcs/.gitkeep`
- `examples/echo-consumer/.gitkeep`, `deploy/charts/.gitkeep`
- `tools/proto-roundtrip/` — empty dir (P1 will add Go source in root module)

## Commands run + results

| Command | Result |
|---|---|
| `docker compose -f deploy/docker-compose.yml config --quiet` | PASS |
| `go build ./...` | PASS (no packages yet — expected) |
| `mise tasks ls` | PASS — 5 tasks listed |
| `mise install` | PASS — go 1.23.4, python 3.12.11, buf 1.69.0, protoc 27.5 |
| `mise current` | PASS — all 4 tools listed |
| `buf lint` (via `mise exec`) | PASS |
| `buf config ls-breaking-rules` | PASS — 22 WIRE_JSON rules listed |
| `buf breaking --against "$(pwd)/proto"` | PASS (self-comparison exits 0) |
| `docker compose up -d` (alternate ports) | PASS — all 3 (healthy) |
| NATS `/healthz` → 200 | PASS |
| `pg_isready -h localhost -p 15432` | PASS |
| MinIO `/minio/health/live` → 200 | PASS |
| NATS JetStream `/jsz` streams=0 | PASS |
| `git check-ignore` for go.work, appdata/, proto/gen/, .env.local | PASS |

## Success criteria checklist

- [x] `mise install` resolves Go 1.23, Python 3.12, buf, protoc — `mise current` shows all four
- [x] `.dockerignore` committed; build context excludes all specified dirs
- [x] `make up` brings up NATS + Postgres + MinIO; all three `(healthy)` — verified with alternate ports due to existing services on dev machine
- [x] Healthcheck endpoints: NATS `/healthz` 200, `pg_isready` OK, MinIO `/minio/health/live` 200
- [x] NATS JetStream streams=0 (clean state; gateway provisions in P3)
- [x] `buf lint` (STANDARD) passes; `buf config ls-breaking-rules` lists WIRE_JSON rules (22 rules)
- [x] `go build ./...` succeeds (module valid, no sources)
- [x] `.gitignore` excludes `go.work`, `go.work.sum`, `appdata/`, `proto/gen/`, `.env.local`
- [x] `.env.example` committed; README documents port-override pattern
- [x] `init.sql` is comment-only placeholder; re-running `make down -v && make up` succeeds cleanly
- [ ] Repo pushed to `vanducng/mio` — orchestrator said "DO NOT push", so this criterion deferred

## Out-deferred

- `buf breaking --against '.git#branch=main'` fully clean only after this commit lands on main (the placeholder.proto must be in the baseline)
- `gateway-build` / `gateway-build-local` Makefile targets untestable until P3 adds `gateway/Dockerfile`
- `tools/proto-roundtrip/` is an empty dir — P1 adds the Go source file in the root module per pre-cook fix (c)
- `proto/channels.yaml` is a placeholder skeleton — first registry entry (`zoho_cliq`) lands in P3

## Open questions / risks

- `ubi:bufbuild/buf` deprecation warning from mise — non-fatal now; migrate to `github:bufbuild/buf` before mise 2027.1.0
- Port collision on dev machines is real: this dev machine had 4222, 8222, 9000, 9001, 5432 all occupied by other services; `.env.example` + NATS_MON_PORT override made smoke-verify possible without touching existing containers
- `protoc` version pinned to `27` (resolves to 27.5 via mise's protoc backend) — aligns with plan spec; buf remote plugins don't need a local protoc, so this is informational-only for P0
