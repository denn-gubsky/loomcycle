"""list_user_agents — status lists, awaited-state filter, truncated (the
ListUserAgents RPC). Stub-mock pattern; the server side is covered in
internal/api/grpc/list_user_agents_filter_test.go."""

from __future__ import annotations

import grpc
import grpc.aio
import pytest

from loomcycle import AgentList, LoomcycleClient
from loomcycle._generated import loomcycle_pb2 as pb
from loomcycle.errors import InvalidArgumentError


def _make_client() -> LoomcycleClient:
    return LoomcycleClient(channel=grpc.aio.insecure_channel("127.0.0.1:1"))


def _capture(client: LoomcycleClient, resp: pb.ListUserAgentsResponse) -> dict:
    captured: dict = {}

    async def fake(req, metadata=None):
        captured["req"] = req
        return resp

    client._stub.ListUserAgents = fake  # type: ignore[attr-defined]
    return captured


@pytest.mark.asyncio
async def test_a_single_status_string_is_sent_as_before():
    client = _make_client()
    captured = _capture(client, pb.ListUserAgentsResponse(agents=[pb.Agent(agent_id="a1", status="failed")]))

    out = await client.list_user_agents("alice", status="failed")
    req = captured["req"]
    assert (req.status, list(req.statuses), list(req.awaited_states)) == ("failed", [], [])
    assert [a["agent_id"] for a in out] == ["a1"]
    assert isinstance(out, list) and out.truncated is False


@pytest.mark.asyncio
async def test_status_and_awaited_state_lists_go_in_the_repeated_fields():
    client = _make_client()
    captured = _capture(client, pb.ListUserAgentsResponse())

    await client.list_user_agents("alice", status=["completed", "failed"])
    req = captured["req"]
    assert (req.status, list(req.statuses)) == ("", ["completed", "failed"])

    await client.list_user_agents("alice", awaited_state="review")
    assert list(captured["req"].awaited_states) == ["review"]

    await client.list_user_agents("alice", status=("running",), awaited_state=["review", "input"])
    req = captured["req"]
    assert (list(req.statuses), list(req.awaited_states)) == (["running"], ["review", "input"])


@pytest.mark.asyncio
async def test_the_answer_carries_truncated_from_the_server():
    client = _make_client()
    _capture(
        client,
        pb.ListUserAgentsResponse(
            agents=[pb.Agent(agent_id="a1", status="running", awaited_state="review")],
            truncated=True,
        ),
    )
    out = await client.list_user_agents("alice", awaited_state="review")
    assert isinstance(out, AgentList)
    assert out.truncated is True
    assert out[0]["awaited_state"] == "review"


class _InvalidArgument(grpc.aio.AioRpcError):
    def __init__(self) -> None:
        super().__init__(
            grpc.StatusCode.INVALID_ARGUMENT,
            grpc.aio.Metadata(),
            grpc.aio.Metadata(),
            details='awaited_states: "reviw" is not one of channel, interrupted, review, input, children',
        )


@pytest.mark.asyncio
async def test_an_unknown_filter_value_raises_invalid_argument():
    client = _make_client()

    async def fake(req, metadata=None):
        raise _InvalidArgument()

    client._stub.ListUserAgents = fake  # type: ignore[attr-defined]
    with pytest.raises(InvalidArgumentError):
        await client.list_user_agents("alice", awaited_state="reviw")
