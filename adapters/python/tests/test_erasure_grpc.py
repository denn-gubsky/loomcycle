"""Subject erasure over gRPC: the structured ``access`` field — the groups,
lists and revoked tokens an erasure names — reaches the returned mapping on
both erasure_report and erasure_execute."""

import grpc
import pytest

from loomcycle import LoomcycleClient
from loomcycle._generated import loomcycle_pb2 as pb

_ACCESS = pb.ErasureAccess(
    groups=["ops"],
    lists=["readers"],
    tokens=[pb.ErasureToken(def_id="def_1", name="u-alice")],
)

_WANT = {
    "groups": ["ops"],
    "lists": ["readers"],
    "tokens": [{"def_id": "def_1", "name": "u-alice"}],
}


class _Stub:
    async def ErasureReport(self, req, metadata=None):
        return pb.ErasureReportResponse(tenant="acme", subject=req.subject, access=_ACCESS)

    async def ErasureExecute(self, req, metadata=None):
        return pb.ErasureExecuteResponse(tenant="acme", subject=req.subject, dry_run=True, access=_ACCESS)


def _client() -> LoomcycleClient:
    client = LoomcycleClient(channel=grpc.aio.insecure_channel("127.0.0.1:1"))
    client._stub = _Stub()  # type: ignore[assignment]
    return client


@pytest.mark.asyncio
async def test_report_returns_the_access_field():
    got = await _client().erasure_report("alice")
    assert got["access"] == _WANT


@pytest.mark.asyncio
async def test_execute_returns_the_access_field():
    got = await _client().erasure_execute("alice")
    assert got["access"] == _WANT


@pytest.mark.asyncio
async def test_an_unset_access_is_empty_lists_not_missing():
    class _Empty:
        async def ErasureExecute(self, req, metadata=None):
            return pb.ErasureExecuteResponse(subject=req.subject)

    client = LoomcycleClient(channel=grpc.aio.insecure_channel("127.0.0.1:1"))
    client._stub = _Empty()  # type: ignore[assignment]
    got = await client.erasure_execute("alice")
    assert got["access"] == {"groups": [], "lists": [], "tokens": []}


_BOUND = [pb.ErasureBoundSubject(document_id="d1", natural_key="person:ada", path="/facts/ada")]
_WANT_BOUND = [{"document_id": "d1", "natural_key": "person:ada", "path": "/facts/ada"}]


@pytest.mark.asyncio
async def test_report_and_execute_return_the_bound_subjects():
    class _Bound:
        async def ErasureReport(self, req, metadata=None):
            return pb.ErasureReportResponse(subject=req.subject, bound_subjects=_BOUND, bound_subjects_withheld=2)

        async def ErasureExecute(self, req, metadata=None):
            return pb.ErasureExecuteResponse(
                subject=req.subject, bound_subjects=_BOUND, bound_subjects_withheld=2, not_announced="why"
            )

    client = LoomcycleClient(channel=grpc.aio.insecure_channel("127.0.0.1:1"))
    client._stub = _Bound()  # type: ignore[assignment]
    rep = await client.erasure_report("u-ada")
    assert (rep["bound_subjects"], rep["bound_subjects_withheld"]) == (_WANT_BOUND, 2)
    res = await client.erasure_execute("u-ada")
    assert (res["bound_subjects"], res["bound_subjects_withheld"], res["not_announced"]) == (_WANT_BOUND, 2, "why")
