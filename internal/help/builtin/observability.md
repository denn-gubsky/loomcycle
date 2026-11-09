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

This is **off by default**. When `OTEL_EXPORTER_OTLP_ENDPOINT` is
unset, the entire telemetry subsystem is a no-op — no goroutines, no
exporter buffers, no measurable overhead. Operators opt in by pointing
at their OTLP collector. Configuration is the standard OpenTelemetry
`OTEL_*` environment (see the reference at the end), over OTLP/HTTP
(default) or OTLP/gRPC.

## What spans capture

The span tree of one run:

```
loomcycle.run                      one per run (sub-runs nest under the
 └ loomcycle.iteration             parent's iteration); one per turn
    ├ loomcycle.model.call         the whole streamed model call
    │  └ loomcycle.provider.call   one per HTTP attempt (retries are siblings)
    └ loomcycle.tool.call          one per tool call
       └ loomcycle.mcp.call        when the tool is on an MCP server
```

plus `loomcycle.memory.search` and `loomcycle.memory.consolidate` for
memory work.

**Every span** a run opens carries `loomcycle.tenant` (omitted for work
that belongs to no tenant). Child spans do not inherit attributes, so it is
stamped on each one: filter any span kind by tenant.

**Run span** (`loomcycle.run`): `loomcycle.run_id`, `loomcycle.agent_id`,
`loomcycle.agent_name`, `loomcycle.user_hash` (see below),
`loomcycle.parent_agent_id` (sub-runs), `loomcycle.queue_wait_ms`; at the
end `loomcycle.input_tokens`, `loomcycle.output_tokens`,
`loomcycle.cache_read_tokens`, `loomcycle.stop_reason`. GenAI names:
`gen_ai.operation.name=invoke_agent`, `gen_ai.agent.name`,
`gen_ai.agent.id`, `gen_ai.conversation.id` (the chat/session id).

**Model-call span** (`loomcycle.model.call`): from just before the provider
is called until its stream ends — the done event, an error, or a cancel.
`loomcycle.provider`, `loomcycle.model`, `loomcycle.effort`,
`loomcycle.ttft_ms` (time to the first streamed token), `loomcycle.stop_reason`,
usage (`loomcycle.input_tokens`, `loomcycle.output_tokens`,
`loomcycle.cache_read_tokens`, `loomcycle.cache_write_tokens`),
`loomcycle.cost_usd` (when the call is priced in US dollars), and
`loomcycle.fallback_from` (`provider/model`) on the first call after a
provider fallback. GenAI names: `gen_ai.operation.name=chat`,
`gen_ai.provider.name` (`anthropic`, `openai`, `deepseek`, `gcp.gemini`,
`aws.bedrock`, `ollama`, else the provider id), `gen_ai.request.model`,
`gen_ai.request.temperature` / `top_p` / `max_tokens` when set,
`gen_ai.response.model`, `gen_ai.response.finish_reasons`,
`gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`,
`gen_ai.usage.cache_read.input_tokens`,
`gen_ai.usage.cache_creation.input_tokens`.

**Provider attempt span** (`loomcycle.provider.call`): one HTTP request.
It ends when the response headers arrive, so its duration is time to
headers; the model-call span above is the whole generation.
`loomcycle.provider`, `loomcycle.model`, `loomcycle.effort`,
`http.response.status_code`, `loomcycle.attempt` (1, 2, … within the call),
`loomcycle.retry_after_ms` (on a retry: how long it waited first), and on
failure `error.type`: `rate_limited` (429), `overloaded` (529 or 503),
`timeout`, `network`, `cancelled`, `client_error` or `server_error`. Find
rate limiting with `error.type=rate_limited`, not by searching error text.

**Tool span** (`loomcycle.tool.call`): `loomcycle.tool`,
`gen_ai.operation.name=execute_tool`, `gen_ai.tool.name`,
`gen_ai.tool.call.id` (the model's tool-use id), and
`loomcycle.tool_is_error=true` when the tool returned an error result.

**MCP span** (`loomcycle.mcp.call`): `loomcycle.mcp_server`,
`loomcycle.mcp_tool`, and `loomcycle.tool_is_error=true` for a server's
`isError` answer.

The `gen_ai.*` names follow the OpenTelemetry GenAI semantic conventions,
release 1.41.0, and are emitted beside the `loomcycle.*` names, which stay
stable while the GenAI conventions are still changing.

### Span status

A span's status is Error when something failed, so a collector's "keep
error traces" policy keeps the runs that went wrong:

- a failed run, and a cancelled run (described `cancelled`; filter
  `loomcycle.stop_reason=cancelled` to drop cancels);
- a model call that failed or was cancelled;
- a provider attempt that got no response or a 4xx/5xx status;
- a tool that broke (returned a Go error), an MCP server that could not be
  reached.

A tool call that returned an error result — a refusal, a policy or hook
denial, an unknown tool, an MCP server's `isError` answer — is not: the
model reads it and goes on, so it sets `loomcycle.tool_is_error` instead.

### Users on spans

A span names its user by `loomcycle.user_hash`: the first 16 hex characters
of an HMAC-SHA256 over `tenant|user`, keyed by a key derived from
`LOOMCYCLE_SECRET_KEY`. It is stable, so one user's traces group together,
and `loomcycle otel user-hash <tenant> <user>` prints a user's hash to
search for. `LOOMCYCLE_OTEL_RAW_USER_ID=1` adds the raw `loomcycle.user_id`
beside it; with no secret key set the raw id is all a span carries, and the
startup warns.

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
- **No user ids.** A span names its user by `loomcycle.user_hash`
  (above), never the id itself, unless the operator opts in. No
  user-content text crosses the span boundary.
- **Outside erasure's reach once exported.** A subject erasure cannot
  delete spans already in a tracing backend; the backend's retention is
  the control. The hash is what limits what an exported span says about
  a person.

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

export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
./bin/loomcycle --config loomcycle.example.yaml
```

Fire any agent run (curl `/v1/runs` or click around the Web UI). Open
`http://localhost:16686`, pick `loomcycle` from the Service dropdown,
hit "Find Traces." Each agent run appears as one row. Click into a
trace to see the run → iteration → provider/tool tree.

Filter expressions:

- `loomcycle.run_id=run_abc...` — pin to one run
- `loomcycle.user_hash=<hash>` — all runs by one user (`loomcycle otel user-hash <tenant> <user>`)
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
export OTEL_EXPORTER_OTLP_ENDPOINT=https://api.honeycomb.io
export OTEL_EXPORTER_OTLP_HEADERS=x-honeycomb-team=$HONEYCOMB_API_KEY
export OTEL_SERVICE_NAME=loomcycle-prod
./bin/loomcycle --config loomcycle.yaml
```

Honeycomb's BubbleUp + heatmap views are stronger than Jaeger's
timeline for "what's slow?" diagnosis. Their Triggers feature can fire
a webhook when a latency p99 exceeds a threshold — useful for
"alert me when a provider degrades."

The `x-honeycomb-team` header carries your API key. Treat it as a
secret; `OTEL_EXPORTER_OTLP_HEADERS` takes a comma-separated
`key=value,key2=value2` list so multiple auth or routing headers
compose:

```sh
OTEL_EXPORTER_OTLP_HEADERS=x-honeycomb-team=$KEY,x-honeycomb-dataset=prod
```

To keep the key out of the process environment (visible in
`docker inspect` and deployment UIs), write the same pairs — one per
line or comma-separated — to a file and set
`LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE` to its path instead. The
file is read once at startup; setting both is a startup error.

## Walkthrough 4 — DataDog APM

DataDog accepts OTLP/HTTP when the local Agent is configured with
the `otlp_config` block. Loomcycle then points at the local Agent
(default 127.0.0.1:4318) — same env var as Jaeger:

```sh
export OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318
```

DataDog's UI groups by `service.name`, `service.version`, and
`deployment.environment.name`. Loomcycle sets `service.name=loomcycle`,
`service.version` from the binary, `service.instance.id` from
`LOOMCYCLE_REPLICA_ID` and `deployment.environment.name` from
`LOOMCYCLE_ENVIRONMENT`. `OTEL_SERVICE_NAME` and
`OTEL_RESOURCE_ATTRIBUTES` override any of them.

## Sampling guidance

The default sampler, `parentbased_always_on`, captures every run. At a JobEmber-class workload (~200 runs/hour × 8 iterations ×
4 tools = 8K spans/hour) this is fine — OTLP/HTTP at ~200 bytes per
span is ~1.5MB/hour, ~1GB/month. Most operators can leave it at 1.0.

Reduce when:

- Your collector's storage is bounded (Tempo local-disk; Honeycomb's
  20M-events/month free tier — set 0.05 to fit a high-volume workload)
- You're shipping to a SaaS APM that bills per-event (DataDog at
  $1.27/M, Honeycomb beyond free tier)

Set `OTEL_TRACES_SAMPLER=parentbased_traceidratio` and
`OTEL_TRACES_SAMPLER_ARG=0.1`: when a parent span is sampled, all
children are also sampled (so a trace stays whole), and the ratio
applies only to roots. It keeps every tenth run end-to-end and drops
the rest entirely — never partial traces.

**Per tenant.** A run that starts a trace can be sampled at its own
tenant's ratio instead — keep every trace of one tenant, sample the rest:

```yaml
observability:
  tracing:
    tenant_sampler_ratios:
      acme: 1
      beta: 0.1
```

or `LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS=acme=1,beta=0.1` (wins per
tenant). A run that joins a caller's trace, or a sub-agent's run, follows
the trace's existing decision; a tenant not listed uses the global
sampler. Read at startup.

**Keeping failures at a low ratio** needs a decision made after the run
ends: use the collector's tail-sampling processor (OpenTelemetry Collector
or Alloy) with three policies — keep `status=ERROR`, keep `loomcycle.run`
spans longer than your threshold, sample the rest.

## A caller's trace

When the caller that starts a run sends a W3C `traceparent` header (or
gRPC metadata), the run joins that trace. It is read only once the
request is authenticated (a bearer, a verified webhook signature, an
accepted A2A peer), so an anonymous caller cannot choose a trace or
force sampling. `LOOMCYCLE_OTEL_INBOUND_CONTEXT` picks what happens:
`parent` (default) makes the caller's span the run's parent, `link`
starts a new trace linked to the caller's span (the caller cannot
influence sampling), `ignore` reads nothing. Baggage is never copied
onto a span.

The run's `traceparent` is passed on to the operator's own MCP servers
over HTTP, so their spans join the run's trace. It is never sent to a
model provider, to a server a tenant registered, or by the HTTP and
WebFetch tools.

## From a run to its trace

A sampled run records its trace id: `trace_id` on get_run (HTTP, gRPC,
MCP) and on the run's `started` event. Set
`LOOMCYCLE_TRACE_URL_TEMPLATE` (for example
`https://grafana.example/explore?traceId={trace_id}`) and the Web UI's
run view shows an "Open trace" link.

## Filtering best practices

Three filter expressions that pay for themselves on the first
incident:

1. **Slow runs**: `service:loomcycle duration>30s` — surface runs
   that took longer than the operator's SLO threshold.
2. **Per-user noise**: group-by `loomcycle.user_hash`, sort by count —
   identify which user is driving the run volume.
3. **Provider degradation**: `loomcycle.provider=anthropic status=error
   span_name=loomcycle.provider.call` — Anthropic-specific errors
   over the last hour; add `error.type=rate_limited` for its 429s.
   Cross-reference with the resolver matrix (`GET /v1/_resolve/matrix`)
   to verify the matrix marked anthropic stalled.

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
| Which model call took 8s? | Spans (model.call duration; provider.call is time to headers) |
| Did any tool error? | Spans (Error-status leaf spans) |
| How many cache_read_tokens? | Spans (run and model-call attributes) |
| Why is p99 latency creeping up? | Spans + your APM's stats UI |

Use the transcript for "what happened," the spans for "how fast,
where, why slow."

## Troubleshooting

**Symptom**: `otel: tracer enabled` in the loomcycle log but no
spans in Jaeger/Tempo/Honeycomb.

- Run `curl -v http://localhost:4318/v1/traces -X POST -d '{}'` from
  the loomcycle host. Expect a 200 or 400 — anything else (connection
  refused, DNS failure, certificate error) is a connectivity problem.
  Loomcycle logs `otel: span export failing` once when exports start
  failing, a summary every five minutes while they keep failing, and
  `otel: span export recovered` when they succeed again.
- `OTEL_EXPORTER_OTLP_ENDPOINT` is a URL with its scheme: `http://`
  is plain text, `https://` is TLS. For gRPC set
  `OTEL_EXPORTER_OTLP_PROTOCOL=grpc` and use the collector's gRPC port
  (usually 4317).
- `/metrics` reports `loomcycle_otel_spans_exported_total`,
  `loomcycle_otel_spans_dropped_total` and
  `loomcycle_otel_span_export_failures_total`.

**Symptom**: traces show up but `service.name=unknown_service`.

- Check `OTEL_SERVICE_NAME` / `OTEL_RESOURCE_ATTRIBUTES` do not set an
  empty name. The default `"loomcycle"` should always populate.

**Symptom**: high collector latency under load.

- Sample less: `OTEL_TRACES_SAMPLER=parentbased_traceidratio` with
  `OTEL_TRACES_SAMPLER_ARG=0.1` or 0.01.
- Watch `loomcycle_otel_spans_dropped_total` on `/metrics`: spans are
  dropped, never waited on, when the export queue
  (`OTEL_BSP_MAX_QUEUE_SIZE`, default 2048) is full.

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

The OTLP exporter reads the standard `OTEL_EXPORTER_OTLP_*` variables
itself (and their `_TRACES_` forms): `_ENDPOINT`, `_HEADERS`,
`_TIMEOUT`, `_COMPRESSION`, `_CERTIFICATE`, `_CLIENT_CERTIFICATE`,
`_CLIENT_KEY`, `_INSECURE`.

| Variable | Default | Purpose |
|---|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | empty | A URL with its scheme. Empty (and no `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`) disables tracing. |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `http/protobuf` | Or `grpc`. `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL` wins when set. |
| `OTEL_EXPORTER_OTLP_HEADERS` | empty | Collector auth as `key=value,...`. A secret. |
| `OTEL_TRACES_EXPORTER` | `otlp` | A list; without `otlp` (e.g. `none`) tracing is off. |
| `OTEL_SDK_DISABLED` | `false` | `true` turns tracing off. |
| `OTEL_TRACES_SAMPLER` | `parentbased_always_on` | Also `always_on`, `always_off`, `traceidratio`, `parentbased_always_off`, `parentbased_traceidratio`. |
| `OTEL_TRACES_SAMPLER_ARG` | `1` | The ratio for the two ratio samplers, 0 to 1. |
| `OTEL_SERVICE_NAME` | `loomcycle` | `service.name` resource attribute. |
| `OTEL_RESOURCE_ATTRIBUTES` | empty | Extra resource attributes; override loomcycle's defaults. |
| `OTEL_PROPAGATORS` | `tracecontext,baggage` | Or `none`; other names are ignored. |
| `OTEL_BSP_MAX_QUEUE_SIZE` / `_MAX_EXPORT_BATCH_SIZE` / `_EXPORT_TIMEOUT` / `_SCHEDULE_DELAY` | 2048 / 512 / 10000 ms / 5000 ms | Export queue bounds. |
| `LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE` | empty | A file holding the collector headers instead of the env. |
| `LOOMCYCLE_OTEL_INBOUND_CONTEXT` | `parent` | What a caller's trace context does: `parent`, `link` or `ignore`. |
| `LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS` | empty | Per-tenant ratio for runs that start a trace, `tenant=ratio,...`. |
| `LOOMCYCLE_OTEL_RAW_USER_ID` | off | `1` adds the raw `loomcycle.user_id` beside `loomcycle.user_hash`. |
| `LOOMCYCLE_ENVIRONMENT` | empty | `deployment.environment.name` resource attribute. |
| `LOOMCYCLE_TRACE_URL_TEMPLATE` | empty | Web UI "Open trace" link; must contain `{trace_id}`. |

With tracing on, a value loomcycle cannot honour (an endpoint without
an http(s) scheme, an unknown sampler or protocol, a ratio outside 0–1,
a malformed collector header, an unreadable headers file) stops the
startup with an error naming the variable. Unsupported propagator or
exporter names are logged and ignored. With tracing off, `OTEL_*` values
never stop the startup.
