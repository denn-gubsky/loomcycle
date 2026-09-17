"""Per-run overrides on every Python request path.

The client grew the override keywords without a test on any of them, and what
caught it was UNRELATED tests breaking on a TypeError — `_build_run_request`
never got the matching signature, and the committed proto stubs had no override
fields at all, so `ContinueRequest` rejected `model` outright.

So these assert the three paths that carry them: a fresh run, a continuation,
and a fan-out CHILD (the batch builder is a third enumeration of the same list
and was the one still missing them).

Stub-mock pattern, no live server — the server side is covered in
internal/api/grpc/server_test.go.
"""

from __future__ import annotations

import grpc.aio
import pytest

from loomcycle import LoomcycleClient
from loomcycle._generated import loomcycle_pb2 as pb


def _make_client() -> LoomcycleClient:
    return LoomcycleClient(channel=grpc.aio.insecure_channel("127.0.0.1:1"))


class _EmptyStream:
    def __aiter__(self):
        return self

    async def __anext__(self):
        raise StopAsyncIteration


class _CaptureStub:
    def __init__(self) -> None:
        self.req = None

    def Run(self, req, metadata=None):
        self.req = req
        return _EmptyStream()

    def Continue(self, req, metadata=None):
        self.req = req
        return _EmptyStream()


# The routing + budget knobs are plain proto3 fields: "" / 0 is unset, so they
# carry their value with no presence handling.
ROUTING_AND_BUDGET = {
    "model": "some-model",
    "provider": "some-provider",
    "tier": "middle",
    "effort": "high",
    "max_tokens": 4096,
    "max_iterations": 12,
    "max_concurrent_children": 2,
}

# The tuning row is proto3 `optional` precisely because each has a meaningful
# zero. Every value here IS that zero, so a client that forwarded None as the
# default — or dropped the field as falsy — fails.
TUNING_ZEROS = {
    "unbounded_iterations": False,
    "retry_attempts": 0,
    "memory_inject_max_tokens": 0,
    "memory_index_max_bytes": 0,
    "inject_tool_guide": False,
}


def _assert_carries_everything(req) -> None:
    for name, want in ROUTING_AND_BUDGET.items():
        assert getattr(req, name) == want, f"{name}: got {getattr(req, name)!r}, want {want!r}"
    for name, want in TUNING_ZEROS.items():
        assert req.HasField(name), f"{name} is unset — its meaningful zero was dropped as falsy"
        assert getattr(req, name) == want, f"{name}: got {getattr(req, name)!r}, want {want!r}"


@pytest.mark.asyncio
async def test_run_streaming_carries_every_override():
    stub = _CaptureStub()
    client = _make_client()
    client._stub = stub  # type: ignore[assignment]

    async for _ in client.run_streaming(
        agent="default", segments=[], **ROUTING_AND_BUDGET, **TUNING_ZEROS
    ):
        pass

    _assert_carries_everything(stub.req)


@pytest.mark.asyncio
async def test_continue_session_carries_every_override():
    stub = _CaptureStub()
    client = _make_client()
    client._stub = stub  # type: ignore[assignment]

    async for _ in client.continue_session(
        session_id="s_1", segments=[], **ROUTING_AND_BUDGET, **TUNING_ZEROS
    ):
        pass

    _assert_carries_everything(stub.req)


@pytest.mark.asyncio
async def test_spawn_run_batch_child_carries_every_override():
    """A fan-out child takes the same overrides, so one batch can run the same
    agent across several models or budgets."""
    client = _make_client()
    captured: dict = {}

    async def fake(req, metadata=None):
        captured["req"] = req
        return pb.BatchSpawnResult(spawned=1, results=[pb.SpawnResult(agent_id="a1")])

    client._stub.SpawnRunBatch = fake  # type: ignore[attr-defined]

    await client.spawn_run_batch(
        [{"agent": "reviewer", "segments": [], **ROUTING_AND_BUDGET, **TUNING_ZEROS}]
    )

    _assert_carries_everything(captured["req"].spawns[0])


@pytest.mark.asyncio
async def test_omitted_overrides_stay_unset():
    """Non-vacuity, and the contract itself: saying nothing must reach the wire
    as an UNSET field, never as the zero that means "off"."""
    stub = _CaptureStub()
    client = _make_client()
    client._stub = stub  # type: ignore[assignment]

    async for _ in client.run_streaming(agent="default", segments=[]):
        pass

    for name in TUNING_ZEROS:
        assert not stub.req.HasField(name), f"{name} was set without the caller asking for it"
    for name in ROUTING_AND_BUDGET:
        assert not getattr(stub.req, name), f"{name} was set without the caller asking for it"
