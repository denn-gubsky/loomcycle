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
class AgentEvent:
    """One frame from a Run/Continue stream.

    Field semantics mirror loomcycle's ``providers.Event`` Go type
    1:1. The proto encoding nullable-message fields as
    sub-messages translates here to ``Optional[T]`` for tool_use /
    usage / retry — they're only set on events of the matching
    type. Adapters should switch on ``event.type`` to know which
    sub-fields to read.
    """

    # "text" | "tool_use" | "tool_result" | "usage" | "retry" |
    # "done" | "error" | "session" | "agent"
    type: str
    text: str = ""
    tool_use: Optional[ToolUse] = None
    usage: Optional[Usage] = None
    retry: Optional[Retry] = None
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
        return cls(
            type=ev.type,
            text=ev.text,
            tool_use=tu,
            usage=u,
            retry=r,
            error=ev.error,
            is_error=ev.is_error,
            stop_reason=ev.stop_reason,
        )
