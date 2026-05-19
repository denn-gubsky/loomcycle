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
    AlreadyPausingError,
    AuthError,
    BackpressureError,
    HookNotFoundError,
    InvalidArgumentError,
    LoomcycleError,
    NotPausedError,
    PauseNotConfiguredError,
    SessionBusyError,
    SessionNotFoundError,
    SnapshotNotFoundError,
    SnapshotTooLargeError,
    SnapshotVersionError,
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

    # ---- Hook management (hooks-connector series, PR D) ----

    async def register_hook(
        self,
        *,
        owner: str,
        name: str,
        phase: str,
        callback_url: str,
        agents: Optional[Sequence[str]] = None,
        tools: Optional[Sequence[str]] = None,
        fail_mode: str = "open",
        timeout_ms: int = 0,
    ) -> Mapping[str, Any]:
        """Register a pre- or post-tool webhook. Returns
        ``{"id": "hook_..."}``.

        Re-registering the same ``(owner, name)`` replaces the prior
        entry with a fresh id (idempotent app-restart contract).
        Raises ``LoomcycleError`` with ``code=INVALID_ARGUMENT`` on
        bad URL / phase / missing required fields.

        Phase is ``"pre"`` or ``"post"``; fail_mode is ``"open"``
        (default — webhook errors pass through) or ``"closed"``
        (webhook errors fail the tool call). The callback half is
        HTTP — loomcycle POSTs ``PreHookCall`` / ``PostHookCall``
        payloads to ``callback_url``; the consumer runs the
        receiver in whatever framework they use."""
        try:
            resp = await self._stub.RegisterHook(
                pb.RegisterHookRequest(
                    owner=owner,
                    name=name,
                    phase=phase,
                    agents=list(agents or []),
                    tools=list(tools or []),
                    callback_url=callback_url,
                    fail_mode=fail_mode,
                    timeout_ms=timeout_ms,
                ),
                metadata=self._auth_metadata(),
            )
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return {"id": resp.id}

    async def list_hooks(self) -> Sequence[Mapping[str, Any]]:
        """Return every currently-registered hook in registration
        order. In-memory only — empty after a loomcycle restart."""
        try:
            resp = await self._stub.ListHooks(
                pb.ListHooksRequest(),
                metadata=self._auth_metadata(),
            )
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return [_hook_to_dict(h) for h in resp.hooks]

    async def delete_hook(self, hook_id: str) -> bool:
        """Delete a hook by id. Returns ``True`` on success. Raises
        ``HookNotFoundError`` when no hook has that id.

        Success is determined by the RPC returning at all (no
        ``AioRpcError`` raised); the proto's ``deleted`` echo field
        is not inspected — the server's wire contract is "raise
        NotFound for unknown, return DeleteHookResponse for known"."""
        try:
            await self._stub.DeleteHook(
                pb.DeleteHookRequest(id=hook_id),
                metadata=self._auth_metadata(),
            )
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return True

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

    # ---- v0.8.18 Pause / Resume / State + Snapshot lifecycle ----

    async def pause_runtime(self, *, timeout_ms: int = 0) -> Mapping[str, Any]:
        """Quiesce the runtime. Idempotent tools cancel immediately;
        non-idempotent + external tools get a grace window (default
        30 s; max 5 min) then force-cancel. New /v1/runs return 503
        while paused. Returns a dict matching ``PauseRuntimeResponse``
        (status, duration_ms, force_cancelled_count, paused_runs_count,
        warnings).

        Raises ``AlreadyPausingError`` (FailedPrecondition) when the
        runtime is already pausing or paused. Raises
        ``PauseNotConfiguredError`` (Unavailable) when the deployment
        doesn't have a Store backend wired."""
        try:
            resp = await self._stub.PauseRuntime(
                pb.PauseRuntimeRequest(timeout_ms=timeout_ms),
                metadata=self._auth_metadata(),
            )
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return {
            "status": resp.status,
            "duration_ms": resp.duration_ms,
            "force_cancelled_count": resp.force_cancelled_count,
            "paused_runs_count": resp.paused_runs_count,
            "warnings": list(resp.warnings),
        }

    async def resume_runtime(self) -> Mapping[str, Any]:
        """Release the runtime quiesce. Each previously-paused run
        flips back to running; the runner goroutines re-enter their
        loops. Returns a dict matching ``ResumeRuntimeResponse``
        (status, resumed_run_count, warnings).

        Raises ``NotPausedError`` (FailedPrecondition) when the runtime
        is not paused."""
        try:
            resp = await self._stub.ResumeRuntime(
                pb.ResumeRuntimeRequest(), metadata=self._auth_metadata(),
            )
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return {
            "status": resp.status,
            "resumed_run_count": resp.resumed_run_count,
            "warnings": list(resp.warnings),
        }

    async def get_runtime_state(self) -> Mapping[str, Any]:
        """Return the current runtime quiesce state. Returns a dict
        with ``status`` (``"running"`` | ``"pausing"`` | ``"paused"``),
        ``paused_at`` (ISO8601 string or empty), ``paused_run_count``,
        and ``snapshots_count`` (best-effort count from the snapshots
        table)."""
        try:
            resp = await self._stub.GetRuntimeState(
                pb.GetRuntimeStateRequest(), metadata=self._auth_metadata(),
            )
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return {
            "status": resp.status,
            "paused_at": _ts_to_iso(resp.paused_at),
            "paused_run_count": resp.paused_run_count,
            "snapshots_count": resp.snapshots_count,
        }

    async def create_snapshot(
        self,
        *,
        description: str = "",
        include_history: bool = False,
        since_ts: Optional[str] = None,
        max_bytes: int = 0,
    ) -> Mapping[str, Any]:
        """Capture running-state into a per-section-semver JSON
        envelope. Returns a SnapshotDescriptor dict (snapshot_id,
        created_at, size_bytes, includes_history, since_ts, description,
        format_version). The envelope itself is fetched via
        ``get_snapshot`` / ``export_snapshot``.

        ``since_ts`` is RFC3339; only honoured when ``include_history``
        is True. ``max_bytes`` overrides the server's
        LOOMCYCLE_SNAPSHOT_MAX_BYTES cap for this call.

        Raises ``SnapshotTooLargeError`` (ResourceExhausted) when the
        envelope exceeds the cap."""
        req = pb.CreateSnapshotRequest(
            include_history=include_history,
            description=description,
            max_bytes=max_bytes,
        )
        if since_ts:
            # FromJsonString raises a bare ValueError on malformed RFC3339.
            # Catch + re-raise as InvalidArgumentError so callers can
            # discriminate client-side validation from server-returned
            # errors (which carry a non-None gRPC code).
            try:
                req.since_ts.FromJsonString(since_ts)
            except ValueError as exc:
                raise InvalidArgumentError(
                    f"create_snapshot: invalid since_ts (expected RFC3339): {exc}"
                ) from exc
        try:
            resp = await self._stub.CreateSnapshot(
                req, metadata=self._auth_metadata(),
            )
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return _snapshot_descriptor_to_dict(resp)

    async def list_snapshots(self) -> Sequence[Mapping[str, Any]]:
        """List captured snapshots (most-recent first; capped at 200).
        Returns metadata only — use ``get_snapshot`` / ``export_snapshot``
        for the JSON envelope."""
        try:
            resp = await self._stub.ListSnapshots(
                pb.ListSnapshotsRequest(), metadata=self._auth_metadata(),
            )
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return [_snapshot_descriptor_to_dict(s) for s in resp.snapshots]

    async def get_snapshot(self, snapshot_id: str) -> Mapping[str, Any]:
        """Return the full snapshot envelope including JSON content.
        Returns a dict with snapshot_id, created_at, description,
        format_version, size_bytes, and ``json_content`` (raw bytes —
        caller decodes via ``json.loads``).

        Raises ``SnapshotNotFoundError`` (NotFound) when no snapshot
        matches."""
        try:
            resp = await self._stub.GetSnapshot(
                pb.GetSnapshotRequest(snapshot_id=snapshot_id),
                metadata=self._auth_metadata(),
            )
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return {
            "snapshot_id": resp.snapshot_id,
            "created_at": _ts_to_iso(resp.created_at),
            "description": resp.description,
            "format_version": resp.format_version,
            "size_bytes": resp.size_bytes,
            "json_content": bytes(resp.json_content),
        }

    async def export_snapshot(self, snapshot_id: str) -> Mapping[str, Any]:
        """Return canonical envelope bytes for a snapshot id.
        Returns a dict with snapshot_id, file_path (empty unless the
        server materialised to disk), checksum (ditto), size_bytes,
        and ``raw_json`` (raw bytes for streaming consumers).

        Raises ``SnapshotNotFoundError`` (NotFound) when no snapshot
        matches."""
        try:
            resp = await self._stub.ExportSnapshot(
                pb.ExportSnapshotRequest(snapshot_id=snapshot_id),
                metadata=self._auth_metadata(),
            )
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return {
            "snapshot_id": resp.snapshot_id,
            "file_path": resp.file_path,
            "checksum": resp.checksum,
            "size_bytes": resp.size_bytes,
            "raw_json": bytes(resp.raw_json),
        }

    async def restore_snapshot(
        self,
        *,
        snapshot_id: str = "",
        raw_json: Optional[bytes] = None,
        include_history: bool = False,
    ) -> Mapping[str, Any]:
        """Restore from a same-instance ``snapshot_id`` OR cross-
        instance ``raw_json`` bytes. Exactly one must be supplied.
        Idempotent: ON CONFLICT DO NOTHING per row; counters reflect
        rows actually written.

        Returns a dict with per-section counters
        (memory_restored, paused_runs_restored, transcript_events_restored,
        synthesized_sessions, etc.) plus warnings + format_migrations.

        Raises ``SnapshotNotFoundError`` (NotFound) when ``snapshot_id``
        doesn't exist. Raises ``SnapshotVersionError`` (FailedPrecondition)
        when a section's declared version is newer than the reader
        supports."""
        if not snapshot_id and not raw_json:
            raise InvalidArgumentError("restore_snapshot: snapshot_id or raw_json required")
        if snapshot_id and raw_json:
            raise InvalidArgumentError("restore_snapshot: pass only one of snapshot_id or raw_json")
        req = pb.RestoreSnapshotRequest(
            snapshot_id=snapshot_id,
            raw_json=raw_json or b"",
            include_history=include_history,
        )
        try:
            resp = await self._stub.RestoreSnapshot(
                req, metadata=self._auth_metadata(),
            )
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return {
            "agent_defs_restored": resp.agent_defs_restored,
            "agent_def_active_restored": resp.agent_def_active_restored,
            "memory_restored": resp.memory_restored,
            "channel_messages_restored": resp.channel_messages_restored,
            "channel_cursors_restored": resp.channel_cursors_restored,
            "evaluations_restored": resp.evaluations_restored,
            "paused_runs_restored": resp.paused_runs_restored,
            "synthesized_sessions": resp.synthesized_sessions,
            "transcript_events_restored": resp.transcript_events_restored,
            "interaction_history_restored": resp.interaction_history_restored,
            "warnings": list(resp.warnings),
            "format_migrations": list(resp.format_migrations),
        }

    async def delete_snapshot(self, snapshot_id: str) -> bool:
        """Delete a snapshot. Idempotent — succeeds whether or not the
        row existed (mirrors HTTP DELETE /v1/_snapshots/{id} = 204).
        Returns True on success."""
        try:
            resp = await self._stub.DeleteSnapshot(
                pb.DeleteSnapshotRequest(snapshot_id=snapshot_id),
                metadata=self._auth_metadata(),
            )
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return resp.deleted

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
        tenant_id: str = "",
        user_tier: str = "",
        user_bearer: str = "",
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

        Per-run policy fields (v0.8.x):

          - ``tenant_id``    — recorded on a fresh session; ignored
            for continuations.
          - ``user_tier``    — v0.8.2+ tier policy name (maps to
            ``cfg.UserTiers[<name>]``). Server returns
            ``InvalidArgumentError`` (INVALID_ARGUMENT) on unknown
            tier. Empty falls through to ``default``.
          - ``user_bearer``  — v0.8.x+ per-run MCP bearer substituted
            into outbound MCP headers containing
            ``${run.user_bearer}``. Charset
            ``[A-Za-z0-9._\\-+/=]{16,512}``. Sub-agents inherit
            identically. Never persisted, never logged in full.

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
            tenant_id=tenant_id,
            user_tier=user_tier,
            user_bearer=user_bearer,
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
        user_tier: str = "",
        user_bearer: str = "",
        on_handle: Optional[Callable[["RunHandle"], None]] = None,
    ) -> AsyncIterator[AgentEvent]:
        """Continue an existing session. Same yield shape as
        ``run_streaming``; the agent + user_id + tenant_id are
        inherited from the existing session row server-side.

        ``user_tier`` and ``user_bearer`` are per-call (not session-
        bound) — a user upgrading mid-session sees the new tier
        applied to the next continuation; the bearer threads through
        for ``${run.user_bearer}`` substitution on each turn. See
        ``run_streaming`` for the policy field semantics.

        Sync-returning — see ``run_streaming`` for consumption
        pattern."""
        req = pb.ContinueRequest(
            session_id=session_id,
            segments=_segments_to_proto(segments),
            allowed_tools=list(allowed_tools or ()),
            web_search_filter=web_search_filter,
            agent_id=agent_id,
            user_tier=user_tier,
            user_bearer=user_bearer,
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


def _hook_to_dict(h: pb.Hook) -> Mapping[str, Any]:
    """Convert proto Hook → public dict. Mirrors hooks.Hook JSON
    field names exactly so consumers porting from HTTP /v1/hooks
    don't have to relearn keys."""
    return {
        "id": h.id,
        "owner": h.owner,
        "name": h.name,
        "phase": h.phase,
        "agents": list(h.agents),
        "tools": list(h.tools),
        "callback_url": h.callback_url,
        "fail_mode": h.fail_mode,
        "timeout_ms": h.timeout_ms,
        "registered_at": _ts_to_iso(h.registered_at) if h.HasField("registered_at") else "",
    }


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


def _snapshot_descriptor_to_dict(d: pb.SnapshotDescriptor) -> Mapping[str, Any]:
    """Convert proto SnapshotDescriptor → public dict. Reused by
    create_snapshot + list_snapshots."""
    return {
        "snapshot_id": d.snapshot_id,
        "created_at": _ts_to_iso(d.created_at),
        "size_bytes": d.size_bytes,
        "includes_history": d.includes_history,
        "since_ts": _ts_to_iso(d.since_ts) if d.HasField("since_ts") else "",
        "description": d.description,
        "format_version": d.format_version,
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

    Discriminates ``NotFound`` between session/hook/agent by
    inspecting the status message — the server's wire-stable
    strings are the source of truth:

      - ``"session not found"`` (Continue, GetTranscript) →
        ``SessionNotFoundError``
      - ``"no hook with id"`` (DeleteHook) → ``HookNotFoundError``
        — must precede the agent fallback since the hooks message
        doesn't say "agent".
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
        # v0.8.18: snapshot not-found gets its own typed error to
        # distinguish from session / agent NotFound cases that
        # other RPCs return.
        #
        # Priority order (most-specific first): "snapshot" wins
        # over "session" wins over "agent" (the default). This
        # matters when a server message contains multiple keywords —
        # e.g., "restore_snapshot: session_id snap_sess_X not found"
        # routes to SnapshotNotFoundError rather than
        # SessionNotFoundError because the snapshot is the operation
        # that failed; the session reference is incidental.
        # If new RPCs introduce overlapping keywords, document the
        # priority here and add a regression test in
        # tests/test_pause_snapshot_errors.py.
        if "snapshot" in msg_lower:
            raise SnapshotNotFoundError(msg, code=code) from err
        if "session" in msg_lower:
            raise SessionNotFoundError(msg, code=code) from err
        if "hook" in msg_lower:
            raise HookNotFoundError(msg, code=code) from err
        raise AgentNotFoundError(msg, code=code) from err
    if code == grpc.StatusCode.FAILED_PRECONDITION:
        # Server uses FailedPrecondition for ErrSessionRequired /
        # ErrSessionBusy / pause-state mismatches (AlreadyPausing /
        # NotPaused) / snapshot version skew. The message string is
        # the discriminator (matches connector.Err* sentinels).
        if "already pausing" in msg_lower or "already paused" in msg_lower:
            raise AlreadyPausingError(msg, code=code) from err
        if "not paused" in msg_lower:
            raise NotPausedError(msg, code=code) from err
        if "snapshot section version" in msg_lower or "version too new" in msg_lower or "version unknown" in msg_lower:
            raise SnapshotVersionError(msg, code=code) from err
        if "session busy" in msg_lower or "another request" in msg_lower:
            raise SessionBusyError(msg, code=code) from err
        raise LoomcycleError(msg, code=code) from err
    if code == grpc.StatusCode.ALREADY_EXISTS:
        raise AgentIDInUseError(msg, code=code) from err
    if code == grpc.StatusCode.RESOURCE_EXHAUSTED:
        # v0.8.18: snapshot-too-large reuses ResourceExhausted; the
        # message discriminates from BackpressureError (concurrency
        # semaphore rejection).
        if "snapshot" in msg_lower:
            raise SnapshotTooLargeError(msg, code=code) from err
        raise BackpressureError(msg, code=code) from err
    if code == grpc.StatusCode.UNAUTHENTICATED:
        raise AuthError(msg, code=code) from err
    if code == grpc.StatusCode.UNAVAILABLE:
        # v0.8.18: pause-not-configured is a specific Unavailable
        # subcase (operator hasn't wired a pause Manager) — distinct
        # from generic network failures.
        if "pause manager not configured" in msg_lower or "pause_not_configured" in msg_lower:
            raise PauseNotConfiguredError(msg, code=code) from err
        raise UnavailableError(msg, code=code) from err
    if code == grpc.StatusCode.INVALID_ARGUMENT:
        raise LoomcycleError(msg, code=code) from err
    raise LoomcycleError(msg, code=code) from err


# Help the linter / mypy understand asyncio is in use even when
# imports are wrapped in TYPE_CHECKING.
_ = asyncio
