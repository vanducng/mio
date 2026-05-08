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
reference that label, never `channel`.

## Goal & Outcome

**Goal:** A user types in Zoho Cliq, sees an echo reply in the same thread, all from in-cluster — and an operator can trace one message end-to-end through Grafana / Prometheus / logs.

**Outcome:** Demo-able. Cliq → Cloudflare/Cloud LB → Ingress → gateway → JetStream → echo-consumer → JetStream → gateway-sender → Cliq REST → user's thread.

## Files

- **Create:**
  - `deploy/gke/dns-and-tls.sh` — cert-manager + Cloud DNS / Cloudflare records
  - `deploy/gke/observability/values.yaml` — kube-prometheus-stack overrides for the mio dashboards
  - `deploy/gke/observability/grafana-dashboards/mio-overview.json`
  - `deploy/gke/observability/grafana-dashboards/mio-cliq.json`
  - `deploy/gke/observability/alerts.yaml` — PrometheusRule manifests
  - `deploy/gke/observability/otel-collector.yaml` — Tempo/Jaeger backend choice
  - `docs/runbooks/cliq-webhook-down.md`
  - `docs/runbooks/jetstream-degraded.md`
  - `docs/runbooks/outbound-rate-limit.md`
- **Modify:**
  - `deploy/charts/mio-gateway/values.yaml` — Ingress hostname, TLS issuer
  - `deploy/charts/mio-gateway/templates/ingress.yaml` — annotations for cert-manager + GCE LB

## Observability stack

- **Metrics**: Prometheus via kube-prometheus-stack; ServiceMonitors from charts.
- **Traces**: OTel Collector (DaemonSet) → Tempo (in-cluster) for POC. SDK already emits `traceparent` through NATS headers (P2). Sampling: 100% for POC, drop to head-based 10% later.
- **Logs**: stdout (JSON) → Cloud Logging (default GKE). Structured fields: `trace_id`, `span_id`, `tenant_id`, `account_id`, `channel_type`, `conversation_id`, `mio_message_id`. Never log `text` body in info; debug only, behind a flag.

All three telemetry signals correlate via `trace_id`. A user-reported "my
message didn't go through" → operator clicks the trace → sees gateway
inbound span, JS publish span, AI consumer span, JS publish span,
gateway sender span, Cliq REST span; logs at each correlate by `trace_id`.

## Alerts (PrometheusRule)

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

  - alert: MioGatewayOutboundFailureSpike
    expr: rate(mio_gateway_outbound_total{outcome="error"}[5m]) > 0.05
    for: 5m
    labels: { severity: page }

  - alert: MioRateLimitBucketLeak
    expr: mio_gateway_ratelimit_buckets_active > 1000
    for: 15m
    labels: { severity: warn }

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

- name: mio.sink
  rules:
  - alert: MioSinkInflightFilesStuck
    expr: mio_sink_gcs_inflight_files > 10
    for: 30m
    labels: { severity: warn }
```

All metric expressions key on `channel_type` (the registry slug), never
`channel`. If a query returns empty, suspect a label-name mismatch first.

## Steps

1. Provision public DNS for `mio.<your-domain>` pointing at the Ingress; cert-manager issues TLS via Let's Encrypt staging first → prod once happy.
2. Update Cliq incoming webhook target to `https://mio.<your-domain>/webhooks/zoho-cliq`.
3. Install kube-prometheus-stack (or use existing); apply ServiceMonitors from charts; import the two dashboards (overview + cliq-specific).
4. Apply `alerts.yaml` PrometheusRule; verify each alert is loaded (`amtool alert query`).
5. Install OTel Collector DaemonSet → Tempo backend (or Jaeger if simpler). Verify trace from a synthetic curl shows up end-to-end.
6. Smoke test: type "ping" in Cliq, confirm echo within 5s, screenshot the trace in Grafana (Tempo data source).
7. Load test: 100 messages in 10s; observe metrics, verify p99 stays <500ms inbound, <2s outbound.
8. Failure injection:
   - Kill one NATS pod, watch quorum hold; messages keep flowing.
   - Kill one gateway pod, watch second replica handle traffic; redeploy.
   - Inject 4xx on Cliq REST (toxic-proxy) → confirm `MioGatewayOutboundFailureSpike` fires.
   - Burst one account → confirm fairness for another account stays <2s p99.
9. Two runbooks merged with concrete kubectl/nats/psql commands and decision trees.

## Success Criteria

- [ ] User-visible echo reply in Cliq from a real webhook hitting the cluster
- [ ] One message's trace visible end-to-end in Grafana/Tempo (gateway inbound → JS publish → AI consumer → JS publish → gateway sender → Cliq REST)
- [ ] Logs at every span carry `trace_id` and `account_id`; no `text` body in info-level
- [ ] All five Prometheus alerts fire on injected failure (latency, bad-sig spike, outbound failure, JS lag, replica loss)
- [ ] Killing one NATS pod → cluster stays healthy, quorum holds, messages keep flowing
- [ ] 100-message burst: p99 inbound <500ms, p99 outbound <2s
- [ ] Account-fairness load test passes (P5 success criterion verified in cluster)
- [ ] GCS bucket has the day's archived payloads with `channel_type=zoho_cliq/date=YYYY-MM-DD/` partitions
- [ ] Three runbooks merged: `cliq-webhook-down.md`, `jetstream-degraded.md`, `outbound-rate-limit.md`

## Risks

- **TLS cert provisioning** — Let's Encrypt rate limits; use staging first, swap to prod once flow is stable.
- **Cloud LB health check tuning** — gateway must respond <10s on `/readyz`; default health checks too aggressive in some configs.
- **Cliq webhook IP allowlist** — if Cliq publishes from a known range, the Ingress should restrict; otherwise rely on signature verification only (already enforced gateway-side).
- **Trace propagation gaps** — if any service forgets to inject OTel context, trace breaks; fix the gap, not the dashboard.
- **Dashboard label drift** — every panel + alert keys on `channel_type`; a chart that uses `channel` silently returns empty. Lint with `promtool` against captured queries before merging.
- **Cost spikes from observability** — Tempo + kube-prometheus-stack on `e2-small` nodes can OOM; allocate one `e2-standard-2` for observability, taint it.

## Out (deferred to P9)

- Second channel adapter — separate phase. POC ships at this milestone with one channel.
