---
phase: 7
title: "Helm charts + NATS on GKE"
status: pending
priority: P1
effort: "1–2d"
depends_on: [3, 5, 6]
---

# P7 — Helm charts + NATS on GKE

## Overview

Cluster-side artifact creation. Four Helm charts (`mio-nats`, `mio-jetstream-bootstrap`,
`mio-gateway`, `mio-sink-gcs`) bring up a 3-replica JetStream cluster + gateway +
sink-gcs on GKE. Only K8s primitives + GCS/Workload Identity (fenced behind
values overrides so the charts also run on kind for smoke-testing).

**Cross-phase contract owned by this phase** (made explicit so later phases don't
re-litigate it):

- **Stream/consumer provisioning is gateway-startup AUTHORITATIVE.** Single source
  of truth is `gateway/internal/store/jetstream.go::AddOrUpdateStream` from P3.
  The `mio-jetstream-bootstrap` Job is **VERIFY-ONLY** — it asserts that streams
  and consumers exist post-install and FAILS the Helm release if they're missing.
  It does NOT call `nats stream add` or `nats consumer add`. Two writers = race;
  pick one, and we picked the gateway (P3 owns the spec).
- Metric labels across all charts: `channel_type`, `direction`, `outcome`.
  ServiceMonitor scrape paths consistent across charts.
- Subject grammar: `mio.<dir>.<channel_type>.<account_id>.<conversation_id>[.<message_id>]`
  validated at the gateway side; underscore-only slugs (`zoho_cliq`, never `zoho-cliq`).
- Schema-version: enforced via SDK on publish; charts pass `MIO_SCHEMA_VERSION_STRICT=true`.
- Filename scheme (sink-gcs): offset-based `<consumer-id>-<seq-start>-<seq-end>.ndjson`
  per the P6 fix — required for multi-replica sink. This phase enables that path
  but stays at 1 replica for POC.

## Goal & Outcome

**Goal:** `helm install` brings up a 3-replica JetStream cluster + gateway + sink-gcs
on GKE; charts pass `helm lint` and a kind cluster smoke test.

**Outcome:** GKE has a running `mio` namespace with healthy NATS quorum (verified
via `nats stream cluster info`), gateway behind a GCE Ingress, sink-gcs pointed
at a real GCS bucket via Workload Identity, and the bootstrap Job has confirmed
all four streams + three consumers exist (created earlier by gateway/sink startup).

## Files

- **Create:**
  - `deploy/charts/mio-nats/Chart.yaml` — declares `nats-io/k8s` upstream chart as a dependency (don't hand-roll routes)
  - `deploy/charts/mio-nats/values.yaml` — values overlay only: replicas=3, pd-balanced PVC, zone-spread, PDB, ServiceMonitor
  - `deploy/charts/mio-nats/templates/{servicemonitor.yaml}` — only chart-specific extras; routes/STS come from upstream
  - `deploy/charts/mio-jetstream-bootstrap/Chart.yaml` — verify-only Helm hook chart
  - `deploy/charts/mio-jetstream-bootstrap/templates/{configmap.yaml,job.yaml,rbac.yaml}` — ConfigMap holds EXPECTED specs (audit/diff); Job runs `nats stream info` + `nats consumer info` and exits non-zero on miss
  - `deploy/charts/mio-gateway/Chart.yaml`
  - `deploy/charts/mio-gateway/values.yaml`
  - `deploy/charts/mio-gateway/templates/{deployment.yaml,service.yaml,ingress.yaml,hpa.yaml,configmap.yaml,secret.yaml,servicemonitor.yaml,migration-job.yaml}`
  - `deploy/charts/mio-sink-gcs/Chart.yaml`
  - `deploy/charts/mio-sink-gcs/values.yaml`
  - `deploy/charts/mio-sink-gcs/templates/{deployment.yaml,serviceaccount.yaml,servicemonitor.yaml}`
  - `deploy/gke/setup.sh` — provisions cluster, Cloud SQL, GCS bucket, GSA + WI binding, waits for IAM propagation, creates `ghcr-pull` imagePullSecret in `mio` namespace from `GHCR_PAT` env var, installs charts in order
  - `deploy/gke/README.md`
  - `.github/workflows/ci.yaml` — single GHA workflow on `push` + `pull_request: main`. Jobs: `changes` (dorny/paths-filter@v3 → outputs `gateway`, `sdk-py`, `proto`, `helm` flags); `test-proto` (mise + `buf lint` + `buf breaking --against 'origin/main'`); `test-gateway` (mise + golangci-lint + `go test -race ./gateway/... ./sdk-go/...`); `test-python` (mise + `ruff check` + `pytest`); `helm-lint` (helm 3 + `chart-testing ct lint`); `build-gateway` and `build-sink-gcs` (only on `push: main` or tags) — `docker/build-push-action@v6` to `ghcr.io/vanducng/mio/{gateway,sink-gcs}:${SHA_SHORT}` + `:main` + `:v<semver>` on tag, registry cache `cache-from/to=type=registry,ref=...:cache,mode=max`. Permissions: `contents: read, packages: write`. mise activated via `jdx/mise-action@v2 with: cache: true`.
  - `.github/workflows/deploy.yaml` — runs on `push: main` after `ci.yaml` succeeds (`workflow_run` trigger). One job: authenticate to GKE via `google-github-actions/auth@v2` + `get-gke-credentials@v2` (using `secrets.GCP_SA_JSON`); run `helm upgrade --install mio-gateway deploy/charts/mio-gateway --namespace mio --set image.tag=${{ github.sha }} --wait --timeout 5m`. Post-deploy: `kubectl rollout status deployment/mio-gateway -n mio --timeout=300s`.
  - `.github/dependabot.yml` — weekly bumps for `github-actions`, `gomod` (`gateway/`, `sdk-go/`, `sink-gcs/`), `pip` (`sdk-py/`, `examples/echo-consumer/`), `docker` (Dockerfiles). Group minor + patch.
- **Modify:**
  - `Makefile` — `helm-lint`, `kind-up`, `kind-deploy`, `kind-echo-test` (full smoke before GKE)
  - `deploy/charts/mio-gateway/values.yaml` — image block: `image.registry: ghcr.io`, `image.repository: vanducng/mio/gateway`, `image.tag: latest` (overridden by GHA `--set image.tag=<sha>`), `image.pullPolicy: IfNotPresent`, `imagePullSecrets: [{ name: ghcr-pull }]`. Same pattern in `mio-sink-gcs/values.yaml`.

## JetStream stream definitions (locked here; provisioned by gateway in P3)

```
# stream MESSAGES_INBOUND
subjects:    mio.inbound.>
retention:   limits
storage:     file
replicas:    3
max_age:     168h                    # 7d; archive owns durability past this
duplicates:  120s                    # Nats-Msg-Id dedup window (gateway side)
discard:     old                     # drop oldest if max_bytes reached
max_bytes:   50Gi                    # tune in production

# stream MESSAGES_OUTBOUND
subjects:    mio.outbound.>
retention:   workqueue                # consumed-once
storage:     file
replicas:    3
max_age:     24h
duplicates:  60s
max_bytes:   10Gi
```

Subject grammar (matches P2 SDK):
```
mio.inbound.<channel_type>.<account_id>.<conversation_id>
mio.outbound.<channel_type>.<account_id>.<conversation_id>[.<message_id>]
```

`<channel_type>` is the registry slug — `zoho_cliq`, `slack` (underscore;
no hyphens), per `proto/channels.yaml`. Wildcards: `mio.inbound.zoho_cliq.>`
matches all Cliq inbound; never `mio.inbound.zoho-cliq.>`. Validation runs at
the gateway, not at NATS — NATS will accept either form, so the gateway is the
guard. The bootstrap Job confirms the registry slug subjects are bound to
real streams after install.

## Consumer definitions (created by their owning service on first start)

```
# ai-consumer (P4 owns it; created on first echo-consumer / AI-service start)
stream:           MESSAGES_INBOUND
durable_name:     ai-consumer
ack_policy:       explicit
ack_wait:         30s
max_ack_pending:  1                   # per-conversation ordering
max_deliver:      5
deliver_policy:   all
filter_subject:   mio.inbound.>
replay_policy:    instant

# sender-pool (P5; created by gateway on start)
stream:           MESSAGES_OUTBOUND
durable_name:     sender-pool
ack_policy:       explicit
ack_wait:         30s
max_ack_pending:  32                  # parallelism across accounts
max_deliver:      5

# gcs-archiver (P6; created by sink-gcs on start)
stream:           MESSAGES_INBOUND
durable_name:     gcs-archiver
ack_policy:       explicit
ack_wait:         60s
max_ack_pending:  64                  # batching > strict ordering
max_deliver:      -1                  # never give up; archival is durable
```

## Steps

### 1. `mio-nats` chart — values overlay on upstream `nats-io/k8s`

1.1. `Chart.yaml` declares dependency on `nats-io/k8s` (`nats` chart, pinned minor version). Pull via `helm dependency update`.
1.2. `values.yaml` sets:
- `cluster.replicas: 3`, `cluster.enabled: true`, JetStream file storage enabled.
- `topologySpreadConstraints` with `topologyKey: topology.kubernetes.io/zone`, `maxSkew: 1`, `whenUnsatisfiable: DoNotSchedule` — 1 replica per zone on 3-zone regional GKE.
- PVC: `storageClass: pd-balanced`, `size: 10Gi`. (Research-validated: ~4.3 days retention at medium load; resize to 20Gi if alert fires; pd-ssd is overkill for streaming append-only writes.)
- PDB: `minAvailable: 2` — protects RAFT quorum during single node drain (zone loss, autoscaler scale-down, rolling upgrade). Accepts slower scale-down as the tradeoff.
- ServiceMonitor stub for Prometheus scrape (chart-local extras folder).
1.3. Do NOT hand-roll cluster routes config — upstream chart auto-discovers via headless service FQDN.
1.4. Pin upstream chart version in `Chart.yaml` so NATS server upgrades are intentional.

### 2. `mio-jetstream-bootstrap` chart — VERIFY-ONLY Helm hook

2.1. `templates/configmap.yaml` holds EXPECTED stream + consumer specs as YAML (audit / diff source-of-truth artifact). The Job consumes this for the verification list, not for creation.
2.2. `templates/job.yaml` annotated `helm.sh/hook: post-install,post-upgrade`, `helm.sh/hook-weight: "5"`. Job runs a shell script that:
- Polls `nats stream cluster info MESSAGES_INBOUND` + `MESSAGES_OUTBOUND` until each reports 3 healthy peers (RAFT quorum). Backoff retry 5×, ~2s sleep, ~60s budget.
- For each expected stream in the ConfigMap, runs `nats stream info <NAME>` and exits non-zero if missing or if `subjects` / `replicas` / `retention` drift from spec.
- For each expected consumer, runs `nats consumer info <STREAM> <DURABLE>` and exits non-zero if missing.
- Logs `OK <name>` per resource for the audit trail.
- Does NOT call `nats stream add`, `nats stream update`, `nats consumer add`, or any other mutating verb.
2.3. `templates/rbac.yaml` — minimal SA + Role for the Job's pod (no cluster-wide perms; only namespace read).
2.4. **Negative test (smoke gate):** A test values overlay deletes a stream from gateway startup so the Job hits a missing stream — install must fail. This proves the verification gate is real, not a no-op.

### 3. `mio-gateway` chart

3.1. Deployment with 2 replicas (HPA min) — **sequential rolling updates** via `maxSurge: 0, maxUnavailable: 1` until stream-config split-brain risk (P3 deferred-out) is resolved. Sequential rollout enforces single-writer for `AddOrUpdateStream` on startup; allowing two new-version pods concurrently with old (`maxSurge: 1`) lets disagreeing config race during upgrade.
3.2. Migration Job (`templates/migration-job.yaml`) annotated `helm.sh/hook: pre-install,pre-upgrade`, `helm.sh/hook-weight: "-5"`. Runs `migrate up` against Cloud SQL Postgres before any gateway pod starts. Single-writer guarantee.
3.3. Deployment `env`: `MIO_MIGRATE_ON_START=false` — Deployment pods never run migrations. Race-free with multi-replica scale-up.
3.4. Ingress `ingressClassName: gce`, cert-manager annotations (`cert-manager.io/issuer: letsencrypt-staging` for P7; flip to prod in P8). Body size limits per GCE LB defaults (32MB).
3.5. HPA via Prometheus Adapter (cloud-agnostic; avoids GCP lock-in). Target metric `mio_gateway_inbound_latency_seconds:rate1m`, threshold 500ms. `minReplicas: 2`, `maxReplicas: 10`.
3.6. ServiceMonitor scrapes `/metrics` on port 9090 with labels `channel_type`, `direction`, `outcome` (consistent contract).
3.7. ConfigMap for non-secret config (`MIO_TENANT_ID`, NATS URL, `MIO_SCHEMA_VERSION_STRICT=true`); Secret for Postgres URL + Cliq creds.
3.8. ServiceAccount can stay default — gateway doesn't need GCS access (sink-gcs owns the WI path).

### 4. `mio-sink-gcs` chart

4.1. Deployment **1 replica** for POC — explicitly enabled by P6's offset-based filename scheme (`<consumer-id>-<seq-start>-<seq-end>.ndjson`), but multi-replica is mandatory work for production after this phase.
4.2. ServiceAccount annotated `iam.gke.io/gcp-service-account: mio-sink-gcs@<PROJECT>.iam.gserviceaccount.com` for Workload Identity.
4.3. `env`: bucket name, flush settings, `MIO_SCHEMA_VERSION_STRICT=true`.
4.4. ServiceMonitor for Prometheus.
4.5. Liveness/readiness probes on a `/healthz` endpoint.

### 5. Lint + render gate

5.1. `helm lint deploy/charts/*` clean for all four charts.
5.2. `helm template` against representative `values-kind.yaml` and `values-gke.yaml` renders without errors.
5.3. `chart-testing (ct lint)` in CI for stricter coverage.

### 6. Kind smoke test

6.1. `make kind-up` creates a 1-node kind cluster, installs cert-manager.
6.2. `make kind-deploy` installs in order: MinIO (sink override) → `mio-nats` → `mio-jetstream-bootstrap` → `mio-gateway` (which triggers migration Job) → `mio-sink-gcs` (overridden to MinIO endpoint).
6.3. `make kind-echo-test`:
- Port-forward gateway, POST a synthetic Cliq webhook payload.
- Echo consumer (P4 binary, deployed in-cluster for the smoke) emits to `mio.outbound.zoho_cliq...`.
- Outbound path runs through gateway sender, which (in smoke mode) writes to a fake transport.
- Assert: stream + consumer list materializes; bootstrap Job reports `Completed`; MinIO contains the archived NDJSON object.
6.4. Negative test: delete a consumer pre-install, observe bootstrap Job fail the install (verification gate proven).
6.5. Goal: catch chart-level bugs (ordering, RBAC, label selectors) before any GKE spend.

### 7. GKE setup script (`deploy/gke/setup.sh`)

Idempotent, runs once per environment. Steps:

7.1. Create regional GKE cluster (3 zones), `e2-small` nodes, autoscaler `min=1 max=3` per zone, Workload Identity enabled (`--workload-pool=<PROJECT>.svc.id.goog`).
7.2. Defer `kube-prometheus-stack` to P8 — 3× e2-small RAM is too tight for full observability + NATS + gateway + sink (research finding). Leave a TODO comment.
7.3. Provision Cloud SQL Postgres 16, **private IP only** via Private Service Access + VPC peering (POC simplicity; Auth Proxy sidecar deferred to P8 if encryption/audit requirements force it).
7.4. Create GCS bucket `mio-messages-<env>` with lifecycle: Standard → Nearline @30d → Coldline @90d.
7.5. Create GSA `mio-sink-gcs@<PROJECT>.iam.gserviceaccount.com` with `roles/storage.objectAdmin` on the bucket.
7.6. Bind GSA → KSA `mio-sink-gcs` in the `mio` namespace via `roles/iam.workloadIdentityUser`.
7.7. **Wait 60 seconds** for IAM propagation. Then run a verification pod (`google/cloud-sdk:alpine`, `gsutil ls`) using the KSA — abort if it fails.
7.8. Apply Helm charts in dependency order: `mio-nats` → `mio-jetstream-bootstrap` (verify-only post-install hook) → `mio-gateway` (pre-install migration Job runs first) → `mio-sink-gcs`.
7.9. **imagePullSecret bootstrap.** `kubectl create secret docker-registry ghcr-pull -n mio --docker-server=ghcr.io --docker-username=<gh-bot> --docker-password=$GHCR_PAT --docker-email=ci@vanducng.dev`. PAT is a fine-grained GitHub PAT with `read:packages` on `vanducng/mio` only, 6-month expiry; rotation is a manual operator action documented in `deploy/gke/README.md`. (Workload Identity Federation to ghcr.io is deferred — ghcr does NOT support GKE WIF natively as of 2026-05; PAT is the cheapest path. Tracked as risk + P10 follow-up.)
7.10. Final assertion: `kubectl exec -n mio mio-nats-0 -- nats stream cluster info MESSAGES_INBOUND` shows `replicas=3, healthy`.

### 8. CI/CD + image publish to ghcr.io

8.1. **`.github/workflows/ci.yaml`** runs on every `push` and `pull_request` to `main`. Job topology:
- `changes` — `dorny/paths-filter@v3` produces flags (`gateway`, `sdk-py`, `proto`, `helm`); downstream jobs gate on the relevant flag so a docs-only PR skips Go test runs.
- `test-proto` — needs `changes.proto`. Steps: checkout (with `fetch-depth: 0` for `buf breaking`), `jdx/mise-action@v2` (cache enabled), `buf lint proto`, `buf breaking --against 'origin/main' proto`. Schema-version is enforced on publish (P2 invariant); breaking check protects forward-compat.
- `test-gateway` — needs `changes.gateway`. mise activate, `golangci-lint run ./gateway ./sdk-go`, `go test -race -cover ./gateway/... ./sdk-go/...`.
- `test-python` — needs `changes.sdk-py`. mise activate, `ruff check sdk-py examples/echo-consumer`, `ruff format --check`, `pytest sdk-py examples/echo-consumer`.
- `helm-lint` — needs `changes.helm`. `helm lint deploy/charts/*`, `chart-testing ct lint --target-branch main`.
- `build-gateway` + `build-sink-gcs` — only on `push: main` or tag push. `docker/setup-buildx-action@v3`, `docker/login-action@v3` (registry `ghcr.io`, username `${{ github.actor }}`, password `${{ secrets.GITHUB_TOKEN }}`; `packages: write` permission), `docker/build-push-action@v6` with:
  - `context: .` (repo root, per Dockerfiles in P3/P6)
  - `tags: ghcr.io/vanducng/mio/gateway:${{ github.sha }}` + `:main` + (on tag) `:v<semver>` + `:latest`. **No floating `:latest` on non-tag pushes** (research Section 3 Tag Policy).
  - `cache-from: type=registry,ref=ghcr.io/vanducng/mio/gateway:cache`, `cache-to: ...,mode=max` — registry cache (not gha cache; gha cache 10GB-cap thrashes for two services).
  - `platforms: linux/amd64` (POC scope). Multi-arch deferred — `--platform linux/amd64,linux/arm64` is a 1-line change when needed.
- **NO** `latest` floating tag on `main` pushes (only on git tag releases). Deploy job pins `image.tag=${{ github.sha }}` for full reproducibility.

8.2. **`.github/workflows/deploy.yaml`** triggers on `workflow_run` of `CI` (success on `main`). Job:
- `google-github-actions/auth@v2` with `credentials_json: ${{ secrets.GCP_SA_JSON }}` (static service-account JSON; WIF deferred to P10 — saves ~2 hours of GCP IAM yak-shaving for POC).
- `get-gke-credentials@v2` (cluster `mio-poc`, location `<region>`).
- `helm upgrade --install mio-gateway deploy/charts/mio-gateway --namespace mio --set image.tag=${{ github.sha }} --set image.pullPolicy=IfNotPresent --wait --timeout 5m`. Same for `mio-sink-gcs`.
- `kubectl rollout status deployment/mio-gateway -n mio --timeout=300s` — fail loud on rollout stall.
- Post-deploy smoke: curl `/healthz` via Ingress, assert 200.

8.3. **Image visibility.** `vanducng/mio/gateway` package on ghcr.io stays **private** (POC code may evolve; cheap to flip public later). Cluster pull uses `ghcr-pull` imagePullSecret (Step 7.9). Repo→package linkage: enable in package settings ("Manage Actions access" → grant `vanducng/mio` repo write).

8.4. **Tag policy** (`docs/runbooks/release.md` lives in P8, but tag rules locked here):
- `<sha>` (always, per CI build) — used by `deploy.yaml` for reproducibility.
- `main` (rolling, on `main` push) — for human inspection (`crane manifest ghcr.io/vanducng/mio/gateway:main`); never used for deploys.
- `v<semver>` (on git tag push) — manual release; triggers a separate `release.yaml` later (deferred).
- **No `latest`** outside of tag-triggered builds. Avoids the "what version is in prod?" ambiguity.

8.5. **Deferred to P10** (research POC-vs-defer):
- cosign keyless signing + SBOM (syft) — adds ~1 min/build + provenance verification config; defer until first prod release.
- Multi-arch builds (linux/arm64) — 1-line change in `platforms:`.
- Trivy / grype scanning gate — Trivy GHA action was supply-chain-compromised March 2026 (75/76 tags poisoned); use grype report-only if visibility needed; gate scanning is post-POC.
- ArgoCD / Flux GitOps — `helm upgrade` direct from GHA is right-sized for solo dev + 1 cluster.
- Workload Identity Federation (GHA→GCP, GKE→ghcr.io) — POC uses static JSON SA + PAT; rotation manual every 6 months.
- Per-PR preview environments — deferred; relies on GitOps + namespace-per-PR.

## Success Criteria

- [ ] `helm lint` clean on all four charts; `chart-testing (ct lint)` clean.
- [ ] Kind smoke test (`make kind-echo-test`): full echo loop runs in-cluster, bootstrap Job reports Completed, MinIO archive populated, all four streams + three consumers materialized.
- [ ] **Negative bootstrap test:** With a stream removed from gateway startup, `helm install mio-jetstream-bootstrap` FAILS with non-zero exit and clear logs (`MISSING stream MESSAGES_INBOUND`). Proves verification is not a no-op.
- [ ] On GKE: `kubectl get pods -n mio` → 3 NATS pods Ready (one per zone), 2 gateway, 1 sink-gcs, 1 jetstream-bootstrap Job Completed.
- [ ] **Quorum confirmed:** `kubectl exec -n mio mio-nats-0 -- nats stream cluster info MESSAGES_INBOUND` shows 3 replicas, all `current=true`.
- [ ] `nats stream ls` shows `MESSAGES_INBOUND` and `MESSAGES_OUTBOUND` with `replicas=3` healthy.
- [ ] `nats consumer ls MESSAGES_INBOUND` shows `ai-consumer` and `gcs-archiver`; `nats consumer ls MESSAGES_OUTBOUND` shows `sender-pool`.
- [ ] **Subject grammar test (gateway-side validation):** `POST /webhooks/zoho-cliq` (URL hyphen) with a synthetic message yields publish to `mio.inbound.zoho_cliq.<acct>.<conv>` (subject underscore — registry key); a synthetic message claiming `channel_type=zoho-cliq` (hyphen in the registry-key field) is rejected at gateway validation before any NATS publish.
- [ ] Migration Job runs once per release; gateway pods boot with `MIO_MIGRATE_ON_START=false` (verify env in pod spec).
- [ ] Gateway Ingress reachable; `/healthz` returns 200.
- [ ] Sink-gcs writes to real GCS bucket via Workload Identity (no static keys in any Secret); verification pod from setup.sh succeeded.
- [ ] PDB enforced: cordoning + draining one NATS node leaves 2 healthy and quorum intact (manual test).
- [ ] **CI green on `main`:** `ci.yaml` runs all jobs (paths-filter triggers `test-proto` + `test-gateway` + `test-python` + `helm-lint` + `build-gateway` + `build-sink-gcs`); `buf breaking` exits 0; `golangci-lint` clean; `go test -race` passes; `helm lint` + `chart-testing ct lint` clean.
- [ ] **Image published:** `crane ls ghcr.io/vanducng/mio/gateway` shows `<sha>` + `main` tags after `main` push; image size ≤25 MB (gateway) / ≤25 MB (sink-gcs); `crane manifest` shows `linux/amd64`, USER 65532 (nonroot).
- [ ] **Deploy works:** `deploy.yaml` runs after CI success on `main`; `helm upgrade --install` completes; `kubectl rollout status` returns success; gateway pod spec shows `image: ghcr.io/vanducng/mio/gateway:<sha>` (not `:latest`).
- [ ] **Pull works:** `kubectl describe pod -n mio mio-gateway-xxx` shows successful pull from ghcr via `ghcr-pull` secret; no `ImagePullBackOff`.
- [ ] **Reproducibility:** rerunning `helm upgrade --set image.tag=<old-sha>` rolls back to the prior sha (verify pod-spec image tag changes).

## Risks

- **Dual-writer race for stream provisioning** — gateway startup vs. bootstrap Job both writing → last-write-wins drift. **Mitigation:** bootstrap Job is verify-only (this phase's contract); single writer is gateway startup from P3. Negative smoke test proves the gate.
- **NATS cluster bootstrap on K8s** — routes config in-cluster is finicky. **Mitigation:** depend on upstream `nats-io/k8s` chart; do not hand-roll.
- **PVCs + StatefulSet ordering** — pod-0 must be Ready before pod-1 joins. **Mitigation:** upstream chart owns `serviceName` + ordering; PVC bound to pod identity.
- **Bootstrap Job fires before quorum forms** — Helm `post-install` hook fires after pods Ready, but JetStream cluster.state may still be ERRORED. **Mitigation:** Job's polling loop on `nats stream cluster info` (5× backoff retry) gates verification.
- **Workload Identity IAM propagation lag** — bindings take ~10–60s to propagate to GCE metadata service. **Mitigation:** setup.sh `sleep 60` + verification pod that runs `gsutil ls` before installing sink-gcs.
- **PDB blocking scale-down** — `minAvailable: 2` with autoscaler can stall scale-down indefinitely if drain hits PDB. **Mitigation:** configure GKE autoscaler `--max-graceful-termination-sec=120` and document scale-down delay tradeoff in runbook; can switch to `minAvailable: 1` with manual cordon if scale-down becomes critical.
- **Cloud SQL connection storm on rolling deploy** — 2-replica gateway rolling restart could spike connection count beyond Postgres `max_connections`. **Mitigation:** observe in P8 staging and configure `pgxpool.MaxConns` per replica; deferred unless observed in smoke test.
- **Migration Job + multiple gateway replicas** — only the Job applies migrations; deployments don't run them on start. **Mitigation:** `MIO_MIGRATE_ON_START=false` in chart values, `pre-install` hook ordering.
- **3× e2-small RAM tight** — 2.1Gi usable RAM after k8s overhead doesn't fit kube-prometheus-stack alongside NATS + gateway + sink. **Mitigation:** defer Prometheus to P8; rely on autoscaler to bump nodes if needed.
- **Cluster cost** — 3-zone regional GKE ~$126/mo idle. **Mitigation:** autoscaler floor at 1 per zone; teardown script for non-active environments.
- **GHCR PAT in K8s Secret (base64 only, not encrypted at rest).** If etcd is compromised, attacker reads PAT → pulls private images (read-only PAT, no push). **Mitigation:** PAT scope is `read:packages` on `vanducng/mio` only; 6-month expiry; rotation runbook in `deploy/gke/README.md`. WIF for ghcr is deferred (ghcr does not natively support GKE WIF as of 2026-05).
- **GHCR rate limits at scale.** Free tier has undocumented pull-rate limits (~10 pulls/min per IP); pod cold-starts on a 10-replica HPA can hit limits. **Mitigation:** `imagePullPolicy: IfNotPresent` (set in values.yaml); pre-pull DaemonSet deferred to P10; revisit if pod churn becomes real.
- **Trivy GHA action supply-chain compromise (March 2026, 75 of 76 tags poisoned).** **Mitigation:** Trivy is NOT in CI gates (research deferred scanning). If added later, pin to commit SHA, never tag.
- **`buf breaking --against 'origin/main'` flakiness on first PR.** Empty `origin/main` proto dir + new proto file = false positive. **Mitigation:** `if: needs.changes.outputs.proto == 'true' && github.base_ref == 'main'` skips on initial PR; document in runbook.

## Out (deferred to P8)

- Real Cliq webhook hitting Ingress with TLS — requires DNS + cert-manager prod issuer + a public IP, separate phase.
- `kube-prometheus-stack` install on GKE — RAM constraint defers it.
- Tempo + traces — observability stack lands in P8.
- Cloud SQL Auth Proxy sidecar — switch from direct private IP to sidecar if encryption/audit requirements appear.
- Multi-replica sink-gcs — enabled by P6's offset-based filenames, but operational rollout is post-POC.
- Production secret management — for POC, K8s Secrets are fine; rotate to Secret Manager in P8.

## Research backing

[`plans/reports/research-260508-1056-p7-helm-nats-jetstream-gke.md`](../../reports/research-260508-1056-p7-helm-nats-jetstream-gke.md)

Critical clarification — **single source of truth for stream/consumer provisioning**:
gateway startup (`AddOrUpdateStream` from P3) is authoritative. The
`mio-jetstream-bootstrap` Job is **verify-only** (asserts streams + consumers
exist, FAILS the release if not). Two writers = race; pick one.

Other validated picks integrated above:
- Use `nats-io/k8s` upstream chart as a dependency with values overlay rather than hand-rolling routes config.
- 3 zones × 1 replica with `topologySpreadConstraints` (regional GKE survives AZ loss; quorum holds at 2/3).
- pd-balanced 10Gi PVCs (vs pd-ssd) — 4.3 days retention at expected POC rate; resize easy.
- `minAvailable: 2` PDB protects quorum during node drain.
- Bootstrap Job polls for quorum (`nats stream cluster info` returns 3/3 healthy) before declaring success; backoff retry 5×.
- Migration Job in `pre-install,pre-upgrade` hook, gateway sets `MIO_MIGRATE_ON_START=false`. Race-free.
- Cloud SQL via direct private IP + VPC peering for POC (simpler than Auth Proxy sidecar). Auth Proxy upgrade in P8 if needed.
- GCE Ingress + cert-manager (HTTP-01) confirmed for P8 Let's Encrypt staging→prod flow.
- HPA via Prometheus Adapter (cloud-agnostic) over Stackdriver Custom Metrics Adapter (GCP lock-in).
- Kind smoke test validates chart install ordering before any GKE spend.
- Cluster cost ~$126/mo idle (3× e2-small) — defer kube-prometheus-stack to P8 to fit RAM.
- CI: `helm lint` + `helm template` + `chart-testing (ct lint)` + kind smoke under 2min.
