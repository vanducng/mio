---
phase: 5
title: "helm-chart-and-flux-deploy"
status: completed
priority: P1
effort: "2h"
depends_on: [4]
---

# Phase 5: Helm chart + FluxCD release

## Overview

Package the sidecar into a Helm chart (mirrors `mio-sink-gcs`), add it to
the FluxCD `apps/prod/mio/` overlay, and provision the GCS service-account
binding so the chart-deployed pod can write to the bucket prefix.

## Files
- **Create:** `deploy/charts/mio-attachment-downloader/Chart.yaml`
- **Create:** `deploy/charts/mio-attachment-downloader/values.yaml`
- **Create:** `deploy/charts/mio-attachment-downloader/templates/_helpers.tpl`
- **Create:** `deploy/charts/mio-attachment-downloader/templates/deployment.yaml`
- **Create:** `deploy/charts/mio-attachment-downloader/templates/serviceaccount.yaml`
- **Modify:** `.github/workflows/ci.yaml` — add `build-attachment-downloader` job + helm-lint coverage
- **Create (infra repo):** `fluxcd/apps/prod/mio/release-attachment-downloader.yaml` HelmRelease
- **Modify (infra repo):** `fluxcd/apps/prod/mio/kustomization.yaml` — append release file
- **Runbook step (off-PR):** `gcloud iam service-accounts create mio-attachments@dp-prod-7e26.iam.gserviceaccount.com` + `roles/storage.objectAdmin` IAM Condition scoped to `mio/attachments/` prefix; WI binding to `mio/mio-attachment-downloader` KSA

## Steps

### 5.1 Chart structure

Mirror `deploy/charts/mio-sink-gcs/` with these knobs in `values.yaml`:

```yaml
replicaCount: 1   # POC: single-writer; horizontal scale via JS MaxAckPending tuning later

image:
  registry: ghcr.io
  repository: vanducng/mio/attachment-downloader
  tag: "REPLACE_WITH_SHA"   # set by HelmRelease per deploy
  pullPolicy: IfNotPresent

imagePullSecrets: []  # ghcr packages are public; matches gateway/echo pattern

serviceAccount:
  create: true
  name: "mio-attachment-downloader"
  gcpServiceAccount: ""   # e.g. mio-attachments@dp-prod-7e26.iam.gserviceaccount.com

config:
  natsUrl: "nats://mio-nats:4222"
  tenantId: "00000000-0000-0000-0000-000000000001"
  accountId: "00000000-0000-0000-0000-000000000002"
  storageBackend: "gcs"
  storageBucket: "ab-spectrum-backups-prod"
  storagePrefix: "mio/attachments/"
  downloadTimeoutSeconds: "60"
  downloadMaxBytes: "26214400"  # 25 MiB
  signedUrlTtlSeconds: "3600"
  durableName: "attachment-downloader"
  metricsPort: "9090"
  logLevel: "info"

# CLIQ_BOT_TOKEN comes from the existing mio-gateway-secrets Secret.
secrets:
  existingSecret: "mio-gateway-secrets"

resources:
  requests: { cpu: 50m, memory: 64Mi }
  limits:   { cpu: 500m, memory: 256Mi }   # 256Mi covers 25 MB attachment + GCS client overhead

podSecurityContext: { runAsNonRoot: true, runAsUser: 65532, fsGroup: 65532 }
securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities: { drop: ["ALL"] }

# ServiceMonitor disabled — cluster uses GCP Managed Prometheus (matches gateway).
serviceMonitor:
  enabled: false
```

### 5.2 Templates

- `templates/deployment.yaml`: single container, `terminationGracePeriodSeconds: 60` (covers 30s drain + headroom), `envFrom: configMapRef` for non-secret config, explicit `env` block for `MIO_CLIQ_BOT_TOKEN` (`secretKeyRef: mio-gateway-secrets/CLIQ_BOT_TOKEN`).
- `templates/serviceaccount.yaml`: WI annotation `iam.gke.io/gcp-service-account` from `.Values.serviceAccount.gcpServiceAccount`.

No probes for POC (the sidecar is JetStream-driven; readiness ≈ NATS connect; defer probe wiring to P10 alongside echo-consumer probes).

### 5.3 CI

Add to `.github/workflows/ci.yaml`:
- `helm-lint` step: append `helm lint deploy/charts/mio-attachment-downloader` and `helm template ...`.
- `build-attachment-downloader` job: mirror `build-sink-gcs`, build context = repo root, `file: attachment-downloader/Dockerfile`, tags `ghcr.io/vanducng/mio/attachment-downloader:${{ github.sha }}` + `:main`.
- Update `changes` filter to add new path:
  ```yaml
  attachment-downloader:
    - 'attachment-downloader/**'
    - 'sdk-go/**'
    - 'proto/gen/go/**'
  ```

### 5.4 GCP IAM

Off-PR runbook (commit to `docs/runbooks/attachment-downloader-iam.md`):

```bash
# 1. Create GSA
gcloud iam service-accounts create mio-attachments \
  --display-name="mio attachment downloader" \
  --project=dp-prod-7e26

# 2. Grant object-admin scoped to mio/attachments/ prefix
gcloud storage buckets add-iam-policy-binding gs://ab-spectrum-backups-prod \
  --member=serviceAccount:mio-attachments@dp-prod-7e26.iam.gserviceaccount.com \
  --role=roles/storage.objectAdmin \
  --condition='expression=resource.name.startsWith("projects/_/buckets/ab-spectrum-backups-prod/objects/mio/attachments/"),title=mio_attachments_prefix_only'

# 3. Grant token-creator on self for V4 signed-URL signing
gcloud iam service-accounts add-iam-policy-binding \
  mio-attachments@dp-prod-7e26.iam.gserviceaccount.com \
  --role=roles/iam.serviceAccountTokenCreator \
  --member=serviceAccount:mio-attachments@dp-prod-7e26.iam.gserviceaccount.com

# 4. WI binding KSA → GSA
gcloud iam service-accounts add-iam-policy-binding \
  mio-attachments@dp-prod-7e26.iam.gserviceaccount.com \
  --role=roles/iam.workloadIdentityUser \
  --member="serviceAccount:dp-prod-7e26.svc.id.goog[mio/mio-attachment-downloader]"
```

### 5.5 Flux release

`fluxcd/apps/prod/mio/release-attachment-downloader.yaml`:

```yaml
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: mio-attachment-downloader
  namespace: mio
spec:
  interval: 30m0s
  timeout: 10m
  dependsOn:
    - name: mio-nats
    - name: mio-gateway
  chart:
    spec:
      chart: deploy/charts/mio-attachment-downloader
      sourceRef:
        kind: GitRepository
        name: mio-charts
        namespace: mio
  values:
    image:
      tag: "<sha-of-merge-commit>"
    serviceAccount:
      gcpServiceAccount: "mio-attachments@dp-prod-7e26.iam.gserviceaccount.com"
```

Append `- release-attachment-downloader.yaml` to `fluxcd/apps/prod/mio/kustomization.yaml`.

## Tests
- [ ] `helm lint deploy/charts/mio-attachment-downloader` passes
- [ ] `helm template ...` renders cleanly
- [ ] `kubectl kustomize fluxcd/apps/prod/mio | grep attachment-downloader` shows the rendered HelmRelease

## Success Criteria
- [ ] Image `ghcr.io/vanducng/mio/attachment-downloader:<sha>` published by CI on main merge
- [ ] After Flux reconcile, `kubectl -n mio get hr mio-attachment-downloader` Ready=True
- [ ] Pod logs show `nats: connected`, `jetstream: consumer attached`, no error spam
- [ ] `kubectl -n mio get sa mio-attachment-downloader -o yaml | grep iam.gke.io` shows WI annotation

## Risks
- **GSA creation needs prod IAM access** — confirm operator has roles/iam.serviceAccountAdmin on the project; otherwise route through abs-infra skill
- **Helm chart vs HelmChart cache** — same gotcha as P8: bumping chart `version` is required for helm-controller to re-package; document in chart README
- **Bucket lifecycle conflict** — phase-06 sets prefix-scoped lifecycle; ensure it doesn't clobber existing CNPG-backup lifecycle rules on the same bucket
