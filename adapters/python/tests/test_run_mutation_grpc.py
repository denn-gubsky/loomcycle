"""v0.8.0 — run-mutation + resolver gRPC parity:
spawn_run_batch, compact_run, resolve_probe, and the per-run
sampling/compaction overrides on run_streaming.

Stub-mock pattern (no live server). The server-side path is covered by
internal/api/grpc/server_test.go.
"""

from __future__ import annotations

from typing import Any

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


class _EmptyStream:
    def __aiter__(self):
        return self

    async def __anext__(self):
        raise StopAsyncIteration


@pytest.mark.asyncio
async def test_spawn_run_batch_builds_children_and_decodes_envelope():
    client = _make_client()
    resp = pb.BatchSpawnResult(
        spawned=2,
        results=[
            pb.SpawnResult(agent_id="a1", run_id="r1", status="completed", final_text="hi"),
            pb.SpawnResult(agent_id="a2", run_id="r2", status="failed", error="boom"),
        ],
    )
    fake, captured = _async_returning(resp)
    client._stub.SpawnRunBatch = fake  # type: ignore[attr-defined]

    out = await client.spawn_run_batch(
        [
            {"agent": "reviewer", "segments": [], "user_tier": "pro"},
            {"agent": "reviewer", "segments": []},
        ],
        timeout_ms=5000,
    )
    # Request: two RunRequest children, mode default "join", timeout threaded.
    req = captured["req"]
    assert len(req.spawns) == 2
    assert req.spawns[0].agent == "reviewer"
    assert req.spawns[0].user_tier == "pro"
    assert req.mode == "join"
    assert req.timeout_ms == 5000
    # Response: index-aligned envelope, per-child failure in-envelope.
    assert out["spawned"] == 2
    assert out["results"][0]["status"] == "completed"
    assert out["results"][0]["final_text"] == "hi"
    assert out["results"][1]["status"] == "failed"
    assert out["results"][1]["error"] == "boom"


@pytest.mark.asyncio
async def test_compact_run_decodes_result():
    client = _make_client()
    resp = pb.CompactRunResult(
        run_id="r1", compacted=True, before_tokens=1000, after_tokens=300, applied="live"
    )
    fake, captured = _async_returning(resp)
    client._stub.CompactRun = fake  # type: ignore[attr-defined]

    out = await client.compact_run("r1", reason="manual")
    assert captured["req"].run_id == "r1"
    assert captured["req"].reason == "manual"
    assert out == {
        "run_id": "r1",
        "compacted": True,
        "before_tokens": 1000,
        "after_tokens": 300,
        "applied": "live",
    }


@pytest.mark.asyncio
async def test_cancel_turn_decodes_result():
    client = _make_client()
    resp = pb.CancelTurnResponse(run_id="r1", stopped=True, parked=True)
    fake, captured = _async_returning(resp)
    client._stub.CancelTurn = fake  # type: ignore[attr-defined]

    out = await client.cancel_turn("r1", reason="too slow")
    assert captured["req"].run_id == "r1"
    assert captured["req"].reason == "too slow"
    assert out == {"run_id": "r1", "stopped": True, "parked": True}


@pytest.mark.asyncio
async def test_resolve_interrupt_answer_decodes_result():
    client = _make_client()
    resp = pb.ResolveInterruptResponse(interrupt_id="i1", status="resolved")
    fake, captured = _async_returning(resp)
    client._stub.ResolveInterrupt = fake  # type: ignore[attr-defined]

    out = await client.resolve_interrupt("r1", "i1", answer="Yes")
    req = captured["req"]
    assert req.run_id == "r1"
    assert req.interrupt_id == "i1"
    assert req.answer == "Yes"
    assert req.disposition == ""
    assert out == {"interrupt_id": "i1", "status": "resolved"}


@pytest.mark.asyncio
async def test_cancel_interrupt_sends_declined_without_answer():
    client = _make_client()
    resp = pb.ResolveInterruptResponse(interrupt_id="i1", status="declined")
    fake, captured = _async_returning(resp)
    client._stub.ResolveInterrupt = fake  # type: ignore[attr-defined]

    out = await client.cancel_interrupt("r1", "i1")
    req = captured["req"]
    assert req.disposition == "declined"
    assert req.answer == ""
    assert out == {"interrupt_id": "i1", "status": "declined"}


@pytest.mark.asyncio
async def test_resolve_probe_decodes_matrix():
    client = _make_client()
    resp = pb.ResolverMatrixResponse()
    prov = resp.providers["anthropic"]
    prov.excluded = False
    prov.reachable = True
    prov.models["claude-opus-4-8"].listed = True
    prov.models["claude-opus-4-8"].stalled = False
    fake, _ = _async_returning(resp)
    client._stub.ResolveProbe = fake  # type: ignore[attr-defined]

    out = await client.resolve_probe()
    assert out["providers"]["anthropic"]["reachable"] is True
    assert out["providers"]["anthropic"]["models"]["claude-opus-4-8"]["listed"] is True


class _CaptureRunStub:
    def __init__(self) -> None:
        self.last_run_req = None

    def Run(self, req, metadata=None):
        self.last_run_req = req
        return _EmptyStream()


@pytest.mark.asyncio
async def test_run_streaming_threads_sampling_preserving_zero_temperature():
    """The exp7/F33 contract: an explicit temperature 0.0 is
    deterministic, NOT dropped as falsy. Proto3 optional presence
    distinguishes set-to-zero from unset."""
    stub = _CaptureRunStub()
    client = _make_client()
    client._stub = stub  # type: ignore[assignment]

    async for _ in client.run_streaming(
        agent="default",
        segments=[],
        sampling={"temperature": 0.0, "top_p": 0.9, "stop": ["END"]},
        compaction={"enabled": True, "keep_last_n": 8},
    ):
        pass

    req = stub.last_run_req
    assert req.HasField("sampling")
    assert req.sampling.HasField("temperature")
    assert req.sampling.temperature == 0.0
    assert req.sampling.top_p == 0.9
    assert list(req.sampling.stop) == ["END"]
    # top_k was not supplied → unset (presence false).
    assert not req.sampling.HasField("top_k")
    assert req.HasField("compaction")
    assert req.compaction.enabled is True
    assert req.compaction.keep_last_n == 8


@pytest.mark.asyncio
async def test_run_streaming_omits_sampling_when_not_supplied():
    stub = _CaptureRunStub()
    client = _make_client()
    client._stub = stub  # type: ignore[assignment]
    async for _ in client.run_streaming(agent="default", segments=[]):
        pass
    assert not stub.last_run_req.HasField("sampling")
    assert not stub.last_run_req.HasField("compaction")
