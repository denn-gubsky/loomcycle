"""RFC AI — interactive-session gRPC parity: run_input, stream_run, and the
``interactive=True`` flag on run_streaming / continue_session.

Stub-mock pattern (no live server). The server-side path is covered by
internal/api/grpc/interactive_test.go.
"""

from __future__ import annotations

from typing import Any, Iterable

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


class _FakeStream:
    def __init__(self, frames: Iterable[pb.Event]) -> None:
        self._frames = list(frames)

    def __aiter__(self):
        return _FakeStreamIter(self._frames)


class _FakeStreamIter:
    def __init__(self, frames):
        self._iter = iter(frames)

    def __aiter__(self):
        return self

    async def __anext__(self):
        try:
            return next(self._iter)
        except StopIteration:
            raise StopAsyncIteration


@pytest.mark.asyncio
async def test_run_input_builds_request_and_decodes_response():
    client = _make_client()
    fake, captured = _async_returning(pb.RunInputResponse(run_id="r_abc", delivered=True))
    client._stub.RunInput = fake  # type: ignore[attr-defined]

    out = await client.run_input("r_abc", "focus on the failing test")

    assert captured["req"].run_id == "r_abc"
    assert captured["req"].text == "focus on the failing test"
    assert out == {"run_id": "r_abc", "delivered": True}


@pytest.mark.asyncio
async def test_run_streaming_sets_interactive_flag():
    client = _make_client()
    captured: dict = {}

    def fake_run(req, metadata=None):
        captured["req"] = req
        return _FakeStream([])

    client._stub.Run = fake_run  # type: ignore[attr-defined]

    async for _ in client.run_streaming(agent="chat", segments=[], interactive=True):
        pass
    assert captured["req"].interactive is True


@pytest.mark.asyncio
async def test_stream_run_yields_interactive_events():
    client = _make_client()
    frames = [
        pb.Event(type="text", text="working"),
        pb.Event(type="awaiting_input", awaiting_input=pb.AwaitingInput(since_turn=3)),
        pb.Event(type="steer", user_input=pb.UserInput(text="ship it", source="replay")),
    ]
    captured: dict = {}

    def fake_stream(req, metadata=None):
        captured["req"] = req
        return _FakeStream(frames)

    client._stub.StreamRun = fake_stream  # type: ignore[attr-defined]

    events = [ev async for ev in client.stream_run("r_abc", from_seq=7)]

    assert captured["req"].run_id == "r_abc"
    assert captured["req"].from_seq == 7
    # re-attach has no synthetic session/agent frames → all 3 frames surface.
    assert [e.type for e in events] == ["text", "awaiting_input", "steer"]
    assert events[1].awaiting_input is not None
    assert events[1].awaiting_input.since_turn == 3
    assert events[2].user_input is not None
    assert events[2].user_input.text == "ship it"
    assert events[2].user_input.source == "replay"
