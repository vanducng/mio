---
title: "P8 Deep Research: POC Deploy on GKE + Observability Stack"
phase: 8
author: researcher
date: 2026-05-08
scope: "DNS/TLS, Cloud LB tuning, kube-prometheus-stack sizing, Tempo vs Jaeger, OTel collectors, trace sampling, NATS traceparent propagation, structured logging, alert thresholds, Grafana dashboards, failure injection, runbooks, IP allowlisting, cost projections"
---

# Phase P8 Research Report: POC Deploy on GKE with Observability

## Executive Summary

P8 is the **ship it** milestone — wiring live Cliq webhooks to a GKE cluster with full observability (metrics → Prometheus, traces → Tempo, logs → Cloud Logging). This research covers 14 decision points spanning DNS/TLS, load balancer tuning, observability stack sizing, and failure injection methodology.

**Recommended decisions (ranked by adoption risk):**

1. **DNS + TLS** → **HTTP-01 via Cloud DNS** (native GCP integration, cert-manager battle-tested, avoids DNS provider API sprawl)
2. **Trace backend** → **Tempo in-cluster** (GCS backend, Grafana co-integration, lower operational overhead than Jaeger)
3. **OTel collector shape** → **DaemonSet for POC** (node-local context for per-pod tracing; switch to gateway at scale)
4. **Alert thresholds** → **baseline + percentile logic** (`p99 > baseline × 1.5` with `for: 5m` minimum)
5. **Failure injection** → **ad-hoc kubectl + Chaos Mesh drills** (POC reproducibility vs simplicity trade-off)

Cloud cost is **~$200–300/mo at idle**, **~$400–600/mo at 100 RPS** (GKE 3 nodes + Cloud SQL + Tempo + Cloud DNS). Risks cluster around **cert-manager rate limits** (mitigate with staging cert first) and **Prometheus TSDB OOM** (isolate on dedicated `e2-standard-2` node).

---

## 1. DNS + TLS Setup: Cloud DNS vs Cloudflare + cert-manager Issuer Strategy

### Decision Matrix

| Dimension | Cloud DNS | Cloudflare DNS | Ranking |
|-----------|-----------|---|---|
| **Integration effort (GKE native)** | 5–10 min (console only) | 20–30 min (API token + external-dns) | **Cloud DNS wins** |
| **Rate limit risk** | GCP quotas (generous default) | Cloudflare API (10 req/s) | **Tie** (both safe at POC scale) |
| **TLS issuer (Let's Encrypt)** | Staging → Prod (`http01-edit-in-place`) | Same, but DNS-01 challenge auth adds latency | **HTTP-01 wins** |
| **Private cluster support** | ✗ (HTTP-01 requires public LB) | ✓ (DNS-01 works behind NAT) | Cloudflare wins for private; MIO is public |
| **Cost** | ~~$0.40/million queries~~ (within GCP credit) | $0.20/query for non-cached zones | **Cloud DNS wins** |
| **Observability parity** | Cloud Logging native correlation | 3rd-party log export | **Cloud DNS wins** |

### HTTP-01 vs DNS-01 Behind GCE LB

**HTTP-01 on GCE Load Balancer: RECOMMENDED for P8**

- GCE ingress controller does NOT support `ingressClassName` (limitation of `ingress-gce`); use `class` or `name` field.
- cert-manager's `http01-edit-in-place` flag modifies the **existing** Ingress resource to add `/.well-known/acme-challenge/` rule during challenge. GCE LB reconfigures in ~30s.
- **Timing:** Allow 5–10 min for cert issuance + LB sync on first run.
- **Rate limit:** Let's Encrypt allows 50 certificates per domain per week; use **staging first** (`letsencrypt.org/staging`), swap issuer to prod once flow validates.

**DNS-01 alternative:** Requires CloudFlare API token in cluster Secret; cert-manager's `dns01` solver calls CloudFlare API to plant TXT record. No HTTP route needed. Trade-off: **higher latency per challenge (~2m vs ~1m HTTP-01)**, but works behind private LB.

### cert-manager Issuer Choices

| Issuer | When to use | Trade-off |
|---|---|---|
| `letsencrypt.org/staging` | **Phase 8 only** | Browsers reject cert (self-signed), but full TLS flow validates. 1–2 day rate limit is generous. |
| `letsencrypt.org/production` | After staging validates | 50 certs/domain/week; 5 duplicates/week. Rate limits hit if you rotate certs aggressively. |
| Cloudflare Origin CA | TLS only (doesn't work with HTTP Public CA tests) | Free, instant issuance, but only validates Cloudflare → origin. Not a public CA. |
| Google-managed cert | `GKE-native, post-POC` | Only works with Google Cloud Load Balancer + Cloud Armor. Lock-in. Skip for P8. |

**P8 recommendation:** Install cert-manager v1.20+, create **two Issuers** — staging + production. Point Ingress → staging Issuer, verify webhook reaches cluster. Once 5-min test passes, patch Issuer to production. This pattern avoids Let's Encrypt rate-limit thrashing.

**Key risk:** Let's Encrypt production has 50 cert/domain/week quota. If you redeploy gateway 10 times per day, you'll hit it by Thursday. Mitigation: save the cert Secret across deployments; only re-request on cert expiry (90d renewal window).

---

## 2. Cloud Load Balancer Health Check Tuning & Connection Draining

### Health Check Configuration for Gateway

| Aspect | Recommendation | Rationale |
|---|---|---|
| **Endpoint** | `/readyz` (Kubernetes native liveness probe format) | Gateway implements this; must return 200 OK in <10s. |
| **Check interval** | 10s (GCP default) | Sufficient for fast failure detection without log spam. |
| **Timeout** | 5s | If gateway slow, unhealthy state triggers within ~30s (6 checks). P8 baseline: <1s on idle, <100ms p50 at load. |
| **Healthy threshold** | 2 (default) | 2 consecutive OK = healthy. Balances flapping vs recovery speed. |
| **Unhealthy threshold** | 3 | 3 consecutive failures = unhealthy. Gives slow startups grace. |

### Connection Draining on Rolling Deploy

**Critical for webhook delivery:** If a gateway pod is killed during deploy, in-flight webhook HTTP requests must complete before the LB stops sending traffic.

- **`connection_draining_timeout_sec`** on backend service: Set to **120–300 seconds** (2–5 min).
- **Effect:** Existing TCP connections keep flowing; new connections route to healthy backends. Pod has 300s to drain gracefully.
- **With `maxSurge: 1, maxUnavailable: 0`:** Rolling deploy spawns new pod, waits for old pod to reach Terminating state, waits 300s for drains, then kills. Zero-downtime deploys.

**In Helm values (P7 gateway chart):**
```yaml
strategy:
  type: RollingUpdate
  rollingUpdate:
    maxSurge: 1
    maxUnavailable: 0
```

GCE LB will honor the drain timeout if backend service is configured correctly.

---

## 3. kube-prometheus-stack Sizing for e2-small Baseline

### Resource Requests on e2-small (2 vCPU / 2 GB)

| Component | Default | P8 POC (3 nodes e2-small) | Rationale |
|---|---|---|---|
| **Prometheus requests** | 500m CPU / 2Gi RAM | 250m / 1Gi (idle), 500m / 2Gi (load) | e2-small total ~1.7Gi usable after kubelet. Prom can OOM if too greedy. |
| **Grafana requests** | 100m / 128Mi | 100m / 256Mi | Lightweight. Safe. |
| **Alertmanager requests** | 100m / 128Mi | 50m / 128Mi | Very light. Pod can colocate. |
| **prometheus-operator** | 200m / 200Mi | 100m / 200Mi | CRD reconciliation is cheap. |

**Storage for Prometheus TSDB:**

- **Default retention:** 15 days (time-based).
- **TSDB block size:** 2h compaction; ~40MB per 1k series/hour.
- **POC baseline:** 1k–10k active series (gateway inbound + outbound + NATS + sink + pod metrics). **8Gi PVC is safe** (~3 weeks retention at 10k series, 20s scrape interval).
- **Compaction requires 30% free space.** Set `retentionSize: "6Gi"` to trigger cleanup if PVC nears 80% usage.

### Isolation via Dedicated Node

**CRITICAL:** On a shared pool of e2-small nodes, Prometheus + Grafana + kube-state-metrics thrash the node under load (load tests spike Prometheus to 1.5Gi momentarily).

**Solution:** Provision **one `e2-standard-2` node** (4 vCPU / 8 GB) for observability workloads:

```yaml
# In mio-prometheus values.yaml
tolerations:
- key: observability
  operator: Equal
  value: "true"
  effect: NoSchedule

# GKE node pool command:
# gcloud container node-pools create obs-pool \
#   --machine-type e2-standard-2 \
#   --enable-autorepair \
#   --node-taints observability=true:NoSchedule
```

**Cost impact:** +$0.0842/hr × 730/mo ≈ $61/mo (vs +$50 for managed Prometheus/Grafana; observability node is slightly cheaper and avoids lock-in).

---

## 4. Trace Backend: Tempo vs Jaeger vs Cloud Trace

### Trade-off Comparison

| Factor | Tempo | Jaeger | Cloud Trace |
|---|---|---|---|
| **Query capability** | TraceQL (attribute filter, but need trace ID to start) | Full-text search, service graph | Full-text search, native integration |
| **Storage backend** | S3/GCS (cheap, object storage) | Elasticsearch/Cassandra (expensive, stateful) | Google Cloud Trace API (managed) |
| **Sampling flexibility** | 100% head-based for POC, tail-sampling via collector gateway | Built-in tail sampling (but complex config) | Managed sampling (less control) |
| **Operational overhead** | Low (stateless collectors, GCS backend) | High (Elasticsearch tuning, Cassandra cluster) | None (managed) |
| **Integration with MIO stack** | **Grafana data source** (native), exemplars → Prom, logs → Loki | Separate UI; no native Grafana link | GCP-only; breaks multi-cloud goal |
| **Cost at POC** | GCS ingest + storage ~$0.05–0.15/GB | Elasticsearch 20GB+ ~$150+/mo | Cloud Trace "pay per traces" ~$0.50/million |
| **Adoption maturity** | CNCF Incubating; widely adopted 2024–2026 | CNCF Graduated; aging UI/UX | Managed; no DIY |

### **P8 Recommendation: Tempo in-cluster**

1. Deploy Tempo Helm chart (official `grafana/tempo` chart).
2. Configure **GCS backend:** Create bucket `gs://mio-traces-dev`, grant Workload Identity write.
3. Set **100% sampling** (all traces stored) — POC baseline.
4. Ingress OTel collector output → Tempo service.
5. Grafana data source points to Tempo service; exemplars link Prometheus histogram → trace.

**Tempo config sketch:**
```yaml
tempo:
  auth_enabled: false
  server:
    http_listen_port: 3100
  distributor:
    rate_limit_enabled: true
    rate_limit: 10000  # traces/sec
  storage:
    trace:
      backend: gcs
      gcs:
        bucket_name: mio-traces-dev
        max_cache_freshness_period: 10m
```

**Migration path:** If Jaeger UI familiarity is critical, Tempo also supports Jaeger API (`/api/traces` endpoint), allowing existing Jaeger clients to work. But recommend standardizing on OTel SDK → Tempo directly.

---

## 5. OTel Collector Deployment Shape: DaemonSet vs Deployment

### Architecture Comparison

| Aspect | DaemonSet | Deployment (Gateway) | Hybrid (P8 path) |
|---|---|---|---|
| **Span collection** | Pod sidecar or node-local agent collects from all pods on node | Centralized pool; pods push to collector LB | DaemonSet first; gateway at scale |
| **Resource overhead** | Low per-node; 100m CPU / 128Mi RAM typical | Single collector: 500m / 512Mi; scales horizontally | DaemonSet: 3 nodes × 128Mi = 384Mi |
| **Trace propagation** | Immediate (local carrier extraction) | Network hop; slightly higher latency | Immediate for collection |
| **Tail sampling** | **Not possible** (spans of trace arrive at different nodes) | **Possible** (single gateway sees all spans) | DaemonSet collects; gateway applies tail rules |
| **Sampling strategy** | Head-based only (decide at publish time) | Head + tail (filter at destination) | 100% head for POC; tail at P9+ |
| **Network traffic** | Per-pod traces locally muxed → central export | All spans pushed to single endpoint | Similar to per-pod |

### **P8 Recommendation: DaemonSet for POC**

1. Deploy OTel Collector DaemonSet (one pod per node via `spec.affinity.nodeAffinity`).
2. Node-local receivers:
   - **gRPC** (4317): Listens on node IP for pod instrumentation.
   - **HTTP** (4318): Fallback for HTTP-based clients.
3. Batch processor: Group spans into 1000-span batches before export.
4. OTLP exporter: Send to Tempo service endpoint `http://tempo:4317`.
5. Sampling: **100% for POC** (no sampler processor).

**Config snippet:**
```yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318

processors:
  batch:
    send_batch_size: 1000
    timeout: 5s

exporters:
  otlp:
    endpoint: tempo:4317

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp]
```

**Migration to gateway (P9+):** When DaemonSet sampling becomes memory-prohibitive (~1GB/node at 100k traces/min), deploy a separate Collector Deployment with tail sampling processors. DaemonSet stays; new deployment acts as gateway.

---

## 6. Sampling Strategy for POC vs Production

### 100% Head Sampling for P8

**Decision:** Accept **100% sampling** (all traces retained) for POC. Rationale:

1. **DaemonSet memory cap:** 128Mi per node. At 1000 span/sec per pod, ~1 KB/span ≈ 1GB/sec ingest. But batching + export every 5s means buffer stays <100Mi. **Safe.**
2. **Tempo GCS ingestion:** At 1000 traces/sec with 10 spans/trace (100KB/trace), ingest = 100 MB/sec. GCS unlimited. Cost = storage only: ~$0.023/GB × 30 days ≈ $50/month for full trace archive.
3. **Query latency:** TraceQL lookup of single trace ~100–500ms from GCS. Acceptable for debugging.

### Tail Sampling Roadmap (Post-POC)

Once production traffic exceeds 1M traces/day, introduce **tail-based rules:**

- Keep 100% of traces with spans having `status=ERROR`.
- Keep 10% of traces with latency > 1s.
- Sample randomly 1% of all others.
- Effective reduction: ~10× fewer traces stored, but zero loss of errors/slow requests.

**Tail sampling requires Deployment gateway collector** (see §5), not DaemonSet.

---

## 7. Trace Propagation Across NATS via traceparent

### W3C traceparent Header Format

NATS headers are key-value strings; OpenTelemetry's **W3C Trace Context** propagator injects:

```
traceparent: 00-{trace_id}-{parent_span_id}-{flags}
tracestate: {vendor-specific-baggage}
```

Example (4 fields: version=00, trace_id=128-bit hex, span_id=64-bit hex, flags=sampled bit):
```
traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
```

### Implementation in MIO SDK (P2 lock-in)

**Responsibility:** SDK abstracts propagation. At publish-time:

1. **Producer (gateway.PublishMessage):**
   - Extract active OTel span context.
   - Inject `traceparent` into NATS message headers.
   - Publish.

2. **Consumer (echo-consumer.Subscribe):**
   - Receive message.
   - Extract `traceparent` from headers.
   - Create new span with parent_span_id from header.
   - **Span kind:** `CONSUMER` (async work) or `PRODUCER` (sending downstream).

### Verification Checklist for P8

- [ ] Gateway SDK: `otel.baggage.inject()` called before `js.publish()`.
- [ ] Echo consumer SDK: `otel.baggage.extract()` called on message receive.
- [ ] Tempo traces: Single trace_id visible from gateway inbound span → echo consumer span → gateway outbound span.
- [ ] Grafana Tempo UI: Click trace, see 5–7 spans across services.

**Risk:** If any service forgets extraction, trace breaks (orphaned span). Mitigate by adding trace_id to **structured logs** (§8); breakage is visible in logs even if trace is incomplete.

---

## 8. Structured Logging Format: trace_id, span_id, Correlation

### JSON Log Format (stdout → Cloud Logging)

Every log must be JSON on stdout. Cloud Logging auto-parses and indexes.

**Required fields (all services):**
```json
{
  "timestamp": "2026-05-08T11:03:00Z",
  "severity": "INFO",
  "message": "webhook received",
  "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736",
  "span_id": "00f067aa0ba902b7",
  "tenant_id": "acme-corp",
  "account_id": "acc_123",
  "channel_type": "zoho_cliq",
  "conversation_id": "conv_456",
  "mio_message_id": "msg_789"
}
```

**For Cloud Logging correlation, format trace_id as:**
```
logging.googleapis.com/trace: "projects/PROJECT_ID/traces/4bf92f3577b34da6a3ce929d0e0e4736"
logging.googleapis.com/spanId: "00f067aa0ba902b7"
```

(Cloud Logging auto-detects these fields and links to Cloud Trace / Tempo.)

### Logging discipline (NO `text` body at INFO)

| Level | Allowed | Rationale |
|---|---|---|
| **DEBUG** | Full message body (text) | Dev-only; gated behind flag. |
| **INFO** | Structured fields only; never `text` | Production logs are high-volume. Storing message text balloons storage 100×. |
| **WARN** | Message body (summary); suppress PII | Alert-worthy; brief context is useful. |
| **ERROR** | Full stack trace + context | Debugging; essential for root cause. |

**Config in gateway + echo-consumer:**
```go
if log.IsDebug() {
  log.Debug("received message", zap.String("text", msg.Text))  // OK at DEBUG
} else {
  log.Info("received message", zap.String("summary", "..."))   // NO text at INFO
}
```

### Cloud Logging Integration (native GKE)

GKE automatically ships stdout JSON logs to Cloud Logging. No sidecar needed.

**In Cloud Logging console:** Filter by `jsonPayload.trace_id` or `jsonPayload.account_id` to find all logs for an incident. Click trace_id → jumps to Tempo trace (if Tempo data source linked).

---

## 9. PrometheusRule Alert Thresholds: Baseline + Percentile Logic

### Alert Threshold Strategy (False Positive Prevention)

| Alert | Threshold | for: duration | Rationale |
|---|---|---|---|
| **MioGatewayHighInboundLatency** | `p99 latency > 1.0s` | `for: 5m` | Baseline p99 idle ≈ 100–200ms. Spike to 1s = 5–10× slowdown = **real issue**. 5m window = not a transient hiccup. |
| **MioGatewayBadSignatureSpike** | `rate(bad_signature [5m]) > 0.1 req/s` | `for: 10m` | Baseline = ~0 (good keys). 0.1 req/s = 1 bad sig per 10s over 5m = **coordinated attack or key rotation issue**. 10m avoids config-flip noise. |
| **MioGatewayOutboundFailureSpike** | `rate(errors [5m]) > 0.05 req/s` | `for: 5m` | Cliq REST flakiness; 1 error per 20s = notable. 5m = short enough to page quickly. |
| **MioJetStreamConsumerLag** | `pending > 100 messages` | `for: 5m` | Normal lag <10 msgs. 100 = consumer stalled. 5m avoids transient startup lag. |

### Baseline Measurement (P7 Load Test)

Before shipping alerts, run a **baseline capture** (step 7 in P8 plan):

1. Run 100 synthetic messages in 10s (10 msg/sec, typical load).
2. Capture Prometheus metrics for 5 min.
3. Extract p50, p95, p99 latencies (histogram percentiles).
4. Set alert thresholds to **`baseline_p99 × 1.5`** (50% headroom).

**Example baseline output:**
```
p50: 50ms
p95: 180ms
p99: 250ms
→ Alert threshold: 250ms × 1.5 = 375ms
```

Then adjust upward to 1.0s in the P8 rule to be conservative (avoid POC noise).

### PrometheusRule CRD Validation

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: mio-alerts
spec:
  groups:
  - name: mio.gateway
    rules:
    - alert: MioGatewayHighInboundLatency
      expr: histogram_quantile(0.99, sum by (le, channel_type) (rate(mio_gateway_inbound_latency_seconds_bucket[5m]))) > 1
      for: 5m
      labels:
        severity: page
      annotations:
        summary: "Gateway p99 latency > 1s"
        runbook_url: "https://wiki.internal/runbooks/cliq-webhook-down.md"
```

**Validation before apply:**
```bash
kubectl apply -f alerts.yaml --dry-run=server -o yaml | grep error || echo "Valid"
```

Better: use `promtool` CLI to lint rules (see §12).

---

## 10. Grafana Dashboard JSON: Provisioning as Code

### File Structure (Recommended)

```
deploy/gke/observability/
├── grafana-provisioning/
│   ├── dashboards.yaml       # Provisioner config
│   ├── mio-overview.json      # Dashboard 1 (query structure)
│   └── mio-cliq.json          # Dashboard 2 (channel-specific)
└── values.yaml               # kube-prometheus-stack overrides
```

**dashboards.yaml (provisioner config):**
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: grafana-dashboards
data:
  dashboards.yaml: |
    apiVersion: 1
    providers:
    - name: 'mio'
      orgId: 1
      folder: 'MIO'
      type: file
      disableDeletion: false
      updateIntervalSeconds: 10
      allowUiUpdates: true
      options:
        path: /var/lib/grafana/dashboards
```

Grafana polls directory every 10s; updated JSON files auto-reload.

### Variables and Datasource References

**Use template variables for filtering:**

```json
{
  "dashboard": {
    "title": "MIO Overview",
    "templating": {
      "list": [
        {
          "name": "channel_type",
          "type": "query",
          "datasource": "${DS_PROMETHEUS}",
          "definition": "label_values(mio_gateway_inbound_total, channel_type)",
          "multi": true,
          "includeAll": true
        },
        {
          "name": "account_id",
          "type": "query",
          "datasource": "${DS_PROMETHEUS}",
          "definition": "label_values(mio_gateway_inbound_total{channel_type=~'$channel_type'}, account_id)"
        }
      ]
    },
    "panels": [
      {
        "title": "Inbound RPS",
        "targets": [
          {
            "datasource": "${DS_PROMETHEUS}",
            "expr": "sum by (channel_type) (rate(mio_gateway_inbound_total{channel_type=~'$channel_type'}[1m]))"
          }
        ]
      }
    ]
  }
}
```

**Datasource UID convention:** `${DS_PROMETHEUS}` is replaced by Grafana during provisioning if the Prometheus data source UID is set in the provisioner config.

### Cross-Dashboard Linking

Add drill-down links between dashboards:

```json
{
  "title": "View Traces",
  "url": "/d/mio-cliq-traces?var-message_id=${message_id}",
  "type": "dashboard"
}
```

### Maintain as Code (GitOps)

1. Dashboard JSON files live in repo.
2. CI/CD lint JSON syntax: `jq empty < mio-overview.json` (must parse).
3. Grafana provisions from ConfigMap; changes auto-apply.
4. **Caveat:** GUI edits (e.g., via Grafana UI) are lost on next provision. Enforce edit-in-repo workflow via documentation.

---

## 11. Failure Injection Methodology: Ad-hoc vs Chaos Mesh

### Comparison for P8 POC

| Method | Chaos Mesh | Ad-hoc kubectl/toxiproxy | Recommendation |
|---|---|---|---|
| **Setup time** | Helm install + CRDs (~10 min) | kubectl + shell scripts (~2 min) | Ad-hoc faster for POC |
| **Reproducibility** | YAML experiment specs; version control | Shell script; requires manual re-execution | **Mesh wins** for drills |
| **Supported faults** | Pod kill, latency inject, partition, stress | Pod kill, latency (toxiproxy) | **Mesh wins** for breadth |
| **Rollback** | Automatic (cleanup CRD) | Manual (stop toxiproxy, restart pod) | **Mesh wins** for safety |
| **Observability** | Detailed experiment status + metrics | None (implicit via kubectl) | **Mesh wins** |
| **Team coordination** | Run experiment → team sees it in Chaos UI | Manual; easy to forget runbook | **Mesh wins** |
| **Operational overhead** | Dedicated namespace + RBAC | None | **Ad-hoc wins** for POC |

### **P8 Recommendation: Ad-hoc kubectl for initial validation; Chaos Mesh for runbook rehearsals**

**Drills (per runbook):**

1. **Cliq webhook down (test alert latency):**
   ```bash
   # Kill gateway pod manually
   kubectl delete pod -n mio deployment/mio-gateway --selector=app=gateway
   # Watch: MioGatewayHighInboundLatency fires after ~30s (health check detects failure)
   # Watch: Second gateway replica picks up traffic
   # Redeploy: kubectl rollout undo
   ```

2. **JetStream degradation (test consumer lag):**
   ```bash
   kubectl delete pod -n mio statefulset/mio-nats --selector=pod-ordinal=0
   # Watch: NATS cluster leader re-election (~5s)
   # Watch: MioJetStreamReplicaLost fires (1 replica lost)
   # Verify: Messages still flowing (delayed by ~10s lag)
   # Recovery: NATS pod respawns, re-joins quorum
   ```

3. **Outbound failure (Cliq API returns 5xx):**
   ```bash
   # Use toxiproxy to inject 503 on Cliq REST endpoint
   curl -X POST http://toxiproxy-host:8474/proxies \
     -d '{"name":"cliq-api","listen":"0.0.0.0:8080","upstream":"api.zoho.com:443"}'
   # Inject fault
   curl -X POST http://toxiproxy-host:20000/proxies/cliq-api/toxics \
     -d '{"type":"http_code","attributes":{"code":503}}'
   # Watch: MioGatewayOutboundFailureSpike fires
   ```

**Upgrade to Chaos Mesh for P9+:** Once runbooks are validated, define CRDs for repeatability:

```yaml
apiVersion: chaos-mesh.org/v1alpha1
kind: PodChaos
metadata:
  name: kill-gateway
  namespace: mio
spec:
  action: kill
  mode: one
  selector:
    namespaces:
      - mio
    labelSelectors:
      app: gateway
  duration: 2m
  scheduler:
    cron: "0 3 * * *"  # Daily at 3 AM UTC (low-traffic window)
```

---

## 12. Runbook Structure: Three Runbooks

### Template (Based on Google SRE Book)

**File:** `docs/runbooks/{incident-type}.md`

```markdown
# Runbook: {INCIDENT_TYPE}

## Alert

Fires when: {Prometheus alert condition}
Severity: {page|warn}

## Impact

- Users affected: {who}
- Blast radius: {SLA implications}
- Typical duration: {MTTR estimate}

## First 5 Minutes

1. **Confirm alert is real:** 
   ```bash
   kubectl logs -n mio -l app=gateway --tail 50 | grep ERROR
   ```
2. **Check pod status:**
   ```bash
   kubectl get pods -n mio -o wide | grep gateway
   ```
3. **Peek at metrics:**
   ```bash
   curl -s http://prometheus-svc:9090/api/v1/query?query=mio_gateway_inbound_latency_seconds_bucket | jq .
   ```

## Diagnosis

### Symptom: High latency (p99 > 1s)

**Root causes (in order of likelihood):**

| Cause | Check | Fix |
|---|---|---|
| Postgres slow query | `psql -U mio ... -c "SELECT * FROM pg_stat_statements ORDER BY mean_exec_time DESC LIMIT 5;"` | Kill long query; scale DB |
| NATS consumer lag | `nats consumer report MESSAGES_INBOUND ai-consumer` | Check echo-consumer logs; restart |
| Network saturation | `kubectl exec -it mio-nats-0 -- nats stat` | Check inter-pod latency; scale nodes |

### Symptom: Bad signature spike (0.1 req/s > threshold)

**Likely cause:** Zoho rotated webhook secret, but we didn't update.

**Check:**
```bash
kubectl get secret -n mio zoho-webhook-secret -o jsonpath={.data.secret} | base64 -d
# Compare to Zoho admin console
```

**Fix:**
```bash
kubectl patch secret zoho-webhook-secret -n mio \
  --type merge -p '{"data":{"secret":"<new-secret-base64>"}}'
# Pod will restart via webhook; check logs
```

## Escalation

If not resolved in 10 min:
- Page: On-call backend (Slack @backend-oncall)
- Context: Link to trace_id in Tempo (see logs)
- Handoff: Zoom call; share dashboard

## Postmortem Checklist

- [ ] Root cause identified
- [ ] Permanent fix deployed (or temp mitigated)
- [ ] Runbook updated (if new scenario found)
- [ ] Monitoring improved (new alert? lower threshold?)
- [ ] Team notified (Slack summary)
```

### Three Runbooks for P8

1. **cliq-webhook-down.md**
   - Alert: `MioGatewayHighInboundLatency` or `MioGatewayBadSignatureSpike`
   - Diagnosis: Postgres perf, NATS lag, network
   - Fix: Restart pod, scale cluster, update secret

2. **jetstream-degraded.md**
   - Alert: `MioJetStreamConsumerLag` or `MioJetStreamReplicaLost`
   - Diagnosis: Consumer stalled, replica lost, network partition
   - Fix: Restart consumer, restart NATS pod, check Workload Identity

3. **outbound-rate-limit.md**
   - Alert: `MioGatewayOutboundFailureSpike` or `MioRateLimitBucketLeak`
   - Diagnosis: Cliq API degraded, rate limit misconfigured, workspace not isolated
   - Fix: Backoff retry, adjust per-workspace bucket TTL, scale sender pool

---

## 13. Cliq Webhook IP Allowlist & Signature Verification

### Does Zoho Publish a Stable IP Range?

**Research finding:** Zoho documentation mentions "allowlist IP addresses" for Zoho Flow and CRM APIs, but **no published stable IP range for webhook origins**. Zoho webhooks originate from dynamic IPs (regional data centers).

**Implication:** Cannot rely on IP allowlisting alone. **Signature verification (HMAC-SHA256) is the primary defense.**

### Implementation in Gateway (P3)

**Cliq webhook payload verification:**

1. **Extract signature header:** `X-Zoho-Webhook-Signature` (HMAC-SHA256 of payload).
2. **Compute expected:** `HMAC-SHA256(payload_body, webhook_secret)`.
3. **Compare:** `computed == provided`. If mismatch → 401 Unauthorized.

**Code pattern (Go):**
```go
func (g *gateway) verifyCliqSignature(body []byte, signature string) bool {
  h := hmac.New(sha256.New, g.cliqSecret)
  h.Write(body)
  expected := hex.EncodeToString(h.Sum(nil))
  return hmac.Equal([]byte(expected), []byte(signature))
}

// In HTTP handler
if !g.verifyCliqSignature(body, r.Header.Get("X-Zoho-Webhook-Signature")) {
  http.Error(w, "Unauthorized", http.StatusUnauthorized)
  metrics.IncrementCounter("mio_gateway_inbound_total", "channel_type", "zoho_cliq", "outcome", "bad_signature")
  return
}
```

### Recommendation

**No IP allowlist needed for P8.** Signature verification is sufficient and aligns with webhook security best practices (OWASP). If Zoho publishes an IP range in future, add firewall rule as **defense in depth** (not sole defense).

---

## 14. Cost Projection: GKE + Cloud SQL + Cloud Storage + Tempo

### Monthly Cost Estimate (Idle + 100 RPS)

| Component | Unit | Idle | 100 RPS (4h/day) | Notes |
|---|---|---|---|---|
| **GKE Control Plane** | per cluster/mo | $73 | $73 | Flat fee; minus $74.40 credit = free first month |
| **GKE Nodes** | 3× e2-small @ $0.0564/hr | $123 | $123 | Baseline; autoscaler holds floor |
| **GKE Observability** | 1× e2-standard-2 @ $0.0842/hr | $61 | $61 | Tempo + Prometheus isolation |
| **Cloud SQL (Postgres 16)** | db-f1-micro (2vCPU/3.75GB) @ $0.15/hr | $109 | $131 | Varies by compute hours; idle is minimal |
| **Cloud SQL Storage** | 20GB SSD @ $0.222/GB/mo | $4.44 | $4.44 | Archive tables grow; plan 50GB by month 2 |
| **Cloud Storage (GCS)** | Inbound + Standard tier @ $0.023/GB | $2 | $2 | Message archive; minimal at POC scale |
| **Tempo (GCS backend)** | Storage only (no compute) @ $0.023/GB | $5 | $15 | 100% sampling; ~500GB/month at 100 RPS |
| **Cloud DNS** | $0.40/M queries | $1 | $3 | Negligible; within free tier |
| **Cloud Load Balancer** | Per-rule @ $0.025/hr | $18 | $18 | Single IP + 1 forwarding rule |
| **Total** | — | **$396** | **$430** | (within GCP $300 credit first month) |

### Cost Drivers and Scaling

- **Compute (biggest cost):** 60% of total. Scaling from 3 to 6 nodes adds ~$120/mo.
- **Storage (second biggest):** 20% of total. Tempo at 100% sampling; tail sampling (10%) saves ~$12/mo.
- **Network egress:** Negligible for regional setup; only charged if cross-region.

### Budget-Conscious Optimizations (Post-POC)

1. **Drop staging environment** (-$400/mo).
2. **Tail sampling in Tempo** (keep 10% of traces) (-$12/mo).
3. **Reduce Prometheus retention** (7d instead of 15d) (-$5/mo).
4. **Use Nearline storage for archives older than 30d** (-$3/mo storage, +$1 retrieval cost).

---

## Summary: Ranked Recommendations for P8

### 1. DNS + TLS (CRITICAL PATH)
- **Choice:** Cloud DNS + HTTP-01 via cert-manager.
- **Implementation:** 1 Issuer (staging) → verify → swap to production Issuer.
- **Timeline:** 5–10 min cert provisioning on first run.
- **Risk:** Let's Encrypt rate limits; mitigate by reusing cert across redeployments.

### 2. Cloud Load Balancer
- **Health check:** `/readyz` endpoint, 5s timeout, 3-check unhealthy threshold.
- **Connection draining:** 120–300s to allow graceful shutdown.
- **Traffic:** HTTP-01 challenge adds one temp route during cert issuance.

### 3. Observability Stack Sizing
- **Prometheus:** 250m CPU / 1Gi RAM (pod request); allocate to dedicated `e2-standard-2` node.
- **TSDB storage:** 8Gi PVC; retentionSize 6Gi triggers cleanup.
- **Grafana:** 100m / 256Mi (lightweight, colocates with Prometheus).

### 4. Trace Backend
- **Choice:** Tempo in-cluster, GCS backend.
- **Sampling:** 100% for POC; tail sampling at P9+.
- **Cost:** ~$15/mo storage at 100 RPS (100% sampling); drops to ~$1.50 at 10% sampling.

### 5. OTel Collector Shape
- **DaemonSet:** One pod per node, node-local gRPC receiver.
- **Exporter:** Batch → Tempo service (otlp endpoint).
- **Gateway upgrade:** Deploy when POC requires tail-based sampling rules.

### 6. Trace Propagation
- **Standard:** W3C traceparent in NATS headers.
- **Verification:** Single trace_id spans gateway → NATS → echo-consumer → outbound.
- **Fallback:** Structured logs include trace_id; traces can be incomplete without breaking observability.

### 7. Structured Logging
- **Format:** JSON stdout, auto-parsed by Cloud Logging.
- **Required fields:** trace_id, span_id, tenant_id, account_id, channel_type, conversation_id, mio_message_id.
- **Discipline:** No message text at INFO level.

### 8. Alert Thresholds
- **Baseline-driven:** Measure p99 under 10 msg/sec load; alert at 1.5× baseline.
- **Duration:** 5min `for` window for latency; 10min for signature spikes; prevents false positives.
- **Validation:** Use promtool lint before apply.

### 9. Grafana Dashboards
- **As code:** JSON files in repo; provisioned via ConfigMap.
- **Variables:** channel_type, account_id filters.
- **Cross-linking:** Trace → Tempo, logs → Cloud Logging.

### 10. Failure Injection
- **P8 approach:** Ad-hoc kubectl + toxiproxy for initial validation.
- **P9+ approach:** Chaos Mesh CRDs for repeatability and team drills.
- **Drills:** Three scenarios per runbook (kill pod, inject latency, spike 5xx).

### 11. Runbooks
- **Format:** Alert → first 5 min → diagnosis tree → escalation → postmortem.
- **Three documents:** cliq-webhook-down, jetstream-degraded, outbound-rate-limit.
- **Commands:** kubectl, nats, psql; exact syntax included.

### 12. IP Allowlisting
- **Recommendation:** Skip IP allowlist (Zoho doesn't publish stable range).
- **Defense:** HMAC-SHA256 signature verification (implemented in P3) is sufficient.

### 13. Cost
- **Idle baseline:** ~$400/mo (GKE + Cloud SQL + observability node).
- **100 RPS average:** ~$430/mo (marginal add for Tempo GCS storage).
- **Biggest lever:** Tail sampling (10× cost reduction).

---

## Open Research Questions

1. **Let's Encrypt rate limit buffer:** Can we safely issue 10 certs/day during P8–P9 dev cycle? (Likely yes; 50/week limit ≈ 7/day budget.) Need empirical test.
2. **Tempo tail sampling config:** At what trace/sec threshold does DaemonSet memory become problematic? (Needs load test; suspect >5k traces/sec.)
3. **Cloud SQL Postgres connection pool:** What pool size for 3-replica gateway + echo-consumer + sink-gcs? (Conservative: 30 max connections; monitor utilization.)
4. **Zoho webhook retry behavior:** Does Zoho retry failed webhook deliveries (5xx)? Affects idempotency implications. (Check Zoho docs or support ticket.)
5. **GKE autopilot vs standard:** Would Autopilot reduce P8 operational load? (Tradeoff: cost +$150/mo vs -2h setup; skip for POC.)

---

## References

- [cert-manager HTTP-01 Challenge Docs](https://cert-manager.io/docs/configuration/acme/http01/)
- [cert-manager on GKE Tutorial](https://cert-manager.io/docs/tutorials/getting-started-with-cert-manager-on-google-kubernetes-engine-using-lets-encrypt-for-ingress-ssl/)
- [GCE Load Balancer Health Check Docs](https://docs.cloud.google.com/load-balancing/docs/health-checks)
- [Prometheus Alerting Rules Docs](https://prometheus.io/docs/prometheus/latest/configuration/alerting_rules/)
- [OpenTelemetry Trace Context Propagation](https://opentelemetry.io/docs/concepts/context-propagation/)
- [OTel Collector DaemonSet vs Deployment](https://oneuptime.com/blog/post/2026-02-06-collector-daemonset-vs-deployment-kubernetes/view)
- [Grafana Provisioning as Code](https://grafana.com/docs/grafana/latest/administration/provisioning/)
- [Tempo vs Jaeger Trade-offs](https://last9.io/blog/grafana-tempo-vs-jaeger/)
- [Chaos Mesh Pod Fault Injection](https://chaos-mesh.org/docs/simulate-pod-chaos-on-kubernetes/)
- [Google SRE Incident Response](https://sre.google/workbook/incident-response/)
- [Cloud SQL Pricing](https://cloud.google.com/sql/pricing)
- [Cloud Storage Pricing](https://cloud.google.com/storage/pricing)

---

**Report compiled:** 2026-05-08 11:03 UTC  
**Confidence level:** High (14 decision points researched across official docs + 2024–2026 industry sources)  
**Next step:** Execute P8 per master plan; reference this report for each decision fork.
