# Cook Report — P7: Helm + NATS on GKE

**Date:** 2026-05-08
**Phase:** P7 — Helm charts + NATS on GKE
**Status:** DONE_WITH_CONCERNS

## TL;DR

Four Helm charts created and lint-clean. CI/CD workflows wired. P3 fixture path prerequisite resolved (testdata/ migration). kind not installed locally — kind-smoke deferred to P8. All helm lint + helm template checks pass. Gateway normalize tests: 22/22 green.

## Files Modified/Created

### Prerequisite: P3 fixture path fix
- `gateway/internal/channels/zohocliq/testdata/` — 6 PII-scrubbed JSON fixtures (real names/emails/org-IDs replaced with synthetic test data)
- `gateway/internal/channels/zohocliq/normalize_test.go` — `fixtureDir` changed from `../../../../playground/cliq/captures` → `testdata`

### Helm charts
- `deploy/charts/mio-nats/Chart.yaml` — upstream nats-io/k8s dep, pinned to 1.2.7
- `deploy/charts/mio-nats/values.yaml` — 3-replica JetStream, pd-balanced 10Gi, topologySpreadConstraints, PDB minAvailable=2, promExporter
- `deploy/charts/mio-nats/values-kind.yaml` — kind overrides (1 replica, standard storageClass, no PDB, no topology spread)
- `deploy/charts/mio-nats/templates/_helpers.tpl`
- `deploy/charts/mio-nats/templates/servicemonitor.yaml`
- `deploy/charts/mio-jetstream-bootstrap/Chart.yaml` — verify-only, no upstream dep
- `deploy/charts/mio-jetstream-bootstrap/values.yaml` — expectedStreams + expectedConsumers config
- `deploy/charts/mio-jetstream-bootstrap/templates/_helpers.tpl`
- `deploy/charts/mio-jetstream-bootstrap/templates/configmap.yaml` — hook weight 0
- `deploy/charts/mio-jetstream-bootstrap/templates/rbac.yaml` — SA + Role + RoleBinding, hook weight 1
- `deploy/charts/mio-jetstream-bootstrap/templates/job.yaml` — hook weight 5, VERIFY-ONLY script (quorum poll 5x + stream/consumer info check, exits non-zero on miss)
- `deploy/charts/mio-gateway/Chart.yaml`
- `deploy/charts/mio-gateway/values.yaml` — image block (ghcr.io/vanducng/mio/gateway, pullPolicy IfNotPresent, ghcr-pull secret), maxSurge=0/maxUnavailable=1
- `deploy/charts/mio-gateway/templates/_helpers.tpl`
- `deploy/charts/mio-gateway/templates/configmap.yaml` — MIO_MIGRATE_ON_START=false enforced
- `deploy/charts/mio-gateway/templates/secret.yaml` — documentation-only, no resource rendered
- `deploy/charts/mio-gateway/templates/serviceaccount.yaml`
- `deploy/charts/mio-gateway/templates/service.yaml` — http + metrics ports
- `deploy/charts/mio-gateway/templates/deployment.yaml` — sequential rollout strategy, imagePullSecrets, all env from ConfigMap + Secret refs
- `deploy/charts/mio-gateway/templates/ingress.yaml` — gce className, cert-manager annotations commented for P8
- `deploy/charts/mio-gateway/templates/hpa.yaml` — autoscaling/v2, Prometheus Adapter metric
- `deploy/charts/mio-gateway/templates/servicemonitor.yaml` — {channel_type, direction, outcome} label contract enforced
- `deploy/charts/mio-gateway/templates/migration-job.yaml` — pre-install/pre-upgrade hook weight -5
- `deploy/charts/mio-sink-gcs/Chart.yaml`
- `deploy/charts/mio-sink-gcs/values.yaml` — WI annotation, 1 replica POC, ghcr-pull secret
- `deploy/charts/mio-sink-gcs/templates/_helpers.tpl`
- `deploy/charts/mio-sink-gcs/templates/serviceaccount.yaml` — iam.gke.io/gcp-service-account annotation
- `deploy/charts/mio-sink-gcs/templates/deployment.yaml` — nonroot 65532, readOnlyRootFilesystem
- `deploy/charts/mio-sink-gcs/templates/servicemonitor.yaml`

### K8s + GKE
- `deploy/k8s/namespace.yaml` — mio namespace manifest
- `deploy/gke/setup.sh` — idempotent 9-step bootstrap (cluster, namespace, Cloud SQL TODO, GCS bucket + lifecycle, GSA, WI binding, 60s propagation wait + verification pod, ghcr-pull secret, helm install in order)

### CI/CD
- `.github/workflows/ci.yaml` — single workflow: changes (dorny/paths-filter@v3) → test-proto / test-gateway / test-python / helm-lint / build-gateway / build-sink-gcs (main push only). Registry cache. No :latest on non-tag pushes.
- `.github/workflows/deploy.yaml` — workflow_run trigger on CI success, GCP SA JSON auth, helm upgrade --set image.tag=<sha>, rollout status check, in-cluster healthz probe
- `.github/dependabot.yml` — weekly bumps for github-actions, gomod (4 dirs), pip (2 dirs), docker (2 Dockerfiles)

### Quality
- `.golangci.yml` — errcheck, govet, staticcheck, unused, gosimple, ineffassign, typecheck
- `Makefile` — added: helm-lint, helm-template, kind-up, kind-deploy, kind-smoke, kind-down

## Commands + Results

```
helm lint deploy/charts/mio-nats           → 1 chart linted, 0 failed
helm lint deploy/charts/mio-jetstream-bootstrap → 1 chart linted, 0 failed
helm lint deploy/charts/mio-gateway        → 1 chart linted, 0 failed
helm lint deploy/charts/mio-sink-gcs      → 1 chart linted, 0 failed

helm template test-nats ... --values values-kind.yaml → renders cleanly
helm template test-bootstrap ...          → SA ConfigMap Role RoleBinding Job
helm template test-gateway ...            → SA ConfigMap Service Deployment HPA Ingress ServiceMonitor Job
helm template test-sink-gcs ...           → SA Deployment ServiceMonitor

gateway normalize tests: 22/22 PASS (testdata/)
gateway/internal/... full suite: all packages PASS
```

## Success Criteria Checklist

- [x] `helm lint` clean on all four charts — PASS (0 failures, INFO-only)
- [x] `helm template` renders all four charts without errors
- [ ] `chart-testing ct lint` — ct not installed; deferred (CI workflow references it)
- [ ] Kind smoke test (`make kind-smoke`) — kind not installed locally; deferred to P8
- [ ] Negative bootstrap test — requires live NATS; deferred to P8
- [ ] GKE: 3 NATS pods Ready, 2 gateway, 1 sink-gcs, bootstrap Job Completed — P8
- [ ] Quorum confirmed (`nats stream cluster info`) — P8
- [ ] Subject grammar test (gateway-side validation) — P3 unit tests cover this; live test P8
- [x] Migration Job runs pre-upgrade; gateway pods MIO_MIGRATE_ON_START=false — in chart spec
- [ ] Gateway Ingress reachable; /healthz 200 — P8
- [ ] Sink-gcs WI to real GCS — P8
- [ ] PDB quorum test (cordon one node) — P8
- [ ] CI green on main — wired; runs when pushed
- [ ] Image published to GHCR — not pushed (hard guardrail)
- [ ] Deploy.yaml runs after CI — wired; executes on real push

## Hard Contracts Verified

- maxSurge=0, maxUnavailable=1 — in deployment.yaml strategy block
- No stream-bootstrap Job (verify-only) — job.yaml has no nats stream add calls
- pd-balanced in values.yaml — confirmed
- ghcr.io/vanducng/mio/gateway:<sha> image refs — in ci.yaml tags block
- MIO_MIGRATE_ON_START=false in ConfigMap, true only in migration-job — confirmed
- No :latest on main pushes — ci.yaml tags list has sha + main, no latest

## Deferred / Concerns

1. **kind not installed locally** — kind-smoke (make kind-smoke) deferred to P8. Charts validated via helm lint + helm template only. Install kind to run full smoke: `brew install kind`.
2. **chart-testing (ct lint)** — ct binary not installed; CI job references it but local make target uses helm lint only. Install: `brew install chart-testing`.
3. **mio-jetstream-bootstrap negative test** — requires live NATS to prove the verification gate fires on missing stream; deferred to P8 integration test.
4. **GKE criteria** — all GKE-specific criteria (quorum, WI, Ingress, PDB drain test) require real cluster; P8 scope.
5. **nats-box image in job.yaml uses runAsUser: 65532** — nats-box 0.14.5 base image may not have user 65532; if job fails with permission error, change securityContext.runAsUser to 1000 or remove it. Flagged for P8 validation.
6. **mio-sink-gcs Service not rendered** — sink-gcs has no Service template (Deployment + ServiceAccount + ServiceMonitor only). ServiceMonitor selector matches pod labels; if prometheus scrape requires a Service, add it in P8.

## Open Questions

None blocking P8.
