"""get_run — the run read addressed by run id (GetRun RPC). Stub-mock
pattern; the server side is covered in internal/api/grpc/get_run_test.go."""

from __future__ import annotations

import grpc
import grpc.aio
import pytest

from loomcycle import LoomcycleClient
from loomcycle._generated import loomcycle_pb2 as pb
from loomcycle.errors import AgentNotFoundError


def _make_client() -> LoomcycleClient:
    return LoomcycleClient(channel=grpc.aio.insecure_channel("127.0.0.1:1"))


@pytest.mark.asyncio
async def test_get_run_sends_the_run_id_and_decodes_the_agent():
    client = _make_client()
    captured: dict = {}

    async def fake(req, metadata=None):
        captured["req"] = req
        return pb.Agent(
            agent_id="team:triage",
            run_id="r_walk_1",
            status="running",
            live=True,
            awaited_state="review",
        )

    client._stub.GetRun = fake  # type: ignore[attr-defined]

    out = await client.get_run("r_walk_1")
    assert captured["req"].run_id == "r_walk_1"
    assert out["run_id"] == "r_walk_1"
    assert out["agent_id"] == "team:triage"
    assert out["live"] is True
    assert out["awaited_state"] == "review"


@pytest.mark.asyncio
async def test_get_run_decodes_a_queued_runs_place():
    client = _make_client()

    async def fake(req, metadata=None):
        return pb.Agent(run_id="r_q", status="queued", queue_position=3, queue_reason="user_cap")

    client._stub.GetRun = fake  # type: ignore[attr-defined]

    out = await client.get_run("r_q")
    assert out["status"] == "queued"
    assert out["queue_position"] == 3
    assert out["queue_reason"] == "user_cap"


class _NotFound(grpc.aio.AioRpcError):
    def __init__(self) -> None:
        super().__init__(
            grpc.StatusCode.NOT_FOUND,
            grpc.aio.Metadata(),
            grpc.aio.Metadata(),
            details='no run found for run_id "r_missing"',
        )


@pytest.mark.asyncio
async def test_get_run_unknown_raises_not_found():
    client = _make_client()

    async def fake(req, metadata=None):
        raise _NotFound()

    client._stub.GetRun = fake  # type: ignore[attr-defined]

    with pytest.raises(AgentNotFoundError):
        await client.get_run("r_missing")
