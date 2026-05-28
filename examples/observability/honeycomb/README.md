# Profile B — Honeycomb wiring

Send loomcycle's OTEL traces to Honeycomb cloud + reference queries for the most useful operator views.

Locked at [`rfcs/observability-profiles.md`](../../../doc-internal/rfcs/observability-profiles.md) Profile B.

## What this profile ships vs what you author

Honeycomb is SaaS — there's no docker stack to bring up. This profile ships:

- **Wiring** (`.env.example`, `loomcycle.yaml`) — environment variables for the Honeycomb OTLP endpoint + sampler ratio
- **Query reference** — the canonical Honeycomb queries (below) for the panels operators most often want
- **Derived column definitions** — pre-built derived-column expressions for per-agent error rate, per-user queue wait, parent → child span graph

What you author against your own Honeycomb account (since boards are tenant-bound):

- The Honeycomb board itself — the JSON `honeycomb-board.json` shipped here is a placeholder; building the board takes ~10 minutes against your dataset (walkthrough below)

If you author a board against your own tenant and want to contribute it back: PRs welcome. A redacted board export (no API keys, no internal hostnames) is contributable as `honeycomb-board.json`.

## Quickstart

```sh
cd examples/observability/honeycomb
cp .env.example .env
# Edit .env: HONEYCOMB_API_KEY + at least one provider API key.
# Then source the env when starting loomcycle:
source .env && loomcycle serve --config loomcycle.yaml
```

Spawn a test run:

```sh
curl -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
     -H "Content-Type: application/json" \
     http://localhost:8787/v1/runs \
     -d '{"agent":"demo","user_id":"alice","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hello"}]}]}'
```

Within ~30 seconds, the trace appears in Honeycomb under your dataset (default: `loomcycle-prod`). Open https://ui.honeycomb.io → select the dataset → "Recent traces."

## Canonical Honeycomb queries

Paste these directly into Honeycomb's query builder ("New Query"). Each query targets one of the 10 panels Profile A's Grafana dashboard shows; together they reproduce the same operator view inside Honeycomb's UI.

### 1. Run rate by agent

```
VISUALIZE: COUNT
GROUP BY:   loomcycle.agent_name
WHERE:      name = "loomcycle.run"
```

### 2. Run latency p50 / p95 / p99

```
VISUALIZE: HEATMAP(duration_ms)
WHERE:     name = "loomcycle.run"
```

Switch the visualization to **Percentile** in the query builder for the line-chart shape.

### 3. Provider RTT p95 by provider

```
VISUALIZE: P95(duration_ms)
GROUP BY:  loomcycle.provider
WHERE:     name = "loomcycle.provider.call"
```

### 4. Tool call rate by tool

```
VISUALIZE: COUNT
GROUP BY:  loomcycle.tool
WHERE:     name = "loomcycle.tool.call"
```

### 5. MCP call latency p95 by server

```
VISUALIZE: P95(duration_ms)
GROUP BY:  loomcycle.mcp_server
WHERE:     name = "loomcycle.mcp.call"
```

### 6. Error rate by provider

```
VISUALIZE: COUNT
GROUP BY:  loomcycle.provider
WHERE:     name = "loomcycle.provider.call" AND error exists
```

Pair with the unconditional `COUNT GROUP BY loomcycle.provider` to get the ratio.

### 7. Token spend by agent

```
VISUALIZE: SUM(loomcycle.input_tokens), SUM(loomcycle.output_tokens)
GROUP BY:  loomcycle.agent_name
WHERE:     name = "loomcycle.provider.call"
```

### 8. Recent slowest traces (top 20)

```
VISUALIZE: MAX(duration_ms)
GROUP BY:  trace.trace_id
WHERE:     name = "loomcycle.run"
ORDER BY:  MAX(duration_ms) DESC
LIMIT:     20
```

Click any row in the result table → "Trace" tab → see the full span tree.

## Derived columns (paste once, reuse forever)

In Honeycomb: Dataset → Schema → Derived Columns → New. Author each of these once per dataset; queries above can then reference them by short name.

### `per_agent_error_rate`

Expression:

```
IF(EXISTS($error), 1, 0)
```

Use as: `COUNT_WHERE(per_agent_error_rate, EQUALS(per_agent_error_rate, 1)) / COUNT GROUP BY loomcycle.agent_name`.

### `per_user_queue_wait_p95`

Already a span attribute (`loomcycle.queue_wait_ms`). Query directly:

```
VISUALIZE: P95(loomcycle.queue_wait_ms)
GROUP BY:  loomcycle.user_id
WHERE:     name = "loomcycle.run"
```

### `parent_child_span_graph`

Trace-view query (no derived column needed):

```
WHERE: loomcycle.parent_agent_id exists
```

Then open any matching trace → the span tree shows the full parent → child agent hierarchy.

## Authoring the board

Roughly 10 minutes against your account:

1. Run a load (e.g. Profile A's `seed.sh` against loomcycle, but pointed at your Honeycomb endpoint via the env vars above) so the dataset has data
2. In Honeycomb: Boards → New → "Loomcycle overview"
3. For each of the 8 queries above: build the query in the query builder, click "Add to Board," select the board, name the panel
4. Arrange the panels (drag-to-reorder)
5. Save the board
6. Export: Board menu → "Export as JSON"
7. Redact any tenant-specific fields (dataset ID is fine; team ID is fine; remove API keys if present)
8. (Optional) Open a PR replacing `honeycomb-board.json` with your authored board

## Customisation

| Want to | Change |
|---|---|
| Lower / raise sampling | `.env`: `LOOMCYCLE_OTEL_SAMPLER_RATIO=0.1` (10%; raise to 1.0 for debugging, lower for cost control) |
| Send to a different Honeycomb environment | `LOOMCYCLE_OTEL_SERVICE_NAME=loomcycle-staging` (Honeycomb routes by service name) |
| Add trace-tail / fast-fail sampling | Configure Honeycomb's [Sampling Proxy](https://docs.honeycomb.io/manage-data-volume/sampling/) sidecar; loomcycle's OTLP endpoint env var points at the proxy instead |

## See also

- [`../grafana-tempo/`](../grafana-tempo/) — self-hosted equivalent (Profile A)
- [`../datadog/`](../datadog/) — Datadog APM equivalent (Profile C)
- `Context.help observability` in the loomcycle binary — OTEL substrate reference
- [Honeycomb OTLP setup docs](https://docs.honeycomb.io/send-data/opentelemetry/)
