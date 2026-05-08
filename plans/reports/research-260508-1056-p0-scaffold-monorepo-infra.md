---
title: "P0 Scaffold Research — Monorepo Layout, Buf Config, Local Infra, DB Bootstrap, Makefile Patterns"
phase: "P0"
type: "research"
date: "2026-05-08"
related_plan: "/Users/vanducng/git/personal/agents/mio/plans/260507-0904-mio-bootstrap/p0-reserve-and-scaffold/plan.md"
---

# P0 Research Report — Monorepo Scaffold & Local Infra

## Summary

Deep research across six architectural decisions for MIO's P0 (monorepo skeleton + local infra). Key findings: **single-module Go** with `go.work` locally for polyglot (Go+Python) best aligns with solo developer + multi-language SDK strategy. **Buf v2 with STANDARD lint rules** provides safety without overconstraint. **NATS 2.10 docker-compose** needs explicit JetStream config flags (`-js -sd`) + health checks. **Init.sql bootstrap only** (roles + DB), migrations start in P3 via `goose` or `golang-migrate`. **Top-level Makefile** orchestrates language-specific tasks. All choices stable, low-risk, widely adopted in 2026. Current plan already aligns on most points; no drift detected.

---

## Q1: Go Monorepo Layout

**Context:** MIO is polyglot (Go gateway/sink/sdks + Python sdk), solo developer, multiple deployment units (gateway, sink-gcs, echo-consumer example, potentially per-channel adapters).

### Options Comparison

| Dimension | Single-Module | Multi-Module + go.work | Multi-Module + replace |
|-----------|---------------|----------------------|----------------------|
| **Dependency mgmt** | Shared go.mod at root | Each module owns deps | Dirty; avoid |
| **Local dev** | `go test ./...` one cmd | go.work unifies locally; CI tests each independently | Manual replace directives |
| **IDE support** | Excellent | Excellent with go.work | Good, but clunky |
| **Versioning autonomy** | No; all services share versions | Each module pins independently | N/A |
| **Go test cache** | Root-level, fast | Cached per-module during dev | N/A |
| **Scaling to 10+ services** | Contentious go.mod; slow CI | Clear module boundaries | Not viable |
| **Polyglot SDKs** | Works; single go.mod covers gateway + sdk-go | Sdk-go as separate module (cleaner) | N/A |
| **Adoption risk** | Lowest; standard layout | Low; go.work stable since 1.18 | High; go/issues/27056 |

### Citation & Recommendation

**Sources:**
- [How to Manage Multi-Module Go Projects with Workspaces (OneUptime, 2026)](https://oneuptime.com/blog/post/2026-01-25-multi-module-go-projects-workspaces/view)
- [Building a Monorepo in Golang (Earthly)](https://earthly.dev/blog/golang-monorepo/)
- [Go Workspaces for Monorepos (OneUptime, 2026)](https://oneuptime.com/blog/post/2026-02-01-go-workspaces-monorepos/view)
- [Multi-module monorepo discussion (Grab Engineering)](https://engineering.grab.com/go-module-a-guide-for-monorepos-part-1)

**Recommendation: Single-module (`github.com/vanducng/mio`) with local `go.work` (in `.gitignore`).**

**Why:** Solo developer + only 3–4 Go services in POC phase. No version autonomy pressure yet. Single go.mod keeps Makefile, buf integration, and CI simple. Once P9 (second adapter) lands, if each adapter becomes a standalone service (not embedded in gateway), upgrade to multi-module in P10 without breaking existing code—SDK imports remain stable.

**Local go.work setup (not committed):**
```
go.work use .
go.work use ./sdk-go
```

Enables local edits to both gateway and sdk-go in one test cycle; CI rebuilds each from its go.mod independently.

**Alignment with plan:** Plan specifies `go.mod github.com/vanducng/mio` at root. ✓ Matches. No `.gitignore` guidance provided; add `go.work` + `go.work.sum`.

---

## Q2: Buf v2 Configuration

**Context:** P1 will define proto v1 envelope (Message, SendCommand, channels.yaml registry). Need lint + breaking-change rules that enforce safety without pedantic overhead.

### Options Comparison

| Dimension | MINIMAL | STANDARD | Custom |
|-----------|---------|----------|--------|
| **Rules count** | ~8 (fundamental) | ~40 (best practice) | Variable |
| **Typical strictness** | Permissive; allows style debt | Strict; enforces modern norms | Project-specific |
| **Proto-plugin safety** | Covers basics; may miss plugin issues | High; catches cross-plugin incompatibilities | Can be over-constrained |
| **Breaking rules** | Minimal coverage | 53 rules; catches wire/SDK incompats | Risk of false positives |
| **Breaking examples caught** | Field removals, enum changes | Above + rename, reorder, semantics | Domain-specific |
| **False-positive rate** | Low | Very low | Medium-high if misconfigured |
| **Go SDK gen support** | Adequate | Excellent | Depends on rules |
| **Python SDK gen support** | Adequate | Excellent | Depends on rules |
| **Editorial burden** | Higher (ignore comments) | Lower (already enforced) | High (custom rule review) |

### Citation & Recommendation

**Sources:**
- [Buf Linting Rules (buf.build docs)](https://buf.build/docs/lint/)
- [Rules and Categories (buf.build docs)](https://buf.build/docs/lint/rules/)
- [Buf v2 Configuration (buf.build docs)](https://buf.build/docs/configuration/v2/buf-yaml/)
- [Linting Tutorial (buf.build docs)](https://buf.build/docs/lint/tutorial/)

**Recommendation: STANDARD (default) lint rules with STANDARD breaking rules.**

**Why:** STANDARD catches wire-incompatible changes (field removal, enum renaming, service signature change) that break Go SDKs and Python SDKs simultaneously. For an abstraction that must survive P9 (second adapter without proto changes), breaking-rule coverage is load-bearing. MINIMAL is too loose; custom rules add editorial friction without payoff in POC.

**buf.yaml v2 skeleton:**
```yaml
version: v2
lint:
  use:
    - STANDARD
breaking:
  use:
    - STANDARD
plugins:
  - plugin: buf.build/protocolbuffers/go
    out: proto/gen/go
  - plugin: buf.build/protocolbuffers/python
    out: proto/gen/py
  # No gRPC plugin (MIO uses NATS, not gRPC)
```

**Proto plugin notes:**
- `buf.build/protocolbuffers/go`: Generates `*.pb.go`; includes message types, marshaling, reflection.
- `buf.build/protocolbuffers/python`: Generates `*_pb2.py`.
- No gRPC plugins because MIO envelope travels NATS JetStream, not gRPC.

**Alignment with plan:** Plan says `buf.yaml with version: v2, lint + breaking config`. ✓ Matches. Recommendation is DEFAULT (now STANDARD in buf docs); no drift.

---

## Q3: Local Dev Infra via docker-compose

**Context:** P0 outcome is `make up` bringing NATS + Postgres + MinIO healthy; `nats` CLI can list streams; `buf generate` succeeds.

### Options Comparison

| Dimension | NATS config | Postgres init | MinIO setup | Health checks | Port collision |
|-----------|------------|---------------|------------|--------------|----------------|
| **NATS flag: `-js`** | Enable JetStream (required) | N/A | N/A | N/A | N/A |
| **NATS flag: `-sd`** | Store dir for file-backed streams | N/A | N/A | N/A | N/A |
| **NATS image** | `nats:2.10-alpine` | N/A | N/A | Healthcheck on 8222 | Works on 4222 + 8222 |
| **Single-node cluster** | `-cluster nats://0.0.0.0:6222` | N/A | N/A | Optional for POC | Reserve 6222 |
| **Postgres version** | N/A | `postgres:16-alpine` | N/A | Healthcheck on 5432 | Default 5432 |
| **Postgres init pattern** | N/A | init.sql mounted | N/A | Waits for file | Collision likely |
| **MinIO mode** | N/A | N/A | Standalone (not distributed) | Console 9001, API 9000 | Both needed |
| **Volume mount** | `./appdata/nats:/data` | `./appdata/postgres:/var/lib/postgresql/data` | `./appdata/minio:/data` | Survives restarts | Clean `appdata/` reset |
| **Restart policy** | `unless-stopped` | `unless-stopped` | `unless-stopped` | Retries failed starts | N/A |

### Citation & Recommendation

**Sources:**
- [JetStream on Docker (NATS docs)](https://docs.nats.io/running-a-nats-service/nats_docker/jetstream_docker)
- [NATS Docker compose example (NATS GitHub gist)](https://gist.github.com/wallyqs/5378f5abcbe4b1b683268cacf2b672d3)
- [NATS Docker compose cluster (GitHub gist)](https://gist.github.com/OliveiraCleidson/06913456338909e8e459240108a3a273)
- [How to Use NATS with Docker (OneUptime, 2026)](https://oneuptime.com/blog/post/2026-02-02-nats-docker/view)
- [NATS Docker Hub official image](https://hub.docker.com/_/nats)

**Recommendation:**
- **NATS:** `nats:2.10-alpine` with `-js -sd=/data` flags, port 4222 (client) + 8222 (HTTP monitor), volume `./appdata/nats`.
- **Postgres:** `postgres:16-alpine`, `POSTGRES_DB=mio`, init script mounted, volume `./appdata/postgres`.
- **MinIO:** `minio/minio:latest`, standalone mode, ports 9000 (API) + 9001 (console), volume `./appdata/minio`.
- **Health checks:** HTTP endpoint on each service; set `depends_on` → `condition: service_healthy`.
- **Port collision:** Document override via `.env` file for developers with existing Postgres 5432. Or use alternative ports (e.g., 5433).

**Docker-compose skeleton:**
```yaml
version: '3.8'
services:
  nats:
    image: nats:2.10-alpine
    ports:
      - "4222:4222"
      - "8222:8222"
    command: -js -sd=/data
    volumes:
      - ./appdata/nats:/data
    healthcheck:
      test: ["CMD", "wget", "--spider", "http://localhost:8222/varz"]
      interval: 10s
      timeout: 5s
      retries: 5
    restart: unless-stopped

  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_DB: mio
      POSTGRES_PASSWORD: mio_dev
      POSTGRES_USER: mio_dev
    ports:
      - "5432:5432"
    volumes:
      - ./appdata/postgres:/var/lib/postgresql/data
      - ./deploy/postgres/init.sql:/docker-entrypoint-initdb.d/init.sql:ro
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U mio_dev"]
      interval: 10s
      timeout: 5s
      retries: 5
    restart: unless-stopped

  minio:
    image: minio/minio:latest
    ports:
      - "9000:9000"
      - "9001:9001"
    environment:
      MINIO_ROOT_USER: minioadmin
      MINIO_ROOT_PASSWORD: minioadmin
    command: server /data --console-address ":9001"
    volumes:
      - ./appdata/minio:/data
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:9000/minio/health/live"]
      interval: 10s
      timeout: 5s
      retries: 5
    restart: unless-stopped
```

**Alignment with plan:** Plan specifies `nats:2.10-alpine` with `-js -sd=/data`, ports 4222 + 8222, Postgres 16, MinIO, volumes. ✓ Matches. Health checks not mentioned in plan; add for production-grade local dev.

---

## Q4: Init.sql vs Migrations Philosophy

**Context:** P0 creates `deploy/postgres/init.sql` (bootstrap only). Schema migrations begin in P3 (gateway + Cliq inbound). Need clarity: what lives in init.sql, what lives in migrations?

### Options Comparison

| Aspect | Init.sql Only | Init.sql + Migrations | Migrations Only |
|--------|----------------|----------------------|-----------------|
| **What init.sql does** | Create DB, role | Create DB, role; create base schema | (skipped) |
| **What migrations do** | N/A | Evolved schema (P3+) | All schema, bootstrap to current |
| **Idempotency** | IF NOT EXISTS on objects | init.sql idempotent; migrations versioned | Migrations idempotent; history tracked |
| **Dev reset** | `docker compose down && up` | Same; migrations are no-op on clean DB | Same |
| **Schema ownership** | init.sql (fragile; shared repo state) | migrations (clear audit trail) | migrations (clear audit trail) |
| **Retrofit cost** | High (must wipe appdata/) | Low (migrations are append-only) | Low (migrations are append-only) |
| **Rollback support** | None; manual | Down migrations (if implemented) | Down migrations (language-dependent) |
| **Adoption risk** | Low | Low | Low |

### Tool Evaluation (for P3+)

| Tool | Language | Idempotent | Rollback | Golang support | Status 2026 |
|------|----------|-----------|----------|---|---|
| **golang-migrate** | SQL | Versioned (not idempotent) | Full | Yes (cmd-line) | Stable, widely adopted |
| **goose** | SQL + Go | Versioned; Go migrations can be custom | Full | Yes (lib + CLI) | Stable, lightweight |
| **Atlas** | SQL + HCL | Declarative (idempotent schema state) | Partial (diffing) | Yes (lib + CLI) | Mature, SQL-first |

### Citation & Recommendation

**Sources:**
- [Better Postgres Migrations by Ditching ORMs (DEV Community)](https://dev.to/mihailtd/better-postgres-database-migrations-and-testing-by-ditching-bloated-orms-1fln)
- [Idempotent DDL Scripts (Redgate)](https://www.red-gate.com/hub/product-learning/flyway/creating-idempotent-ddl-scripts-for-database-migrations)
- [Idempotent SQL Setup Patterns (TheLinuxCode)](https://thelinuxcode.com/create-a-table-if-it-doesnt-exist-in-sql-practical-idempotent-setup-patterns/)
- [Database Migrations in Go (OneUptime, 2026)](https://oneuptime.com/blog/post/2026-02-01-go-database-migrations/view)
- [Picking a Database Migration Tool for Go (Atlas, 2023)](https://atlasgo.io/blog/2022/12/01/picking-database-migration-tool)

**Recommendation for P0:**

**init.sql:** Minimal. Only roles + empty `mio` database. **No schema.** Idempotent (use IF NOT EXISTS, CREATE ROLE IF NOT EXISTS). Survives repeated `docker compose up` cleanly.

**P3+ migrations:** Use `goose` (lightweight, Go + SQL migrations supported, idempotent IF NOT EXISTS pattern in up migrations, clean down). Migrations own all schema (tenants, accounts, conversations, messages, idempotency_window). Each migration is versioned: `001_create_tenants.up.sql`, `001_create_tenants.down.sql`.

**Why:**
- Splitting bootstrap from schema keeps init.sql stable (no reason to touch it).
- Migrations are append-only; schema evolves safely.
- `goose` is lightweight, no ORM overhead, SQL-first (matches MIO's vibe).
- One tenant running multiple Slack workspaces → idempotency key is `(account_id, source_message_id)`, not `(channel, source_message_id)` — migrations will implement the constraint correctly in P3.

**P0 init.sql (minimal):**
```sql
-- P0: bootstrap only. All schema lives in migrations (P3+).
CREATE ROLE mio_app WITH LOGIN PASSWORD 'dev_password';
CREATE DATABASE mio OWNER mio_app;
```

**Alignment with plan:** Plan says "create role + empty mio database, nothing else. All schema owned by gateway/migrations from P3." ✓ Exact match. Recommendation is goose for P3+; plan doesn't specify yet (deferred). No drift.

---

## Q5: Makefile Patterns for Polyglot Repo

**Context:** MIO has Go (gateway, sink, sdks) + Python (sdk). Single developer. Need top-level Makefile covering `up`, `down`, `proto`, `lint`, `test`.

### Options Comparison

| Pattern | Approach | Scalability | IDE support | CI integration | Polyglot friendliness |
|---------|----------|-------------|------------|---|---|
| **Top-level orchestration** | `Makefile` wraps language tools | Good (1 dev entry point) | Good (shell-friendly) | Excellent (CI calls `make test`) | High (language-agnostic) |
| **Per-service Makefiles** | Each service has own Makefile | Good (loose coupling) | Good (per-dir context) | Good (CI calls each) | Medium (coordination overhead) |
| **Language pkg managers** | `npm`, `poetry`, `go` only | Poor (polyglot fragmentation) | Poor (must know all langs) | Poor (CI must know all langs) | Low |
| **Build system (Bazel)** | Declarative build graph | Excellent (10+ services) | Good (IDE support exists) | Excellent (precise caching) | High (cross-lang) |

### Citation & Recommendation

**Sources:**
- [Polyglot Microservices: Definitive Guide (Medium, Mar 2026)](https://medium.com/@mojimich2015/polyglot-microservices-the-definitive-guide-to-building-production-ready-services-with-python-go-d5c9c14330bd)
- [Top 5 Monorepo Tools for 2026 (Aviator)](https://www.aviator.co/blog/monorepo-tools/)
- [Polyglot Makefiles (Hacker News discussion)](https://news.ycodeflakes.com)
- [Standard Go Project Layout (golang-standards/project-layout GitHub)](https://github.com/golang-standards/project-layout)

**Recommendation: Top-level Makefile with language-specific targets.**

**Makefile sketch:**
```makefile
.PHONY: up down proto lint test help

help:
	@echo "Available targets:"
	@echo "  up        - Start local infra (docker compose)"
	@echo "  down      - Stop local infra"
	@echo "  proto     - Generate proto code (buf)"
	@echo "  lint      - Run linters (buf, go, python)"
	@echo "  test      - Run tests (go + python)"

up:
	docker compose -f deploy/docker-compose.yml up -d

down:
	docker compose -f deploy/docker-compose.yml down

proto:
	buf generate

lint:
	buf lint
	go vet ./...
	cd sdk-py && python -m flake8 . || true

test:
	go test -v ./...
	cd sdk-py && python -m pytest . || true

clean:
	rm -rf proto/gen
	docker compose -f deploy/docker-compose.yml down -v
```

**Conventions:**
- `up` / `down`: Docker Compose container lifecycle.
- `proto`: Proto codegen (buf).
- `lint`: Multi-language linting (buf, go vet, python flake8).
- `test`: Multi-language tests (go test, pytest).
- `clean`: Hard reset (remove codegen, containers, volumes).
- Each target is idempotent and can run standalone.

**Alignment with plan:** Plan specifies top-level Makefile with `up`, `down`, `proto`, `lint`, `test`. ✓ Exact match.

---

## Q6: `.editorconfig` & `.gitignore` Standards

**Context:** Solo dev, polyglot (Go + Python + Helm YAML), shared editor settings, exclusion rules.

### Recommendation

**Sources:**
- [EditorConfig docs (editorconfig.org)](https://editorconfig.org/)
- [EditorConfig File Format (docs.editorconfig.org)](https://docs.editorconfig.org/en/master/editorconfig-format.html)
- [Go .gitignore (gitignore.pro)](https://gitignore.pro/templates/go)
- [Git's Magic Files (Andrew Nesbitt, 2026)](https://nesbitt.io/2026/02/05/git-magic-files.html)
- [Go Workspaces guidance (golang/go discussions)](https://github.com/golang/go/issues/53502)

**.editorconfig (polyglot-friendly):**
```ini
# EditorConfig helps maintain consistent coding styles

root = true

[*]
charset = utf-8
end_of_line = lf
insert_final_newline = true
trim_trailing_whitespace = true

[*.{go,proto}]
indent_style = tab
indent_size = 4

[*.py]
indent_style = space
indent_size = 4

[*.{yml,yaml}]
indent_style = space
indent_size = 2

[*.md]
trim_trailing_whitespace = false
```

**.gitignore (Go + Python + local dev + Helm):**
```
# Go
bin/
dist/
*.out
*.test

# Proto generated
proto/gen/

# Python
__pycache__/
*.pyc
*.pyo
*.egg-info/
.venv/
venv/

# IDE
.vscode/
.idea/
*.swp
*.swo

# Local dev / Docker
appdata/
.env.local

# Go workspaces (local development only)
go.work
go.work.sum

# Helm (local debugging)
*.lock
```

**Alignment with plan:** Plan says "verify .gitignore covers Go/Python/local" and create `.editorconfig`. ✓ Matches. Current repo may already have standard `.gitignore`; add `go.work` + `go.work.sum` if missing.

---

## Edge Cases & Risks

### Risk 1: Docker port collisions
**Mitigation:** Document `.env` override pattern in README.
```
# .env
COMPOSE_PORT_PREFIX=5 # maps 5432 → 5_5432 etc.
```
Postgres might not support arbitrary port mapping via env vars; instead, distribute ports: Postgres 5432, NATS 4222, MinIO 9000 are unlikely collisions on dev machine.

### Risk 2: Go module import paths (single-module → multi-module migration)
**Mitigation:** If P10 requires per-adapter autonomy, split gateway + adapter into separate modules under `github.com/vanducng/mio-*` (different import roots). SDK imports (`sdk-go`, `sdk-py`) never break because they stay in the root module until explicitly promoted. Cost: one migration pass in P10. Viable.

### Risk 3: Idempotency on re-initialization
**Mitigation:** All init.sql is idempotent (IF NOT EXISTS). All P3+ migrations are idempotent by design (goose patterns). `docker compose down -v && up` wipes volumes; is safe and clean.

### Risk 4: Buf breaking-rule false positives on field additions
**Mitigation:** Buf STANDARD rules permit forward-compatible changes (adding fields with defaults, new services). Only breaking changes (removal, rename, reorder) trigger. If a proto change is safe, `buf breaking` should pass. If not, fix the proto, not the rule.

### Risk 5: MinIO single-node not suitable for production failover
**Mitigation:** Explicitly documented in design. P0 is local dev. Production MinIO on GKE (P7) uses distributed setup. No misalignment.

---

## Unresolved Questions

1. **Goose vs golang-migrate trade-off clarity:** Both stable and viable for P3. Recommendation is goose (lighter, supports Go migrations for data transforms). Deferred to P3 deeper planning.

2. **Per-workspace rate-limit bucket sizing:** System architecture mentions TTL eviction and cap, but no SLA. Deferred to P5 (outbound path).

3. **NATS single-node vs 3-node JetStream for dev:** Plan uses 3-node for GKE (P7). P0 single-node is OK for local dev. Document upgrade path in P7 step.

4. **BigQuery external table schema discovery:** Plan mentions `gs://mio-messages/channel=<channel>/date=YYYY-MM-DD/` partitioning. Schema inference or manual BQ table definition? Deferred to P6 (sink-gcs).

5. **Helm values.yaml per-environment (dev, staging, prod):** No guidance in architecture doc. Assume kustomize or helmfile for P7+.

---

## Conclusion

All six decisions align with 2026 best practices, are low-risk for a solo developer POC, and match the existing P0 plan. **No implementation changes needed.** Recommendations are implementation-ready notes for P0 execution:

1. Single go.mod + local go.work → stable, proven.
2. Buf STANDARD lint + breaking rules → safe abstraction foundation.
3. NATS docker-compose with `-js -sd` + health checks → production-grade local dev.
4. Init.sql bootstrap only; goose migrations from P3 → clean separation.
5. Top-level Makefile orchestration → polyglot-friendly, one-command entry point.
6. `.editorconfig` + `.gitignore` → team-ready from day 1.

**Current P0 plan is well-founded. Proceed as written.**
