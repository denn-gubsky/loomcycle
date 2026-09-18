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

import json

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


@pytest.mark.asyncio
async def test_retune_run_carries_every_override_and_sends_no_text():
    """The gap a consumer reported: a parked chat could not be retuned through
    the SDK at all. `run_input` took text only, and the proto had no override
    fields, so there was nothing to send them on."""
    client = _make_client()
    captured: dict = {}

    async def fake(req, metadata=None):
        captured["req"] = req
        return pb.RetuneRunResponse(run_id="r_1", retuned=True)

    client._stub.RetuneRun = fake  # type: ignore[attr-defined]

    out = await client.retune_run("r_1", **ROUTING_AND_BUDGET, **TUNING_ZEROS)
    assert out == {"run_id": "r_1", "retuned": True}
    _assert_carries_everything(captured["req"])
    assert captured["req"].run_id == "r_1"
    # No turn rides along — that is the entire point of the separate RPC.
    assert not hasattr(captured["req"], "text")


@pytest.mark.asyncio
async def test_run_input_carries_every_override_beside_the_text():
    """The other half: retune and speak in one call."""
    client = _make_client()
    captured: dict = {}

    async def fake(req, metadata=None):
        captured["req"] = req
        return pb.RunInputResponse(run_id="r_1", delivered=True)

    client._stub.RunInput = fake  # type: ignore[attr-defined]

    await client.run_input("r_1", "carry on", **ROUTING_AND_BUDGET, **TUNING_ZEROS)
    req = captured["req"]
    assert req.text == "carry on"
    _assert_carries_everything(req)


@pytest.mark.asyncio
async def test_run_input_without_overrides_leaves_them_unset():
    """Non-vacuity, and the contract: saying nothing must reach the wire as an
    UNSET field, never as the zero that means 'no retries' / 'inject nothing'."""
    client = _make_client()
    captured: dict = {}

    async def fake(req, metadata=None):
        captured["req"] = req
        return pb.RunInputResponse(run_id="r_1", delivered=True)

    client._stub.RunInput = fake  # type: ignore[attr-defined]

    await client.run_input("r_1", "hello")
    req = captured["req"]
    for name in TUNING_ZEROS:
        assert not req.HasField(name), f"{name} was set without the caller asking"
    for name in ROUTING_AND_BUDGET:
        assert not getattr(req, name), f"{name} was set without the caller asking"


@pytest.mark.asyncio
async def test_metadata_context_and_lineage_reach_both_request_paths():
    """These three were absent from the gRPC wire entirely, so a Python caller
    could not send agent metadata, set the per-run context block, or carry
    cost-attribution lineage — all reachable from HTTP since they shipped.

    Both paths, because they are built DIFFERENTLY: run_streaming goes through
    _build_run_request and continue_session constructs pb.ContinueRequest
    directly. That asymmetry is what left the steer path without overrides, so
    it is asserted rather than assumed.
    """
    meta = {"repo": "loomcycle", "reviewers": 2}
    ctx = {"mode": "stateful", "keep_last_n": 6, "state_schema": {"type": "object"}}
    lineage = {"root_agent_run_id": "r_root", "function_key": "cv", "tier_at_run": "pro"}

    for path in ("run", "continue"):
        stub = _CaptureStub()
        client = _make_client()
        client._stub = stub  # type: ignore[assignment]
        kwargs = dict(metadata=meta, context=ctx, parent_context=lineage)
        if path == "run":
            async for _ in client.run_streaming(agent="default", segments=[], **kwargs):
                pass
        else:
            async for _ in client.continue_session(session_id="s_1", segments=[], **kwargs):
                pass

        req = stub.req
        assert json.loads(req.metadata) == meta, path
        assert req.context.mode == "stateful", path
        assert req.context.keep_last_n == 6, path
        assert json.loads(req.context.state_schema) == {"type": "object"}, path
        assert req.parent_context.root_agent_run_id == "r_root", path
        assert req.parent_context.function_key == "cv", path


@pytest.mark.asyncio
async def test_an_absent_context_key_stays_unset_rather_than_zero():
    """Every Context scalar is proto3 `optional` because each has a meaningful
    zero: keep_last_n 0 means keep none, not 'unset'."""
    stub = _CaptureStub()
    client = _make_client()
    client._stub = stub  # type: ignore[assignment]
    async for _ in client.run_streaming(agent="default", segments=[], context={"mode": "recap"}):
        pass

    req = stub.req
    assert req.context.mode == "recap"
    assert not req.context.HasField("keep_last_n")
    assert not req.context.HasField("recap_max_chars")
    assert req.context.state_schema == b""


@pytest.mark.asyncio
async def test_a_zero_context_value_is_sent_not_dropped():
    """The other half: an explicit 0 must reach the wire as a set field."""
    stub = _CaptureStub()
    client = _make_client()
    client._stub = stub  # type: ignore[assignment]
    async for _ in client.run_streaming(
        agent="default", segments=[], context={"keep_last_n": 0, "recall": False}
    ):
        pass

    req = stub.req
    assert req.context.HasField("keep_last_n"), "an explicit 0 was dropped as falsy"
    assert req.context.keep_last_n == 0
    assert req.context.HasField("recall")
    assert req.context.recall is False
