"""Unit tests for the module-private helpers in client.py.

These cover ``_segments_to_proto``, ``_agent_to_dict``,
``_ts_to_iso`` — every adapter call path goes through them.
"""

from __future__ import annotations

from google.protobuf.timestamp_pb2 import Timestamp

from loomcycle._generated import loomcycle_pb2 as pb
from loomcycle.client import _agent_to_dict, _segments_to_proto, _ts_to_iso


def test_segments_to_proto_handles_simple_text_segment():
    segs = _segments_to_proto([
        {
            "role": "user",
            "content": [{"type": "trusted-text", "text": "hi"}],
        }
    ])
    assert len(segs) == 1
    assert segs[0].role == "user"
    assert len(segs[0].content) == 1
    assert segs[0].content[0].type == "trusted-text"
    assert segs[0].content[0].text == "hi"
    assert segs[0].content[0].cacheable is False


def test_segments_to_proto_propagates_cacheable_flag():
    segs = _segments_to_proto([
        {
            "role": "system",
            "content": [
                {"type": "trusted-text", "text": "system prompt", "cacheable": True}
            ],
        }
    ])
    assert segs[0].content[0].cacheable is True


def test_segments_to_proto_defaults_role_to_user():
    # Role omission should yield "user" — same default as the
    # HTTP API, keeps the adapter symmetric.
    segs = _segments_to_proto([{"content": [{"type": "trusted-text", "text": "x"}]}])
    assert segs[0].role == "user"


def test_segments_to_proto_handles_empty_iterable():
    segs = _segments_to_proto([])
    assert segs == []


def test_ts_to_iso_returns_empty_for_zero_value():
    ts = Timestamp()  # all-zero → "no timestamp"
    assert _ts_to_iso(ts) == ""


def test_ts_to_iso_returns_iso_for_real_value():
    ts = Timestamp()
    ts.FromJsonString("2026-05-08T12:34:56Z")
    out = _ts_to_iso(ts)
    assert out.startswith("2026-05-08T12:34:56")


def test_ts_to_iso_returns_empty_for_none():
    assert _ts_to_iso(None) == ""


def test_agent_to_dict_unwraps_optional_usage():
    a = pb.Agent(
        agent_id="ag-1",
        run_id="rn-1",
        session_id="sess-1",
        user_id="u-1",
        status="completed",
        stop_reason="end_turn",
    )
    a.usage.input_tokens = 100
    a.usage.output_tokens = 50
    a.usage.model = "claude-opus-4-7"
    out = _agent_to_dict(a)
    assert out["agent_id"] == "ag-1"
    assert out["status"] == "completed"
    assert out["usage"]["input_tokens"] == 100
    assert out["usage"]["model"] == "claude-opus-4-7"


def test_agent_to_dict_handles_missing_usage_as_none():
    # Agents that haven't received any usage events yet should
    # surface ``usage=None`` rather than a zero-valued dict —
    # callers can distinguish "no data yet" from "zero tokens".
    a = pb.Agent(
        agent_id="ag-2",
        run_id="rn-2",
        session_id="sess-2",
        user_id="u-2",
        status="running",
    )
    out = _agent_to_dict(a)
    assert out["usage"] is None
    assert out["completed_at"] is None
    assert out["last_heartbeat_at"] is None
