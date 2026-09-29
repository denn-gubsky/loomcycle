"""list_walk_runs — one team walk's runs, paged (ListWalkRuns RPC). Stub-mock
pattern; the server side is covered in internal/api/grpc/list_walk_runs_test.go."""

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
async def test_list_walk_runs_sends_the_page_arguments_and_decodes_the_page():
    client = _make_client()
    captured: dict = {}

    async def fake(req, metadata=None):
        captured["req"] = req
        return pb.ListWalkRunsResponse(
            agents=[
                pb.Agent(agent_id="team:triage", run_id="r_walk", status="running", live=True),
                pb.Agent(agent_id="writer", run_id="r_member", status="running", awaited_state="channel"),
            ],
            next_cursor="run_0000000000000001_r_member",
        )

    client._stub.ListWalkRuns = fake  # type: ignore[attr-defined]

    page = await client.list_walk_runs("r_walk", limit=2, cursor="run_0000000000000000_r_a")
    req = captured["req"]
    assert (req.walk_id, req.limit, req.cursor) == ("r_walk", 2, "run_0000000000000000_r_a")
    assert [a["run_id"] for a in page["agents"]] == ["r_walk", "r_member"]
    assert page["agents"][1]["awaited_state"] == "channel"
    assert page["next_cursor"] == "run_0000000000000001_r_member"


@pytest.mark.asyncio
async def test_list_walk_runs_defaults_send_no_limit_and_no_cursor():
    client = _make_client()
    captured: dict = {}

    async def fake(req, metadata=None):
        captured["req"] = req
        return pb.ListWalkRunsResponse()

    client._stub.ListWalkRuns = fake  # type: ignore[attr-defined]

    page = await client.list_walk_runs("r_walk")
    assert (captured["req"].limit, captured["req"].cursor) == (0, "")
    assert page == {"agents": [], "next_cursor": ""}


class _NotFound(grpc.aio.AioRpcError):
    def __init__(self) -> None:
        super().__init__(
            grpc.StatusCode.NOT_FOUND,
            grpc.aio.Metadata(),
            grpc.aio.Metadata(),
            details='no walk found for walk_id "r_missing"',
        )


@pytest.mark.asyncio
async def test_list_walk_runs_unknown_walk_raises_not_found():
    client = _make_client()

    async def fake(req, metadata=None):
        raise _NotFound()

    client._stub.ListWalkRuns = fake  # type: ignore[attr-defined]

    with pytest.raises(AgentNotFoundError):
        await client.list_walk_runs("r_missing")
