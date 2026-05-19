# loomcycle — async Python client

`loomcycle` is the async Python client for [loomcycle][1]'s gRPC API
(introduced in v0.5.5). It exposes the seven RPC methods on the
`Loomcycle` service through an ergonomic `LoomcycleClient` class —
no need to import generated protobuf types in your application code.

[1]: https://github.com/denn-gubsky/loomcycle

## Status

- Wraps loomcycle ≥ v0.5.5's gRPC server (`LOOMCYCLE_GRPC_ADDR`).
- Async-only (`grpc.aio`). Python 3.9+.
- Equivalent surface to the TypeScript adapter at `adapters/ts/`.
- Production tag: `0.5.5` (matches loomcycle release that ships gRPC).

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
| `RESOURCE_EXHAUSTED` | `BackpressureError` |
| `UNAUTHENTICATED` | `AuthError` |
| `UNAVAILABLE` | `UnavailableError` |
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
