# loomcycle — async Python client

`loomcycle` is the async Python client for [loomcycle][1]'s gRPC API
(introduced in v0.5.5; expanded to ~22 methods in v0.6.0). It exposes
the full RPC surface (run streaming, agent metadata, transcript,
pause / snapshot lifecycle, memory admin, interruption resolve) on
the `Loomcycle` service through an ergonomic `LoomcycleClient` class —
no need to import generated protobuf types in your application code.

[1]: https://github.com/denn-gubsky/loomcycle

## Status

- Wraps loomcycle ≥ v0.5.5's gRPC server (`LOOMCYCLE_GRPC_ADDR`).
- Async-only (`grpc.aio`). Python 3.9+.
- Equivalent surface to the TypeScript adapter at `adapters/ts/`.
- Production tag: `0.6.0` (matches loomcycle v0.8.18 release that
  expanded the adapter to full ~22-method parity).

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

## Errors

Every method translates gRPC error codes to typed Python exceptions:

| gRPC code | Exception |
|---|---|
| `NOT_FOUND` (with agent_id ctx) | `AgentNotFoundError` |
| `NOT_FOUND` (with session_id ctx) | `SessionNotFoundError` |
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
