---
phase: 8
title: "POC deploy on GKE"
status: pending
priority: P1
effort: "1d"
depends_on: [7]
---

# P8 — POC deploy on GKE

## Overview

Wire the live Cliq webhook to the cluster. End-to-end loop running
in-cluster, fully observable. This is the **ship it** milestone.

All metric labels here use the registry slug (`channel_type="zoho_cliq"`,
underscore) — matches the SDK + chart conventions; alerts and dashboards
reference that label, never `channel`. A query against `channel` silently
returns empty (research-flagged failure mode).

## Goal & Outcome

**Goal:** A user types in Zoho Cliq, sees an echo reply in the same thread, all from in-cluster — and an operator can trace one message end-to-end through Grafana / Tempo / Cloud Logging.

**Outcome:** Demo-able. Cliq → Cloud LB → Ingress → gateway → JetStream → echo-consumer → JetStream → gateway-sender → Cliq REST → user's thread. Single `trace_id` visible across all hops.

## Cross-phase contracts (consumed, not changed)

- **Metric labels (P5/P7):** `channel_type`, `direction`, `outcome`. Dashboards/alerts key on `channel_type`.
- **Subject grammar / underscore slugs (P3):** validated gateway-side; observability is consumer-only.
- **Stream/consumer provisioning (P3/P7):** gateway startup is authoritative; bootstrap Job is verify-only. P8 observes via metrics, doesn't mutate.
- **Schema-version (P2):** mismatches surface as `mio_sdk_publish_total{outcome="schema_mismatch"}`; alert + dashboard panel.
- **Trace propagation (P2):** SDK injects/extracts W3C `traceparent` in NATS headers. **Hard dependency** — if P2 contract breaks, P8 trace continuity breaks.

## Files

- **Create:**
  - `deploy/gke/dns-and-tls.sh` — Cloud DNS record + cert-manager bootstrap
  - `deploy/gke/observability/cert-issuer.yaml` — staging + prod `Issuer` (Let's Encrypt)
  - `deploy/gke/observability/values.yaml` — kube-prometheus-stack overrides (tainted obs node, sizing)
  - `deploy/gke/observability/tempo-values.yaml` — Tempo Helm values (GCS backend)
  - `deploy/gke/observability/otel-collector.yaml` — DaemonSet (POC shape)
  - `deploy/gke/observability/grafana-dashboards/mio-overview.json`
  - `deploy/gke/observability/grafana-dashboards/mio-cliq.json`
  - `deploy/gke/observability/grafana-dashboards-configmap.yaml` — provisioner ConfigMap
  - `deploy/gke/observability/alerts.yaml` — `PrometheusRule` (5 rules)
  - `docs/runbooks/cliq-webhook-down.md`
  - `docs/runbooks/jetstream-degraded.md`
  - `docs/runbooks/outbound-rate-limit.md`
  - `docs/runbooks/release.md` — GHCR PAT rotation, GCP_SA_JSON rotation, image-tag promotion (`<sha>` → `v<semver>`), rollback procedure
- **Modify:**
  - `deploy/charts/mio-gateway/values.yaml` — Ingress hostname, TLS issuer, `/readyz` probe tuning
  - `deploy/charts/mio-gateway/templates/ingress.yaml` — annotations: cert-manager issuer, GCE LB connection drain (120–300s)
  - `deploy/gke/setup.sh` — confirm `ghcr-pull` imagePullSecret bootstrap (added in P7 §7.9) ran successfully; idempotent re-run safe
- **GitHub Repo Secrets (configure once before P8):**
  - `GCP_SA_JSON` — JSON key for `mio-deploy@<PROJECT>.iam.gserviceaccount.com` with `roles/container.developer` + `roles/container.clusterViewer`. Used by `.github/workflows/deploy.yaml` via `google-github-actions/auth@v2`. Rotate every 90 days (manual; runbook). WIF deferred to P10.
  - `GHCR_PAT` — fine-grained PAT, scope `read:packages` on `vanducng/mio` only, 6-month expiry. Used by `setup.sh` to bootstrap the cluster's `ghcr-pull` Secret. Rotate every 6 months (manual; runbook).

## Observability stack (locked picks)

- **Metrics:** Prometheus via kube-prometheus-stack; ServiceMonitors from charts. Pinned to dedicated `e2-standard-2` node (tainted `observability=true:NoSchedule`).
  - Prometheus: 250m / 1Gi req, 500m / 2Gi limit; 8Gi PVC, `retentionSize: 6Gi` (cleanup at ~80%).
  - Grafana: 100m / 256Mi. Alertmanager: 50m / 128Mi.
- **Traces:** OTel Collector **DaemonSet** → **Tempo in-cluster, GCS backend** (bucket `gs://mio-traces-dev`, Workload Identity write).
  - Sampling: **100% head sampling** for POC. DaemonSet 128Mi buffer holds ~1k traces/sec safely.
  - Upgrade path: Deployment-gateway shape post-POC for tail sampling (ERROR-keep 100%, slow-keep 10%, random 1%).
  - **Not Jaeger** (Elasticsearch ops cost), **not Cloud Trace** (GCP lock-in). Tempo pairs with sink-gcs (same storage layer).
- **Logs:** stdout JSON → Cloud Logging (default GKE; no sidecar).
  - Required fields at every span: `trace_id`, `span_id`, `tenant_id`, `account_id`, `channel_type`, `conversation_id`, `mio_message_id`.
  - Cloud Logging correlation keys: `logging.googleapis.com/trace`, `logging.googleapis.com/spanId`.
  - **Discipline:** No `text` body at INFO (cost + privacy). DEBUG only, behind flag. WARN may include redacted summary.

All three signals correlate via `trace_id`. Operator clicks alert → Tempo trace → 5–7 spans (gateway inbound → JS publish → AI consumer → JS publish → gateway sender → Cliq REST) → Cloud Logging by `trace_id` for narrative context.

## Alerts (PrometheusRule)

Each threshold = **baseline × 1.5** (baseline captured in step 7). `for: 5m` minimum. All expressions key on `channel_type` (slug).

```yaml
groups:
- name: mio.gateway
  rules:
  - alert: MioGatewayHighInboundLatency
    expr: histogram_quantile(0.99, sum by (le, channel_type) (rate(mio_gateway_inbound_latency_seconds_bucket[5m]))) > 1
    for: 5m
    labels: { severity: page }
    annotations:
      summary: "Gateway p99 inbound latency > 1s for {{ $labels.channel_type }}"
      runbook: "docs/runbooks/cliq-webhook-down.md"

  - alert: MioGatewayBadSignatureSpike
    expr: rate(mio_gateway_inbound_total{outcome="bad_signature"}[5m]) > 0.1
    for: 10m
    labels: { severity: warn }
    annotations:
      runbook: "docs/runbooks/cliq-webhook-down.md"

  - alert: MioGatewayOutboundFailureSpike
    expr: rate(mio_gateway_outbound_total{outcome="error"}[5m]) > 0.05
    for: 5m
    labels: { severity: page }
    annotations:
      runbook: "docs/runbooks/outbound-rate-limit.md"

  - alert: MioRateLimitBucketLeak
    expr: mio_gateway_ratelimit_buckets_active > 1000
    for: 15m
    labels: { severity: warn }
    annotations:
      runbook: "docs/runbooks/outbound-rate-limit.md"

- name: mio.jetstream
  rules:
  - alert: MioJetStreamConsumerLag
    expr: nats_consumer_num_pending{stream_name=~"MESSAGES_(INBOUND|OUTBOUND)"} > 100
    for: 5m
    labels: { severity: page }
    annotations:
      runbook: "docs/runbooks/jetstream-degraded.md"

  - alert: MioJetStreamReplicaLost
    expr: nats_stream_cluster_replicas{stream_name=~"MESSAGES_.*"} < 3
    for: 2m
    labels: { severity: page }
    annotations:
      runbook: "docs/runbooks/jetstream-degraded.md"
```

**Validation:** `promtool lint` runs in CI on `alerts.yaml`. Empty query (label drift) → CI fail.

## Steps

1. **DNS + TLS bootstrap** (15 min)
   1.1. Create Cloud DNS A record `mio.<domain>` → reserved Ingress IP (allocate first via `gcloud compute addresses create`).
   1.2. Apply `cert-issuer.yaml`: two `Issuer` objects — `letsencrypt-staging` + `letsencrypt-prod` (HTTP-01 solver, GCE class).
   1.3. Patch gateway Ingress annotation `cert-manager.io/issuer: letsencrypt-staging`. Wait ~5–10 min for cert provisioning + LB sync.
   1.4. Verify staging cert resolves end-to-end (browser will reject; that's fine — TLS handshake validates).
   1.5. Swap annotation to `letsencrypt-prod`. cert-manager **reuses the existing Secret** on subsequent reconciliations — critical for staying under the 50-cert/domain/week LE limit during dev iteration.

2. **Cloud LB tuning**
   2.1. Backend service health check: endpoint `/readyz`, **5s timeout**, **3-fail unhealthy threshold**, 2-pass healthy threshold, 10s interval.
   2.2. `connection_draining_timeout_sec: 300` (5min) for graceful rolling deploys.
   2.3. Helm `strategy.rollingUpdate`: `maxSurge: 0, maxUnavailable: 1` (sequential, set in P7 for split-brain safety). LB drain happens via `connection_draining_timeout_sec: 300` on the surviving replica; one of two pods stays Ready throughout the rollout.

3. **GHA → GKE auth bootstrap** (one-time, before first deploy)
   3.0.1. Create GCP service account `mio-deploy@<PROJECT>.iam.gserviceaccount.com` with `roles/container.developer` + `roles/container.clusterViewer` on the cluster.
   3.0.2. `gcloud iam service-accounts keys create mio-deploy.json --iam-account=mio-deploy@<PROJECT>.iam.gserviceaccount.com` — copy contents into GitHub repo secret `GCP_SA_JSON`. Delete local `mio-deploy.json` immediately after upload.
   3.0.3. Verify `.github/workflows/deploy.yaml` (P7 §8.2) authenticates: trigger manually via `workflow_dispatch`; observe `Cluster credentials retrieved successfully` log.
   3.0.4. Verify `ghcr-pull` Secret exists in `mio` namespace: `kubectl get secret ghcr-pull -n mio` — created by `setup.sh` §7.9 from `GHCR_PAT` env var. If missing (e.g., setup.sh ran without `GHCR_PAT`), run `kubectl create secret docker-registry ghcr-pull -n mio --docker-server=ghcr.io --docker-username=<gh-user> --docker-password=$GHCR_PAT --docker-email=ci@vanducng.dev`.

4. **Cliq webhook target swap**
   4.1. Update Cliq incoming webhook URL to `https://mio.<domain>/webhooks/zoho-cliq` (URL hyphen per web convention).
   4.2. Note: **no IP allowlist** (Zoho doesn't publish stable range). HMAC signature verification (P3 contract) is the sole durable defense.

5. **Observability node + kube-prometheus-stack**
   5.1. `gcloud container node-pools create obs-pool --machine-type e2-standard-2 --num-nodes 1 --node-taints observability=true:NoSchedule`.
   5.2. `helm install kube-prometheus-stack` with `values.yaml`: tolerations + `nodeSelector` pinning all obs workloads to obs-pool.
   5.3. ServiceMonitors auto-discover from chart labels (gateway, echo-consumer, sink, NATS).
   5.4. Apply `grafana-dashboards-configmap.yaml`: ConfigMap with `mio-overview.json` + `mio-cliq.json`. Grafana sidecar auto-loads.

6. **Tempo + OTel Collector**
   6.1. Create GCS bucket `mio-traces-dev`, bind Workload Identity SA with `storage.objectAdmin`.
   6.2. `helm install tempo grafana/tempo --values tempo-values.yaml` — single-binary mode for POC, GCS storage backend.
   6.3. Apply `otel-collector.yaml` (DaemonSet): OTLP gRPC :4317 + HTTP :4318 receivers, batch processor (1000 spans / 5s), OTLP exporter to `tempo:4317`.
   6.4. Add Tempo as Grafana data source. Verify synthetic curl against gateway produces a complete trace.

7. **Trace propagation validation** (P2 contract check)
   7.1. Send single test message through Cliq.
   7.2. In Tempo: confirm one `trace_id` spans **gateway inbound → JS publish → echo-consumer → JS publish → gateway sender → Cliq REST** (5–7 spans).
   7.3. In Cloud Logging: filter by that `trace_id`; every log line at every hop must carry it.
   7.4. **If trace breaks:** fix the SDK gap (P2), don't paper over with dashboard logic.

8. **Baseline capture + smoke test**
   8.1. Type "ping" in Cliq, confirm echo within 5s. Screenshot trace in Grafana.
   8.2. Run 100 messages in 10s (10 msg/sec). Capture 5 min of metrics. Extract p50/p95/p99 inbound + outbound.
   8.3. **Lock alert thresholds:** set each = `baseline_p99 × 1.5` (then ceiling at the conservative values in `alerts.yaml`). Apply `alerts.yaml`.
   8.4. Verify rules loaded: `amtool alert query` and `promtool check rules alerts.yaml`.

9. **Failure injection** (ad-hoc; no Chaos Mesh for POC)
   9.1. **Gateway pod kill:** `kubectl delete pod -l app=gateway` → `MioGatewayHighInboundLatency` fires within ~30s; second replica absorbs traffic.
   9.2. **NATS pod kill:** `kubectl delete pod mio-nats-0` → quorum re-elects; `MioJetStreamReplicaLost` fires; messages keep flowing.
   9.3. **Outbound 5xx via toxiproxy:** inject `http_code: 503` on Cliq REST upstream → `MioGatewayOutboundFailureSpike` fires within `for: 5m`.
   9.4. **Account fairness:** burst one `account_id`; confirm a second account's p99 stays <2s (P5 contract verified in cluster).

10. **Runbooks** — four documents, identical structure: alert → first-5min diagnosis → kubectl/nats/psql commands → escalation → postmortem checklist.
   10.1. `cliq-webhook-down.md` — covers `MioGatewayHighInboundLatency`, `MioGatewayBadSignatureSpike`. Diagnosis tree: pod health, Postgres slow query, NATS lag, Zoho secret rotation.
   10.2. `jetstream-degraded.md` — covers `MioJetStreamConsumerLag`, `MioJetStreamReplicaLost`. Diagnosis: consumer stalled, replica lost, Workload Identity expiry, network partition.
   10.3. `outbound-rate-limit.md` — covers `MioGatewayOutboundFailureSpike`, `MioRateLimitBucketLeak`. Diagnosis: Cliq API degraded, per-workspace bucket TTL, sender pool sizing.
   10.4. `release.md` — image-tag promotion (`<sha>` → `v<semver>`), `helm rollback` procedure, `GHCR_PAT` rotation (6mo), `GCP_SA_JSON` rotation (90d), pull-secret refresh on PAT change.

## Success Criteria

- [ ] **GHA → GKE deploy works:** `.github/workflows/deploy.yaml` (P7) authenticates via `GCP_SA_JSON` secret, runs `helm upgrade --set image.tag=<sha>` against the cluster, `kubectl rollout status` returns success. First push to `main` produces a green deploy run.
- [ ] **Image pulled from ghcr.io:** `kubectl describe pod -n mio mio-gateway-xxx` shows `Successfully pulled image "ghcr.io/vanducng/mio/gateway:<sha>"` via `ghcr-pull` Secret; no `ImagePullBackOff` events.
- [ ] **Reproducibility:** `helm rollback mio-gateway 1 -n mio` swaps image tag back to prior `<sha>`; pod spec verified.
- [ ] **`docs/runbooks/release.md`** documents: image tag promotion (`<sha>` → `v<semver>` via git tag), `helm rollback` procedure, `GHCR_PAT` 6-month rotation, `GCP_SA_JSON` 90-day rotation, pull-secret refresh after PAT rotation.
- [ ] User-visible echo reply in Cliq from a real webhook hitting the cluster
- [ ] cert-manager issued cert from staging, then prod; **Secret reused across at least one redeploy** (verified via `kubectl describe secret` resourceVersion stability)
- [ ] One message produces a single `trace_id` visible end-to-end in Tempo (gateway inbound → JS publish → AI consumer → JS publish → gateway sender → Cliq REST)
- [ ] Structured logs at every span carry `trace_id`, `span_id`, `account_id`; **no `text` body at INFO** (grep audit on a 5-min window)
- [ ] All five Prometheus alerts pass `promtool lint` in CI
- [ ] Failure-injection drills produce expected alert firings (3 scenarios from step 8)
- [ ] Killing one NATS pod → cluster stays healthy, quorum holds, messages keep flowing
- [ ] 100-message burst: p99 inbound <500ms, p99 outbound <2s
- [ ] Account-fairness load test passes (P5 contract verified in cluster)
- [ ] GCS bucket has the day's archived payloads with `channel_type=zoho_cliq/date=YYYY-MM-DD/` partitions
- [ ] Four runbooks merged with concrete kubectl/nats/psql commands and decision trees (cliq-webhook-down, jetstream-degraded, outbound-rate-limit, release)
- [ ] Cost projection documented: **~$400/mo idle, ~$430/mo @ 100rps** (recorded in plan or `docs/cost-projection.md`)

## Risks

- **Let's Encrypt rate limits** (50 certs/domain/week) — mitigated by **reusing the cert Secret across redeploys**. cert-manager only re-requests on expiry (90d window), not per-deploy. Always validate with staging Issuer first.
- **Prometheus OOM on shared pool** — mitigated by **dedicated tainted `e2-standard-2` obs node**; Prometheus capped at 1Gi req / 2Gi limit; `retentionSize: 6Gi` triggers cleanup.
- **Trace propagation gap** — depends on P2 SDK contract. If a service forgets `traceparent` injection/extraction, trace breaks. Mitigated by: (a) structured logs always carry `trace_id` so partial traces remain debuggable; (b) step 6 validation gates phase completion.
- **False-positive alerts** — mitigated by **baseline-driven thresholds** (`p99_idle × 1.5`) + `for: 5m` minimum + `promtool lint` in CI. Avoids paging on transient hiccups.
- **Cloud LB health check too aggressive** — `/readyz` 5s timeout, 3-fail threshold gives slow startups grace; connection draining 300s preserves in-flight webhooks during rollout.
- **Dashboard label drift** (`channel` vs `channel_type`) — silent failure mode. Mitigated by `promtool` lint on captured queries before merge.
- **Cliq webhook auth** — Zoho doesn't publish stable IP range, so no allowlist. HMAC signature (P3) is the only durable defense; documented as accepted risk.
- **Observability cost runaway** — 100% sampling at scale would cost ~$150/mo at 1k tps. POC budget OK; tail sampling is the post-POC cost lever (10× reduction).

## Out (deferred)

- **Tail sampling / Deployment-gateway collector** — DaemonSet stays; gateway shape lands when traffic justifies (>5k traces/sec).
- **Chaos Mesh CRDs** — ad-hoc drills first; CRD-based reproducibility once runbooks stabilize.
- **Second channel adapter** — separate phase. POC ships at this milestone with Cliq only.
- **Cloud Armor / WAF** — add post-POC if attack surface warrants.

## Research backing

[`plans/reports/research-260508-1056-p8-poc-deploy-observability-gke.md`](../../reports/research-260508-1056-p8-poc-deploy-observability-gke.md)

Validated picks (14 questions):
- **Cloud DNS + cert-manager HTTP-01** (LE staging → prod). Reuse cert Secret across redeploys (LE 50/domain/week).
- **Cloud LB:** `/readyz` 5s timeout, 3-fail threshold; connection draining 120–300s.
- **Dedicated tainted `e2-standard-2` obs node**; Prom 250m/1Gi req, 8Gi PVC, retentionSize 6Gi.
- **Tempo in-cluster + GCS backend** (over Jaeger/Cloud Trace). Pairs with sink-gcs.
- **OTel Collector DaemonSet** for POC. Upgrade to Deployment-gateway post-POC for tail sampling.
- **100% head sampling**; DaemonSet 128Mi buffer.
- **NATS trace propagation** via SDK-injected `traceparent` (P2 contract); validated in step 6.
- **Structured JSON logging to stdout** → Cloud Logging auto-parse. Required fields locked. No `text` at INFO.
- **Alert thresholds = baseline × 1.5**, `for: 5m` minimum; `promtool lint` in CI.
- **Grafana dashboards as code** (JSON in ConfigMap), variables `channel_type` + `account_id`, cross-link to Tempo + Cloud Logging.
- **Failure injection POC-style:** ad-hoc `kubectl delete pod` + `toxiproxy`; Chaos Mesh deferred.
- **Skip Cliq IP allowlist** — Zoho has no stable range; HMAC is the durable defense.
- **Cost:** ~$400/mo idle, ~$430/mo @ 100rps. Tail sampling is the biggest post-POC lever.

Three runbook templates land here; structure: alert → first-5min diagnosis → kubectl/nats/psql commands → escalation → postmortem checklist.
