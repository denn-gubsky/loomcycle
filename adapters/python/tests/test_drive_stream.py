"""Unit tests for the synthetic-frame swallowing logic in
``LoomcycleClient._drive_stream``.

The gRPC server emits two synthetic frames at the start of every
Run/Continue stream:

  Frame 0: type="session", text=<session_id>
  Frame 1: type="agent",   text=<agent_id>,
                           stop_reason=<parent_agent_id>,
                           error=<JSON envelope: {agent_id,run_id,
                                                   session_id,
                                                   parent_agent_id}>

These should NOT be yielded to the caller — they should be
captured into a ``RunHandle`` and surfaced via the optional
``on_handle`` callback. Real provider events that follow are
yielded as ``AgentEvent``.

This is the contract test that protects that behavior; if we
ever regress it, downstream adapters would start seeing two
phantom events at the head of every stream.
"""

from __future__ import annotations

import asyncio
import json
from typing import Iterable, List

import grpc.aio
import pytest

from loomcycle import AgentEvent, LoomcycleClient, RunHandle
from loomcycle._generated import loomcycle_pb2 as pb


# ---- Fakes ----


class _FakeStream:
    """Stand-in for the ``UnaryStreamCall`` returned by stub.Run /
    stub.Continue. We only need ``__aiter__``."""

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


def _make_client() -> LoomcycleClient:
    """Build a client without dialing — pass a sham channel that
    will never be touched by these tests (every call goes
    through our ``_FakeStream``)."""
    # ``insecure_channel`` is lazy enough that constructing one
    # against a non-existent target doesn't actually open a
    # socket. We never close it because we never use it.
    channel = grpc.aio.insecure_channel("127.0.0.1:1")
    return LoomcycleClient(channel=channel)


def _registration_frames(
    *,
    session_id: str = "sess-1",
    agent_id: str = "ag-1",
    run_id: str = "rn-1",
    parent_agent_id: str = "",
) -> List[pb.Event]:
    """Build the two synthetic frames the server emits."""
    envelope = json.dumps({
        "agent_id": agent_id,
        "run_id": run_id,
        "session_id": session_id,
        "parent_agent_id": parent_agent_id,
    })
    return [
        pb.Event(type="session", text=session_id),
        pb.Event(
            type="agent",
            text=agent_id,
            stop_reason=parent_agent_id,
            error=envelope,
        ),
    ]


# ---- Tests ----


@pytest.mark.asyncio
async def test_drive_stream_swallows_session_and_agent_frames():
    client = _make_client()
    frames = _registration_frames() + [
        pb.Event(type="text", text="hello"),
        pb.Event(type="text", text="world"),
        pb.Event(type="done", stop_reason="end_turn"),
    ]
    out: List[AgentEvent] = []
    async for ev in client._drive_stream(_FakeStream(frames), on_handle=None):
        out.append(ev)
    assert [e.type for e in out] == ["text", "text", "done"]
    assert out[0].text == "hello"
    assert out[1].text == "world"
    assert out[2].stop_reason == "end_turn"


@pytest.mark.asyncio
async def test_drive_stream_invokes_on_handle_with_runhandle():
    client = _make_client()
    captured: List[RunHandle] = []
    frames = _registration_frames(
        session_id="s9", agent_id="ag9", run_id="rn9"
    ) + [
        pb.Event(type="text", text="x"),
    ]
    async for _ in client._drive_stream(
        _FakeStream(frames), on_handle=captured.append
    ):
        pass
    assert len(captured) == 1
    h = captured[0]
    assert h.session_id == "s9"
    assert h.agent_id == "ag9"
    assert h.run_id == "rn9"
    assert h.parent_agent_id == ""


@pytest.mark.asyncio
async def test_drive_stream_propagates_parent_agent_id_for_subagents():
    client = _make_client()
    captured: List[RunHandle] = []
    frames = _registration_frames(
        agent_id="ag-child", parent_agent_id="ag-parent"
    )
    async for _ in client._drive_stream(_FakeStream(frames), on_handle=captured.append):
        pass
    assert captured[0].parent_agent_id == "ag-parent"


@pytest.mark.asyncio
async def test_drive_stream_survives_callback_exception():
    """A buggy ``on_handle`` callback must not tear down the stream."""
    client = _make_client()
    frames = _registration_frames() + [pb.Event(type="text", text="ok")]

    def boom(_h):
        raise RuntimeError("user-supplied bug")

    out: List[AgentEvent] = []
    async for ev in client._drive_stream(_FakeStream(frames), on_handle=boom):
        out.append(ev)
    assert [e.type for e in out] == ["text"]


@pytest.mark.asyncio
async def test_drive_stream_handles_malformed_envelope_gracefully():
    """If the server's JSON envelope changes shape (or is corrupt),
    the stream should still drive — we just leave run_id empty."""
    client = _make_client()
    frames = [
        pb.Event(type="session", text="s1"),
        pb.Event(type="agent", text="ag1", error="not json {"),
        pb.Event(type="text", text="ok"),
    ]
    captured: List[RunHandle] = []
    out: List[AgentEvent] = []
    async for ev in client._drive_stream(_FakeStream(frames), on_handle=captured.append):
        out.append(ev)
    assert captured[0].agent_id == "ag1"
    assert captured[0].run_id == ""
    assert [e.type for e in out] == ["text"]
