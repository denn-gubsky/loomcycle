---
name: observability
description: OpenTelemetry distributed traces — Jaeger / Grafana Tempo / Honeycomb setup, attribute reference, sampling guidance.
---
Loomcycle v0.10.0 emits OpenTelemetry distributed traces for every
agent run. Operators see a span tree per run — top-level `loomcycle.run`
→ per-iteration `loomcycle.iteration` → per-provider-call
`loomcycle.provider.call` and per-tool-call `loomcycle.tool.call` (with
nested `loomcycle.mcp.call` when a tool is an MCP server). Sub-agent
spawns nest as child spans of the parent's iteration, so a multi-level
`cv-batch-adapter → cv-adapter → ...` tree shows the whole hierarchy
in one trace view.

This is **off by default**. When `LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT`
is unset, the entire telemetry subsystem is a no-op — no goroutines,
no exporter buffers, no measurable overhead. Operators opt in by
pointing at their OTLP/HTTP collector.

## What spans capture

Every span carries the same identity attribute set so traces correlate
cleanly with the loomcycle transcript views (`/ui/agents/<id>`):

- `loomcycle.run_id`, `loomcycle.agent_id`, `loomcycle.agent_name`
- `loomcycle.user_id`, `loomcycle.parent_agent_id` (sub-runs)

Provider spans add: `loomcycle.provider` (`anthropic` | `openai` |
`deepseek` | `gemini` | `ollama` | `ollama-local`), `loomcycle.model`,
`loomcycle.tier`, `loomcycle.effort`.

Tool spans add: `loomcycle.tool` (the dispatched name).

MCP spans add: `loomcycle.mcp_server`, `loomcycle.mcp_tool`.

The run span gets closing attributes when the run finishes:
`loomcycle.input_tokens`, `loomcycle.output_tokens`,
`loomcycle.cache_read_tokens`, `loomcycle.stop_reason`. Errors set the
span's status to Error with the error message — Jaeger surfaces these
as red span markers in the trace timeline.

## What spans deliberately do NOT capture

By design:

- **No transcript bodies.** No system prompts, user prompts, model
  text, tool input arguments, tool result text. The transcript view in
  the Web UI is the authoritative record of "what did the agent see /
  say"; spans show only "what shape did the call take, how long did it
  cost, did it error."
- **No secrets.** API keys, bearer tokens, OAuth secrets, header
  values never reach span attributes. The v1.x per-run credentials
  map (see `per-run-credentials` topic) inherits this
  posture — credential values never appear in span attributes,
  span events, or span resource attributes.
- **No PII.** `loomcycle.user_id` is the operator's opaque user
  identifier — same shape as the transcript event payload — but no
  user-content text crosses the span boundary.

This posture is load-bearing: telemetry endpoints (Honeycomb,
DataDog, etc.) have different trust postures than the loomcycle
bearer auth. Keeping secrets and bodies out of attributes means
opting into tracing doesn't widen the secret surface.

## Walkthrough 1 — Local Jaeger via Docker

The fastest way to see your first trace. Jaeger's `all-in-one` image
ships an OTLP/HTTP receiver on port 4318 and a UI on port 16686:

```sh
docker run --rm -d --name jaeger \
  -p 16686:16686 -p 4318:4318 \
  jaegertracing/all-in-one:latest

export LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
./bin/loomcycle --config loomcycle.example.yaml
```

Fire any agent run (curl `/v1/runs` or click around the Web UI). Open
`http://localhost:16686`, pick `loomcycle` from the Service dropdown,
hit "Find Traces." Each agent run appears as one row. Click into a
trace to see the run → iteration → provider/tool tree.

Filter expressions:

- `loomcycle.run_id=run_abc...` — pin to one run
- `loomcycle.user_id=user_42` — all runs by one user
- `loomcycle.provider=anthropic` — all Anthropic-routed calls
- `loomcycle.tool=Bash` — all Bash invocations
- Tag filter `status=error` — failed spans only

Tear down: `docker stop jaeger`.

## Walkthrough 2 — Grafana Tempo

Tempo is the production-grade alternative to Jaeger — same OTLP/HTTP
ingestion, plus Grafana's panels and per-trace correlation with
metrics and logs. Minimum `docker-compose.yml`:

```yaml
services:
  tempo:
    image: grafana/tempo:latest
    command: ["-config.file=/etc/tempo.yaml"]
    volumes: ["./tempo.yaml:/etc/tempo.yaml"]
    ports: ["4318:4318", "3200:3200"]
  grafana:
    image: grafana/grafana:latest
    ports: ["3000:3000"]
    environment:
      - GF_AUTH_ANONYMOUS_ENABLED=true
      - GF_AUTH_ANONYMOUS_ORG_ROLE=Admin
```

Tempo config (`tempo.yaml`):

```yaml
server: { http_listen_port: 3200 }
distributor:
  receivers:
    otlp:
      protocols:
        http: { endpoint: 0.0.0.0:4318 }
storage:
  trace: { backend: local, local: { path: /tmp/tempo/blocks } }
```

Point loomcycle at the same `http://localhost:4318` endpoint. In
Grafana (port 3000): Configuration → Data sources → Add Tempo →
`http://tempo:3200` → Save & test. Then Explore → Tempo → Search by
service `loomcycle`.

The advantage over Jaeger: per-trace metrics + logs correlation via
Grafana's link-to-logs feature. The disadvantage: more moving parts
to operate.

## Walkthrough 3 — Honeycomb

The hosted option. Sign up at honeycomb.io for the free tier (20M
events/month, which covers a JobEmber-class workload at default
sampling).

```sh
export LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT=https://api.honeycomb.io
export LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS=x-honeycomb-team=$HONEYCOMB_API_KEY
export LOOMCYCLE_OTEL_SERVICE_NAME=loomcycle-prod
./bin/loomcycle --config loomcycle.yaml
```

Honeycomb's BubbleUp + heatmap views are stronger than Jaeger's
timeline for "what's slow?" diagnosis. Their Triggers feature can fire
a webhook when a latency p99 exceeds a threshold — useful for
"alert me when a provider degrades."

The `x-honeycomb-team` header carries your API key. Treat it as a
secret; LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS supports the same
comma-separated `key=value,key2=value2` shape so multiple auth or
routing headers compose:

```sh
LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS=x-honeycomb-team=$KEY,x-honeycomb-dataset=prod
```

## Walkthrough 4 — DataDog APM

DataDog accepts OTLP/HTTP when the local Agent is configured with
the `otlp_config` block. Loomcycle then points at the local Agent
(default 127.0.0.1:4318) — same env var as Jaeger:

```sh
export LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318
```

DataDog's UI groups by `service.name`, `service.version`, and
`deployment.environment`. Loomcycle's bootstrap sets `service.name`
from `LOOMCYCLE_OTEL_SERVICE_NAME` (default `loomcycle`) and
`service.version` from the binary's `buildVersion` automatically.
Set `OTEL_RESOURCE_ATTRIBUTES=deployment.environment=production` in
the loomcycle process env to populate the env attribute (loomcycle
respects the OTEL SDK's standard resource-attribute env var).

## Sampling guidance

The default `LOOMCYCLE_OTEL_TRACES_SAMPLER_RATIO=1.0` captures every
span. At a JobEmber-class workload (~200 runs/hour × 8 iterations ×
4 tools = 8K spans/hour) this is fine — OTLP/HTTP at ~200 bytes per
span is ~1.5MB/hour, ~1GB/month. Most operators can leave it at 1.0.

Reduce when:

- Your collector's storage is bounded (Tempo local-disk; Honeycomb's
  20M-events/month free tier — set 0.05 to fit a high-volume workload)
- You're shipping to a SaaS APM that bills per-event (DataDog at
  $1.27/M, Honeycomb beyond free tier)

The sampler is `parentbased_traceidratio`: when a parent span is
sampled, all children are also sampled (so a trace stays whole). The
ratio applies only to untraced roots. Setting `RATIO=0.1` keeps every
tenth run end-to-end, drops the rest entirely — never partial traces.

## Filtering best practices

Three filter expressions that pay for themselves on the first
incident:

1. **Slow runs**: `service:loomcycle duration>30s` — surface runs
   that took longer than the operator's SLO threshold.
2. **Per-user noise**: group-by `loomcycle.user_id`, sort by count —
   identify which user is driving the run volume.
3. **Provider degradation**: `loomcycle.provider=anthropic status=error
   span_name=loomcycle.provider.call` — Anthropic-specific errors
   over the last hour. Cross-reference with the resolver matrix
   (`GET /v1/_resolve/matrix`) to verify the matrix marked anthropic
   stalled.

## What spans tell you that the transcript doesn't

The transcript (visible at `/ui/agents/<agent_id>`) is the canonical
record of agent content. The spans complement, not replace, it:

| Question | Look at |
|---|---|
| What did the agent say? | Transcript (`/ui/agents/<id>`) |
| What did the model emit? | Transcript (text + tool_call events) |
| What inputs did the tool get? | Transcript (tool_call.input) |
| How long did this run take? | Spans (root span duration) |
| Which iteration was slow? | Spans (per-iteration timing) |
| Which provider call took 8s? | Spans (provider.call duration) |
| Did any tool error? | Spans (Error-status leaf spans) |
| How many cache_read_tokens? | Spans (run-span attribute) |
| Why is p99 latency creeping up? | Spans + your APM's stats UI |

Use the transcript for "what happened," the spans for "how fast,
where, why slow."

## Troubleshooting

**Symptom**: `otel: tracer enabled` in the loomcycle log but no
spans in Jaeger/Tempo/Honeycomb.

- Run `curl -v http://localhost:4318/v1/traces -X POST -d '{}'` from
  the loomcycle host. Expect a 200 or 400 — anything else (connection
  refused, DNS failure, certificate error) is a connectivity problem.
  Loomcycle's BatchSpanProcessor logs the error to stderr periodically.
- For HTTPS collectors (Honeycomb), ensure
  `LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT` includes the `https://`
  scheme; loomcycle's bootstrap detects `http://` and opts into
  `WithInsecure` automatically.

**Symptom**: traces show up but `service.name=unknown_service`.

- Check `LOOMCYCLE_OTEL_SERVICE_NAME` is set. The default
  `"loomcycle"` should always populate.

**Symptom**: high collector latency under load.

- Drop the sampler: `LOOMCYCLE_OTEL_TRACES_SAMPLER_RATIO=0.1` or 0.01.
- Check the BSP isn't dropping (the SDK logs to stderr when the
  internal queue overflows).

**Symptom**: trace tree breaks across replicas (sub-runs spawn on a
different loomcycle instance).

- This is expected in v0.10.0 — multi-replica HA ships in a later
  v0.10.x slice. Until then, sub-agent runs that span replicas show
  as separate disconnected traces.

## Bundled profiles (v1.x)

Three drop-in profile directories live under `examples/observability/`
in the loomcycle repo. Pick the one matching your existing stack;
each ships its own README with a five-minute quickstart.

- **`examples/observability/grafana-tempo/`** — self-hosted Grafana
  + Tempo + Prometheus + Loki + OTEL Collector stack via docker-
  compose. Pre-built 10-panel dashboard auto-provisioned on Grafana
  startup. The OTEL Collector's `spanmetrics` connector aggregates
  loomcycle spans into Prometheus histograms downstream — no
  in-process span processor; the substrate stays clean.
- **`examples/observability/honeycomb/`** — wiring + 8 canonical
  Honeycomb queries + 3 derived-column definitions + 10-minute
  board-authoring walkthrough.
- **`examples/observability/datadog/`** — wiring (via the
  operator's existing Datadog agent OTLP receiver) + 8 canonical
  Datadog APM queries + per-tag-tracking setup + 15-minute
  dashboard-authoring walkthrough.

Plus a `/metrics` Prometheus text-format endpoint on loomcycle
itself (added v1.x, slice A.0) exposing substrate counters (process
RSS, heap, goroutines, concurrency state, build_info) — scraped by
Profile A's Prometheus and reusable by any operator-existing
Prometheus deployment regardless of which profile they pick.

## Process-resource metrics (diagnosing host pressure)

Distinct from the OTEL traces above: loomcycle has a built-in **process-resource
sampler** that records its own RSS / heap / goroutines / concurrency state — and,
optionally, **system-wide CPU + memory** — to a `process_samples` table, exposed
at `GET /v1/_metrics/*` (bearer-authed). This is the surface for capacity planning
and for diagnosing "the box got slow" incidents.

- **Off by default.** Enable with `LOOMCYCLE_METRICS_ENABLED=1` (cadence
  `LOOMCYCLE_METRICS_SAMPLE_INTERVAL_MS`, default 5 s; sleeps when no run is
  active — no DB writes / `/proc` reads while idle).
- **System-wide CPU% + memory stay OFF even when metrics are on.** Set
  `LOOMCYCLE_METRICS_COLLECT_SYSTEM=1` to also read `/proc/stat` + `/proc/meminfo`
  (Linux only; silently ignored on macOS/Windows). Without it the
  `system_cpu_pct_x100` / `system_mem_*_mb` columns stay NULL — so a runaway
  driven by *co-tenant* host pressure (a hypervisor balloon / ZFS ARC eating RAM
  in a shared VM) is invisible to loomcycle's own metrics.
- **The sampler is not a substitute for a host monitor.** It samples only while a
  run is active and only what the kernel exposes to its own process — pressure
  that arrives *between* runs is missed. For always-on host telemetry also run an
  external monitor (node_exporter / `vmstat` / your cloud's host metrics).
- Endpoints: `GET /v1/_metrics/samples?since=&until=&limit=`,
  `GET /v1/_metrics/runs/{run_id}` (peak/mean RSS + max CPU% for the run window),
  `GET /v1/_metrics/summary?period=1h|24h|7d`. The Web UI's Activity tab renders
  these live.

## Related topics

- `pause-resume-snapshot` — runtime quiesce + snapshot lifecycle.
  Pause + resume don't link spans across replicas; each pause-induced
  cancellation closes its run span at the cancel boundary.
- `system-channels` — operator-declared `_system/*` channels.
  Channel-publish events don't currently emit spans; they're an
  operator-side concern handled by the bus.

## Reference: env var summary

| Variable | Default | Purpose |
|---|---|---|
| `LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT` | empty | Empty disables tracing. Otherwise OTLP/HTTP endpoint (host:port or http(s)://host:port). |
| `LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS` | empty | Comma-separated `key=value` list for collector auth. Whitespace trimmed. |
| `LOOMCYCLE_OTEL_SERVICE_NAME` | `loomcycle` | `service.name` resource attribute. |
| `LOOMCYCLE_OTEL_TRACES_SAMPLER_RATIO` | `1.0` | Head-based sampling ratio. Clamped to `[0.0, 1.0]`. |
