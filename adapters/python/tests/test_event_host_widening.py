"""Unit test for HostWidening decoding in AgentEvent._from_proto.

The v0.8.17 host-widening audit payload was added to the proto in
the v0.8.20 cross-transport parity PR. Without this test, a future
regen + AgentEvent edit could silently drop the nested fields.
"""

from __future__ import annotations

from loomcycle import HostWidening, AgentEvent
from loomcycle._generated import loomcycle_pb2 as pb


def test_from_proto_surfaces_host_widening_payload():
    raw = pb.Event(
        type="host_widened",
        host_widening=pb.HostWidening(
            tool_call_id="tu_abc123",
            tool_name="WebFetch",
            url="https://api.example.com/v1/things",
            hook_owner="jobs-search-web",
            hook_name="scan-webfetch",
            hosts_added=["api.example.com", ".example.org"],
        ),
    )
    ev = AgentEvent._from_proto(raw)
    assert ev.type == "host_widened"
    assert ev.host_widening is not None
    hw = ev.host_widening
    assert isinstance(hw, HostWidening)
    assert hw.tool_call_id == "tu_abc123"
    assert hw.tool_name == "WebFetch"
    assert hw.url == "https://api.example.com/v1/things"
    assert hw.hook_owner == "jobs-search-web"
    assert hw.hook_name == "scan-webfetch"
    assert hw.hosts_added == ("api.example.com", ".example.org")


def test_from_proto_leaves_host_widening_none_when_unset():
    """Non-host_widened events must not synthesize a stub HostWidening
    — Optional[HostWidening] is the typed contract; downstream
    `if ev.host_widening:` checks rely on None for the absent case."""
    raw = pb.Event(type="text", text="hi")
    ev = AgentEvent._from_proto(raw)
    assert ev.host_widening is None
