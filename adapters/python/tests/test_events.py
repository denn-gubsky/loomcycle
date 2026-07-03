"""Unit tests for events.AgentEvent <-> proto conversion.

These run offline (no gRPC channel needed). They're the
load-bearing tests because every event a caller sees flows
through ``AgentEvent._from_proto``.
"""

from __future__ import annotations

from loomcycle.events import AgentEvent, ToolUse, Usage, Retry, LimitInfo
from loomcycle._generated import loomcycle_pb2 as pb


def test_text_event_round_trips():
    proto = pb.Event(type="text", text="hello world")
    ev = AgentEvent._from_proto(proto)
    assert ev.type == "text"
    assert ev.text == "hello world"
    assert ev.tool_use is None
    assert ev.usage is None
    assert ev.retry is None


def test_tool_use_event_unwraps_submessage():
    proto = pb.Event(
        type="tool_use",
        tool_use=pb.ToolUse(id="tu_01", name="Read", input=b'{"path":"/tmp"}'),
    )
    ev = AgentEvent._from_proto(proto)
    assert ev.type == "tool_use"
    assert isinstance(ev.tool_use, ToolUse)
    assert ev.tool_use.id == "tu_01"
    assert ev.tool_use.name == "Read"
    assert ev.tool_use.input == b'{"path":"/tmp"}'


def test_usage_event_carries_token_counts():
    proto = pb.Event(
        type="usage",
        usage=pb.Usage(
            input_tokens=120,
            output_tokens=45,
            cache_creation_tokens=10,
            cache_read_tokens=5,
            model="claude-opus-4-7",
        ),
    )
    ev = AgentEvent._from_proto(proto)
    assert isinstance(ev.usage, Usage)
    assert ev.usage.input_tokens == 120
    assert ev.usage.output_tokens == 45
    assert ev.usage.cache_creation_tokens == 10
    assert ev.usage.cache_read_tokens == 5
    assert ev.usage.model == "claude-opus-4-7"


def test_retry_event_carries_provider_and_attempt():
    proto = pb.Event(
        type="retry",
        retry=pb.Retry(
            provider="anthropic",
            attempt=2,
            wait_ms=2500,
            reason="header",
        ),
    )
    ev = AgentEvent._from_proto(proto)
    assert isinstance(ev.retry, Retry)
    assert ev.retry.provider == "anthropic"
    assert ev.retry.attempt == 2
    assert ev.retry.wait_ms == 2500
    assert ev.retry.reason == "header"


def test_error_event_propagates_flag_and_message():
    proto = pb.Event(type="error", error="upstream 500", is_error=True)
    ev = AgentEvent._from_proto(proto)
    assert ev.type == "error"
    assert ev.error == "upstream 500"
    assert ev.is_error is True


def test_limit_event_unwraps_budget_payload():
    # RFC AW: a `limit` frame carries a token-budget crossing. Fails before the
    # events parser learns the limit sub-message (ev.limit would stay None).
    proto = pb.Event(
        type="limit",
        limit=pb.LimitInfo(
            scope="tenant",
            scope_id="acme",
            severity="soft",
            window="month",
            used=1200,
            limit=1000,
            message="tenant acme soft budget reached",
        ),
    )
    ev = AgentEvent._from_proto(proto)
    assert ev.type == "limit"
    assert isinstance(ev.limit, LimitInfo)
    assert ev.limit.scope == "tenant"
    assert ev.limit.scope_id == "acme"
    assert ev.limit.severity == "soft"
    assert ev.limit.used == 1200
    assert ev.limit.limit == 1000
    assert ev.limit.message == "tenant acme soft budget reached"


def test_agentevent_is_frozen():
    ev = AgentEvent(type="text", text="x")
    try:
        ev.text = "y"  # type: ignore[misc]
    except Exception:
        return
    assert False, "AgentEvent should be frozen — caller must not mutate"
