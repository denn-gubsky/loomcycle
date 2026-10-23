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
export LOOMCYCLE_AUTH_TOKEN="<bearer-token>"
./bin/loomcycle --config loomcycle.yaml
```

`:8788` binds every interface. With no `LOOMCYCLE_AUTH_TOKEN`, no config
`principals:` and no admin operator token the server is in open mode, and it refuses to start on a
listener beyond loopback — HTTP or gRPC — unless `LOOMCYCLE_ALLOW_OPEN_MODE=1`
is set. For a local open-mode test, bind `127.0.0.1:8788`.

There is no yaml equivalent for `LOOMCYCLE_GRPC_ADDR` — gRPC binding
is a deploy-time concern (port allocation, firewall, TLS termination)
rather than a feature toggle, so it lives in env-only.

| Setting | Env | Default | Notes |
|---|---|---|---|
| gRPC listener address | `LOOMCYCLE_GRPC_ADDR` | (off) | E.g. `":8788"` or `"127.0.0.1:8788"`. Unset → no gRPC server. |
| Bearer token | `LOOMCYCLE_AUTH_TOKEN` | (open mode) | Same token as the HTTP middleware. Empty, with no config `principals:` and no admin operator token → unauthenticated (open mode), which is refused on a non-loopback listener unless `LOOMCYCLE_ALLOW_OPEN_MODE=1`. |
| Build tags surfaced via Health() | `LOOMCYCLE_BUILD_COMMIT` / `LOOMCYCLE_BUILD_TIME` | (empty) | Set by your release pipeline. Adapters read these for compatibility checks. |

## Wire surface

The core run and agent RPCs of the `loomcycle.v1.Loomcycle` service are
below. The service also carries the substrate definitions, channels, memory,
documents, paths and the rest; `proto/loomcycle.proto` is the full list. Each
maps 1:1 to an HTTP route — wire-shape parity is a stability guarantee, so an
adapter can be trivially ported between the two.

| RPC | HTTP equivalent | Streaming | Notes |
|---|---|---|---|
| `Run(RunRequest) → stream Event` | `POST /v1/runs` (text/event-stream) | server-stream | Drives one fresh agent run end-to-end. |
| `Continue(ContinueRequest) → stream Event` | `POST /v1/sessions/{session_id}/continue` | server-stream | Continues an existing session. |
| `SpawnRunBatch(BatchSpawnRequest) → BatchSpawnResult` | `POST /v1/runs:batch` | unary | **RFC Y external fan-out (v0.33.0).** Spawn up to 32 fresh runs concurrently under the per-user admission gate; returns an index-aligned envelope (`results[]` of `SpawnResult` + `spawned`). Mode `"join"` (the default) returns when every child has settled; an optional `timeout_ms` bounds the wait, and a child the batch started that is still running then is cancelled and reported `cancelled` in its slot. Without `timeout_ms` the server's `LOOMCYCLE_BATCH_JOIN_TIMEOUT` (`1h`) only stops the wait: a child still running then is not cancelled — it comes back with its current status (`running`, or `queued`) and its ids, and goes on after the call returns. Either way its `error` says the join timed out, and the others are reported as usual. Mode `"detach"` returns once every child has started (status `"running"` with its `run_id`) or been refused in its slot; the runs go on after the call returns. A per-child failure rides in its `status`+`error`, never failing the batch; an empty or over-cap batch, a child with no `segments` or an invalid `parent_context` / `user_credentials`, an unknown mode, or `timeout_ms` with `mode:"detach"` → `INVALID_ARGUMENT`. A child may carry `idempotency_key`: a key already held by a run starts nothing and the child's `SpawnResult` is that run, `deduplicated: true` (a join waits for it; its timeout never cancels it — the child comes back with the run's current status, usually `running`, its ids, and an `error` saying the join timed out). A child (and `Run`) may also carry `max_wall_seconds`, a lifetime limit that belongs to the run: past it the run ends `cancelled` with `stop_reason` `wall_limit`; it is the time bound a `detach` child takes. A child (and `Run`) may carry `retention: "none"` to be a no-trace run, whose content is delivered and stored nowhere; what such a run cannot do is refused `FAILED_PRECONDITION` with code `retention_none_unsupported`. On `Run`, the same key makes the stream the existing run's — its `agent` frame envelope carries `"deduplicated": true`. |
| `ListTeamChannels(ListTeamChannelsRequest) → ListTeamChannelsResponse` | `GET /v1/_teamdef/{team}/channels` | unary | The channels a team declares for itself, each with its definition, the version that declares it (`declared_in`: `active`, `inactive` or `retired`) and its counts (`message_count`, `held_count`, `awaiting_hooks_count`). `ListChannels` never lists these. The team is resolved in the caller's tenant; an unknown team, or one in another tenant, → `NOT_FOUND`. Counts for a user-scoped channel are summed over every user. Read-only. |
| `PeekTeamChannel(PeekTeamChannelRequest) → PeekTeamChannelResponse` | `GET /v1/_teamdef/{team}/channels/{name}/peek` | unary | Messages on one of a team's own channels, addressed by team and local name; advances no cursor. Held messages are not listed. A user-scoped channel is read at the caller's own user; `user_id` reads another's only for a caller who may read any user's channel. A name the team does not declare → `NOT_FOUND`; a user-scoped channel with no user to read → `INVALID_ARGUMENT`. `PeekChannel` keeps refusing the stored name. |
| `ReleaseTeamChannel(ReleaseTeamChannelRequest) → ReleaseTeamChannelResponse` | `POST /v1/_teamdef/{team}/channels/{name}/release` | unary | Delivers the oldest held messages on one of a team's own channels to the team's readers. `count` defaults to 1 (at most 1000; out of range → `InvalidArgument`). Answers `released` (message ids), `released_count` and `still_held`; with nothing held it releases nothing. Addressed and confined as `PeekTeamChannel`, `user_id` included. |
| `DeleteRunTree(DeleteRunTreeRequest) → DeleteRunTreeResult` | `DELETE /v1/runs/{run_id}/tree` · `DELETE /v1/sessions/{session_id}?cascade=children` | unary | Delete a run and every descendant run (`run_id`), or every run of a session with their descendants (`session_id`); exactly one. The usage ledger is kept. Needs `runs:delete` and read access to the root. An unknown, already deleted or unreadable root returns zero counts, not an error. A tree with a live or paused run → `FAILED_PRECONDITION`, nothing deleted. Returns `{deleted_runs, counts, kept, not_reached}`. |
| `CompactRun(CompactRunRequest) → CompactRunResult` | `POST /v1/runs/{run_id}/compact` | unary | **v0.33.0.** Summarize a run's context (keyed on `run_id`). A live run must be PARKED — mid-turn → `FAILED_PRECONDITION`. Returns `{compacted, before_tokens, after_tokens, applied}` (`applied` ∈ `live`/`marker`/`noop`). |
| `GetAgent(GetAgentRequest) → Agent` | `GET /v1/agents/{agent_id}` | unary | Read one agent's status + usage. |
| `GetRun(GetRunRequest) → Agent` | `GET /v1/runs/{run_id}` | unary | `GetAgent` addressed by the run itself. An agent id can name many runs (every walk of a team is filed under `team:<name>`, which `GetAgent` cannot address) and `GetAgent` answers the latest; a run id names one. Same tenant / isolated-member `NOT_FOUND` fold as `GetAgent`. |
| `CancelAgent(CancelAgentRequest) → CancelAgentResponse` | `POST /v1/agents/{agent_id}/cancel` | unary | Cascades to children via `parent_agent_id`. |
| `ListUserAgents(ListUserAgentsRequest) → ListUserAgentsResponse` | `GET /v1/users/{user_id}/agents` | unary | Up to 100 runs. Status filter optional: the single `status` (verbatim, `""` = every status) or the repeated `statuses` (a run in any of them; `["all"]` = every status; an unknown status is `INVALID_ARGUMENT`), not both. `awaited_states` (`channel` / `interrupted` / `review` / `input` / `children`) lists only running runs blocked on one of those waits — it implies running, reads at most 1,000 running runs, and sets `truncated` when it left some unread. Same filters as HTTP `?status=a,b` / `?awaited_state=`. |
| `ListWalkRuns(ListWalkRunsRequest) → ListWalkRunsResponse` | `GET /v1/runs?walk_id=` | unary | One team walk's runs — the walk's own run (its `run_id` is the walk id) and every member it spawned, ordered by `started_at` then `run_id`, `limit` per page (0 = 100, at most 1000) with an opaque `next_cursor` (`""` on the last page). Not capped like `ListUserAgents`. Same tenant / isolated-member `NOT_FOUND` fold as `GetRun`, applied to the walk's own run; a foreign `cursor` is `INVALID_ARGUMENT`. The walk's own row carries `result` (as `GetRun` does), whose `terminal` names the end state the walk reached — absent when it failed or was cancelled; member rows leave `result` empty. |
| `ListChildRuns(ListChildRunsRequest) → ListChildRunsResponse` | `GET /v1/runs?parent_run_id=` | unary | One run's direct children — the runs whose `parent_run_id` is that run — of every status, ordered and paged like `ListWalkRuns`. Not the run itself, and not its grandchildren. Same tenant / isolated-member `NOT_FOUND` fold as `GetRun`, applied to the parent run; a foreign `cursor` is `INVALID_ARGUMENT`. No row carries `result`. |
| `GetTranscript(GetTranscriptRequest) → Transcript` | `GET /v1/sessions/{session_id}/transcript` | unary | Persisted event log; payloads are raw JSON bytes. |
| `RetentionOverride(RetentionOverrideRequest) → RetentionOverrideResponse` | `GET` / `PUT` / `DELETE /v1/_retention/overrides` | unary | The timed override of an agent's no-trace mode, one op-based RPC (`op`: `list`, `set`, `clear`). `set` takes `agent` and `until` (RFC 3339, at most 24 hours ahead) and stores that agent's runs until then; `clear` ends it and reports `cleared`. Operator-only: an admin for any tenant, a tenant operator for its own (`PERMISSION_DENIED` otherwise). A bad request is `INVALID_ARGUMENT`; an override whose audit record could not be written is not set (`UNAVAILABLE`). |
| `Health(HealthRequest) → HealthResponse` | `GET /healthz` | unary | Unauthenticated. Returns build commit + uptime. |

**Per-run `sampling` + `compaction` (v0.33.0).** `RunRequest` and
`ContinueRequest` carry optional `Sampling` and `Compaction` messages —
per-run overrides merged per-field over the agent's own (mirroring the HTTP
`sampling`/`compaction` body fields). Every scalar uses proto3 `optional` so
"unset" (inherit the agent's value) is distinct from an explicit zero — a
`temperature` of `0.0` stays deterministic, `enabled: false` stays "off".
`BatchSpawnRequest.spawns` reuses `RunRequest`, so a fan-out child sets these
the same way.

**Per-run `routing`.** `RunRequest.routing` and `ContinueRequest.routing` (a
`RunRouting` message: `queue_aware`, `switch_after_ms`) are the run's own
`routing:` block — how the agent's tier candidates are walked when the run
starts (see CONFIGURATION.md, "Per-agent `routing:`"). Unset leaves the
agent's block; a present message replaces it whole, so an empty one turns
queue-aware routing off for the run. `queue_aware` where the run resolves to a
provider/model pin — the agent's, or one the request makes with `model` — and a
negative `switch_after_ms` are `INVALID_ARGUMENT`; an agent with
`allow_overrides: false`, or a token whose overrides policy leaves `routing`
out, refuses it with `PERMISSION_DENIED`. A batch child and a configured run
carry it the same way.

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

## Access-control management RPCs

Groups, access-control lists, bindings and the authz questions have four op-dispatched RPCs, shaped like `OperatorTokenDef`: `rpc X(SubstrateRequest) returns (SubstrateResponse)`. `input_json` is the HTTP route's JSON body plus `"op"` and, for an admin, a `"tenant"` focus (the route's `?tenant=`); `output_json` is the route's JSON body. A refusal is `is_error: true` with the route's error body (`{"code": …, "error": …}`) and `error_info` carrying its category — the gRPC status stays `OK`, as for every substrate RPC.

| RPC | Ops | HTTP routes | Scope |
|---|---|---|---|
| `GroupDef` | `list`, `get`, `effective`, `create`, `delete`, `add_member`, `remove_member` | `/v1/_groups*` | `substrate:tenant` |
| `AclDef` | `create`, `update`, `put`, `get`, `list`, `retire`, `validate`, `catalog`, `names` | `POST /v1/_acldef`, `GET /v1/_acldef/names` | `substrate:tenant` |
| `AclBinding` | `list`, `create`, `delete` | `/v1/_aclbinding*` | `substrate:tenant` |
| `Authz` | `evaluation`, `evaluations`, `explain`, `simulate` | `POST /v1/_authz/*` | any principal, about itself |

The four RPCs and the HTTP routes call one implementation, and that implementation holds every gate: tenant confinement (an admin alone may focus another tenant), the tenant-operator floor for groups, lists and bindings (a member is refused; gRPC has no member path), each op's own access-control question (a list write asks `author def:acl/<name>`, a retire `retire`, a binding write `author def:acl/*`, a group write `manage principal:group:*`), the self-elevation guards, the audit events and cache invalidation on every replica. `Authz` answers about the caller; naming another `subject` needs a tenant operator. None of them is reachable from inside a run. What each op returns and refuses is the HTTP route's; see the configuration guide's *Managing groups, ACL lists and bindings*.

```sh
# input_json is bytes, so grpcurl takes it base64-encoded; there is no
# reflection service, so point grpcurl at the proto.
grpcurl -plaintext -import-path proto -proto loomcycle.proto \
  -H "authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -d '{"input_json":"'"$(printf '%s' '{"op":"create","name":"support"}' | base64)"'"}' \
  127.0.0.1:8788 loomcycle.v1.Loomcycle/GroupDef
```

The Python adapter's dispatch methods for these four RPCs ship separately.

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

A run that cannot start at once (its user already has its cap of runs
active, or every run slot is taken) is accepted with status `queued`
and, after the two registration frames, the stream carries a `queued`
frame — `admission.reason` (`user_cap` | `global_cap`) and
`admission.position` (1 = next to start) — then an `admitted` frame
(`admission.wait_ms`) when it gets a slot. A run can report `queued`
twice: waiting on its user's cap, then for a slot. `RunRequest.admission:
"reject"` refuses such a run at once with `RESOURCE_EXHAUSTED` instead;
any value other than `wait`, `reject` or empty is `INVALID_ARGUMENT`.
`GetAgent` / `GetRun` report a queued run's current `queue_position` and
`queue_reason`.

Once a run is running, a model call that has to wait for its provider
(its call lane is full, or the provider has its cap of models in flight)
puts a `call_queued` frame on the stream — `admission.reason` (`lane` |
`models_in_flight`), `admission.position`, `admission.provider` and
`admission.kind` — then `call_admitted` (`admission.wait_ms`, with the
same provider and kind) when it gets its slot. A decision or an embedding
one of the run's tools makes reports the same way (`kind` `decision` /
`embedder`).

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
| `NOT_FOUND` | `ErrSessionNotFound` (Continue/GetTranscript), or unknown `agent_id` (GetAgent/CancelAgent) / `run_id` (GetRun) / `walk_id` (ListWalkRuns) / HTTP 404 | Discriminate via message text — "session not found" vs. "no run found for agent_id" / "no run found for run_id" / "no walk found for walk_id". |
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
