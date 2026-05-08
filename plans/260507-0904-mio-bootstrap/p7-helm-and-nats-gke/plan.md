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

Cluster-side artifact creation. Helm charts for NATS (3-replica
StatefulSet), gateway, and sink-gcs. Only K8s primitives; no
managed-cloud-only resources beyond GCS + Workload Identity (and those
are fenced behind values overrides).

This phase also locks the **JetStream stream/consumer definitions** so
they're applied uniformly cluster-side, not invented per-service. Stream
defs come straight from P3 (which authors them in `gateway/internal/store/jetstream.go`)
— here they're materialized as a one-shot Job for cluster bootstrap.

## Goal & Outcome

**Goal:** `helm install` brings up a 3-replica JetStream cluster + gateway + sink-gcs on GKE; charts pass `helm lint` and a kind cluster smoke test.

**Outcome:** GKE has a running `mio` namespace with healthy NATS quorum, gateway behind an Ingress, sink-gcs pointed at a real GCS bucket via Workload Identity, and the four streams/consumers provisioned.

## Files

- **Create:**
  - `deploy/charts/mio-nats/Chart.yaml`
  - `deploy/charts/mio-nats/values.yaml`
  - `deploy/charts/mio-nats/templates/{statefulset.yaml,service.yaml,configmap.yaml,pdb.yaml,servicemonitor.yaml}`
  - `deploy/charts/mio-jetstream-bootstrap/Chart.yaml` — one-shot Job that runs `nats stream add` / `nats consumer add` from a ConfigMap
  - `deploy/charts/mio-jetstream-bootstrap/templates/{configmap.yaml,job.yaml,rbac.yaml}`
  - `deploy/charts/mio-gateway/Chart.yaml`
  - `deploy/charts/mio-gateway/values.yaml`
  - `deploy/charts/mio-gateway/templates/{deployment.yaml,service.yaml,ingress.yaml,hpa.yaml,configmap.yaml,secret.yaml,servicemonitor.yaml,migration-job.yaml}`
  - `deploy/charts/mio-sink-gcs/Chart.yaml`
  - `deploy/charts/mio-sink-gcs/values.yaml`
  - `deploy/charts/mio-sink-gcs/templates/{deployment.yaml,serviceaccount.yaml,servicemonitor.yaml}`
  - `deploy/gke/setup.sh` — one-shot: cluster, node pool, IAM bindings, namespace, helm install order
  - `deploy/gke/README.md`
- **Modify:**
  - `Makefile` — `helm-lint`, `kind-up`, `kind-deploy` (kind smoke test before GKE)

## JetStream stream definitions (locked here)

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
matches all Cliq inbound; never `mio.inbound.zoho-cliq.>`.

## Consumer definitions

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

The bootstrap Job applies these via `nats consumer add --config <yaml>`
on first install; idempotent (`-f`). Subsequent service starts reconcile
without recreating.

## Steps

1. **mio-nats chart** — StatefulSet of 3 NATS pods, JetStream cluster mode, pd-ssd PVCs (10Gi each), zone-spread via `topologySpreadConstraints`, NATS server config in ConfigMap, `PodDisruptionBudget{minAvailable: 2}`, ServiceMonitor for Prom. **Reference the official `nats-io/k8s` Helm chart values for the cluster routes config — don't hand-roll routes.**
2. **mio-jetstream-bootstrap chart** — ConfigMap with stream + consumer specs (above); Job runs `nats` CLI against the cluster service to add/update; RBAC for the SA. Helm hook `post-install,post-upgrade`.
3. **mio-gateway chart** — Deployment (2 replicas), Ingress with TLS via cert-manager (DNS+TLS in P8), HPA on `mio_gateway_inbound_latency_seconds:rate1m`, env from Secret (Cliq creds, Postgres creds), ConfigMap for non-secret config (`MIO_TENANT_ID`, NATS URL, etc.). Migration Job (Helm hook `pre-install,pre-upgrade`) runs `golang-migrate up` against Postgres.
4. **mio-sink-gcs chart** — Deployment (1 replica), ServiceAccount annotated for Workload Identity (`iam.gke.io/gcp-service-account`), env for bucket name + flush settings.
5. `helm lint deploy/charts/*` clean; `helm template` outputs render without errors against representative values files.
6. **Kind smoke** — bring up a kind cluster, install all four charts (mio-nats → mio-jetstream-bootstrap → mio-gateway → mio-sink-gcs) pointing at MinIO instead of GCS via values overrides; assert end-to-end echo loop runs in-cluster on kind. Catch chart bugs before GKE.
7. **GKE setup script** — provisions:
   - Regional cluster (3 zones), `e2-small` nodes initially
   - GCS bucket `mio-messages-<env>` with lifecycle Standard→Nearline@30d→Coldline@90d
   - GSA `mio-sink-gcs@<project>.iam.gserviceaccount.com` with `roles/storage.objectAdmin` on the bucket
   - Workload Identity binding to KSA `mio-sink-gcs` in `mio` namespace
   - Cloud SQL (Postgres 16) + private IP into the VPC; secret with creds
   - Apply charts in dependency order: nats → jetstream-bootstrap → gateway (which runs migration job) → sink-gcs
8. Verify quorum: `kubectl exec -n mio mio-nats-0 -- nats stream cluster info MESSAGES_INBOUND` shows 3 replicas in sync.

## Success Criteria

- [ ] `helm lint` clean on all four charts
- [ ] Kind smoke test: full echo loop running in-cluster on kind, no chart-level errors, all four streams + three consumers materialized
- [ ] On GKE: `kubectl get pods -n mio` shows 3 NATS pods Ready, 2 gateway, 1 sink-gcs, 1 jetstream-bootstrap Job Completed
- [ ] `nats stream ls` (from inside cluster) shows `MESSAGES_INBOUND` and `MESSAGES_OUTBOUND` with `replicas=3` healthy
- [ ] `nats consumer ls MESSAGES_INBOUND` shows `ai-consumer` and `gcs-archiver`
- [ ] `nats consumer ls MESSAGES_OUTBOUND` shows `sender-pool`
- [ ] Subject test: `nats pub mio.inbound.zoho_cliq.<acct>.<conv> 'x'` is accepted; `nats pub mio.inbound.zoho-cliq.<acct>.<conv> 'x'` is rejected by gateway-side validation (registry check).
- [ ] Gateway Ingress reachable; `/healthz` returns 200
- [ ] Sink-gcs writes to real GCS bucket via WI (no static keys)

## Risks

- **NATS cluster bootstrap on K8s** — getting `routes` config right in-cluster is finicky; reference the official NATS Helm chart values, don't hand-roll routes.
- **PVCs + StatefulSet ordering** — pod-0 must be up before pod-1 joins; `serviceName` correctness; headless service for routes.
- **JetStream bootstrap Job ordering** — Helm hook `post-install` runs after all release resources are Ready, but NATS pods take ~30s to form quorum; the Job retries with backoff or waits-for-ready.
- **Workload Identity permission propagation** — IAM changes can take ~minutes; setup script must wait + verify before installing sink-gcs.
- **Cluster cost** — `e2-small` × 3 zones is ~$80/mo idle; keep autoscaler floor at 1 per zone.
- **Migration Job + multiple gateway replicas** — only the Job applies migrations; deployments don't run them on start (`MIO_MIGRATE_ON_START=false` in chart).

## Out (deferred to P8)

- Real Cliq webhook hitting Ingress with TLS — requires DNS + cert-manager + a public IP, separate phase.
- Production secret management — for POC, K8s Secrets are fine.
