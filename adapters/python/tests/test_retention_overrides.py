"""The timed override of an agent's no-trace mode: list, set and clear over
the RetentionOverride RPC. These tests pin the request each method builds and
the shape it returns."""

import datetime

import grpc
import pytest

from loomcycle import LoomcycleClient
from loomcycle._generated import loomcycle_pb2 as pb


class _Stub:
    def __init__(self, resp: pb.RetentionOverrideResponse) -> None:
        self.resp = resp
        self.last: pb.RetentionOverrideRequest = None  # type: ignore[assignment]

    async def RetentionOverride(self, req, metadata=None):
        self.last = req
        return self.resp


def _client(stub: _Stub) -> LoomcycleClient:
    client = LoomcycleClient(channel=grpc.aio.insecure_channel("127.0.0.1:1"))
    client._stub = stub  # type: ignore[assignment]
    return client


_ENTRY = pb.RetentionOverrideEntry(
    tenant="acme", agent="judge", until="2026-10-12T18:00:00Z", set_by="ops", created_at="2026-10-12T16:00:00Z"
)


@pytest.mark.asyncio
async def test_list_sends_op_and_tenant_and_returns_dicts():
    stub = _Stub(pb.RetentionOverrideResponse(overrides=[_ENTRY]))
    got = await _client(stub).list_retention_overrides(tenant="globex")
    assert (stub.last.op, stub.last.tenant) == ("list", "globex")
    assert got == [
        {
            "tenant": "acme",
            "agent": "judge",
            "until": "2026-10-12T18:00:00Z",
            "set_by": "ops",
            "created_at": "2026-10-12T16:00:00Z",
        }
    ]


@pytest.mark.asyncio
async def test_set_takes_a_string_or_an_aware_datetime():
    stub = _Stub(pb.RetentionOverrideResponse(overrides=[_ENTRY]))
    client = _client(stub)
    got = await client.set_retention_override(agent="judge", until="2026-10-12T18:00:00Z")
    assert (stub.last.op, stub.last.agent, stub.last.until, stub.last.tenant) == (
        "set",
        "judge",
        "2026-10-12T18:00:00Z",
        "",
    )
    assert got["agent"] == "judge"

    # An aware datetime in another zone is sent as the same instant in UTC.
    plus2 = datetime.timezone(datetime.timedelta(hours=2))
    await client.set_retention_override(
        agent="judge", until=datetime.datetime(2026, 10, 12, 20, 0, tzinfo=plus2), tenant="globex"
    )
    assert (stub.last.until, stub.last.tenant) == ("2026-10-12T18:00:00Z", "globex")

    # A naive one does not say which instant it is.
    with pytest.raises(ValueError):
        await client.set_retention_override(agent="judge", until=datetime.datetime(2026, 10, 12, 18, 0))


@pytest.mark.asyncio
async def test_clear_reports_whether_there_was_one():
    stub = _Stub(pb.RetentionOverrideResponse(cleared=True))
    assert await _client(stub).clear_retention_override(agent="judge") is True
    assert (stub.last.op, stub.last.agent) == ("clear", "judge")
    stub.resp = pb.RetentionOverrideResponse()
    assert await _client(stub).clear_retention_override(agent="judge", tenant="globex") is False
    assert stub.last.tenant == "globex"
