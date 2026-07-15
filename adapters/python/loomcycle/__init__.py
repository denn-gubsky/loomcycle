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
    spawn_run_batch(...)      — spawn up to 32 runs concurrently (RFC Y)
    compact_run(...)          — summarize a parked run's context
    get_agent(...)            — read one agent's status + usage
    cancel_agent(...)         — cancel a live agent (cascades to children)
    list_user_agents(...)     — list a user's recent runs
    stream_user_run_states(...) — stream a user's run-state transitions
    get_transcript(...)       — read the persisted event log for a session
    resolve_probe()           — resolver provider/model availability matrix
    health()                  — liveness probe

As of v1.1.1 the client covers all 42 gRPC RPCs: the substrate-def family
(agent_def / skill_def / mcp_server_def / schedule_def / a2a_server_card_def /
a2a_agent_def / webhook_def / memory_backend_def / operator_token_def /
volume_def / team_def), the channel ops (list_channels / publish_channel /
subscribe_channel / peek_channel / ack_channel / await_channels /
broadcast_channels), the RFC AI interactive session (run_input + stream_run +
an ``interactive=True`` flag on run_streaming / continue_session),
pause/resume/state, the snapshot lifecycle, and hook
management. run_streaming / continue_session /
spawn_run_batch accept per-run ``sampling`` + ``compaction`` overrides. The
HTTP-only surface (memory-entry admin, interruptions, library enumeration, the
LLM gateway, whoami/list-users) has no gRPC RPC and is not exposed here.

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
from .events import (
    AgentEvent,
    ToolUse,
    Usage,
    Retry,
    HostWidening,
    AwaitingInput,
    UserInput,
    LimitInfo,
)
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
    SubstrateToolRefusedError,
    InvalidArgumentError,
)

__all__ = [
    "LoomcycleClient",
    "RunHandle",
    "AgentEvent",
    "ToolUse",
    "Usage",
    "Retry",
    "HostWidening",
    "AwaitingInput",
    "UserInput",
    "LimitInfo",
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
    # v0.8.22 substrate admin
    "SubstrateToolRefusedError",
]

__version__ = "1.21.0"
