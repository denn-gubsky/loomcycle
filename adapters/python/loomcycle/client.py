"""Async LoomcycleClient over loomcycle's gRPC API.

The class is a thin wrapper over the generated ``loomcycle_pb2_grpc``
stubs. It exists so callers don't have to:

  - Import generated protobuf modules directly (which leak codegen
    versioning into user code).
  - Build ``loomcycle_pb2.RunRequest`` by hand for every call.
  - Translate gRPC ``codes.*`` into try/except branches (we map to
    typed exceptions in ``errors.py``).
  - Decode the synthetic ``"session"`` / ``"agent"`` registration
    frames the gRPC server emits before the first provider event
    (we capture them into ``RunHandle``).

Construct one client per ``(target, auth_token)`` pair and reuse it
across runs — the underlying ``grpc.aio.Channel`` is connection-pooled
and threadsafe.
"""

from __future__ import annotations

import asyncio
import json
from dataclasses import dataclass
from typing import (
    Any,
    AsyncIterator,
    Callable,
    Iterable,
    List,
    Mapping,
    Optional,
    Sequence,
    Tuple,
)

import grpc
import grpc.aio

from ._generated import loomcycle_pb2 as pb
from ._generated import loomcycle_pb2_grpc as pb_grpc

from .events import AgentEvent
from .errors import (
    AgentIDInUseError,
    AgentNotFoundError,
    AuthError,
    BackpressureError,
    LoomcycleError,
    SessionBusyError,
    SessionNotFoundError,
    UnavailableError,
)


# Type aliases for the public input shape. Callers can pass plain
# dicts (the canonical form used in the README + examples) or
# typed objects — we accept both for ergonomic friendliness.
PromptContent = Mapping[str, Any]
PromptSegment = Mapping[str, Any]


@dataclass(frozen=True)
class RunHandle:
    """Captures the IDs the gRPC server emits in its synthetic
    registration frames before the first provider event.

    Returned alongside a stream when callers want to know the
    server-assigned ``run_id`` / ``session_id`` without having to
    re-decode the first two stream frames themselves.
    """

    agent_id: str
    run_id: str
    session_id: str
    parent_agent_id: str = ""


class LoomcycleClient:
    """Async gRPC client for loomcycle.

    Construct once, reuse across runs. Caller is responsible for
    calling ``await client.close()`` when done; using
    ``async with LoomcycleClient(...)`` does this automatically.
    """

    def __init__(
        self,
        *,
        target: str = "127.0.0.1:8788",
        auth_token: Optional[str] = None,
        # Channel options for advanced operators (TLS, keepalive
        # tuning). Empty by default — cleartext H2.
        channel_options: Optional[Sequence[Tuple[str, Any]]] = None,
        # Pre-built channel for tests + advanced wiring (e.g. a
        # dialed-in mock or a TLS-credentialled channel).
        channel: Optional[grpc.aio.Channel] = None,
    ) -> None:
        self._auth_token = auth_token
        if channel is not None:
            self._channel = channel
            self._owns_channel = False
        else:
            opts = list(channel_options or ())
            self._channel = grpc.aio.insecure_channel(target, options=opts)
            self._owns_channel = True
        self._stub = pb_grpc.LoomcycleStub(self._channel)

    async def __aenter__(self) -> "LoomcycleClient":
        return self

    async def __aexit__(self, exc_type, exc, tb) -> None:
        await self.close()

    async def close(self) -> None:
        """Close the underlying channel. Idempotent. Skips when the
        client was constructed with a caller-supplied channel
        (caller owns lifecycle)."""
        if self._owns_channel:
            await self._channel.close()
            self._owns_channel = False

    # ---- Metadata RPCs ----

    async def health(self) -> Mapping[str, Any]:
        """Liveness probe. Unauthenticated. Returns a dict matching
        the proto ``HealthResponse`` shape."""
        try:
            resp = await self._stub.Health(pb.HealthRequest())
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return {
            "ok": resp.ok,
            "commit": resp.commit,
            "built": resp.built,
            "uptime_seconds": resp.uptime_seconds,
        }

    async def get_agent(self, agent_id: str) -> Mapping[str, Any]:
        """Read one agent's status + usage stats. Raises
        ``AgentNotFoundError`` if the ID is unknown."""
        try:
            resp = await self._stub.GetAgent(
                pb.GetAgentRequest(agent_id=agent_id),
                metadata=self._auth_metadata(),
            )
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return _agent_to_dict(resp)

    async def cancel_agent(
        self, agent_id: str, *, reason: str = ""
    ) -> int:
        """Cancel a live agent (cascades to children via
        ``parent_agent_id``). Returns the count of agents cancelled
        (1 + descendants). 0 when the agent already terminated.
        Raises ``AgentNotFoundError`` if the ID is unknown."""
        try:
            resp = await self._stub.CancelAgent(
                pb.CancelAgentRequest(agent_id=agent_id, reason=reason),
                metadata=self._auth_metadata(),
            )
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return resp.cancelled_count

    async def list_user_agents(
        self,
        user_id: str,
        *,
        status: str = "",
    ) -> Sequence[Mapping[str, Any]]:
        """List a user's recent agent runs, optionally filtered by
        status (``"running"`` | ``"completed"`` | ``"failed"`` |
        ``"cancelled"``; empty = all)."""
        try:
            resp = await self._stub.ListUserAgents(
                pb.ListUserAgentsRequest(user_id=user_id, status=status),
                metadata=self._auth_metadata(),
            )
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return [_agent_to_dict(a) for a in resp.agents]

    async def get_transcript(self, session_id: str) -> Sequence[Mapping[str, Any]]:
        """Read the full event log for a session. Each entry is a
        dict with ``seq``, ``run_id``, ``ts``, ``type``, ``payload``
        (raw JSON bytes — caller decodes via ``json.loads``).
        Raises ``SessionNotFoundError`` if the session is unknown."""
        try:
            resp = await self._stub.GetTranscript(
                pb.GetTranscriptRequest(session_id=session_id),
                metadata=self._auth_metadata(),
            )
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return [
            {
                "seq": e.seq,
                "session_id": e.session_id,
                "run_id": e.run_id,
                "ts": _ts_to_iso(e.ts),
                "type": e.type,
                "payload": bytes(e.payload),
            }
            for e in resp.events
        ]

    # ---- Streaming RPCs ----

    def run_streaming(
        self,
        *,
        agent: str,
        segments: Iterable[PromptSegment],
        allowed_tools: Optional[Sequence[str]] = None,
        allowed_hosts: Optional[Sequence[str]] = None,
        web_search_filter: str = "",
        user_id: str = "",
        agent_id: str = "",
        session_id: str = "",
        on_handle: Optional[Callable[["RunHandle"], None]] = None,
    ) -> AsyncIterator[AgentEvent]:
        """Drive one agent run end-to-end, yielding each
        ``AgentEvent`` as it arrives.

        The synthetic ``"session"`` and ``"agent"`` registration
        frames the server emits before the first provider event are
        NOT yielded to the caller. Pass an ``on_handle`` callback
        (sync function ``(RunHandle) -> None``) to receive the
        captured ``RunHandle`` once the agent registers.

        ``allowed_hosts`` semantics mirror the HTTP API:

          - ``None``        → no narrowing
          - ``[]``          → deny-all
          - ``["foo.com"]`` → intersection with operator's static list

        Returns an ``AsyncIterator[AgentEvent]`` directly (this
        method is sync — the underlying gRPC stub call is lazy
        enough that no ``await`` is needed at the call site;
        consume with ``async for``).
        """
        req = pb.RunRequest(
            agent=agent,
            session_id=session_id,
            segments=_segments_to_proto(segments),
            allowed_tools=list(allowed_tools or ()),
            web_search_filter=web_search_filter,
            user_id=user_id,
            agent_id=agent_id,
        )
        if allowed_hosts is not None:
            req.allowed_hosts.list.extend(allowed_hosts)

        return self._drive_stream(
            self._stub.Run(req, metadata=self._auth_metadata()),
            on_handle=on_handle,
        )

    def continue_session(
        self,
        *,
        session_id: str,
        segments: Iterable[PromptSegment],
        allowed_tools: Optional[Sequence[str]] = None,
        allowed_hosts: Optional[Sequence[str]] = None,
        web_search_filter: str = "",
        agent_id: str = "",
        on_handle: Optional[Callable[["RunHandle"], None]] = None,
    ) -> AsyncIterator[AgentEvent]:
        """Continue an existing session. Same yield shape as
        ``run_streaming``; the agent + user_id are inherited from
        the existing session row server-side. Sync-returning — see
        ``run_streaming`` for consumption pattern."""
        req = pb.ContinueRequest(
            session_id=session_id,
            segments=_segments_to_proto(segments),
            allowed_tools=list(allowed_tools or ()),
            web_search_filter=web_search_filter,
            agent_id=agent_id,
        )
        if allowed_hosts is not None:
            req.allowed_hosts.list.extend(allowed_hosts)

        return self._drive_stream(
            self._stub.Continue(req, metadata=self._auth_metadata()),
            on_handle=on_handle,
        )

    # ---- Internal ----

    async def _drive_stream(
        self,
        stream: grpc.aio.UnaryStreamCall,
        *,
        on_handle: Optional[Any],
    ) -> AsyncIterator[AgentEvent]:
        """Consume the gRPC server-stream, capture the synthetic
        registration frames into a RunHandle, and yield every
        provider event as an AgentEvent. Translates gRPC errors
        to typed Python exceptions on the way out.
        """
        # Capture the first two synthetic frames before yielding to
        # the caller. The contract from internal/api/grpc/server.go
        # driveStream:
        #
        #   Frame 0: type="session", text=<session_id>
        #   Frame 1: type="agent", text=<agent_id>,
        #            stop_reason=<parent_agent_id>,
        #            error=<JSON envelope: {agent_id,run_id,session_id,parent_agent_id}>
        #
        # If the server returns an error (e.g. ErrUnknownAgent →
        # InvalidArgument) before any frame, the first await raises
        # AioRpcError. Translate via _raise_from_grpc.
        registration_seen = 0
        run_handle: Optional[RunHandle] = None

        try:
            async for raw in stream:
                t = raw.type
                if t == "session" and registration_seen == 0:
                    # Frame 0 — session ID. Keep until frame 1
                    # arrives so we can build the RunHandle in one
                    # shot.
                    registration_seen = 1
                    session_id = raw.text
                    continue
                if t == "agent" and registration_seen == 1:
                    # Frame 1 — agent ID + JSON envelope. Build the
                    # RunHandle, fire the callback, suppress the
                    # frame from the caller's iterator.
                    registration_seen = 2
                    parent_agent_id = raw.stop_reason
                    run_id = ""
                    try:
                        env = json.loads(raw.error or "{}")
                        run_id = str(env.get("run_id", ""))
                    except (ValueError, TypeError):
                        # Server changed the envelope shape — leave
                        # run_id empty rather than crashing the stream.
                        pass
                    run_handle = RunHandle(
                        agent_id=raw.text,
                        run_id=run_id,
                        session_id=session_id,  # type: ignore[has-type]
                        parent_agent_id=parent_agent_id,
                    )
                    if on_handle is not None:
                        try:
                            on_handle(run_handle)
                        except Exception:
                            # The callback is best-effort — a
                            # callback exception must not tear down
                            # the stream. Swallow + continue.
                            pass
                    continue
                # Real provider event — yield to caller.
                yield AgentEvent._from_proto(raw)
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)

    def _auth_metadata(self) -> Sequence[Tuple[str, str]]:
        """gRPC metadata header carrying the bearer token. Empty
        when ``auth_token`` wasn't supplied (open-mode server)."""
        if not self._auth_token:
            return ()
        return (("authorization", f"Bearer {self._auth_token}"),)


# ---- Module-private helpers ----


def _segments_to_proto(segments: Iterable[PromptSegment]) -> List[pb.PromptSegment]:
    """Convert the public dict-shape segments to proto messages."""
    out: List[pb.PromptSegment] = []
    for s in segments:
        blocks = []
        for b in s.get("content", []):
            blocks.append(
                pb.PromptContentBlock(
                    type=b.get("type", "trusted-text"),
                    text=b.get("text", ""),
                    cacheable=bool(b.get("cacheable", False)),
                )
            )
        out.append(pb.PromptSegment(role=s.get("role", "user"), content=blocks))
    return out


def _agent_to_dict(a: pb.Agent) -> Mapping[str, Any]:
    """Convert proto Agent → public dict. Mirrors HTTP's
    agentResponse JSON shape so adapters porting from HTTP+SSE
    don't have to relearn keys."""
    return {
        "agent_id": a.agent_id,
        "run_id": a.run_id,
        "session_id": a.session_id,
        "user_id": a.user_id,
        "parent_agent_id": a.parent_agent_id,
        "status": a.status,
        "started_at": _ts_to_iso(a.started_at),
        "completed_at": _ts_to_iso(a.completed_at) if a.HasField("completed_at") else None,
        "stop_reason": a.stop_reason,
        "error": a.error,
        "usage": {
            "input_tokens": a.usage.input_tokens,
            "output_tokens": a.usage.output_tokens,
            "cache_creation_tokens": a.usage.cache_creation_tokens,
            "cache_read_tokens": a.usage.cache_read_tokens,
            "model": a.usage.model,
        } if a.HasField("usage") else None,
        "last_heartbeat_at": _ts_to_iso(a.last_heartbeat_at) if a.HasField("last_heartbeat_at") else None,
        "live": a.live,
    }


def _ts_to_iso(ts) -> str:
    """Convert google.protobuf.Timestamp → ISO 8601 string. Empty
    timestamps surface as ``""`` (the caller distinguishes via the
    field's ``HasField`` rather than a sentinel)."""
    if ts is None:
        return ""
    if ts.seconds == 0 and ts.nanos == 0:
        return ""
    return ts.ToJsonString()


def _raise_from_grpc(err: grpc.aio.AioRpcError) -> "None":
    """Translate a gRPC error into one of our typed exceptions.
    Mirrors the inverse of internal/api/grpc/server.go's
    ``mapRunnerErr`` plus the direct ``codes.NotFound`` emissions
    in ``GetAgent`` / ``CancelAgent`` / ``GetTranscript``.

    Discriminates ``NotFound`` between session-vs-agent by
    inspecting the status message — the server's wire-stable
    strings are the source of truth:

      - ``"session not found"`` (Continue, GetTranscript) →
        ``SessionNotFoundError``
      - ``"no live run for"`` / ``"no run found for agent_id"``
        (GetAgent, CancelAgent) → ``AgentNotFoundError``

    Keeping the function context-free means the streaming path
    (which can't easily thread call-kind context through
    ``_drive_stream``) routes correctly without special-casing.

    Always raises — the function is annotated as returning ``None``
    only because Python's type system needs a return for ``except``
    handlers that fall through to it.
    """
    code = err.code()
    msg = err.details() or str(err)
    msg_lower = msg.lower()
    if code == grpc.StatusCode.NOT_FOUND:
        if "session" in msg_lower:
            raise SessionNotFoundError(msg, code=code) from err
        raise AgentNotFoundError(msg, code=code) from err
    if code == grpc.StatusCode.FAILED_PRECONDITION:
        # Server uses FailedPrecondition for both ErrSessionRequired
        # and ErrSessionBusy. The message carries the discriminator.
        if "session busy" in msg_lower or "another request" in msg_lower:
            raise SessionBusyError(msg, code=code) from err
        raise LoomcycleError(msg, code=code) from err
    if code == grpc.StatusCode.ALREADY_EXISTS:
        raise AgentIDInUseError(msg, code=code) from err
    if code == grpc.StatusCode.RESOURCE_EXHAUSTED:
        raise BackpressureError(msg, code=code) from err
    if code == grpc.StatusCode.UNAUTHENTICATED:
        raise AuthError(msg, code=code) from err
    if code == grpc.StatusCode.UNAVAILABLE:
        raise UnavailableError(msg, code=code) from err
    if code == grpc.StatusCode.INVALID_ARGUMENT:
        raise LoomcycleError(msg, code=code) from err
    raise LoomcycleError(msg, code=code) from err


# Help the linter / mypy understand asyncio is in use even when
# imports are wrapped in TYPE_CHECKING.
_ = asyncio
