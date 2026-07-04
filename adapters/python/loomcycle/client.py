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
    SubstrateToolRefusedError,
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

    async def usage_report(
        self,
        *,
        group_by: Sequence[str] = (),
        from_time: str = "",
        to_time: str = "",
        tenant: str = "",
    ) -> Mapping[str, Any]:
        """Aggregated token-usage + cost report (RFC AV). Group by any of
        ``tenant``/``user``/``provider``/``model``/``source`` over an optional
        RFC3339 window (``from_time``/``to_time``); group by ``source`` for the
        operator-vs-tenant split. Tenant-scoped server-side (a tenant principal
        sees only its own tenant; ``tenant`` is an admin-only focus). Returns
        ``{"group_by": [...], "rows": [{...}, ...]}``."""
        try:
            resp = await self._stub.UsageReport(
                pb.UsageReportRequest(
                    group_by=list(group_by),
                    from_time=from_time,
                    to_time=to_time,
                    tenant=tenant,
                ),
                metadata=self._auth_metadata(),
            )
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return {
            "group_by": list(resp.group_by),
            "rows": [
                {
                    "tenant_id": r.tenant_id,
                    "user_id": r.user_id,
                    "provider": r.provider,
                    "model": r.model,
                    "credential_source": r.credential_source,
                    "input_tokens": r.input_tokens,
                    "output_tokens": r.output_tokens,
                    "cache_creation_tokens": r.cache_creation_tokens,
                    "cache_read_tokens": r.cache_read_tokens,
                    "cost": r.cost,
                    "currency": r.currency,
                    "call_count": r.call_count,
                    "unpriced_calls": r.unpriced_calls,
                }
                for r in resp.rows
            ],
        }

    # ---- RFC AW per-scope token budgets (TokenLimit RPC) ----

    async def list_token_limits(self, *, tenant: str = "") -> Sequence[Mapping[str, Any]]:
        """List the per-scope token budgets visible to the caller (RFC AW), each
        with its live month-to-date usage. Tenant-scoped server-side: a tenant
        principal sees only its own tenant's budgets; ``tenant`` is an admin-only
        focus. Returns a list of dicts with ``tenant_id``/``scope``/``scope_id``/
        ``soft_limit``/``hard_limit`` (``None`` when a tier is unset)/``used``/
        ``updated_at``/``updated_by``."""
        try:
            resp = await self._stub.TokenLimit(
                pb.TokenLimitRequest(op="list", tenant=tenant),
                metadata=self._auth_metadata(),
            )
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return [_token_limit_entry(e) for e in resp.limits]

    async def set_token_limit(
        self,
        *,
        scope: str,
        scope_id: str = "",
        tenant: str = "",
        soft_limit: Optional[int] = None,
        hard_limit: Optional[int] = None,
    ) -> Mapping[str, Any]:
        """Upsert one token budget (RFC AW). A non-``None`` ``soft_limit``/
        ``hard_limit`` sets that tier; ``None`` clears it (unlimited on that
        axis) — a full-row upsert. The operator-global scope and any cross-tenant
        ``tenant`` are admin-only (PermissionDenied otherwise). Returns the
        written row (same shape as ``list_token_limits`` entries)."""
        req = pb.TokenLimitRequest(op="set", scope=scope, scope_id=scope_id, tenant=tenant)
        if soft_limit is not None:
            req.soft_limit = soft_limit
        if hard_limit is not None:
            req.hard_limit = hard_limit
        try:
            resp = await self._stub.TokenLimit(req, metadata=self._auth_metadata())
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return _token_limit_entry(resp.limits[0]) if resp.limits else {}

    async def delete_token_limit(
        self, *, scope: str, scope_id: str = "", tenant: str = ""
    ) -> None:
        """Delete a token budget → the scope is unlimited again (RFC AW). Same
        tenant-confinement as ``set_token_limit``."""
        try:
            await self._stub.TokenLimit(
                pb.TokenLimitRequest(op="delete", scope=scope, scope_id=scope_id, tenant=tenant),
                metadata=self._auth_metadata(),
            )
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)

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

    # ---- v0.8.22 substrate admin (AgentDef + SkillDef) ----

    async def agent_def(self, input: Mapping[str, Any]) -> Mapping[str, Any]:
        """Invoke the AgentDef substrate tool. Mirror of the MCP
        `agentdef` meta-tool and the HTTP POST /v1/_agentdef
        endpoint — different transport, identical semantics.

        ``input`` is the op-discriminated body the in-process tool
        accepts: ``{op: "create"|"fork"|"get"|"list"|"promote"|"retire",
        name?, def_id?, parent_def_id?, overlay?, description?,
        promote?, retired?}``. Returns the tool's output JSON
        deserialised as a dict.

        Raises :class:`SubstrateToolRefusedError` when the tool
        itself refused the call (scope deny, empty body, allowed-
        tools widening, etc.) — distinct from transport failures
        like :class:`UnavailableError` (503) or
        :class:`InvalidArgumentError` (malformed JSON body)."""
        return await self._dispatch_substrate("AgentDef", input)

    async def skill_def(self, input: Mapping[str, Any]) -> Mapping[str, Any]:
        """Invoke the SkillDef substrate tool. Mirror of
        :meth:`agent_def` for skills (v0.8.22+). Same input
        grammar, same error contract. See :meth:`agent_def` for
        the full shape."""
        return await self._dispatch_substrate("SkillDef", input)

    async def mcp_server_def(self, input: Mapping[str, Any]) -> Mapping[str, Any]:
        """Invoke the MCPServerDef substrate tool — dynamic MCP-server
        registration (verify-or-create, content-addressed). Mirror of
        :meth:`agent_def`; same op-discriminated input + error contract."""
        return await self._dispatch_substrate("MCPServerDef", input)

    async def schedule_def(self, input: Mapping[str, Any]) -> Mapping[str, Any]:
        """Invoke the ScheduleDef substrate tool — runtime-mutable
        scheduled runs (RFC E). Mirror of :meth:`agent_def`."""
        return await self._dispatch_substrate("ScheduleDef", input)

    async def a2a_server_card_def(self, input: Mapping[str, Any]) -> Mapping[str, Any]:
        """Invoke the A2AServerCardDef substrate tool — agent-to-agent
        server card publication (RFC G). Mirror of :meth:`agent_def`."""
        return await self._dispatch_substrate("A2AServerCardDef", input)

    async def a2a_agent_def(self, input: Mapping[str, Any]) -> Mapping[str, Any]:
        """Invoke the A2AAgentDef substrate tool — agent-to-agent peer
        registration (RFC G). Mirror of :meth:`agent_def`."""
        return await self._dispatch_substrate("A2AAgentDef", input)

    async def webhook_def(self, input: Mapping[str, Any]) -> Mapping[str, Any]:
        """Invoke the WebhookDef substrate tool — inbound webhook trigger
        registration (RFC H). Mirror of :meth:`agent_def`."""
        return await self._dispatch_substrate("WebhookDef", input)

    async def memory_backend_def(self, input: Mapping[str, Any]) -> Mapping[str, Any]:
        """Invoke the MemoryBackendDef substrate tool — pluggable memory
        backend registration. Mirror of :meth:`agent_def`."""
        return await self._dispatch_substrate("MemoryBackendDef", input)

    async def operator_token_def(self, input: Mapping[str, Any]) -> Mapping[str, Any]:
        """Invoke the OperatorTokenDef substrate tool — mint/list/revoke
        per-principal bearer tokens (RFC L multi-tenant auth). Mirror of
        :meth:`agent_def`."""
        return await self._dispatch_substrate("OperatorTokenDef", input)

    async def volume_def(self, input: Mapping[str, Any]) -> Mapping[str, Any]:
        """Invoke the VolumeDef substrate tool — dynamic filesystem-volume
        provisioning (RFC AH). Op-discriminated: create / get / list /
        delete / purge. Tenant-confined; the runtime derives the path inside
        an operator-blessed parent, so you pass name + mode, never a host
        path. Mirror of :meth:`agent_def`."""
        return await self._dispatch_substrate("VolumeDef", input)

    async def path(self, input: Mapping[str, Any]) -> Mapping[str, Any]:
        """Invoke the Path VFS tool — a Unix-like filesystem over your Memory
        entries, Volume mounts, and Documents (RFC AL). Op-discriminated:
        resolve / ls / stat / mkdir / mv / rm. Address resources by
        human-readable paths (e.g. ``/docs/launch``). Scope (agent/user/
        tenant) + tenant are resolved server-side from the authenticated
        principal, never the wire. Mirror of :meth:`agent_def`."""
        return await self._dispatch_substrate("Path", input)

    async def document(self, input: Mapping[str, Any]) -> Mapping[str, Any]:
        """Invoke the Document tool — chunked-graph documents where each chunk
        is a first-class unit (UUID, hierarchy, type, fields, edges, Markdown
        body) that agents and humans co-author (RFC AK). Op-discriminated (13
        ops: document/chunk lifecycle, edges, query_chunks, type defs). Scope
        agent/user (tenant deferred). Requires SQL Memory enabled on the
        sidecar. Mirror of :meth:`agent_def`."""
        return await self._dispatch_substrate("Document", input)

    async def _dispatch_substrate(
        self, tool: str, input: Mapping[str, Any]
    ) -> Mapping[str, Any]:
        """Shared body of the substrate-def methods. Serialises
        ``input`` as JSON, dispatches the matching gRPC RPC (the stub
        method name equals ``tool``, and every substrate RPC shares the
        SubstrateRequest→SubstrateResponse shape), decodes the response.
        Tool-level refusals (is_error=True) raise
        :class:`SubstrateToolRefusedError`; transport-level errors map
        via _raise_from_grpc as usual."""
        input_json = json.dumps(input).encode("utf-8")
        req = pb.SubstrateRequest(input_json=input_json)
        try:
            rpc = getattr(self._stub, tool)
            resp = await rpc(req, metadata=self._auth_metadata())
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        output_text = (resp.output_json or b"").decode("utf-8")
        if resp.is_error:
            raise SubstrateToolRefusedError(output_text, tool=tool)
        if not output_text:
            return {}
        parsed = json.loads(output_text)
        if not isinstance(parsed, dict):
            raise LoomcycleError(
                f"{tool}: unexpected non-dict response: {output_text[:200]}"
            )
        return parsed

    # ---- v0.8.0 run mutation + resolver (gRPC parity) ----

    async def spawn_run_batch(
        self,
        spawns: Iterable[Mapping[str, Any]],
        *,
        mode: str = "join",
        timeout_ms: int = 0,
    ) -> Mapping[str, Any]:
        """Spawn up to 32 fresh runs server-side-concurrent in ONE call
        (RFC Y; mirror of the HTTP ``POST /v1/runs:batch`` + the
        ``spawn_runs`` MCP tool). ``spawns`` is an iterable of run dicts
        whose keys mirror :meth:`run_streaming`'s kwargs (``agent``,
        ``segments``, ``tools``, ``allowed_hosts``, ``user_id``,
        ``tenant_id``, ``user_tier``, ``user_bearer``, ``sampling``,
        ``compaction``, …). ``mode="join"`` (default) blocks until every
        child settles; ``timeout_ms`` optionally caps the join (a child
        still running is cancelled + reported in-envelope).

        Returns ``{"spawned": int, "results": [<spawn result>, …]}``
        index-aligned with ``spawns``; a per-child failure rides in that
        child's result (``status`` + ``error``), never raising."""
        req = pb.BatchSpawnRequest(
            spawns=[_run_request_from_dict(s) for s in spawns],
            mode=mode,
            timeout_ms=timeout_ms,
        )
        try:
            resp = await self._stub.SpawnRunBatch(req, metadata=self._auth_metadata())
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return {
            "spawned": resp.spawned,
            "results": [_spawn_result_to_dict(r) for r in resp.results],
        }

    async def compact_run(self, run_id: str, *, reason: str = "") -> Mapping[str, Any]:
        """Summarize a parked run's context (mirror of the HTTP
        ``POST /v1/runs/{run_id}/compact`` + the ``compact_run`` MCP
        tool). ``reason`` is an optional audit note. Returns
        ``{run_id, compacted, before_tokens, after_tokens, applied}``
        where ``applied`` is ``"live"`` | ``"marker"`` | ``"noop"``.
        A run parked at a boundary that can't be compacted maps to a
        :class:`NotPausedError` (FailedPrecondition)."""
        req = pb.CompactRunRequest(run_id=run_id, reason=reason)
        try:
            resp = await self._stub.CompactRun(req, metadata=self._auth_metadata())
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return {
            "run_id": resp.run_id,
            "compacted": resp.compacted,
            "before_tokens": resp.before_tokens,
            "after_tokens": resp.after_tokens,
            "applied": resp.applied,
        }

    async def run_input(self, run_id: str, text: str) -> Mapping[str, Any]:
        """Push an operator steering message into a LIVE interactive run
        (RFC AI; mirror of ``POST /v1/runs/{run_id}/input``). The run must
        be in-flight — parked at end_turn awaiting input, or mid-turn (the
        message is drained at the next iteration boundary). Returns
        ``{run_id, delivered}``. An unknown / cross-tenant run_id maps to
        :class:`AgentNotFoundError` (NotFound); a full steer queue to
        :class:`BackpressureError` (ResourceExhausted). The injected source
        is server-stamped (``"api"``), never sent from here."""
        req = pb.RunInputRequest(run_id=run_id, text=text)
        try:
            resp = await self._stub.RunInput(req, metadata=self._auth_metadata())
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return {"run_id": resp.run_id, "delivered": resp.delivered}

    def stream_run(self, run_id: str, *, from_seq: int = 0) -> AsyncIterator[AgentEvent]:
        """Re-attach to a run's event stream by ``run_id`` (RFC AI; mirror
        of ``GET /v1/runs/{run_id}/stream``), replaying from ``from_seq``
        then live-tailing. The operator's own turns are replayed too (as
        ``steer`` events with ``user_input.source == "replay"``), so a
        cold client — e.g. resuming on another device — reconstructs the
        whole conversation. A PARKED interactive run keeps streaming until
        it ends or the call's context is cancelled. An unknown /
        cross-tenant run_id maps to :class:`AgentNotFoundError` (NotFound).

        Sync-returning — consume with ``async for`` (see
        :meth:`run_streaming`)."""
        req = pb.StreamRunRequest(run_id=run_id, from_seq=from_seq)
        return self._drive_stream(
            self._stub.StreamRun(req, metadata=self._auth_metadata()),
            on_handle=None,
        )

    async def resolve_probe(self) -> Mapping[str, Any]:
        """Return the resolver's current per-(provider, model)
        availability matrix (issue #88 operator escape hatch; mirror of
        ``GET /v1/_resolve/probe``). Returns
        ``{"generated_at": <ISO8601>, "providers": {<id>: {...}}}``."""
        try:
            resp = await self._stub.ResolveProbe(
                pb.ResolveProbeRequest(), metadata=self._auth_metadata()
            )
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return _resolver_matrix_to_dict(resp)

    # ---- v0.8.0 channels (gRPC parity) ----

    async def list_channels(self) -> Sequence[Mapping[str, Any]]:
        """List the operator-declared + runtime channels with cheap
        aggregate stats (mirror of ``GET /v1/_channels``)."""
        try:
            resp = await self._stub.ListChannels(
                pb.ListChannelsRequest(), metadata=self._auth_metadata()
            )
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return [_channel_descriptor_to_dict(c) for c in resp.channels]

    async def publish_channel(
        self,
        channel: str,
        *,
        payload: bytes,
        scope: str = "global",
        scope_id: str = "",
        deliver_at: str = "",
    ) -> Mapping[str, Any]:
        """Publish one message to a channel. ``payload`` is raw JSON
        bytes (the server validates ``json.Valid`` + the size cap).
        ``deliver_at`` (RFC3339Nano) defers visibility; empty publishes
        immediately. Returns ``{msg_id, channel, created_at, visible_at}``."""
        req = pb.PublishChannelRequest(
            channel=channel,
            scope=scope,
            scope_id=scope_id,
            payload=payload,
            deliver_at=deliver_at,
        )
        try:
            resp = await self._stub.PublishChannel(req, metadata=self._auth_metadata())
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return {
            "msg_id": resp.msg_id,
            "channel": resp.channel,
            "created_at": resp.created_at,
            "visible_at": resp.visible_at,
        }

    async def subscribe_channel(
        self,
        channel: str,
        *,
        scope: str = "global",
        scope_id: str = "",
        from_cursor: str = "",
        max_messages: int = 0,
        wait_ms: int = 0,
    ) -> Mapping[str, Any]:
        """Long-poll a channel. ``from_cursor`` empty = committed cursor,
        ``"cur_0"`` = replay from oldest. ``wait_ms`` 0 = poll-and-return.
        Returns ``{channel, messages: [{id, value, published_at}, …],
        next_cursor}`` (``value`` is raw JSON bytes)."""
        req = pb.SubscribeChannelRequest(
            channel=channel,
            scope=scope,
            scope_id=scope_id,
            from_cursor=from_cursor,
            max_messages=max_messages,
            wait_ms=wait_ms,
        )
        try:
            resp = await self._stub.SubscribeChannel(req, metadata=self._auth_metadata())
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return {
            "channel": resp.channel,
            "messages": [_channel_message_to_dict(m) for m in resp.messages],
            "next_cursor": resp.next_cursor,
        }

    async def peek_channel(
        self,
        channel: str,
        *,
        scope: str = "global",
        scope_id: str = "",
        from_cursor: str = "",
        max_messages: int = 0,
    ) -> Mapping[str, Any]:
        """Non-destructively read a channel without advancing any cursor.
        Returns ``{channel, messages: [...]}``."""
        req = pb.PeekChannelRequest(
            channel=channel,
            scope=scope,
            scope_id=scope_id,
            from_cursor=from_cursor,
            max_messages=max_messages,
        )
        try:
            resp = await self._stub.PeekChannel(req, metadata=self._auth_metadata())
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return {
            "channel": resp.channel,
            "messages": [_channel_message_to_dict(m) for m in resp.messages],
        }

    async def ack_channel(
        self,
        channel: str,
        *,
        cursor: str,
        scope: str = "global",
        scope_id: str = "",
    ) -> bool:
        """Commit a channel cursor (advance the read position). Returns
        the server's ``ok`` flag."""
        req = pb.AckChannelRequest(
            channel=channel, scope=scope, scope_id=scope_id, cursor=cursor
        )
        try:
            resp = await self._stub.AckChannel(req, metadata=self._auth_metadata())
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return resp.ok

    async def await_channels(
        self,
        channels: Sequence[str],
        *,
        scope: str = "global",
        scope_id: str = "",
        mode: str = "any",
        n: int = 0,
        from_cursor: str = "",
        max_messages: int = 0,
        wait_ms: int = 0,
    ) -> Mapping[str, Any]:
        """Multi-channel fan-in (RFC S): wait for messages across
        ``channels`` per ``mode`` (``"any"`` | ``"all"`` | ``"at_least"``
        with threshold ``n``), or until ``wait_ms`` elapses. Non-committing.
        Returns ``{satisfied, timed_out, mode, fired: [...], total_messages,
        results: {<channel>: {messages: [...], next_cursor}}}``."""
        req = pb.AwaitChannelsRequest(
            channels=list(channels),
            scope=scope,
            scope_id=scope_id,
            mode=mode,
            n=n,
            from_cursor=from_cursor,
            max_messages=max_messages,
            wait_ms=wait_ms,
        )
        try:
            resp = await self._stub.AwaitChannels(req, metadata=self._auth_metadata())
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return _await_result_to_dict(resp)

    async def broadcast_channels(
        self,
        channels: Sequence[str],
        *,
        payload: bytes,
        scope: str = "global",
        scope_id: str = "",
        deliver_at: str = "",
    ) -> Mapping[str, Any]:
        """Multi-channel fan-out (RFC S): publish one ``payload`` (raw
        JSON bytes) to every channel in ``channels``. Atomic at the
        declare pre-flight (one undeclared channel → nothing published).
        Returns ``{published, failed, results: [{channel, msg_id,
        created_at, visible_at, error}, …]}``."""
        req = pb.BroadcastChannelsRequest(
            channels=list(channels),
            scope=scope,
            scope_id=scope_id,
            payload=payload,
            deliver_at=deliver_at,
        )
        try:
            resp = await self._stub.BroadcastChannels(req, metadata=self._auth_metadata())
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)
        return {
            "published": resp.published,
            "failed": resp.failed,
            "results": [
                {
                    "channel": r.channel,
                    "msg_id": r.msg_id,
                    "created_at": r.created_at,
                    "visible_at": r.visible_at,
                    "error": r.error,
                }
                for r in resp.results
            ],
        }

    # ---- Streaming RPCs ----

    def run_streaming(
        self,
        *,
        agent: str,
        segments: Iterable[PromptSegment],
        tools: Optional[Sequence[str]] = None,
        allowed_hosts: Optional[Sequence[str]] = None,
        web_search_filter: str = "",
        user_id: str = "",
        agent_id: str = "",
        session_id: str = "",
        tenant_id: str = "",
        user_tier: str = "",
        user_bearer: str = "",
        sampling: Optional[Mapping[str, Any]] = None,
        compaction: Optional[Mapping[str, Any]] = None,
        interactive: bool = False,
        on_handle: Optional[Callable[["RunHandle"], None]] = None,
    ) -> AsyncIterator[AgentEvent]:
        """Drive one agent run end-to-end, yielding each
        ``AgentEvent`` as it arrives.

        ``interactive=True`` (RFC AI) starts a PERSISTENT run that parks
        at end_turn awaiting operator steering instead of terminating;
        drive it with :meth:`run_input`, re-attach with :meth:`stream_run`,
        and ``cancel_agent`` to end it.

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
        req = _build_run_request(
            agent=agent,
            segments=segments,
            session_id=session_id,
            tools=tools,
            allowed_hosts=allowed_hosts,
            web_search_filter=web_search_filter,
            user_id=user_id,
            agent_id=agent_id,
            tenant_id=tenant_id,
            user_tier=user_tier,
            user_bearer=user_bearer,
            sampling=sampling,
            compaction=compaction,
            interactive=interactive,
        )
        return self._drive_stream(
            self._stub.Run(req, metadata=self._auth_metadata()),
            on_handle=on_handle,
        )

    def continue_session(
        self,
        *,
        session_id: str,
        segments: Iterable[PromptSegment],
        tools: Optional[Sequence[str]] = None,
        allowed_hosts: Optional[Sequence[str]] = None,
        web_search_filter: str = "",
        agent_id: str = "",
        user_tier: str = "",
        user_bearer: str = "",
        sampling: Optional[Mapping[str, Any]] = None,
        compaction: Optional[Mapping[str, Any]] = None,
        interactive: bool = False,
        on_handle: Optional[Callable[["RunHandle"], None]] = None,
    ) -> AsyncIterator[AgentEvent]:
        """Continue an existing session. Same yield shape as
        ``run_streaming``; the agent + user_id + tenant_id are
        inherited from the existing session row server-side.

        ``interactive=True`` (RFC AI) parks the continuation at end_turn
        for operator steering (see :meth:`run_streaming`).

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
            tools=list(tools or ()),
            web_search_filter=web_search_filter,
            agent_id=agent_id,
            user_tier=user_tier,
            user_bearer=user_bearer,
            interactive=interactive,
        )
        if allowed_hosts is not None:
            req.allowed_hosts.list.extend(allowed_hosts)
        if sampling is not None:
            req.sampling.CopyFrom(_build_sampling(sampling))
        if compaction is not None:
            req.compaction.CopyFrom(_build_compaction(compaction))

        return self._drive_stream(
            self._stub.Continue(req, metadata=self._auth_metadata()),
            on_handle=on_handle,
        )

    def stream_user_run_states(
        self,
        user_id: str,
        *,
        statuses: Optional[Sequence[str]] = None,
        agent: str = "",
    ) -> AsyncIterator[Mapping[str, Any]]:
        """Stream run-state transitions for a user's runs (mirror of the
        gRPC ``StreamUserRunStates`` / HTTP
        ``GET /v1/users/{user_id}/agents/stream``). Optional ``statuses``
        filter (empty = all transitions); optional ``agent`` filter
        (empty = any agent). Yields ``{run_id, agent_id, agent, user_id,
        parent_agent_id, status, stop_reason, error, ts}`` dicts as
        transitions arrive.

        Sync-returning — consume with ``async for`` (see
        :meth:`run_streaming`)."""
        req = pb.StreamUserRunStatesRequest(
            user_id=user_id,
            statuses=list(statuses or ()),
            agent=agent,
        )
        return self._drive_run_state_stream(
            self._stub.StreamUserRunStates(req, metadata=self._auth_metadata())
        )

    # ---- Internal ----

    async def _drive_run_state_stream(
        self, stream: grpc.aio.UnaryStreamCall
    ) -> AsyncIterator[Mapping[str, Any]]:
        """Consume the run-state server-stream, yielding each
        RunStateEvent as a public dict. Translates gRPC errors to typed
        exceptions on the way out."""
        try:
            async for raw in stream:
                yield _run_state_event_to_dict(raw)
        except grpc.aio.AioRpcError as e:
            _raise_from_grpc(e)

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
    """Convert the public dict-shape segments to proto messages.

    An image content block (type="image", RFC AT) carries media_type plus raw
    image bytes: ``{"type": "image", "media_type": "image/png", "data": b"..."}``.
    Over gRPC ``data`` is the RAW bytes (the proto field is ``bytes``) — read a
    file with ``open(path, "rb").read()``. The server base64-encodes it for the
    model. Media types: image/png, image/jpeg, image/gif, image/webp.
    """
    out: List[pb.PromptSegment] = []
    for s in segments:
        blocks = []
        for b in s.get("content", []):
            blocks.append(
                pb.PromptContentBlock(
                    type=b.get("type", "trusted-text"),
                    text=b.get("text", ""),
                    cacheable=bool(b.get("cacheable", False)),
                    media_type=b.get("media_type", ""),
                    data=b.get("data", b""),
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


def _token_limit_entry(e: "pb.TokenLimitEntry") -> Mapping[str, Any]:
    """Convert a proto TokenLimitEntry → public dict (RFC AW). soft_limit /
    hard_limit are ``None`` when that tier is unset (no ceiling on the axis) so
    a consumer can distinguish "unlimited" from a real zero ceiling."""
    return {
        "tenant_id": e.tenant_id,
        "scope": e.scope,
        "scope_id": e.scope_id,
        "soft_limit": e.soft_limit if e.HasField("soft_limit") else None,
        "hard_limit": e.hard_limit if e.HasField("hard_limit") else None,
        "used": e.used,
        "updated_at": e.updated_at,
        "updated_by": e.updated_by,
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


# ---- v0.8.0 request builders + response decoders (gRPC parity) ----


def _build_sampling(d: Mapping[str, Any]) -> "pb.Sampling":
    """Map a sampling dict → pb.Sampling, setting only the keys the
    caller provided. Presence (proto3 optional) preserves an explicit
    ``temperature: 0.0`` as deterministic rather than dropping it as
    falsy — the whole point of the per-run override."""
    s = pb.Sampling()
    if d.get("temperature") is not None:
        s.temperature = float(d["temperature"])
    if d.get("top_p") is not None:
        s.top_p = float(d["top_p"])
    if d.get("top_k") is not None:
        s.top_k = int(d["top_k"])
    if d.get("frequency_penalty") is not None:
        s.frequency_penalty = float(d["frequency_penalty"])
    if d.get("presence_penalty") is not None:
        s.presence_penalty = float(d["presence_penalty"])
    if d.get("seed") is not None:
        s.seed = int(d["seed"])
    stop = d.get("stop")
    if stop:
        s.stop.extend(stop)
    return s


def _build_compaction(d: Mapping[str, Any]) -> "pb.Compaction":
    """Map a compaction dict → pb.Compaction, setting only provided
    keys (proto3 optional presence)."""
    c = pb.Compaction()
    if d.get("enabled") is not None:
        c.enabled = bool(d["enabled"])
    if d.get("target_percentage") is not None:
        c.target_percentage = int(d["target_percentage"])
    if d.get("keep_last_n") is not None:
        c.keep_last_n = int(d["keep_last_n"])
    if d.get("keep_first") is not None:
        c.keep_first = bool(d["keep_first"])
    if d.get("autocompact_at_pct") is not None:
        c.autocompact_at_pct = int(d["autocompact_at_pct"])
    if d.get("model") is not None:
        c.model = str(d["model"])
    return c


def _build_run_request(
    *,
    agent: str = "",
    segments: Iterable[PromptSegment] = (),
    session_id: str = "",
    tools: Optional[Sequence[str]] = None,
    allowed_hosts: Optional[Sequence[str]] = None,
    web_search_filter: str = "",
    user_id: str = "",
    agent_id: str = "",
    tenant_id: str = "",
    user_tier: str = "",
    user_bearer: str = "",
    sampling: Optional[Mapping[str, Any]] = None,
    compaction: Optional[Mapping[str, Any]] = None,
    interactive: bool = False,
) -> "pb.RunRequest":
    """Construct a pb.RunRequest from the run params. Shared by
    run_streaming + the batch builder so the field-mapping lives in one
    place. ``allowed_hosts`` keeps the three-state semantics (None = no
    narrowing; [] = deny-all; [...] = intersection)."""
    req = pb.RunRequest(
        agent=agent,
        session_id=session_id,
        segments=_segments_to_proto(segments),
        tools=list(tools or ()),
        web_search_filter=web_search_filter,
        user_id=user_id,
        agent_id=agent_id,
        tenant_id=tenant_id,
        user_tier=user_tier,
        user_bearer=user_bearer,
        interactive=interactive,
    )
    if allowed_hosts is not None:
        req.allowed_hosts.list.extend(allowed_hosts)
    if sampling is not None:
        req.sampling.CopyFrom(_build_sampling(sampling))
    if compaction is not None:
        req.compaction.CopyFrom(_build_compaction(compaction))
    return req


def _run_request_from_dict(spawn: Mapping[str, Any]) -> "pb.RunRequest":
    """Build a pb.RunRequest from a spawn dict (the per-child shape of
    spawn_run_batch). Keys mirror run_streaming's kwargs; unknown keys
    are ignored."""
    return _build_run_request(
        agent=spawn.get("agent", ""),
        segments=spawn.get("segments", ()),
        session_id=spawn.get("session_id", ""),
        tools=spawn.get("tools"),
        allowed_hosts=spawn.get("allowed_hosts"),
        web_search_filter=spawn.get("web_search_filter", ""),
        user_id=spawn.get("user_id", ""),
        agent_id=spawn.get("agent_id", ""),
        tenant_id=spawn.get("tenant_id", ""),
        user_tier=spawn.get("user_tier", ""),
        user_bearer=spawn.get("user_bearer", ""),
        sampling=spawn.get("sampling"),
        compaction=spawn.get("compaction"),
    )


def _usage_to_dict(u: "pb.Usage") -> Mapping[str, Any]:
    """Convert proto Usage → public dict (shared by spawn results)."""
    return {
        "input_tokens": u.input_tokens,
        "output_tokens": u.output_tokens,
        "cache_creation_tokens": u.cache_creation_tokens,
        "cache_read_tokens": u.cache_read_tokens,
        "model": u.model,
    }


def _spawn_result_to_dict(r: "pb.SpawnResult") -> Mapping[str, Any]:
    """Convert proto SpawnResult → public dict (one batch child)."""
    return {
        "agent_id": r.agent_id,
        "run_id": r.run_id,
        "session_id": r.session_id,
        "status": r.status,
        "stop_reason": r.stop_reason,
        "final_text": r.final_text,
        "usage": _usage_to_dict(r.usage) if r.HasField("usage") else None,
        "error": r.error,
    }


def _channel_descriptor_to_dict(c: "pb.ChannelDescriptor") -> Mapping[str, Any]:
    """Convert proto ChannelDescriptor → public dict."""
    return {
        "name": c.name,
        "scope": c.scope,
        "semantic": c.semantic,
        "publisher": c.publisher,
        "period": c.period,
        "default_ttl": c.default_ttl,
        "max_messages": c.max_messages,
        "message_count": c.message_count,
        "oldest_visible_at": c.oldest_visible_at,
        "newest_visible_at": c.newest_visible_at,
    }


def _channel_message_to_dict(m: "pb.ChannelMessage") -> Mapping[str, Any]:
    """Convert proto ChannelMessage → public dict. ``value`` stays raw
    JSON bytes — caller decodes with ``json.loads``."""
    return {"id": m.id, "value": m.value, "published_at": m.published_at}


def _await_result_to_dict(resp: "pb.AwaitChannelsResponse") -> Mapping[str, Any]:
    """Convert proto AwaitChannelsResponse → public dict."""
    return {
        "satisfied": resp.satisfied,
        "timed_out": resp.timed_out,
        "mode": resp.mode,
        "fired": list(resp.fired),
        "total_messages": resp.total_messages,
        "results": {
            ch: {
                "messages": [_channel_message_to_dict(m) for m in entry.messages],
                "next_cursor": entry.next_cursor,
            }
            for ch, entry in resp.results.items()
        },
    }


def _resolver_matrix_to_dict(resp: "pb.ResolverMatrixResponse") -> Mapping[str, Any]:
    """Convert proto ResolverMatrixResponse → public dict."""
    return {
        "generated_at": _ts_to_iso(resp.generated_at),
        "providers": {
            pid: {
                "excluded": p.excluded,
                "reachable": p.reachable,
                "last_check": _ts_to_iso(p.last_check) if p.HasField("last_check") else "",
                "last_error": p.last_error,
                "models": {
                    m: {"listed": s.listed, "stalled": s.stalled}
                    for m, s in p.models.items()
                },
            }
            for pid, p in resp.providers.items()
        },
    }


def _run_state_event_to_dict(e: "pb.RunStateEvent") -> Mapping[str, Any]:
    """Convert proto RunStateEvent → public dict."""
    return {
        "run_id": e.run_id,
        "agent_id": e.agent_id,
        "agent": e.agent,
        "user_id": e.user_id,
        "parent_agent_id": e.parent_agent_id,
        "status": e.status,
        "stop_reason": e.stop_reason,
        "error": e.error,
        "ts": e.ts,
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
        # Surface server-side INVALID_ARGUMENT as InvalidArgumentError
        # so callers can branch on the typed exception class. The
        # client-side variant of this exception has code=None; the
        # server-side variant carries the gRPC code — see the
        # InvalidArgumentError docstring for the discrimination
        # contract.
        raise InvalidArgumentError(msg, code=code) from err
    raise LoomcycleError(msg, code=code) from err


# Help the linter / mypy understand asyncio is in use even when
# imports are wrapped in TYPE_CHECKING.
_ = asyncio
