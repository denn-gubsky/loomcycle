"""RFC DJ — a run held for an operator's verdict: the review_run RPC and the
review arming on every request-building surface. Stub-mock pattern; the server
side is covered in internal/api/grpc/review_test.go."""

from __future__ import annotations

from typing import Any

import grpc
import grpc.aio
import pytest

from loomcycle import LoomcycleClient
from loomcycle import client as client_mod
from loomcycle._generated import loomcycle_pb2 as pb
from loomcycle.errors import LoomcycleError


def _make_client() -> LoomcycleClient:
    return LoomcycleClient(channel=grpc.aio.insecure_channel("127.0.0.1:1"))


class _EmptyStream:
    def __aiter__(self):
        return self

    async def __anext__(self):
        raise StopAsyncIteration


class _CaptureStub:
    def __init__(self) -> None:
        self.req: Any = None

    def Run(self, req, metadata=None):
        self.req = req
        return _EmptyStream()

    def Continue(self, req, metadata=None):
        self.req = req
        return _EmptyStream()


def _capturing(result: Any):
    captured: dict = {}

    async def fn(req, metadata=None):
        captured["req"] = req
        return result

    return fn, captured


@pytest.mark.asyncio
async def test_review_run_sends_the_verdict_and_decodes_the_reply():
    client = _make_client()
    fake, captured = _capturing(pb.ReviewRunResponse(run_id="r1", decision="reject", delivered=True))
    client._stub.ReviewRun = fake  # type: ignore[attr-defined]

    out = await client.review_run("r1", "reject", feedback="cover the rollback")
    req = captured["req"]
    assert (req.run_id, req.decision, req.feedback) == ("r1", "reject", "cover the rollback")
    assert out == {"run_id": "r1", "decision": "reject", "delivered": True}


class _NotHeld(grpc.aio.AioRpcError):
    def __init__(self) -> None:
        super().__init__(
            grpc.StatusCode.FAILED_PRECONDITION,
            grpc.aio.Metadata(),
            grpc.aio.Metadata(),
            details="connector: run is not held for review",
        )


@pytest.mark.asyncio
async def test_review_run_on_a_run_not_held_raises_with_the_code():
    client = _make_client()

    async def fake(req, metadata=None):
        raise _NotHeld()

    client._stub.ReviewRun = fake  # type: ignore[attr-defined]
    with pytest.raises(LoomcycleError) as exc:
        await client.review_run("r1", "approve")
    assert exc.value.code == grpc.StatusCode.FAILED_PRECONDITION


@pytest.mark.asyncio
async def test_run_and_continue_carry_review():
    for method, kwargs in (
        ("run_streaming", {"agent": "default"}),
        ("continue_session", {"session_id": "s_1"}),
    ):
        stub = _CaptureStub()
        client = _make_client()
        client._stub = stub  # type: ignore[assignment]
        async for _ in getattr(client, method)(segments=[], review=True, **kwargs):
            pass
        assert stub.req.review is True, f"{method} dropped review"


def test_a_spawn_child_carries_review_and_defaults_to_unarmed():
    assert client_mod._run_request_from_dict({"agent": "a", "segments": [], "review": True}).review is True
    assert client_mod._run_request_from_dict({"agent": "a", "segments": []}).review is False


@pytest.mark.asyncio
async def test_retune_and_run_input_carry_review_with_false_distinct_from_unset():
    """false disarms — and releases a held run as approved — so it must reach
    the wire SET, not dropped as falsy."""
    client = _make_client()
    fake, captured = _capturing(pb.RetuneRunResponse(run_id="r1", retuned=True))
    client._stub.RetuneRun = fake  # type: ignore[attr-defined]
    await client.retune_run("r1", review=False)
    assert captured["req"].HasField("review") and captured["req"].review is False

    fake, captured = _capturing(pb.RunInputResponse(run_id="r1", delivered=True))
    client._stub.RunInput = fake  # type: ignore[attr-defined]
    await client.run_input("r1", "go on")
    assert not captured["req"].HasField("review"), "an unset review reached the wire set"


@pytest.mark.asyncio
async def test_run_and_continue_carry_the_review_deadline():
    for method, kwargs in (
        ("run_streaming", {"agent": "default"}),
        ("continue_session", {"session_id": "s_1"}),
    ):
        stub = _CaptureStub()
        client = _make_client()
        client._stub = stub  # type: ignore[assignment]
        async for _ in getattr(client, method)(segments=[], review=True, review_ttl_seconds=90, **kwargs):
            pass
        assert stub.req.review_ttl_seconds == 90, f"{method} dropped review_ttl_seconds"
    child = client_mod._run_request_from_dict({"agent": "a", "segments": [], "review_ttl_seconds": 30})
    assert child.review_ttl_seconds == 30


def test_the_held_event_carries_its_deadline():
    from loomcycle.events import AgentEvent

    ev = pb.Event(type="awaiting_review", awaiting_review=pb.AwaitingReview(round=1, expires_at="2026-09-24T12:00:00Z"))
    assert AgentEvent._from_proto(ev).awaiting_review.expires_at == "2026-09-24T12:00:00Z"


@pytest.mark.asyncio
async def test_run_and_continue_carry_the_runs_hook_additions():
    import json as _json

    hooks = {"agent_stop": ["cite@3"]}
    tool_hooks = {"WebFetch": {"pre": [{"name": "gate", "url": "https://h.example"}]}}
    for method, kwargs in (
        ("run_streaming", {"agent": "default"}),
        ("continue_session", {"session_id": "s_1"}),
    ):
        stub = _CaptureStub()
        client = _make_client()
        client._stub = stub  # type: ignore[assignment]
        async for _ in getattr(client, method)(segments=[], hooks=hooks, tool_hooks=tool_hooks, **kwargs):
            pass
        assert _json.loads(stub.req.hooks_json) == {"hooks": hooks, "tool_hooks": tool_hooks}, f"{method} dropped the hooks"
    child = client_mod._run_request_from_dict({"agent": "a", "segments": [], "hooks": hooks})
    assert _json.loads(child.hooks_json) == {"hooks": hooks}
    assert client_mod._run_request_from_dict({"agent": "a", "segments": []}).hooks_json == b""
