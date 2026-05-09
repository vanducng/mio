---
phase: 8
title: "POC deploy on GKE"
status: completed
priority: P1
effort: "1d"
depends_on: [7]
---

# P8 — POC deploy on existing prod GKE via FluxCD

## Overview

Ship Mio POC end-to-end onto the **existing** `dp-prod-7e26` GKE cluster
(us-central1-a) into a dedicated `mio` namespace, reconciled by **FluxCD**
from the infra repo at `/Users/vanducng/git/work/ab-spectrum/infra`.
Mirror the freescout app pattern: HelmRelease + ingress + SOPS-encrypted
secrets + CNPG cluster. No new cluster, no kube-prometheus-stack rebuild,
no Tempo/OTel bootstrap — those defer to P10.

Cliq webhook lands at `https://mio.abspectrumservices.org/cliq` →
ingress-nginx → gateway → JetStream → echo-consumer → JetStream → outbound
dispatcher → Cliq REST → user thread.

## Goal & Outcome

**Goal:** User types in Zoho Cliq `devduc` channel, sees an echo reply in
the same thread, all from in-cluster.

**Outcome:** Demo-able end-to-end loop. Secrets in Git via SOPS. Flux
reconciles on every push to infra repo's main. One runbook for the
inevitable "webhook returns 5xx" case.

## Cross-phase contracts (consumed, not changed)

- **Stream/consumer provisioning (P3/P7):** gateway startup is authoritative;
  `mio-jetstream-bootstrap` is verify-only.
- **Subject grammar / underscore slugs (P3):** validated gateway-side.
- **Metric labels (P5/P7):** `channel_type`, `direction`, `outcome`. Charts
  emit ServiceMonitors; if cluster has prometheus-operator they auto-scrape.
- **HMAC verification (P3):** sole defense for inbound; Deluge script HMAC
  output (hex/base64 fallback in `gateway/internal/channels/zohocliq/signature.go`)
  must match `CLIQ_WEBHOOK_SECRET`.
- **Trace propagation (P2):** soft dep — traces visible only after P10 lands.

## Existing prod-cluster facts (no-op)

- `ingress-nginx` deployed; class name `nginx`.
- `cert-manager` ClusterIssuer `letsencrypt-production` (HTTP-01 via NGINX)
  at `infra/fluxcd/infrastructure/configs/base/certificates/letsencrypt-production.yaml:4`.
- CNPG operator deployed; backup SA `prod-cnpg-backup-sa@dp-prod-7e26.iam.gserviceaccount.com`,
  bucket prefix `gs://ab-spectrum-backups-prod/cnpg/<app>`.
- SOPS + age encryption for secrets in Git. Key at `infra/.secrets/age-key.txt`.
- Reference app: `infra/fluxcd/apps/prod/freescout/` + `infra/fluxcd/databases/prod/freescout/`.

## Files to create / modify

### Mio repo (`/Users/vanducng/git/personal/agents/mio`)

**Modify:**

- `gateway/internal/server/server.go:71` — add server-side alias `/cliq` for
  `zoho_cliq` handler. Diff:
  ```go
  // existing line 71:
  r.Post("/webhooks/{channel}", func(w http.ResponseWriter, r *http.Request) { ... })

  // add below the closing brace of that handler:
  // Server-side alias: /cliq → zoho_cliq handler. Keeps ingress path simple
  // (no rewrite annotations) and matches the locked Cliq webhook URL.
  r.Post("/cliq", func(w http.ResponseWriter, r *http.Request) {
      deps := zohocliq.HandlerDeps{ /* same construction as default branch */ }
      zohocliq.Handler(deps).ServeHTTP(w, r)
  })
  ```
  Refactor: extract the `zoho_cliq` `HandlerDeps` builder into a private
  `func(cfg, m, ...) zohocliq.HandlerDeps` and call from both routes (DRY).
- `deploy/charts/mio-gateway/values.yaml:59` — `className: gce` → `nginx`.
- `deploy/charts/mio-gateway/values.yaml:60-62` — uncomment cluster-issuer
  annotation, drop `kubernetes.io/ingress.allow-http: "false"` (nginx
  default; cert-manager handles ACME challenge on `:80`).
- `deploy/charts/mio-gateway/values.yaml:63-67` — keep `/webhooks/` Prefix
  default; add `/cliq` Exact path (HelmRelease values override host + paths
  for prod).
- `deploy/charts/mio-gateway/values.yaml:70` — leave `tls: []` default;
  HelmRelease sets it for prod.
- `.github/workflows/ci.yaml` — add `build-echo-consumer` job mirroring
  existing `build-sink-gcs` step. Build context = repo root,
  `file: examples/echo-consumer/Dockerfile`, tag
  `ghcr.io/vanducng/mio/echo-consumer:${{ github.sha }}`. Same
  `push: true` gate (main only).

**Create:**

- `deploy/charts/mio-echo-consumer/` — minimal chart for in-cluster echo
  consumer. **Files:**
  - `Chart.yaml` — `name: mio-echo-consumer`, `version: 0.1.0`,
    `appVersion: "0.1.0"`.
  - `values.yaml` — image (`ghcr.io/vanducng/mio/echo-consumer`, tag
    overridden by HelmRelease), `replicaCount: 1`, env
    (`NATS_URL: nats://mio-nats:4222`,
    `MIO_TENANT_ID: default`, `MIO_ACCOUNT_ID: default`),
    `imagePullSecrets: [{name: ghcr-pull}]`, resources (50m/64Mi req,
    200m/128Mi lim), `serviceAccount.create: true`, securityContext
    (non-root, read-only fs).
  - `templates/_helpers.tpl` — copy pattern from `mio-sink-gcs`.
  - `templates/serviceaccount.yaml` — KSA `mio-echo-consumer`.
  - `templates/deployment.yaml` — single-container Deployment, no
    probes for POC (consumer pulls from JetStream; readiness ≈ NATS
    connect; defer probe wiring to P10), env from values.
  - (optional) `templates/servicemonitor.yaml` — gated on
    `serviceMonitor.enabled`; consumer doesn't yet expose `/metrics`,
    so leave default `false` (revisit P10).
- `docs/runbooks/cliq-webhook-down.md` — first-5min diagnosis tree (only
  runbook in P8; jetstream/outbound/release deferred to P10).
- `docs/deployment.md` — short topology note: namespace, services, how to
  rotate `CLIQ_WEBHOOK_SECRET`, how Flux reconciles, NATS HA upgrade path.

### Infra repo (`/Users/vanducng/git/work/ab-spectrum/infra`)

**Create directory `fluxcd/databases/prod/mio/`:**

- `database.yaml` — CNPG `Cluster` (mirror freescout pattern). 1 instance,
  10Gi `premium-rwo`, PG 17.2, bootstrap database `mio` owner `mio`,
  backup `gs://ab-spectrum-backups-prod/cnpg/mio`, WI SA annotation.
- `service-account.yaml` — `ServiceAccount` `mio-postgres` with WI annotation
  to `prod-cnpg-backup-sa@dp-prod-7e26.iam.gserviceaccount.com`.
- `secrets.enc.yaml` — `mio-app-credentials` Secret (SOPS-encrypted) with
  `username=mio` + `password=<autogen 32-byte>`. Used by CNPG `bootstrap.initdb.secret`.
- `kustomization.yaml` — resources: service-account, secrets, database.

**Create directory `fluxcd/apps/base/mio/`** (mirrors freescout convention —
namespace + Flux source live in base; per-env aggregator references base):

- `namespace.yaml` — `Namespace mio`.
- `chart-source.yaml` — `GitRepository` (Flux source-controller) pointing at
  `https://github.com/vanducng/mio` ref branch `main`. Public repo →
  no auth secret needed. HelmReleases reference this with
  `chart.spec.sourceRef.kind: GitRepository`. Avoids publishing to a
  HelmRepository for POC.
- `kustomization.yaml` — resources: namespace, chart-source.

**Create directory `fluxcd/apps/prod/mio/`:**

- `release-nats.yaml` — HelmRelease for `mio-nats` (POC: 1 replica,
  emptyDir, no PVC — accepts data loss per Risk #1). Replica override
  goes through the upstream nats chart key, e.g.
  `values.nats.config.cluster.replicas: 1` (verify final key path
  against `mio-nats/charts/nats-1.2.7.tgz` defaults during §3.7).
  Document HA upgrade path (3 replicas + PDB + JetStream PVC) in
  `docs/deployment.md`.
- `release-gateway.yaml` — HelmRelease for `mio-gateway`. Values override:
  - `image.tag: <pinned-sha>` (manual bump per deploy; CI does not push to
    cluster — Flux pulls)
  - `imagePullSecrets[0].name: ghcr-pull`
  - `config.natsUrl: nats://mio-nats.mio.svc.cluster.local:4222`
  - `secrets.existingSecret: mio-gateway-secrets`
  - `ingress.enabled: false` — managed via `ingress.yaml` for clarity (matches freescout)
  - DB env via `extraEnv` referencing `mio-app-credentials` (CNPG-owned) +
    `mio-gateway-secrets` (cliq creds)
- `release-echo.yaml` — HelmRelease for `mio-echo-consumer` chart (authored
  in mio repo, see "Modify/Create" above). `image.tag: <pinned-sha>`,
  `imagePullSecrets[0].name: ghcr-pull`,
  env: `NATS_URL: nats://mio-nats.mio.svc.cluster.local:4222`.
- `release-sink.yaml` — HelmRelease for sink-gcs chart. Bucket
  `gs://ab-spectrum-backups-prod`, object prefix `mio/` (decision: reuse
  existing backups bucket — single IAM surface, no new bucket creation).
  WI SA binding: new SA `mio-sink-gcs@dp-prod-7e26.iam.gserviceaccount.com`
  with `roles/storage.objectAdmin` scoped via IAM Condition to
  `resource.name.startsWith("projects/_/buckets/ab-spectrum-backups-prod/objects/mio/")`.
  Sink chart values must set `bucket: ab-spectrum-backups-prod` +
  `prefix: mio/` so partition keys land at
  `gs://ab-spectrum-backups-prod/mio/channel_type=.../date=.../`.
- `ingress.yaml` — `ingressClassName: nginx`, host
  `mio.abspectrumservices.org`, path `/cliq` Prefix → service `mio-gateway:80`.
  Annotations: `cert-manager.io/cluster-issuer: letsencrypt-production`,
  `nginx.ingress.kubernetes.io/proxy-body-size: "1m"`,
  `nginx.ingress.kubernetes.io/ssl-redirect: "true"`.
  TLS block: `secretName: mio-gateway-tls`, hosts: same.
- `secrets.enc.yaml` — `mio-gateway-secrets` (SOPS-encrypted) with keys
  matching the chart's `templates/deployment.yaml` env mapping:
  `CLIQ_WEBHOOK_SECRET`, `CLIQ_BOT_TOKEN`, `CLIQ_BOT_SCOPE`,
  `DATABASE_URL` (built from CNPG creds —
  `postgresql://mio:$(password)@mio-postgres-rw.mio.svc.cluster.local:5432/mio`).
  Plaintext source for the Cliq values: `playground/cliq/secrets.env`
  (`CLIQ_BOT_TOKEN` = the long-lived OAuth access token used today;
  in-cluster refresh-token handling is out of scope, see "Out").
- `ghcr-pull.enc.yaml` — `docker-registry` Secret (SOPS-encrypted) for image pulls.
- `kustomization.yaml` — resources: secrets, ghcr-pull, all 4 releases,
  ingress. References `../base/mio` (or `../../base/mio` per repo
  convention) so namespace + GitRepository come from base.

**Modify (infra repo aggregators):**

- `fluxcd/apps/base/kustomization.yaml` — add `- mio` (if a base aggregator
  exists per freescout convention; otherwise the per-env aggregator
  references `../base/mio` directly).
- `fluxcd/apps/prod/kustomization.yaml` — add `- mio`.
- `fluxcd/databases/prod/kustomization.yaml` — add `- mio`.

## Steps

### Prep (off-cluster, ~10 min)

1.1. Activate prod GCP creds via abs-infra skill:
  `bash /Users/vanducng/git/work/ab-spectrum/infra/.claude/skills/abs-infra/activate-abs-infra-gcp-credentials.sh prod`
1.2. Verify context: `kubectl config current-context` → `dp-prod-7e26`.
1.3. Verify cluster controllers present:
  `kubectl get clusterissuer letsencrypt-production`,
  `kubectl get ingressclass nginx`,
  `kubectl get crd clusters.postgresql.cnpg.io`.
1.4. Reuse existing bucket `gs://ab-spectrum-backups-prod` with prefix `mio/`
   (no new bucket creation). Verify access:
   `gcloud storage ls gs://ab-spectrum-backups-prod/`.
1.5. Create WI SA + prefix-scoped binding for sink-gcs:
  ```bash
  gcloud iam service-accounts create mio-sink-gcs --project=dp-prod-7e26
  # Object-admin scoped to mio/ prefix only via IAM Condition.
  gcloud storage buckets add-iam-policy-binding gs://ab-spectrum-backups-prod \
      --member=serviceAccount:mio-sink-gcs@dp-prod-7e26.iam.gserviceaccount.com \
      --role=roles/storage.objectAdmin \
      --condition='expression=resource.name.startsWith("projects/_/buckets/ab-spectrum-backups-prod/objects/mio/"),title=mio_prefix_only'
  # WI binding so KSA mio/mio-sink-gcs can impersonate the GSA.
  gcloud iam service-accounts add-iam-policy-binding \
      mio-sink-gcs@dp-prod-7e26.iam.gserviceaccount.com \
      --role=roles/iam.workloadIdentityUser \
      --member="serviceAccount:dp-prod-7e26.svc.id.goog[mio/mio-sink-gcs]"
  ```
1.6. Create CNPG-backup-SA binding for the new `mio-postgres` KSA:
  `gcloud iam service-accounts add-iam-policy-binding prod-cnpg-backup-sa@dp-prod-7e26.iam.gserviceaccount.com --role=roles/iam.workloadIdentityUser --member="serviceAccount:dp-prod-7e26.svc.id.goog[mio/mio-postgres]"`.
  (Bucket-side IAM already granted at the project level for that SA.)
1.7. (informational, not a deploy gate) Channel name locked: **`devduc`**.
   The cluster Secret carries `CLIQ_BOT_TOKEN`/`CLIQ_BOT_SCOPE` only; the
   `playground/cliq/secrets.env` channel-name field is used by the Deluge
   bot HMAC test loop and can be updated separately.

### Mio repo changes (~30 min)

2.1. Apply gateway alias route diff (file/line above). Run
   `cd /Users/vanducng/git/personal/agents/mio/gateway && go build ./...`
   to verify.
2.2. Update `deploy/charts/mio-gateway/values.yaml` ingress defaults (nginx,
   cluster-issuer annotation, dual paths).
2.3. Author `deploy/charts/mio-echo-consumer/` (Chart.yaml, values.yaml,
   templates per "Files to create" above). Run
   `helm lint deploy/charts/mio-echo-consumer`. Run
   `helm template deploy/charts/mio-echo-consumer | kubectl apply --dry-run=client -f -`
   to catch manifest errors.
2.4. Extend `.github/workflows/ci.yaml` with `build-echo-consumer` job
   (mirror `build-sink-gcs`). Validate locally:
   `act -W .github/workflows/ci.yaml -j build-echo-consumer` (or push to
   a branch first; merge to main only when green).
2.5. Run `helm lint deploy/charts/mio-gateway`.
2.6. Commit + push to `github.com/vanducng/mio:main`. Tag the SHA — this is
   the image tag Flux will pin to for all three components.
2.7. Wait for CI to publish all three images:
   `ghcr.io/vanducng/mio/gateway:<sha>`,
   `ghcr.io/vanducng/mio/sink-gcs:<sha>`,
   `ghcr.io/vanducng/mio/echo-consumer:<sha>` (`gh run watch`).
2.8. Write `docs/runbooks/cliq-webhook-down.md`:
   - Symptom (5xx from Cliq, no echo)
   - Step 1: `kubectl -n mio get pods` — gateway healthy?
   - Step 2: `kubectl -n mio logs -l app.kubernetes.io/name=mio-gateway --tail=100 | grep -E "bad_signature|publish"`
   - Step 3: `kubectl -n mio exec deploy/mio-nats -- nats stream info MESSAGES_INBOUND`
   - Step 4: ingress reachable? `curl -I https://mio.abspectrumservices.org/cliq`
   - Step 5: secret rotation — re-encrypt `secrets.enc.yaml` with new
     `CLIQ_WEBHOOK_SECRET`, push, wait for Flux reconcile, update Cliq bot UI.

### Infra repo changes (~45 min)

3.1. Branch off `main` in infra repo: `feat/mio-poc-deploy`.
3.2. `mkdir fluxcd/databases/prod/mio fluxcd/apps/base/mio fluxcd/apps/prod/mio`.
3.3. Author `databases/prod/mio/database.yaml` (copy freescout, change names
   `freescout` → `mio`, bucket suffix `mio`).
3.4. Author `service-account.yaml` (KSA `mio-postgres` with WI annotation).
3.5. Author `secrets.enc.yaml`: plaintext first, then encrypt:
  ```bash
  cat <<EOF > secrets.yaml
  apiVersion: v1
  kind: Secret
  metadata: { name: mio-app-credentials, namespace: mio }
  type: kubernetes.io/basic-auth
  stringData:
    username: mio
    password: $(openssl rand -base64 32 | tr -d '/+=' | head -c 32)
  EOF
  SOPS_AGE_KEY_FILE=.secrets/age-key.txt sops -e -i secrets.yaml
  mv secrets.yaml secrets.enc.yaml
  ```
3.6. Author `databases/prod/mio/kustomization.yaml`. Append `- mio` to
   `databases/prod/kustomization.yaml`.
3.7. Author base layer: `apps/base/mio/namespace.yaml`,
   `apps/base/mio/chart-source.yaml`, `apps/base/mio/kustomization.yaml`.
   Inspect upstream nats chart values: `helm show values
   ./deploy/charts/mio-nats` to find the cluster-replica key path before
   authoring `release-nats.yaml`. Then author per-env releases at
   `apps/prod/mio/release-{nats,gateway,echo,sink}.yaml` +
   `apps/prod/mio/ingress.yaml`.
3.8. Author `apps/prod/mio/secrets.enc.yaml` and `ghcr-pull.enc.yaml`:
  ```bash
  # mio-gateway-secrets — keys MUST match chart deployment.yaml mapping:
  # CLIQ_WEBHOOK_SECRET, CLIQ_BOT_TOKEN, CLIQ_BOT_SCOPE, DATABASE_URL.
  # Source CLIQ_BOT_TOKEN/CLIQ_BOT_SCOPE from playground/cliq/secrets.env.
  # then SOPS-encrypt as above.
  # ghcr pull:
  kubectl create secret docker-registry ghcr-pull \
      --docker-server=ghcr.io --docker-username=vanducng \
      --docker-password=$GHCR_PAT --docker-email=ci@vanducng.dev \
      -n mio --dry-run=client -o yaml > ghcr-pull.yaml
  SOPS_AGE_KEY_FILE=.secrets/age-key.txt sops -e -i ghcr-pull.yaml
  mv ghcr-pull.yaml ghcr-pull.enc.yaml
  ```
3.9. Author `apps/prod/mio/kustomization.yaml` (resources: secrets,
   ghcr-pull, releases, ingress; references `../../base/mio`). Append
   `- mio` to `apps/prod/kustomization.yaml` (and to
   `apps/base/kustomization.yaml` if a base aggregator exists per
   freescout convention).
3.10. Open PR. Once merged, Flux reconciles within `interval` (typically
    1-5 min). Watch: `flux get kustomizations -A | grep mio`.

### DNS + smoke test (~15 min)

4.1. Once ingress is live, get external IP: `kubectl -n ingress-nginx get svc nginx-ingress-controller -o jsonpath='{.status.loadBalancer.ingress[0].ip}'`.
4.2. **DNS:** Cloud DNS managed zone (manual). Find the zone:
   `gcloud dns managed-zones list --filter="dnsName:abspectrumservices.org" --project=dp-prod-7e26`.
   Add A record (replace `<ZONE>` and `<IP>`):
   ```bash
   gcloud dns record-sets create mio.abspectrumservices.org. \
       --zone=<ZONE> --type=A --ttl=300 --rrdatas=<IP> --project=dp-prod-7e26
   ```
   Verify: `dig +short mio.abspectrumservices.org` returns the ingress IP.
4.3. Wait for cert-manager to issue cert: `kubectl -n mio describe cert mio-gateway-tls` — Status `Ready: True`.
4.4. Probe: `curl -I https://mio.abspectrumservices.org/cliq` → expect 405
   (GET not allowed; means routing + TLS work).
4.5. Update Cliq bot webhook URL in Cliq dashboard:
   `https://mio.abspectrumservices.org/cliq`. Reuse existing
   `CLIQ_WEBHOOK_SECRET` from playground (now in cluster Secret).
4.6. In `devduc` channel: type `ping`. Expect echo reply within 5s.
4.7. Verify GCS archive: `gcloud storage ls gs://ab-spectrum-backups-prod/mio/channel_type=zoho_cliq/date=$(date -u +%F)/`.

## Success Criteria

- [ ] Flux reconciles `apps/prod/mio` and `databases/prod/mio` cleanly
      (`flux get kustomizations -A` all `Ready: True`).
- [ ] CNPG cluster `mio-postgres` healthy; `kubectl -n mio exec mio-postgres-1 -- psql -c '\l'` lists `mio` DB.
- [ ] `mio-nats-0` running; gateway logs show `stream MESSAGES_INBOUND created/verified`.
- [ ] Gateway pod pulls image from `ghcr.io/vanducng/mio/gateway:<sha>` via
      `ghcr-pull` Secret (no `ImagePullBackOff`).
- [ ] cert-manager issued `mio-gateway-tls` from `letsencrypt-production`.
- [ ] `https://mio.abspectrumservices.org/cliq` returns 200 on a real Cliq
      POST with valid HMAC; 401 on tampered HMAC.
- [ ] `mio-echo-consumer` pod Running, logs show JetStream subscription
      established on stream `MESSAGES_INBOUND`.
- [ ] Type "ping" in `devduc` Cliq channel → echo reply ≤ 5s.
- [ ] sink-gcs writes a partitioned object to `gs://ab-spectrum-backups-prod/mio/` for that day.
- [ ] `docs/runbooks/cliq-webhook-down.md` merged to mio repo.
- [ ] `docs/deployment.md` documents secret rotation procedure + NATS HA upgrade path.
- [ ] CI workflow publishes all three images:
      `ghcr.io/vanducng/mio/{gateway,sink-gcs,echo-consumer}:<sha>`.

## Risks

- **Single-instance NATS** — POC accepts data loss on pod loss. Mitigation:
  doc upgrade path (3-replica + PDB) in `docs/deployment.md`. Out of scope for P8.
- **HMAC secret mismatch** — Deluge script uses
  `d3aecd30d153375933beabeba31a13df65acbc7f8bfba528ad4aa56cf8748327`;
  must match cluster Secret exactly. Mitigation: copy from playground
  `secrets.env` verbatim; verify via failed-then-success curl test before
  rotating.
- **CNPG bootstrap race** — gateway pod starts before DB ready ⇒ crashloop.
  Mitigation: gateway already has `/readyz` probe gating on pg ping
  (`gateway/internal/health/`); k8s will retry until DB up.
- **Flux reconcile timing on secret update** — ~1-5 min default interval.
  Mitigation: `flux reconcile kustomization mio --with-source` for forced reconcile.
- **Image tag drift** — Flux pulls whatever tag the HelmRelease pins; CI
  publishes per-SHA but no auto-bump. Manual bump per release. Mitigation:
  document in deployment.md; defer auto-bump (image-reflector-controller) to P10.
- **Single-replica gateway during rollout** — chart default is 2 replicas
  with `maxSurge=0,maxUnavailable=1`; one pod always Ready. Acceptable for POC.

## Out (deferred to P10)

- kube-prometheus-stack rebuild / dedicated obs node (cluster likely already
  has prometheus-operator; ServiceMonitors auto-scrape).
- Tempo + OTel Collector + tail sampling.
- Grafana dashboards as ConfigMap.
- PrometheusRule alerts (5 rules) + `promtool` CI lint.
- Failure-injection drills (kubectl delete pod / toxiproxy / Chaos Mesh).
- Runbooks for `jetstream-degraded`, `outbound-rate-limit`, `release.md`.
- Cost projection doc.
- WIF for GHA (Flux model means GHA never touches the cluster directly).
- image-reflector-controller for auto image-tag bumps.
- NATS HA (3 replicas + PDB).
- Second channel adapter.
- Cloud Armor / WAF.

## Decisions locked (2026-05-09)

1. **DNS:** Cloud DNS managed zone, manual A record via `gcloud dns record-sets create` (Step 4.2).
2. **Channel name:** `devduc`. The cluster Secret carries `CLIQ_BOT_TOKEN` only; channel name is configured in the Cliq bot dashboard, not as a gateway env var.
3. **GCS:** reuse `gs://ab-spectrum-backups-prod` with prefix `mio/`. No new bucket. IAM Condition scopes the new `mio-sink-gcs` SA to the prefix.
4. **Image tag:** pin to commit SHA per deploy in HelmRelease values; manual bump in infra-repo PR per release. Auto-bump (image-reflector-controller) deferred to P10.
5. **Echo consumer:** authored as a chart in this phase (`deploy/charts/mio-echo-consumer`); CI gains a `build-echo-consumer` job and publishes `ghcr.io/vanducng/mio/echo-consumer:<sha>`. HelmRelease at `apps/prod/mio/release-echo.yaml` pulls per-SHA.
6. **Gateway Secret keys:** `CLIQ_WEBHOOK_SECRET`, `CLIQ_BOT_TOKEN`, `CLIQ_BOT_SCOPE`, `DATABASE_URL` only. Matches `deploy/charts/mio-gateway/templates/deployment.yaml` mapping. The longer `ZOHO_CLIQ_*` superset (client_id/secret/refresh_token/bot_id/etc.) is out of scope — gateway uses a pre-baked OAuth access token; in-cluster refresh-token handling is a P10+ concern.
7. **Namespace + GitRepository placement:** `fluxcd/apps/base/mio/` (mirrors freescout). Per-env overlays at `apps/prod/mio/` reference `../../base/mio`.
8. **NATS POC:** 1 replica, emptyDir, no PVC. Accepts data loss per Risk #1. HA upgrade path (3 replicas + PDB + JetStream PVC) documented in `docs/deployment.md`. Replica override key path verified during §3.7 via `helm show values ./deploy/charts/mio-nats`.

## Still to confirm during execution (non-blocking)

- **`apps/base/kustomization.yaml` aggregator:** if a base-level aggregator file exists in the infra repo, append `- mio` to it. If base entries are referenced directly from per-env overlays (no aggregator), nothing to append at base — verify during §3.9 before opening the PR.
- **NATS upstream chart key path** for replica override: confirmed shape is `values.nats.config.cluster.replicas` (typical for upstream nats-io chart) but exact path is verified in §3.7 prep step before authoring `release-nats.yaml`.

## Research backing

[`plans/reports/research-260508-1056-p8-poc-deploy-observability-gke.md`](../../reports/research-260508-1056-p8-poc-deploy-observability-gke.md)
— observability picks deferred to P10. P8 reuses what's already in
`dp-prod-7e26`: ingress-nginx, cert-manager + `letsencrypt-production`,
CNPG, SOPS, FluxCD. Reference deployment pattern: freescout (single
HelmRelease + ingress + SOPS secrets + CNPG cluster).
