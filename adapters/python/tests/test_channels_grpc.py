"""v0.8.0 — channel ops + run-state stream gRPC parity:
list_channels, publish_channel, subscribe_channel, peek_channel,
ack_channel, await_channels, broadcast_channels, stream_user_run_states.

Stub-mock pattern (no live server). Server-side path covered by
internal/api/grpc/server_test.go + n8n.go tests.
"""

from __future__ import annotations

from typing import Any, List

import grpc.aio
import pytest

from loomcycle import LoomcycleClient
from loomcycle._generated import loomcycle_pb2 as pb


def _make_client() -> LoomcycleClient:
    channel = grpc.aio.insecure_channel("127.0.0.1:1")
    return LoomcycleClient(channel=channel)


def _async_returning(result: Any):
    captured: dict = {}

    async def fn(req, metadata=None):
        captured["req"] = req
        captured["metadata"] = metadata
        return result

    return fn, captured


@pytest.mark.asyncio
async def test_list_channels_decodes_descriptors():
    client = _make_client()
    resp = pb.ListChannelsResponse(
        channels=[pb.ChannelDescriptor(name="alarms", scope="global", message_count=3)]
    )
    fake, _ = _async_returning(resp)
    client._stub.ListChannels = fake  # type: ignore[attr-defined]
    out = await client.list_channels()
    assert out == [
        {
            "name": "alarms",
            "scope": "global",
            "semantic": "",
            "publisher": "",
            "period": "",
            "default_ttl": 0,
            "max_messages": 0,
            "message_count": 3,
            "oldest_visible_at": "",
            "newest_visible_at": "",
        }
    ]


@pytest.mark.asyncio
async def test_publish_channel_threads_payload_bytes_and_decodes():
    client = _make_client()
    resp = pb.PublishChannelResponse(msg_id="m1", channel="c", created_at="2026-01-01T00:00:00Z")
    fake, captured = _async_returning(resp)
    client._stub.PublishChannel = fake  # type: ignore[attr-defined]
    out = await client.publish_channel("c", payload=b'{"x":1}', scope="user", scope_id="u1")
    req = captured["req"]
    assert req.channel == "c"
    assert req.scope == "user"
    assert req.scope_id == "u1"
    assert req.payload == b'{"x":1}'
    assert out["msg_id"] == "m1"


@pytest.mark.asyncio
async def test_subscribe_channel_decodes_messages_and_cursor():
    client = _make_client()
    resp = pb.SubscribeChannelResponse(
        channel="c",
        messages=[pb.ChannelMessage(id="m1", value=b'{"a":1}', published_at="2026-01-01T00:00:00Z")],
        next_cursor="cur_1",
    )
    fake, captured = _async_returning(resp)
    client._stub.SubscribeChannel = fake  # type: ignore[attr-defined]
    out = await client.subscribe_channel("c", from_cursor="cur_0", max_messages=5, wait_ms=100)
    assert captured["req"].from_cursor == "cur_0"
    assert captured["req"].wait_ms == 100
    assert out["next_cursor"] == "cur_1"
    assert out["messages"][0]["id"] == "m1"
    assert out["messages"][0]["value"] == b'{"a":1}'


@pytest.mark.asyncio
async def test_peek_channel_decodes_messages():
    client = _make_client()
    resp = pb.PeekChannelResponse(
        channel="c", messages=[pb.ChannelMessage(id="m1", value=b"{}")]
    )
    fake, _ = _async_returning(resp)
    client._stub.PeekChannel = fake  # type: ignore[attr-defined]
    out = await client.peek_channel("c")
    assert out["channel"] == "c"
    assert out["messages"][0]["id"] == "m1"


@pytest.mark.asyncio
async def test_ack_channel_returns_ok():
    client = _make_client()
    fake, captured = _async_returning(pb.AckChannelResponse(ok=True))
    client._stub.AckChannel = fake  # type: ignore[attr-defined]
    ok = await client.ack_channel("c", cursor="cur_2")
    assert ok is True
    assert captured["req"].cursor == "cur_2"


@pytest.mark.asyncio
async def test_await_channels_decodes_fan_in():
    client = _make_client()
    resp = pb.AwaitChannelsResponse(satisfied=True, mode="any", fired=["c1"], total_messages=2)
    resp.results["c1"].next_cursor = "cur_9"
    resp.results["c1"].messages.append(pb.ChannelMessage(id="m1", value=b"{}"))
    fake, captured = _async_returning(resp)
    client._stub.AwaitChannels = fake  # type: ignore[attr-defined]
    out = await client.await_channels(["c1", "c2"], mode="any", wait_ms=200)
    assert list(captured["req"].channels) == ["c1", "c2"]
    assert captured["req"].mode == "any"
    assert out["satisfied"] is True
    assert out["fired"] == ["c1"]
    assert out["results"]["c1"]["next_cursor"] == "cur_9"
    assert out["results"]["c1"]["messages"][0]["id"] == "m1"


@pytest.mark.asyncio
async def test_broadcast_channels_decodes_fan_out():
    client = _make_client()
    resp = pb.BroadcastChannelsResponse(
        published=2,
        failed=0,
        results=[
            pb.BroadcastChannelEntry(channel="c1", msg_id="m1"),
            pb.BroadcastChannelEntry(channel="c2", msg_id="m2"),
        ],
    )
    fake, captured = _async_returning(resp)
    client._stub.BroadcastChannels = fake  # type: ignore[attr-defined]
    out = await client.broadcast_channels(["c1", "c2"], payload=b'{"event":"ping"}')
    assert captured["req"].payload == b'{"event":"ping"}'
    assert out["published"] == 2
    assert [r["channel"] for r in out["results"]] == ["c1", "c2"]


class _ItemStream:
    """Async-iterates a fixed list of proto messages, then stops."""

    def __init__(self, items: List[Any]) -> None:
        self._it = iter(items)

    def __aiter__(self):
        return self

    async def __anext__(self):
        try:
            return next(self._it)
        except StopIteration:
            raise StopAsyncIteration


@pytest.mark.asyncio
async def test_stream_user_run_states_yields_decoded_events():
    client = _make_client()
    events = [
        pb.RunStateEvent(run_id="r1", agent="reviewer", user_id="u1", status="running"),
        pb.RunStateEvent(run_id="r1", agent="reviewer", user_id="u1", status="completed"),
    ]
    captured: dict = {}

    def fake(req, metadata=None):
        captured["req"] = req
        return _ItemStream(events)

    client._stub.StreamUserRunStates = fake  # type: ignore[attr-defined]

    got = []
    async for e in client.stream_user_run_states("u1", statuses=["running", "completed"]):
        got.append(e)

    assert captured["req"].user_id == "u1"
    assert list(captured["req"].statuses) == ["running", "completed"]
    assert [e["status"] for e in got] == ["running", "completed"]
    assert got[0]["run_id"] == "r1"
