"""App sign-ins over gRPC: each method calls its RPC with the request the
server reads, and the returned mapping carries every field of the proto's
AppSignIn — derived from the descriptor, so a field the server adds fails
here rather than silently dropping."""

import grpc
import pytest

from loomcycle import LoomcycleClient
from loomcycle._generated import loomcycle_pb2 as pb

_ROW = pb.AppSignIn(
    family_id="fam1",
    client_id="board",
    client_name="Board",
    gate="sso",
    created_at="2026-11-01T00:00:00Z",
    last_used_at="2026-11-02T00:00:00Z",
    expires_at="2026-11-09T00:00:00Z",
)


class _Stub:
    def __init__(self):
        self.calls = []

    async def ListUserAppSignIns(self, req, metadata=None):
        self.calls.append(("ListUserAppSignIns", req))
        return pb.AppSignInsResponse(subject=req.subject, app_sign_ins=[_ROW])

    async def EndUserAppSignIn(self, req, metadata=None):
        self.calls.append(("EndUserAppSignIn", req))
        return pb.EndAppSignInResponse()

    async def ListMyAppSignIns(self, req, metadata=None):
        self.calls.append(("ListMyAppSignIns", req))
        return pb.AppSignInsResponse(subject="me", app_sign_ins=[_ROW])

    async def EndMyAppSignIn(self, req, metadata=None):
        self.calls.append(("EndMyAppSignIn", req))
        return pb.EndAppSignInResponse()


def _client():
    client = LoomcycleClient(channel=grpc.aio.insecure_channel("127.0.0.1:1"))
    stub = _Stub()
    client._stub = stub  # type: ignore[assignment]
    return client, stub


@pytest.mark.asyncio
async def test_list_user_app_sign_ins_carries_every_proto_field():
    client, stub = _client()
    got = await client.list_user_app_sign_ins("bob")
    assert stub.calls[0][1].subject == "bob"
    assert got["subject"] == "bob"
    fields = {f.name for f in pb.AppSignIn.DESCRIPTOR.fields}
    assert set(got["app_sign_ins"][0]) == fields
    assert got["app_sign_ins"][0]["expires_at"] == "2026-11-09T00:00:00Z"


@pytest.mark.asyncio
async def test_end_user_app_sign_in_names_the_user_and_the_family():
    client, stub = _client()
    assert await client.end_user_app_sign_in("bob", "fam1") is None
    req = stub.calls[0][1]
    assert (req.subject, req.family_id) == ("bob", "fam1")


@pytest.mark.asyncio
async def test_own_app_sign_ins_list_and_end():
    client, stub = _client()
    got = await client.list_my_app_sign_ins()
    assert got["subject"] == "me" and len(got["app_sign_ins"]) == 1
    await client.end_my_app_sign_in("fam1")
    assert [c[0] for c in stub.calls] == ["ListMyAppSignIns", "EndMyAppSignIn"]
    assert stub.calls[1][1].family_id == "fam1"
