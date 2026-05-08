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
  - `go.mod` (module `github.com/vanducng/mio`)
  - `Makefile` (top-level `up`, `down`, `proto`, `lint`, `test`)
  - `buf.yaml`, `buf.gen.yaml` at repo root
  - `proto/mio/v1/.gitkeep` (placeholder for P1)
  - `proto/channels.yaml` (placeholder; populated in P1)
  - `gateway/.gitkeep`, `sdk-go/.gitkeep`, `sdk-py/.gitkeep`, `sink-gcs/.gitkeep`, `examples/echo-consumer/.gitkeep`, `deploy/charts/.gitkeep`
  - `deploy/docker-compose.yml` (NATS + Postgres + MinIO)
  - `deploy/postgres/init.sql` (creates empty `mio` DB only — schema lives in `gateway/migrations/` from P3)
  - `.editorconfig`
- **Modify:**
  - `README.md` — add Quickstart section pointing at `make up`
  - `.gitignore` — already covers Go/Python/local; verify

## Steps

1. `go mod init github.com/vanducng/mio`
2. Add `buf.yaml` with `version: v2`, lint + breaking config; `buf.gen.yaml` targeting `proto/gen/{go,py}` (paths-by-package).
3. Write `deploy/docker-compose.yml`:
   - `nats:2.10-alpine` with `-js -sd=/data`, ports 4222 + 8222, volume `./appdata/nats`
   - `postgres:16-alpine`, `POSTGRES_DB=mio`, init script mounted, volume `./appdata/postgres`
   - `minio/minio:latest` with console on 9001, volume `./appdata/minio`
4. Write `deploy/postgres/init.sql` — minimal: create role + empty `mio` database, nothing else. All schema (tenants, accounts, conversations, messages — see P3 §"DB schema") is owned by `gateway/migrations/`. Foundation: `tenant_id` and `account_id` are NOT NULL from row 1; idempotency address is `(account_id, source_message_id)`, not `(channel, source_message_id)` — no inbound_dedupe table here.
   ```sql
   -- init.sql: bootstrap only. Schema migrations live in gateway/migrations/.
   CREATE DATABASE mio;
   ```
5. Top-level `Makefile`:
   - `up`: `docker compose -f deploy/docker-compose.yml up -d`
   - `down`: `docker compose -f deploy/docker-compose.yml down`
   - `proto`: `buf generate`
   - `lint`: `buf lint && go vet ./...`
   - `test`: `go test ./...`
6. Update `README.md` — Quickstart points at `make up`, `make proto`.
7. Commit + push.

## Success Criteria

- [ ] `make up` brings up NATS + Postgres + MinIO; all healthy
- [ ] `nats` CLI (locally or via `docker compose run`) lists no streams (clean state)
- [ ] `buf lint` passes on empty proto dir
- [ ] `go build ./...` succeeds (no source files yet → no-op, but module valid)
- [ ] Repo pushed to `vanducng/mio` on GitHub

## Risks

- **Go module name vs package import paths** — get `github.com/vanducng/mio` right the first time; rename later forces every consumer to reimport
- **Docker compose port collisions** — Postgres 5432 often in use locally; document override via `.env`
- **Schema in init.sql vs migrations** — keep init.sql empty (DB only). Putting tables in init.sql forces every dev to wipe `appdata/postgres/` whenever migrations evolve. Migrations are the single source of truth from P3 onward.

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
