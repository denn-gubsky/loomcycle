"""Async Python client for loomcycle's gRPC API.

Quick start:

    import asyncio
    from loomcycle import LoomcycleClient

    async def main():
        client = LoomcycleClient(target="127.0.0.1:8788", auth_token="...")
        try:
            async for ev in client.run_streaming(
                agent="default",
                segments=[{"role": "user", "content": [
                    {"type": "trusted-text", "text": "Hello, world."}
                ]}],
            ):
                if ev.type == "text":
                    print(ev.text, end="", flush=True)
        finally:
            await client.close()

    asyncio.run(main())

The client surface mirrors the gRPC service in proto/loomcycle.proto:

    run_streaming(...)        — server-stream events from a fresh run
    continue_session(...)     — server-stream events from a continuation
    get_agent(...)            — read one agent's status + usage
    cancel_agent(...)         — cancel a live agent (cascades to children)
    list_user_agents(...)     — list a user's recent runs
    get_transcript(...)       — read the persisted event log for a session
    health()                  — liveness probe

All methods are async. Server-streaming methods return an
``AsyncIterator[AgentEvent]``. The synthetic ``"session"`` and
``"agent"`` registration frames the gRPC server emits before the
first provider event are NOT yielded to the caller — instead they're
captured into ``RunHandle`` (returned alongside the iterator when
the caller wants the IDs without re-decoding the first frames).

For environments that can't use gRPC, use HTTP+SSE through your own
``httpx``-based client; loomcycle's HTTP+SSE surface is
documented in the project README.
"""

from .client import LoomcycleClient, RunHandle
from .events import AgentEvent, ToolUse, Usage, Retry
from .errors import (
    LoomcycleError,
    AgentNotFoundError,
    SessionNotFoundError,
    SessionBusyError,
    AgentIDInUseError,
    BackpressureError,
    AuthError,
    UnavailableError,
    HookNotFoundError,
    # v0.8.18 — pause/snapshot typed errors.
    PauseNotConfiguredError,
    AlreadyPausingError,
    NotPausedError,
    SnapshotNotFoundError,
    SnapshotTooLargeError,
    SnapshotVersionError,
    InvalidArgumentError,
)

__all__ = [
    "LoomcycleClient",
    "RunHandle",
    "AgentEvent",
    "ToolUse",
    "Usage",
    "Retry",
    "LoomcycleError",
    "AgentNotFoundError",
    "SessionNotFoundError",
    "SessionBusyError",
    "AgentIDInUseError",
    "BackpressureError",
    "AuthError",
    "UnavailableError",
    "HookNotFoundError",
    # v0.8.18 additions.
    "PauseNotConfiguredError",
    "AlreadyPausingError",
    "NotPausedError",
    "SnapshotNotFoundError",
    "SnapshotTooLargeError",
    "SnapshotVersionError",
    "InvalidArgumentError",
]

__version__ = "0.6.0"
