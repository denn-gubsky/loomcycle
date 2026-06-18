# loomcycle — async Python client

`loomcycle` is the async Python client for [loomcycle][1]'s gRPC API
(introduced in v0.5.5). As of **v1.1.1 it covers all 42 gRPC RPCs** —
run streaming + continuation, the RFC AI **interactive session**
(`run_input` + `stream_run` + an `interactive=True` flag), batch
fan-out, run compaction, agent metadata + transcript, pause / resume /
state, the snapshot lifecycle, the resolver probe, the full
substrate-def family (incl. RFC AH volume_def), channel publish /
subscribe / peek / ack / await / broadcast, and run-state streaming —
through an ergonomic `LoomcycleClient` class with no need to import
generated protobuf types in your application code.

[1]: https://github.com/denn-gubsky/loomcycle

## Status

- Wraps loomcycle's gRPC server (`LOOMCYCLE_GRPC_ADDR`).
- Async-only (`grpc.aio`). Python 3.9+.
- **Full parity with the gRPC service surface** (42 RPCs).
- The TypeScript adapter (`adapters/ts/`) additionally exposes
  **HTTP-only** operations that have no gRPC RPC — memory-entry admin,
  interruptions, library enumeration, the LLM gateway, and
  whoami / list-users. Those are not reachable over gRPC and so are not
  in this client; use the HTTP+SSE surface for them.
- Production tag: `1.1.1` (42-RPC parity, version-aligned with the
  loomcycle v1.1.x line; ships on the `python-v1.1.1` tag).

## Install

```bash
pip install loomcycle
```

## Quick start

```python
import asyncio
from loomcycle import LoomcycleClient

async def main():
    async with LoomcycleClient(
        target="127.0.0.1:8788",
        auth_token="<LOOMCYCLE_AUTH_TOKEN>",
    ) as client:
        async for ev in client.run_streaming(
            agent="default",
            segments=[
                {
                    "role": "user",
                    "content": [
                        {"type": "trusted-text", "text": "Summarize loomcycle in one sentence."}
                    ],
                }
            ],
        ):
            if ev.type == "text":
                print(ev.text, end="", flush=True)
            elif ev.type == "tool_use":
                print(f"\n[tool_use: {ev.tool_use.name}]", flush=True)
            elif ev.type == "done":
                print(f"\n[done: {ev.stop_reason}]")

asyncio.run(main())
```

## Capturing the run handle

The gRPC server's first two stream frames are synthetic
registration frames — `LoomcycleClient` swallows them and exposes
the captured IDs via an `on_handle` callback:

```python
from loomcycle import LoomcycleClient, RunHandle

async def main():
    handle: RunHandle | None = None

    def capture(h: RunHandle):
        nonlocal handle
        handle = h
        print(f"agent_id={h.agent_id} session_id={h.session_id} run_id={h.run_id}")

    async with LoomcycleClient(target="127.0.0.1:8788") as client:
        async for ev in client.run_streaming(
            agent="default",
            segments=[...],
            on_handle=capture,
        ):
            ...

    # Use handle.session_id later to continue or read transcript.
```

## API

All methods are coroutine methods on `LoomcycleClient`.

| Method | Returns | Notes |
|---|---|---|
| `run_streaming(agent, segments, ...)` | `AsyncIterator[AgentEvent]` | Server-streams provider events for a fresh run. |
| `continue_session(session_id, segments, ...)` | `AsyncIterator[AgentEvent]` | Continues an existing session. |
| `get_agent(agent_id)` | `dict` | One agent's status + usage. |
| `cancel_agent(agent_id, reason="")` | `int` | Returns count of agents cancelled (cascades to children). |
| `list_user_agents(user_id, status="")` | `list[dict]` | Filters: `running`, `completed`, `failed`, `cancelled`. |
| `get_transcript(session_id)` | `list[dict]` | Persisted event log; `payload` is raw JSON bytes. |
| `health()` | `dict` | Liveness + build info. Unauthenticated. |
| `register_hook(owner, name, phase, callback_url, ...)` | `dict` | Pre/PostTool webhook registration. Returns `{"id": ...}`. |
| `list_hooks()` | `list[dict]` | Every registered hook (in-memory only). |
| `delete_hook(hook_id)` | `bool` | Idempotent on missing id is NOT supported — raises `HookNotFoundError`. |
| `close()` | `None` | Idempotent. Use `async with` to do this automatically. |
| `pause_runtime(timeout_ms=0)` | `dict` | v0.8.18 — quiesce the runtime. Returns `{status, duration_ms, force_cancelled_count, paused_runs_count, warnings}`. |
| `resume_runtime()` | `dict` | v0.8.18 — release the quiesce. Returns `{status, resumed_run_count, warnings}`. |
| `get_runtime_state()` | `dict` | v0.8.18 — current state. Returns `{status, paused_at, paused_run_count, snapshots_count}`. |
| `create_snapshot(description="", include_history=False, since_ts=None, max_bytes=0)` | `dict` | v0.8.18 — capture running-state JSON envelope. |
| `list_snapshots()` | `list[dict]` | v0.8.18 — metadata only; up to 200 most-recent. |
| `get_snapshot(snapshot_id)` | `dict` | v0.8.18 — full envelope including `json_content` bytes. |
| `export_snapshot(snapshot_id)` | `dict` | v0.8.18 — canonical bytes via `raw_json` for streaming consumers. |
| `restore_snapshot(snapshot_id=..., raw_json=..., include_history=False)` | `dict` | v0.8.18 — exactly one of `snapshot_id` / `raw_json`. Per-section counters returned. |
| `delete_snapshot(snapshot_id)` | `bool` | v0.8.18 — idempotent; returns True. |
| `spawn_run_batch(spawns, mode="join", timeout_ms=0)` | `dict` | v0.8.0 — spawn up to 32 runs concurrently (RFC Y); index-aligned `{spawned, results}`, per-child failures in-envelope. |
| `compact_run(run_id, reason="")` | `dict` | v0.8.0 — summarize a parked run's context. `{run_id, compacted, before_tokens, after_tokens, applied}`. |
| `resolve_probe()` | `dict` | v0.8.0 — resolver provider/model availability matrix. |
| `agent_def(input)` / `skill_def(input)` | `dict` | Substrate AgentDef / SkillDef tool; op-discriminated body. |
| `mcp_server_def` / `schedule_def` / `a2a_server_card_def` / `a2a_agent_def` / `webhook_def` / `memory_backend_def` / `operator_token_def` `(input)` | `dict` | v0.8.0 — the rest of the substrate-def family; same shape + `SubstrateToolRefusedError` contract. |
| `volume_def(input)` | `dict` | v0.9.0 — RFC AH dynamic filesystem-volume substrate; op-discriminated (create / get / list / delete / purge), tenant-confined, same `SubstrateToolRefusedError` contract. |
| `run_input(run_id, text)` | `dict` | v1.1.1 — RFC AI; steer a live interactive run. `{run_id, delivered}`. NotFound (`AgentNotFoundError`) / ResourceExhausted (`BackpressureError`) on a gone run / full queue. |
| `stream_run(run_id, from_seq=0)` | `AsyncIterator[AgentEvent]` | v1.1.1 — RFC AI; re-attach to a run's events (replay-then-tail). Operator turns replay as `steer` events (`user_input.source=="replay"`). Pair with `interactive=True` on `run_streaming` / `continue_session`. |
| `list_channels()` | `list[dict]` | v0.8.0 — declared + runtime channels with aggregate stats. |
| `publish_channel(channel, payload, scope="global", scope_id="", deliver_at="")` | `dict` | v0.8.0 — publish raw-JSON `payload` (bytes); `deliver_at` defers. |
| `subscribe_channel(channel, ...)` / `peek_channel(channel, ...)` | `dict` | v0.8.0 — long-poll / non-destructive read; `{messages, next_cursor?}`. |
| `ack_channel(channel, cursor, ...)` | `bool` | v0.8.0 — commit a channel cursor. |
| `await_channels(channels, mode="any", n=0, ...)` | `dict` | v0.8.0 — fan-in across channels (any / all / at_least). |
| `broadcast_channels(channels, payload, ...)` | `dict` | v0.8.0 — fan-out one payload to N channels. |
| `stream_user_run_states(user_id, statuses=None, agent="")` | `AsyncIterator[dict]` | v0.8.0 — stream a user's run-state transitions. |

`run_streaming` / `continue_session` / each `spawn_run_batch` child also accept
per-run `sampling` and `compaction` dict overrides (v0.8.0); an explicit
`temperature: 0.0` is preserved as deterministic.

## Errors

Every method translates gRPC error codes to typed Python exceptions:

| gRPC code | Exception |
|---|---|
| `NOT_FOUND` (with session in msg) | `SessionNotFoundError` |
| `NOT_FOUND` (with hook in msg) | `HookNotFoundError` |
| `NOT_FOUND` (otherwise — agent ctx) | `AgentNotFoundError` |
| `FAILED_PRECONDITION` (session busy) | `SessionBusyError` |
| `FAILED_PRECONDITION` (other) | `LoomcycleError` |
| `ALREADY_EXISTS` | `AgentIDInUseError` |
| `RESOURCE_EXHAUSTED` (snapshot) | `SnapshotTooLargeError` (v0.8.18) |
| `RESOURCE_EXHAUSTED` (other) | `BackpressureError` |
| `UNAUTHENTICATED` | `AuthError` |
| `UNAVAILABLE` (pause not configured) | `PauseNotConfiguredError` (v0.8.18, subclass of UnavailableError) |
| `UNAVAILABLE` (other) | `UnavailableError` |
| `NOT_FOUND` (with snapshot ctx) | `SnapshotNotFoundError` (v0.8.18) |
| `FAILED_PRECONDITION` (already pausing) | `AlreadyPausingError` (v0.8.18) |
| `FAILED_PRECONDITION` (not paused) | `NotPausedError` (v0.8.18) |
| `FAILED_PRECONDITION` (snapshot version) | `SnapshotVersionError` (v0.8.18) |
| `INVALID_ARGUMENT` / `INTERNAL` / other | `LoomcycleError` |

All exceptions inherit from `LoomcycleError` and preserve the
original `grpc.StatusCode` on `.code` for log correlation:

```python
from loomcycle import BackpressureError

try:
    async for ev in client.run_streaming(...): ...
except BackpressureError as e:
    log.warning("loomcycle backpressure (code=%s): %s", e.code, e.message)
```

## Allowed-hosts semantics

`allowed_hosts` mirrors the HTTP API's narrowing semantics:

| Value | Effect |
|---|---|
| `None` (default) | No narrowing; the operator's static allowlist is the floor. |
| `[]` | Deny-all; the agent gets no network access. |
| `["foo.com"]` | Intersection with the operator's static list. |

This is enforced server-side in `internal/tools/builtin/narrowing.go`;
`allowed_hosts` is a trust boundary — it must come from your
application code, never from a model.

## Development

```bash
# One-time setup:
python3 -m venv adapters/python/.venv
adapters/python/.venv/bin/pip install -e adapters/python[dev]

# Run tests (offline):
make python-test

# Regenerate stubs after editing proto/loomcycle.proto:
make python-proto
```

The package commits its generated `loomcycle_pb2.py` /
`loomcycle_pb2_grpc.py` so end users don't need a working `protoc`
to install. Re-run `make python-proto` whenever the proto changes.

## Live integration test

To run the example end-to-end against a local loomcycle:

```bash
# In one shell — start loomcycle with gRPC enabled:
LOOMCYCLE_GRPC_ADDR=127.0.0.1:8788 \
LOOMCYCLE_AUTH_TOKEN=devtoken \
./bin/loomcycle --config loomcycle.yaml

# In another shell — run the example:
LOOMCYCLE_GRPC_ADDR=127.0.0.1:8788 \
LOOMCYCLE_AUTH_TOKEN=devtoken \
adapters/python/.venv/bin/python examples/python-cli/main.py
```

## License

Apache-2.0. Same as loomcycle.
