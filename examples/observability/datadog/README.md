# Profile C — Datadog APM wiring

Send loomcycle's OTEL traces to Datadog APM via the operator's existing Datadog agent + canonical query reference for the most useful operator views.

Locked at [`rfcs/observability-profiles.md`](../../../doc-internal/rfcs/observability-profiles.md) Profile C.

## What this profile ships vs what you author

Datadog APM is SaaS + agent-based (loomcycle sends OTLP to the local Datadog agent; the agent forwards to Datadog's backend). This profile ships:

- **Wiring** (`.env.example`, `loomcycle.yaml`) — env vars pointing OTLP at the operator's existing Datadog agent
- **Query reference** — canonical Datadog APM queries for the panels operators most often want
- **Dashboard placeholder** (`datadog-dashboard.json`) — author against your account per the walkthrough below

Like Profile B (Honeycomb), the dashboard JSON is tenant-bound (org-specific service tags, custom metric setup, retention windows). You author the dashboard once against your Datadog org and optionally contribute the redacted export back.

## Prerequisites

You need an existing Datadog agent reachable from where loomcycle runs. Two common shapes:

- **K8s:** the Datadog agent runs as a DaemonSet; `DD_AGENT_HOST` is the agent service's ClusterIP / hostname
- **VM:** the Datadog agent runs on the same host as loomcycle; `DD_AGENT_HOST=127.0.0.1`

The agent must have OTLP receiver enabled. In the Datadog agent's `datadog.yaml`:

```yaml
otlp_config:
  receiver:
    protocols:
      http:
        endpoint: 0.0.0.0:4318
```

Restart the agent after editing.

## Quickstart

```sh
cd examples/observability/datadog
cp .env.example .env
# Edit .env: DD_AGENT_HOST + at least one provider API key.
source .env && loomcycle serve --config loomcycle.yaml
```

Spawn a test run:

```sh
curl -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
     -H "Content-Type: application/json" \
     http://localhost:8787/v1/runs \
     -d '{"agent":"demo","user_id":"alice","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hello"}]}]}'
```

In Datadog APM (https://app.datadoghq.com/apm/services): within ~30 seconds you'll see a new service `loomcycle` (or whatever `LOOMCYCLE_OTEL_SERVICE_NAME` resolves to). Click it for the auto-generated service-overview panel.

## Canonical Datadog APM queries

Each query produces one of the 10 panels Profile A's Grafana dashboard shows. Paste these into Datadog's query editor; tags map directly to OTEL span attributes (Datadog automatically translates `loomcycle.provider` → tag `loomcycle.provider`).

### 1. Run rate by agent

```
trace.loomcycle.run.hits{service:loomcycle} by {loomcycle.agent_name}.as_rate()
```

### 2. Run latency p95

```
trace.loomcycle.run.duration{service:loomcycle}.p95()
```

Switch the aggregation to `p50` / `p99` for the percentile breakdown.

### 3. Provider RTT p95 by provider

```
trace.loomcycle.provider.call.duration{service:loomcycle} by {loomcycle.provider}.p95()
```

### 4. Tool call rate by tool

```
trace.loomcycle.tool.call.hits{service:loomcycle} by {loomcycle.tool}.as_rate()
```

### 5. MCP call latency p95 by server

```
trace.loomcycle.mcp.call.duration{service:loomcycle} by {loomcycle.mcp_server}.p95()
```

### 6. Error rate by provider

```
(trace.loomcycle.provider.call.errors{service:loomcycle} by {loomcycle.provider}.as_rate()) /
(trace.loomcycle.provider.call.hits{service:loomcycle} by {loomcycle.provider}.as_rate())
```

### 7. Token spend by agent

Token counts are span attributes; Datadog exposes them via custom metrics or via Trace Analytics. Easiest: enable **APM Trace Metrics** for the `loomcycle.input_tokens` attribute, then:

```
trace.loomcycle.provider.call.loomcycle.input_tokens{service:loomcycle} by {loomcycle.agent_name}.as_count()
```

### 8. Recent slowest traces

Datadog APM → Traces → search `service:loomcycle env:prod` → sort by Duration DESC → top 20. Click any row for the full span tree + flame graph.

## Custom metrics — enable per-tag tracking

To make span attributes graphable as Datadog metrics (panel 7 above), enable per-tag tracking in your Datadog APM config:

Settings → APM → Integrations → Span Tag Statistics → Add:

- `loomcycle.provider`
- `loomcycle.tool`
- `loomcycle.agent_name`
- `loomcycle.mcp_server`
- `loomcycle.tier`

This adds ~minimal extra cost (per-tag aggregates are pre-computed) and unlocks the spanmetrics-equivalent queries Datadog ships natively.

## Authoring the dashboard

Roughly 15 minutes against your account:

1. Run a load against loomcycle (e.g. Profile A's `seed.sh` pointed at this loomcycle instance)
2. Datadog → Dashboards → New Dashboard → "Loomcycle overview"
3. For each of the 8 queries above: add a Timeseries panel, paste the query, name it
4. Arrange widgets (drag-to-resize)
5. Save the dashboard
6. Export: Dashboard menu → "Export as JSON" or via the API: `GET /api/v1/dashboard/<id>`
7. Redact tenant-specific fields (your `dd_org_id`, internal hostnames, etc.)
8. (Optional) Open a PR replacing `datadog-dashboard.json` with your authored export

## Customisation

| Want to | Change |
|---|---|
| Lower / raise sampling | `.env`: `LOOMCYCLE_OTEL_SAMPLER_RATIO=0.1` |
| Send to a different Datadog environment | `LOOMCYCLE_OTEL_SERVICE_NAME=loomcycle-staging` (creates a separate APM service) |
| Add APM `env:` tag | `LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS=dd-env=production` (Datadog agent reads this) |
| Use Datadog DogStatsD instead of OTLP | Not in scope — loomcycle's OTLP path is the canonical one. DogStatsD would require a Datadog-specific exporter that violates the substrate stance |

## See also

- [`../grafana-tempo/`](../grafana-tempo/) — self-hosted equivalent (Profile A)
- [`../honeycomb/`](../honeycomb/) — Honeycomb cloud equivalent (Profile B)
- `Context.help observability` in the loomcycle binary — OTEL substrate reference
- [Datadog OTLP ingestion docs](https://docs.datadoghq.com/opentelemetry/otlp_ingest_in_the_agent/)
