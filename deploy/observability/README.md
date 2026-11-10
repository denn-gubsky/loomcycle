# loomcycle observability: dashboards, alerts, collector

The dashboards, alert rules and collector configuration shipped with
loomcycle, versioned with the binary. Every `loomcycle_*` metric a
dashboard or rule here names is one `/metrics` serves: a Go test
(`TestObservabilityAssets_EveryMetricIsEmitted` in `internal/api/http`)
fails the build when one names a metric that is no longer emitted.

| File | What it is |
|---|---|
| `grafana/dashboards/loomcycle-overview.json` | Runs, failure rate, p95 duration, active runs and the queue; tokens and cost by tenant and model; provider outcomes and the rate-limited ratio; retries and fallbacks; tool calls; database pool; memory consolidation lag; trace-export drops. |
| `grafana/dashboards/loomcycle-genai.json` | Model calls: the OpenTelemetry GenAI semantic-convention histograms (`gen_ai.client.operation.duration`, `gen_ai.client.token.usage`, OTLP only) beside loomcycle's own time-to-first-token and model-call series. |
| `grafana/provisioning/dashboards.yaml` | Grafana provisioning for the two dashboards. |
| `prometheus/prometheus.yml` | Scrapes `/metrics` with a `metrics:read` token from a file (`credentials_file`), and loads the rules. |
| `prometheus/rules/loomcycle-alerts.yml` | Run failure rate, provider rate-limited ratio, queue saturated for 5 minutes, database pool waits, consolidation lag, dropped spans, token budget at 90 %. |
| `alloy/config.alloy` | Grafana Alloy receiving OTLP from loomcycle behind a bearer check, tail-sampling traces (keep every failed run and every run slower than 30 s, 10 % of the rest) into Tempo, and forwarding OTLP metrics to Prometheus. |

## Metrics: /metrics or OTLP

`/metrics` carries the substrate (process, concurrency, provider lanes,
model throughput) and the in-process instruments: runs, tokens, cost,
provider requests, model calls, tool calls, database pools, consolidation
and token budgets. They are counted inside loomcycle, so they are right at
any trace sampling ratio, and a scrape reads memory only.

The same instruments go out over OTLP with `OTEL_METRICS_EXPORTER=otlp`
(and an endpoint): useful when the collector is the only thing that can
reach loomcycle. The GenAI convention histograms exist only on that path.
See `docs/CONFIGURATION.md`, "Metrics", for the catalogue, the labels and the
cardinality limit.

## Prometheus

The scrape set-up below requires the `metrics:read` scope and the optional
`LOOMCYCLE_METRICS_LISTEN` listener, which ship in the same release as these
files. On a loomcycle without them, `/metrics` answers only a
`substrate:admin` token (or `LOOMCYCLE_AUTH_TOKEN`): that works, but it puts
a credential that can mint tokens and pause the runtime in a file on the
monitoring host, so it is not recommended — upgrade instead.

1. Mint a scrape token that reaches `/metrics` and nothing else:

   ```sh
   curl -fsS -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" -H "Content-Type: application/json" \
     -d '{"op":"create","name":"prometheus","tenant_id":"ops","scopes":["metrics:read"]}' \
     https://loomcycle.example/v1/_operatortokendef
   ```

   Write the returned `token` into `/etc/prometheus/secrets/loomcycle.token`.
   Prometheus reads it on every scrape, so a rotation needs no restart. With
   `metrics:read` available, never scrape with an admin token. Or set
   `LOOMCYCLE_METRICS_LISTEN` and scrape that listener with no token (it is
   unauthenticated and leaves out the per-user series: keep it on a network
   only the scraper reaches).

2. Mount `prometheus/prometheus.yml` and `prometheus/rules/` into Prometheus
   (`/etc/prometheus/prometheus.yml`, `/etc/prometheus/rules/`).

## Grafana

Mount `grafana/dashboards/` at `/var/lib/grafana/dashboards/loomcycle` and
`grafana/provisioning/dashboards.yaml` under
`/etc/grafana/provisioning/dashboards/`. The dashboards pick the Prometheus
data source through their `datasource` variable. Prometheus with exemplar
storage (`--enable-feature=exemplar-storage`) and a Tempo data source turn the
OTLP path's exemplars into links from a latency point to its trace.

## Alloy

`alloy/config.alloy` reads `LOOMCYCLE_OTLP_TOKEN`, `TEMPO_ENDPOINT` and
`PROMETHEUS_REMOTE_WRITE_URL` from its environment. Point loomcycle at it
with `OTEL_EXPORTER_OTLP_ENDPOINT=http://alloy:4318` and the collector header
in a file (`LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE`, holding
`Authorization=Bearer%20<token>`). Keep loomcycle's sampler at
`parentbased_always_on` so Alloy sees every trace it is to choose from. The
remote-write target must accept writes (Prometheus:
`--web.enable-remote-write-receiver`).

## A demo stack

`examples/observability/grafana-tempo/` runs loomcycle with Postgres, an
OpenTelemetry Collector, Tempo, Prometheus, Loki and Grafana in one
`docker compose up`; its Grafana also loads the dashboards in this directory.
