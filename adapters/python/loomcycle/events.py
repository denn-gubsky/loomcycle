"""Typed wrappers over the proto Event message.

The wire shape is dictated by ``loomcycle_pb2.Event`` — but exposing
the raw protobuf type to callers leaks an implementation detail
(generated code can change between proto compiler versions; users
shouldn't have to import ``loomcycle._generated`` to type-check
their handlers). Mirror the fields here as a frozen dataclass.
"""

from __future__ import annotations
from dataclasses import dataclass
from typing import Optional


@dataclass(frozen=True)
class ToolUse:
    """One ``tool_use`` block emitted by the model."""

    id: str
    name: str
    # Raw JSON bytes — the model's tool_use input. Decode with
    # ``json.loads(input)`` if you want the parsed shape; left as
    # bytes here so the caller's decoder owns the JSON parser
    # (a downstream might want yajl or orjson for hot paths).
    input: bytes


@dataclass(frozen=True)
class Usage:
    """Per-call token accounting."""

    input_tokens: int
    output_tokens: int
    cache_creation_tokens: int
    cache_read_tokens: int
    model: str = ""


@dataclass(frozen=True)
class Retry:
    """Rate-limit retry telemetry. Fired when a provider 429 is being
    retried with backoff; gives adapters live "waiting on rate
    limit" feedback to surface in their UI."""

    provider: str
    attempt: int
    wait_ms: int
    reason: str  # "header" | "schedule"


@dataclass(frozen=True)
class HostWidening:
    """Structured payload on ``host_widened`` events (v0.8.17+).

    Emitted once per dispatched tool call whose Pre-hook
    ``allow_hosts`` grant fired. Operators correlate
    ``(tool_call_id, url, hosts_added)`` to detect confused-deputy
    patterns where the hook echoes the model's requested host
    without independent validation. Mirrors
    ``providers.HostWideningEventInfo`` on the Go side."""

    tool_call_id: str
    tool_name: str
    url: str
    hook_owner: str
    hook_name: str
    hosts_added: tuple  # tuple of str — frozen for the dataclass


@dataclass(frozen=True)
class AwaitingInput:
    """Structured payload on ``awaiting_input`` events (RFC AI) — a
    persistent interactive run parked at end_turn. ``since_turn`` is the
    iteration it parked after. Mirrors
    ``providers.AwaitingInputEventInfo``."""

    since_turn: int


@dataclass(frozen=True)
class AwaitingReview:
    """Structured payload on ``awaiting_review`` events (RFC DJ) — a run armed
    for review finished its answer and is held for an operator's verdict
    (:meth:`LoomcycleClient.review_run`). ``round`` is 1 on the first answer
    and counts up with each revision. Mirrors
    ``providers.AwaitingReviewEventInfo``."""

    since_turn: int
    round: int
    # RFC 3339 UTC: when the hold ends as rejected if nobody rules on it.
    # Empty when the run has no review deadline.
    expires_at: str = ""
    # The agent_stop hook ("<owner>/<name>") that held the answer. Empty when
    # review arming held it; disarming review releases only that kind.
    held_by: str = ""


@dataclass(frozen=True)
class UserInput:
    """Structured payload on ``steer`` events (RFC AI) — an operator
    steering message drained into the conversation, or (on a
    ``stream_run`` re-attach) a replayed operator turn (``source="replay"``).
    Mirrors ``providers.UserInputEventInfo``."""

    text: str
    source: str  # "api" | "webui" | "replay"
    seen_at: str  # RFC3339Nano


@dataclass(frozen=True)
class LimitInfo:
    """Structured payload on ``limit`` events (RFC AW per-scope token budgets).

    Names which scope tripped, how hard (``soft`` warns and the run continues;
    ``hard`` means the NEXT run is refused at admission), and where the scope
    stands against its ceiling. No secrets: ``scope``/``scope_id`` are a
    tenant/subject id (already non-secret, like user_id) and the counts are
    integers. Mirrors ``providers.LimitInfo`` on the Go side."""

    scope: str  # "operator" | "tenant" | "user"
    scope_id: str  # tenant id (scope=tenant), user subject (scope=user), "" (operator)
    severity: str  # "soft" | "hard"
    window: str  # "month" (calendar month, UTC) in Phase 1
    used: int  # scope's month-to-date token total at the crossing
    limit: int  # the tier that was crossed (soft or hard ceiling)
    message: str = ""  # human-readable banner (optional)


@dataclass(frozen=True)
class CapabilityInertInfo:
    """Structured payload on ``capability_inert`` events.

    One tool the agent HOLDS and cannot use, because the capability gate that
    tool reads grants nothing. Server-generated and emitted ONCE at run start —
    the condition belongs to the definition, not to any one call.

    It carries the FIX as well as the fact: the governing yaml key is not
    guessable from the tool name, which is most of why this failure was hard to
    act on. Mirrors ``providers.CapabilityInertInfo`` on the Go side."""

    tool: str  # the granted tool, as named in the agent's `tools`
    gate: str  # the yaml key that governs it, e.g. "agent_def_scopes"
    message: str = ""  # names the tool, the gate, and what to set


@dataclass(frozen=True)
class OverrideInfo:
    """Structured payload on ``override`` events (RFC DC per-run overrides).

    A run's own configuration changed WHILE IT WAS RUNNING, because an operator
    retuned it. Distinct from a provider fallback on purpose: a fallback means
    the runtime moved the run because something failed, this means a person
    chose to, and the two want opposite responses from a consumer.

    Names what MOVED rather than what the settings now are — "the configuration
    changed" answers nothing for someone trying to explain why the answers got
    different after turn 12. Carries only operator-chosen configuration whose
    effects are already visible: a model name, a budget. No credential, no
    operator host. Mirrors ``providers.OverrideInfo`` on the Go side."""

    source: str  # who changed it; "operator" today
    from_model: str = ""  # "provider/model" before, when routing moved
    to_model: str = ""  # "provider/model" after
    fields: tuple[str, ...] = ()  # the override keys the request actually set


@dataclass(frozen=True)
class ErrorInfo:
    """The machine-readable half of a terminal run failure.

    Carried on ``type="error"`` frames. ``None`` on every other event
    type AND on a failure the runtime cannot categorise — there is
    deliberately no "unknown" category, because a bucket that carries
    no decision is worse than an absent field.
    """

    category: str  # "transient" | "validation" | "business" | "permission"
    is_retryable: bool
    description: str = ""
    # Optional backoff hint in milliseconds, meaningful only when
    # is_retryable. ``None`` rather than 0 because an absent hint and a
    # zero one are opposite instructions: "wait as you judge best"
    # versus "retry immediately".
    retry_after_ms: Optional[int] = None


@dataclass(frozen=True)
class HookDecision:
    """Structured payload on ``hook_decision`` events (RFC DK) — what one
    hook did to one tool call, or to the run. ``decision`` is ``deny``,
    ``rewrite_input``, ``rewrite_output``, ``context``, ``block``, ``hold``
    or ``unavailable``; ``block`` and ``hold`` are agent_stop's, and
    ``tool_use_id`` / ``tool_name`` are empty for agent_start / agent_stop.
    ``updated_input`` (JSON bytes) is the input the tool ran with after a
    rewrite. Mirrors ``providers.HookDecisionInfo``."""

    hook: str
    phase: str
    tool_use_id: str
    tool_name: str
    decision: str
    fail_mode: str = ""
    reason: str = ""
    updated_input: bytes = b""
    additional_context: str = ""


@dataclass(frozen=True)
class AgentEvent:
    """One frame from a Run/Continue stream.

    Field semantics mirror loomcycle's ``providers.Event`` Go type
    1:1. The proto encoding nullable-message fields as
    sub-messages translates here to ``Optional[T]`` for tool_use /
    usage / retry / host_widening — they're only set on events of
    the matching type. Adapters should switch on ``event.type`` to
    know which sub-fields to read.
    """

    # "text" | "tool_use" | "tool_result" | "usage" | "retry" |
    # "done" | "error" | "session" | "agent" | "host_widened"
    type: str
    text: str = ""
    tool_use: Optional[ToolUse] = None
    usage: Optional[Usage] = None
    retry: Optional[Retry] = None
    host_widening: Optional[HostWidening] = None
    awaiting_input: Optional[AwaitingInput] = None
    awaiting_review: Optional[AwaitingReview] = None
    hook_decision: Optional[HookDecision] = None
    user_input: Optional[UserInput] = None
    limit: Optional[LimitInfo] = None
    capability_inert: Optional[CapabilityInertInfo] = None
    override: Optional[OverrideInfo] = None
    error_info: Optional[ErrorInfo] = None
    error: str = ""
    is_error: bool = False
    stop_reason: str = ""

    @classmethod
    def _from_proto(cls, ev) -> "AgentEvent":
        """Convert a generated ``loomcycle_pb2.Event`` into the public
        ``AgentEvent``. Internal use — exposed via the package only
        through ``client.run_streaming``."""
        tu: Optional[ToolUse] = None
        if ev.HasField("tool_use"):
            tu = ToolUse(
                id=ev.tool_use.id,
                name=ev.tool_use.name,
                input=ev.tool_use.input,
            )
        u: Optional[Usage] = None
        if ev.HasField("usage"):
            u = Usage(
                input_tokens=ev.usage.input_tokens,
                output_tokens=ev.usage.output_tokens,
                cache_creation_tokens=ev.usage.cache_creation_tokens,
                cache_read_tokens=ev.usage.cache_read_tokens,
                model=ev.usage.model,
            )
        r: Optional[Retry] = None
        if ev.HasField("retry"):
            r = Retry(
                provider=ev.retry.provider,
                attempt=ev.retry.attempt,
                wait_ms=ev.retry.wait_ms,
                reason=ev.retry.reason,
            )
        hw: Optional[HostWidening] = None
        if ev.HasField("host_widening"):
            hw = HostWidening(
                tool_call_id=ev.host_widening.tool_call_id,
                tool_name=ev.host_widening.tool_name,
                url=ev.host_widening.url,
                hook_owner=ev.host_widening.hook_owner,
                hook_name=ev.host_widening.hook_name,
                hosts_added=tuple(ev.host_widening.hosts_added),
            )
        ai: Optional[AwaitingInput] = None
        if ev.HasField("awaiting_input"):
            ai = AwaitingInput(since_turn=ev.awaiting_input.since_turn)
        ar: Optional[AwaitingReview] = None
        if ev.HasField("awaiting_review"):
            ar = AwaitingReview(
                since_turn=ev.awaiting_review.since_turn,
                round=ev.awaiting_review.round,
                expires_at=ev.awaiting_review.expires_at,
                held_by=ev.awaiting_review.held_by,
            )
        hd: Optional[HookDecision] = None
        if ev.HasField("hook_decision"):
            h = ev.hook_decision
            hd = HookDecision(
                hook=h.hook,
                phase=h.phase,
                tool_use_id=h.tool_use_id,
                tool_name=h.tool_name,
                decision=h.decision,
                fail_mode=h.fail_mode,
                reason=h.reason,
                updated_input=h.updated_input,
                additional_context=h.additional_context,
            )
        ui: Optional[UserInput] = None
        if ev.HasField("user_input"):
            ui = UserInput(
                text=ev.user_input.text,
                source=ev.user_input.source,
                seen_at=ev.user_input.seen_at,
            )
        li: Optional[LimitInfo] = None
        if ev.HasField("limit"):
            li = LimitInfo(
                scope=ev.limit.scope,
                scope_id=ev.limit.scope_id,
                severity=ev.limit.severity,
                window=ev.limit.window,
                used=ev.limit.used,
                limit=ev.limit.limit,
                message=ev.limit.message,
            )
        ci: Optional[CapabilityInertInfo] = None
        if ev.HasField("capability_inert"):
            ci = CapabilityInertInfo(
                tool=ev.capability_inert.tool,
                gate=ev.capability_inert.gate,
                message=ev.capability_inert.message,
            )
        ov: Optional[OverrideInfo] = None
        if ev.HasField("override"):
            ov = OverrideInfo(
                source=ev.override.source,
                from_model=ev.override.from_model,
                to_model=ev.override.to_model,
                fields=tuple(ev.override.fields),
            )
        ei: Optional[ErrorInfo] = None
        if ev.HasField("error_info"):
            ei = ErrorInfo(
                category=ev.error_info.category,
                is_retryable=ev.error_info.is_retryable,
                description=ev.error_info.description,
                retry_after_ms=(
                    ev.error_info.retry_after_ms
                    if ev.error_info.HasField("retry_after_ms")
                    else None
                ),
            )
        return cls(
            type=ev.type,
            text=ev.text,
            tool_use=tu,
            usage=u,
            retry=r,
            host_widening=hw,
            awaiting_input=ai,
            awaiting_review=ar,
            hook_decision=hd,
            user_input=ui,
            limit=li,
            capability_inert=ci,
            override=ov,
            error_info=ei,
            error=ev.error,
            is_error=ev.is_error,
            stop_reason=ev.stop_reason,
        )
