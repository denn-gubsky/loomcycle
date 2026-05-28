# Profile A — self-hosted observability stack

Five-minute path from "loomcycle running" to "Grafana dashboard showing per-run latency, queue depth, per-tenant fairness, error rate." Brings up the full open-source stack: Grafana + Tempo (traces) + Prometheus (metrics) + Loki (logs) + OTEL Collector + loomcycle + Postgres.

Locked at [`rfcs/observability-profiles.md`](../../../doc-internal/rfcs/observability-profiles.md) Profile A (Decision 2 — spans aggregated downstream via the OTEL Collector's `spanmetrics` connector; no in-process span processor).

## Quickstart

```sh
cd examples/observability/grafana-tempo
cp .env.example .env
# Edit .env: set at least one provider API key (ANTHROPIC_API_KEY /
# OPENAI_API_KEY / DEEPSEEK_API_KEY).
docker compose up -d
```

Then drive synthetic traffic so the dashboards populate:

```sh
./seed.sh
```

Open the UIs:

| Service | URL | Login |
|---|---|---|
| Loomcycle health | http://localhost:8787/healthz | (none) |
| Loomcycle Web UI | http://localhost:8787/ui?token=demo-token-change-me | (token in URL) |
| **Grafana** | **http://localhost:3001** | **anonymous Viewer, or `admin` / `admin`** |
| Prometheus | http://localhost:9090 | (none) |
| Tempo HTTP | http://localhost:3200 | (none — datasource only) |
| Loki HTTP | http://localhost:3100 | (none — datasource only) |

In Grafana, navigate to **Dashboards → Loomcycle → Loomcycle overview**. Within ~30 seconds of running `seed.sh`:

- Process-resource panels populate
- Concurrency panels show active + queued slots
- Per-agent run rate appears
- Latency p50 / p95 / p99 histograms populate
- Per-provider RTT histograms populate (one series per provider)
- Tool call rate breakdown populates
- The recent-traces panel shows the seeded runs (click any to open the full trace tree)

Tear down (including the Postgres + Tempo volumes):

```sh
docker compose down -v
```

## What's in the stack

```
[loomcycle:8787]
    │
    ├── OTLP /v1/traces ──→ [otel-collector:4318]
    │                          ├── batch
    │                          ├── → [tempo:4317] (storage)
    │                          └── spanmetrics connector
    │                                └── → [prometheus :8889 endpoint]
    │
    └── /metrics ──────────→ [prometheus:9090]  (substrate counters)
                                  │
                                  ↓
                            [grafana:3001]  (queries Prometheus + Tempo + Loki)
```

The **`spanmetrics` connector** is the architectural keystone — it aggregates incoming OTEL spans into Prometheus histograms (`loomcycle_calls_total`, `loomcycle_duration_seconds`) with operator-tunable label dimensions (`provider`, `model`, `tool`, `mcp_server`, `agent_name`, `tier`, `stop_reason`). Edit `otel-collector/config.yaml` to tune the dimension set or histogram buckets.

## Customising the stack

| Want to | Edit |
|---|---|
| Add/remove label dimensions on aggregated metrics | `otel-collector/config.yaml` → `connectors.spanmetrics.dimensions` |
| Change histogram bucket boundaries | `otel-collector/config.yaml` → `connectors.spanmetrics.histogram.explicit.buckets` |
| Scrape an additional target | `prometheus/prometheus.yml` → `scrape_configs` |
| Add a dashboard panel | Edit in Grafana UI → export JSON → save to `grafana/provisioning/dashboards/` |
| Increase Tempo retention | `tempo/tempo.yaml` → `compactor.compaction.block_retention` |
| Send logs to Loki from a service | Add `logging:` block in `compose.yaml` (driver: loki, endpoint: http://loki:3100/loki/api/v1/push) |

## Production deployment

This stack is sized for the **demo / local-dev** use case. For production:

- **Replace the demo bearer token.** Rotate `LOOMCYCLE_AUTH_TOKEN` to a real secret and update `prometheus/prometheus.yml`'s `authorization.credentials` accordingly. Better: load the token via Prometheus's secret-file pattern (`credentials_file: /run/secrets/loomcycle_token`).
- **Replace Tempo single-binary with sharded deployment.** Tempo's `compactor` + `ingester` + `distributor` + `querier` should be separate Deployments in K8s; see [Tempo's operator docs](https://grafana.com/docs/tempo/latest/operations/deployment/).
- **Replace Prometheus local TSDB with remote-write.** Send to Mimir / Thanos / Cortex / Grafana Cloud for long-term retention + cross-replica aggregation.
- **Replace anonymous-Viewer Grafana auth with SSO.** Grafana supports OAuth (GitHub/Google/Okta), LDAP, and SAML.
- **Pin all image tags** (already done in `compose.yaml`; revisit on each bump).
- **Add OTEL Collector pipelines for production:** TLS to Tempo, OTLP authentication, batch + retry processors, tail sampling for cost control.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Grafana dashboards show "No data" | Run `./seed.sh` to drive traffic. Datasources auto-provisioned but produce nothing until data arrives. |
| `loomcycle_calls_total` series absent in Prometheus | Check the OTEL Collector pod is healthy (`docker compose logs otel-collector`) — `spanmetrics` won't emit until it receives at least one batch. |
| `loomcycle` service won't start | Provider API key missing or invalid. Check `docker compose logs loomcycle` — the boot validator names the missing key. |
| Traces appear in Tempo but not in Grafana | Hit Grafana → Configuration → Datasources → Tempo → "Save & test". Should return "Data source is working." If not, network the Grafana ↔ Tempo path. |
| Prometheus shows `403 Forbidden` from `loomcycle` job | `LOOMCYCLE_AUTH_TOKEN` doesn't match `prometheus/prometheus.yml`'s `credentials`. Rotate consistently. |
| `docker compose up` hangs on Postgres | Postgres healthcheck takes ~10s on first start. Wait or run `docker compose up -d` so output stays clean. |
| Dashboards don't appear after `compose up` | Grafana provisioner runs on startup; restart Grafana (`docker compose restart grafana`) if you edited the JSON files. |

## See also

- [`../honeycomb/`](../honeycomb/) — same OTEL telemetry exported to Honeycomb cloud (Profile B)
- [`../datadog/`](../datadog/) — same telemetry exported to Datadog APM (Profile C)
- `Context.help observability` in the loomcycle binary — OTEL substrate reference
- [`../../../docs/CONFIGURATION.md`](../../../docs/CONFIGURATION.md) — full operator config reference
- [`rfcs/observability-profiles.md`](../../../doc-internal/rfcs/observability-profiles.md) — the design lock
