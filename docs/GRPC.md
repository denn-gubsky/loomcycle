# gRPC surface (v0.5.5)

This guide is for operators running loomcycle behind a gRPC API.
HTTP+SSE stays the default and primary surface; gRPC is opt-in via
`LOOMCYCLE_GRPC_ADDR`. Both surfaces serve the same store, the same
cancel registry, and the same agent loop — picking gRPC vs. HTTP is
a wire-format decision, not a feature decision.

For the agent-loop architecture, see [ARCHITECTURE.md](ARCHITECTURE.md).
For the public roadmap, see [PLAN.md](PLAN.md). For the full proto
schema, see [`proto/loomcycle.proto`](../proto/loomcycle.proto).

## When to choose gRPC

| Situation | Recommendation |
|---|---|
| HTTP/JSON consumers (browsers, curl, simple scripts) | **HTTP+SSE.** Default. No client codegen. |
| Polyglot service mesh (Go, Python, Java, Rust services calling loomcycle) | **gRPC.** Strongly typed; one .proto, every language gets a client. |
| Tight latency/throughput on a hot internal path | **gRPC.** HTTP/2 multiplex + protobuf framing is ~30% faster than chunked-JSON SSE for streaming workloads. |
| Migration from another gRPC-native runtime | **gRPC.** Fewer wire-shape mismatches in the adapter layer. |
| Async Python services using `asyncio` | **gRPC** via [`adapters/python/`](../adapters/python/). HTTP+SSE in Python needs a manual SSE parser. |

The two surfaces coexist on the same process — running with both is
the right answer when you have a mixed consumer base, or while
migrating. They share the same backpressure semaphore: every concurrent
run still counts against `LOOMCYCLE_MAX_CONCURRENT_RUNS` regardless of
which wire opened it.

## Configuration

gRPC is off by default. To turn it on, point `LOOMCYCLE_GRPC_ADDR` at
a TCP listener address:

```sh
# Both wires running side by side:
export LOOMCYCLE_HTTP_ADDR=":8787"
export LOOMCYCLE_GRPC_ADDR=":8788"
export LOOMCYCLE_AUTH_TOKEN="<bearer-token>"
./bin/loomcycle --config loomcycle.yaml
```

```sh
# gRPC-only deployment:
unset LOOMCYCLE_HTTP_ADDR    # or set to the empty string
export LOOMCYCLE_GRPC_ADDR=":8788"
./bin/loomcycle --config loomcycle.yaml
```

There is no yaml equivalent for `LOOMCYCLE_GRPC_ADDR` — gRPC binding
is a deploy-time concern (port allocation, firewall, TLS termination)
rather than a feature toggle, so it lives in env-only.

| Setting | Env | Default | Notes |
|---|---|---|---|
| gRPC listener address | `LOOMCYCLE_GRPC_ADDR` | (off) | E.g. `":8788"` or `"127.0.0.1:8788"`. Unset → no gRPC server. |
| Bearer token | `LOOMCYCLE_AUTH_TOKEN` | (open mode) | Same token as the HTTP middleware. Empty → unauthenticated. |
| Build tags surfaced via Health() | `LOOMCYCLE_BUILD_COMMIT` / `LOOMCYCLE_BUILD_TIME` | (empty) | Set by your release pipeline. Adapters read these for compatibility checks. |

## Wire surface

Nine RPCs on the `loomcycle.v1.Loomcycle` service. Each maps 1:1 to
an HTTP route — wire-shape parity is a stability guarantee, so an
adapter can be trivially ported between the two.

| RPC | HTTP equivalent | Streaming | Notes |
|---|---|---|---|
| `Run(RunRequest) → stream Event` | `POST /v1/runs` (text/event-stream) | server-stream | Drives one fresh agent run end-to-end. |
| `Continue(ContinueRequest) → stream Event` | `POST /v1/sessions/{session_id}/continue` | server-stream | Continues an existing session. |
| `SpawnRunBatch(BatchSpawnRequest) → BatchSpawnResult` | `POST /v1/runs:batch` | unary | **RFC Y external fan-out (v0.33.0).** Spawn up to 32 fresh runs concurrently (mode `"join"`) under the per-user admission gate; returns an index-aligned envelope (`results[]` of `SpawnResult` + `spawned`). A per-child failure rides in its `status`+`error`, never failing the batch; an over-cap / `mode:"detach"` request → `INVALID_ARGUMENT`. |
| `CompactRun(CompactRunRequest) → CompactRunResult` | `POST /v1/runs/{run_id}/compact` | unary | **v0.33.0.** Summarize a run's context (keyed on `run_id`). A live run must be PARKED — mid-turn → `FAILED_PRECONDITION`. Returns `{compacted, before_tokens, after_tokens, applied}` (`applied` ∈ `live`/`marker`/`noop`). |
| `GetAgent(GetAgentRequest) → Agent` | `GET /v1/agents/{agent_id}` | unary | Read one agent's status + usage. |
| `CancelAgent(CancelAgentRequest) → CancelAgentResponse` | `POST /v1/agents/{agent_id}/cancel` | unary | Cascades to children via `parent_agent_id`. |
| `ListUserAgents(ListUserAgentsRequest) → ListUserAgentsResponse` | `GET /v1/users/{user_id}/agents` | unary | Status filter optional. |
| `GetTranscript(GetTranscriptRequest) → Transcript` | `GET /v1/sessions/{session_id}/transcript` | unary | Persisted event log; payloads are raw JSON bytes. |
| `Health(HealthRequest) → HealthResponse` | `GET /healthz` | unary | Unauthenticated. Returns build commit + uptime. |

**Per-run `sampling` + `compaction` (v0.33.0).** `RunRequest` and
`ContinueRequest` carry optional `Sampling` and `Compaction` messages —
per-run overrides merged per-field over the agent's own (mirroring the HTTP
`sampling`/`compaction` body fields). Every scalar uses proto3 `optional` so
"unset" (inherit the agent's value) is distinct from an explicit zero — a
`temperature` of `0.0` stays deterministic, `enabled: false` stays "off".
`BatchSpawnRequest.spawns` reuses `RunRequest`, so a fan-out child sets these
the same way.

`HostAllowlist` is a wrapper message rather than a `repeated string`
field directly so the proto can encode the three-state `*[]string`
semantics from the HTTP API:

| Field present? | List value | Effect |
|---|---|---|
| absent | — | No narrowing; operator's static allowlist is the floor. |
| present | `[]` | Deny-all: agent gets no network access. |
| present | `["foo.com"]` | Intersection with operator's static allowlist. |

`allowed_hosts` is **caller-authoritative** — a trust boundary, never
filled in by a model. See [TOOLS.md](TOOLS.md#host-policy) for the
underlying policy layer.

## Streaming model

`Run` and `Continue` are server-streaming. The proto `Event` message
mirrors the agent loop's internal `providers.Event` 1:1, so every
event the loop emits is forwarded verbatim to the client.

The server emits **two synthetic registration frames** at the start of
every stream, before the first provider event:

| Frame index | `type` | `text` | Notes |
|---|---|---|---|
| 0 | `"session"` | `<session_id>` | Server-assigned (or echoed when caller supplied). |
| 1 | `"agent"` | `<agent_id>` | Plus `stop_reason=<parent_agent_id>` and `error=<JSON envelope: {agent_id, run_id, session_id, parent_agent_id}>`. The JSON envelope packs the four IDs without a proto change. |

These are wire-stable; adapters consume them by counting frames at
the head of the stream rather than parsing JSON repeatedly.
[`adapters/python/`](../adapters/python/loomcycle/client.py) swallows
both frames in `_drive_stream` and surfaces them as a `RunHandle` to
the caller's `on_handle` callback.

Real provider events follow: `text`, `tool_use`, `tool_result`,
`usage`, `retry`, `done`, `error`. Stream completion comes via either
a `done` frame followed by a clean half-close, or a gRPC status error
(see error mapping below).

The transcript is persisted to the store regardless of whether the
gRPC client stays connected — if your client disconnects mid-run,
the run continues to completion, and `GetTranscript(session_id)`
returns the full event log.

## Authentication

Bearer token via the standard `authorization` gRPC metadata header,
same token (and same env var) as the HTTP middleware:

```sh
export LOOMCYCLE_AUTH_TOKEN="$(openssl rand -hex 32)"
```

Client side (Python):

```python
client = LoomcycleClient(target="loomcycle.internal:8788", auth_token=os.environ["LOOMCYCLE_AUTH_TOKEN"])
```

Comparison is **constant-time** (`subtle.ConstantTimeCompare`) to
prevent token-recovery via timing side-channels. Empty
`LOOMCYCLE_AUTH_TOKEN` opens the surface (matches HTTP behavior).
Health() is exempt from auth so liveness probes don't need credentials.

## Error code mapping

Server-side: [`mapRunnerErr`](../internal/api/grpc/server.go) plus the
direct `codes.NotFound` emissions in `GetAgent` / `CancelAgent` /
`GetTranscript`. The Python adapter inverts this in
[`_raise_from_grpc`](../adapters/python/loomcycle/client.py).

| gRPC code | Runner sentinel / HTTP status | Meaning |
|---|---|---|
| `INVALID_ARGUMENT` | `ErrUnknownAgent`, `ErrInvalidArgument`, `ErrUnknownProvider` / HTTP 400 | Bad request shape, unknown agent name, missing/invalid field. |
| `FAILED_PRECONDITION` | `ErrSessionRequired`, `ErrSessionBusy` / HTTP 409 / 412 | Operator-state mismatch. Session-busy = another request in flight on the same `session_id`. |
| `NOT_FOUND` | `ErrSessionNotFound` (Continue/GetTranscript), or unknown `agent_id` (GetAgent/CancelAgent) / HTTP 404 | Discriminate via message text — "session not found" vs. "no run found for agent_id". |
| `ALREADY_EXISTS` | `ErrAgentIDInUse` / HTTP 409 | Caller-supplied `agent_id` is already mapped to a live run. |
| `RESOURCE_EXHAUSTED` | `ErrBackpressure` / HTTP 429 | Concurrency semaphore rejected the run; retry with backoff. |
| `UNAUTHENTICATED` | (auth middleware) / HTTP 401 | Bad/missing bearer token. |
| `UNAVAILABLE` | (transport) / (no HTTP equivalent — connection error) | Channel can't reach the server. |
| `INTERNAL` | (default fallthrough) / HTTP 500 | Unexpected runtime error. |
| `CANCELED` | (stream send failed) / (client disconnected) | The stream broke mid-run; the loop kept going for transcript persistence. |

## TLS and production hardening

The built-in listener uses **cleartext H2** — no TLS in the binary.
The expectation is that production deployments terminate TLS at the
ingress layer (envoy, nginx, ALB) or at a sidecar mTLS proxy. This
keeps the binary minimal and lets operators choose their cert-rotation
story.

Recipe — TLS via envoy sidecar:

```yaml
# envoy.yaml — terminate TLS on :443, forward cleartext to :8788
clusters:
  - name: loomcycle
    connect_timeout: 1s
    type: STATIC
    http2_protocol_options: {}
    load_assignment:
      cluster_name: loomcycle
      endpoints:
        - lb_endpoints:
            - endpoint: { address: { socket_address: { address: 127.0.0.1, port_value: 8788 } } }
```

Bind loomcycle to loopback (`LOOMCYCLE_GRPC_ADDR=127.0.0.1:8788`) when
TLS is terminated by a sidecar so external clients can't bypass it.

Native TLS in the loomcycle binary is a v1.0 consideration tracked in
[`doc-internal/PLAN.md`](../doc-internal/PLAN.md); not a v0.5.x deliverable.

## Adapters

| Language | Status | Path / package |
|---|---|---|
| Python (asyncio) | shipped v0.5.5 | [`adapters/python/`](../adapters/python/), `pip install loomcycle` |
| TypeScript / Node | HTTP+SSE only | [`adapters/ts/`](../adapters/ts/), `npm: @loomcycle/client` — gRPC adapter deferred |
| Go (in-tree) | n/a | Use the generated stubs at `internal/api/grpc/loomcyclepb/` directly. |
| Other | hand-rolled | Run `protoc` against [`proto/loomcycle.proto`](../proto/loomcycle.proto) for any [gRPC-supported language](https://grpc.io/docs/languages/). |

The proto is the source of truth — any language with a gRPC compiler
plugin works. Run `make proto` to regenerate the Go stubs after
editing; `make python-proto` for the Python stubs.

## Coexistence with HTTP

Both surfaces share:

- The store (`internal/store/`) — same SQLite/Postgres backend.
- The cancel registry (`internal/cancel/`) — a cancel issued via
  gRPC reaches a run started via HTTP and vice versa.
- The concurrency semaphore (`internal/concurrency/`) — every run
  competes for the same `LOOMCYCLE_MAX_CONCURRENT_RUNS` budget.
- The runner interface (`internal/runner/`) — wire-agnostic. The
  HTTP server satisfies it directly; the gRPC server delegates to
  the same instance.

Operator topologies — pick what your environment needs:

| Topology | When |
|---|---|
| HTTP only (`LOOMCYCLE_GRPC_ADDR` unset) | Default. Most consumers are scripts or browsers. |
| gRPC only (no `LOOMCYCLE_HTTP_ADDR`) | Locked-down internal service mesh; no curl/browser callers. |
| Both | Heterogeneous consumers, or a migration window between the two. Coexists cleanly — no extra resource cost beyond the second listener. |

## Health probe

```sh
grpcurl -plaintext localhost:8788 loomcycle.v1.Loomcycle/Health
```

Returns:

```json
{
  "ok": true,
  "commit": "<git-sha>",
  "built": "<RFC3339>",
  "uptimeSeconds": "1234"
}
```

Health() is unauthenticated (matches HTTP `/healthz`). Adapters running
compatibility checks log `commit` so an old client talking to a new
server is visible in metrics.

## Worked example — Python

```python
import asyncio, os
from loomcycle import LoomcycleClient, RunHandle

async def main():
    target = os.environ["LOOMCYCLE_GRPC_ADDR"]   # "127.0.0.1:8788"
    token  = os.environ["LOOMCYCLE_AUTH_TOKEN"]
    handle: RunHandle | None = None

    def on_handle(h: RunHandle):
        nonlocal handle
        handle = h

    async with LoomcycleClient(target=target, auth_token=token) as client:
        async for ev in client.run_streaming(
            agent="default",
            segments=[
                {"role": "user", "content": [
                    {"type": "trusted-text", "text": "Hello, loomcycle."}
                ]}
            ],
            on_handle=on_handle,
        ):
            if ev.type == "text":
                print(ev.text, end="", flush=True)
            elif ev.type == "done":
                print(f"\n[done {ev.stop_reason}]")

    if handle:
        # Continue, cancel, or read transcript using handle.session_id
        # / handle.agent_id later.
        ...

asyncio.run(main())
```

Full example: [`examples/python-cli/main.py`](../examples/python-cli/main.py).

## Worked example — Go in-tree

```go
import (
    "context"
    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials/insecure"
    "google.golang.org/grpc/metadata"
    "github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
)

conn, _ := grpc.Dial("127.0.0.1:8788", grpc.WithTransportCredentials(insecure.NewCredentials()))
defer conn.Close()
client := loomcyclepb.NewLoomcycleClient(conn)

ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
resp, err := client.Health(ctx, &loomcyclepb.HealthRequest{})
```

The generated Go stubs are committed at
`internal/api/grpc/loomcyclepb/` so the Go module builds without
running `protoc` on a fresh checkout.

## Limitations and open work

- **No native TLS in the binary.** Terminate at ingress / sidecar.
  Native TLS is v1.0.
- **No reflection service.** `grpcurl` works against the proto file
  directly but won't autodiscover the schema. Reflection is a 3-line
  add when an operator asks for it.
- **No streaming `Continue` reconnect/resume.** A dropped client must
  open a new `Continue` call to pick up where it left off; intermediate
  events are visible via `GetTranscript`. Resume-from-seq is v1.0.
- **TS gRPC adapter not yet shipped.** TS callers stay on HTTP+SSE
  via `@loomcycle/client`. Tracked in [`PLAN.md`](PLAN.md).

For the design history (why gRPC was added, the runner-extraction
refactor, the synthetic-frame envelope choice), see
`doc-internal/PLAN.md`.
