# Observability profiles

Three drop-in profiles for wiring loomcycle into your existing observability stack. Pick the one matching where you already send telemetry:

| Profile | What it is | When to use it |
|---|---|---|
| **[`grafana-tempo/`](grafana-tempo/)** | Self-hosted Grafana + Tempo + Prometheus + Loki + OTEL Collector stack via docker-compose. Pre-built 10-panel dashboard. | You don't yet have an observability stack, or you prefer self-hosted open-source tooling. |
| **[`honeycomb/`](honeycomb/)** | Wiring for Honeycomb cloud + canonical query reference + board-authoring walkthrough. | You already use Honeycomb for distributed tracing. |
| **[`datadog/`](datadog/)** | Wiring for Datadog APM via the operator's existing Datadog agent + canonical query reference + dashboard-authoring walkthrough. | You already use Datadog APM. |

## Common architecture

All three profiles consume the same loomcycle OTEL substrate (shipped v0.10.0) and the same `/metrics` Prometheus endpoint (shipped via RFC observability-profiles.md, slice A.0). What differs is the downstream stack — same telemetry, three destinations.

```
loomcycle ──OTLP──▶ ┌──────────────────┐
                    │ Profile A: Grafana-Tempo (self-hosted)
                    │ Profile B: Honeycomb (cloud)
                    │ Profile C: Datadog (APM agent)
                    └──────────────────┘
loomcycle ──/metrics──▶ Prometheus (Profile A) or scraped from each profile's stack
```

The substrate exposes:

- **5 span types** — `loomcycle.run`, `loomcycle.iteration`, `loomcycle.provider.call`, `loomcycle.tool.call`, `loomcycle.mcp.call`
- **19 span attributes** — identity (`run_id`, `agent_id`, `user_id`, `agent_name`, `parent_agent_id`, `iteration`), provider (`provider`, `model`, `tier`, `effort`), tools (`tool`, `mcp_server`, `mcp_tool`), cost (`input_tokens`, `output_tokens`, `cache_read_tokens`), latency (`stop_reason`, `tool_is_error`, `queue_wait_ms`)
- **7 Prometheus metrics** — process resources (RSS, heap, goroutines), concurrency state (active, queued, per-user), build_info
- **OTEL Collector `spanmetrics` connector** (Profile A only) — aggregates spans into Prometheus histograms downstream; same metrics queryable in any Prometheus-compatible backend

## Choosing a profile

```
┌────────────────────────────────────────────────────────┐
│ Do you already send observability data somewhere?      │
├────────────────────────────────────────────────────────┤
│  No → Profile A (Grafana-Tempo, self-hosted)            │
│  Yes, Honeycomb → Profile B                             │
│  Yes, Datadog → Profile C                               │
│  Yes, something else (New Relic, Splunk, Dynatrace,    │
│                       Lightstep, etc.)                  │
│         → Use OTEL substrate primitives directly       │
│           (see Context.help observability for env vars) │
└────────────────────────────────────────────────────────┘
```

Each profile's README has the operator-facing five-minute quickstart + customisation guidance + production-hardening notes. The substrate itself (env vars, attribute schema, span types) is documented in `Context.help observability` available inside any loomcycle agent's conversation.

## Contributing

If you author a real Honeycomb board (Profile B) or Datadog dashboard (Profile C) against your own tenant, redacted JSON exports are welcome contributions — open a PR replacing the placeholder JSON. See each profile's README "Authoring..." section for the walkthrough.

## See also

- [`Context.help observability`](../../internal/help/builtin/observability.md) — OTEL substrate reference (env vars, attribute schema, sampling guidance)
- [`docs/CONFIGURATION.md`](../../docs/CONFIGURATION.md) — full operator config reference
- [`rfcs/observability-profiles.md`](../../doc-internal/rfcs/observability-profiles.md) — the design lock
- [`rfcs/observability-cookbook.md`](../../doc-internal/rfcs/observability-cookbook.md) — original design sketch (now superseded by the locked RFC)
