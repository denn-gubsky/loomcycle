"""Ontology subject merge over gRPC: merge_subjects sends the two references
and an admin's tenant focus, returns the result as a mapping, and a refusal
carries the merge code on ``.reason``."""

import grpc
import pytest

from loomcycle import LoomcycleClient
from loomcycle._generated import loomcycle_pb2 as pb
from loomcycle.errors import LoomcycleError

# A FailedPrecondition status whose details hold
# ErrorInfo{reason: "already_merged", domain: "loomcycle"}, as the server sends it.
_ALREADY_MERGED = (
    "08091237706572736f6e3a6164612d6b696e672077617320616c7265616479206d657267656420"
    "696e746f20616e6f74686572207375626a6563741a470a28747970652e676f6f676c6561706973"
    "2e636f6d2f676f6f676c652e7270632e4572726f72496e666f121b0a0e616c72656164795f6d65"
    "7267656412096c6f6f6d6379636c65"
)


class _Stub:
    def __init__(self):
        self.req = None

    async def MergeSubjects(self, req, metadata=None):
        self.req = req
        return pb.MergeSubjectsResponse(
            tenant="acme",
            duplicate=pb.SubjectRef(document_id="d1", root_chunk_id="r1", natural_key="person:ada-king"),
            survivor=pb.SubjectRef(document_id="d2", root_chunk_id="r2", natural_key="person:ada-lovelace"),
            facts_moved=2,
            statements_repointed=1,
            statements_retired=1,
            statements_left=0,
        )


def _client(stub) -> LoomcycleClient:
    client = LoomcycleClient(channel=grpc.aio.insecure_channel("127.0.0.1:1"))
    client._stub = stub  # type: ignore[assignment]
    return client


@pytest.mark.asyncio
async def test_merge_subjects_sends_the_references_and_returns_the_result():
    stub = _Stub()
    got = await _client(stub).merge_subjects("person:ada-king", "d2", tenant="globex")
    assert (stub.req.duplicate, stub.req.survivor, stub.req.tenant) == ("person:ada-king", "d2", "globex")
    assert got == {
        "tenant": "acme",
        "duplicate": {"document_id": "d1", "root_chunk_id": "r1", "natural_key": "person:ada-king"},
        "survivor": {"document_id": "d2", "root_chunk_id": "r2", "natural_key": "person:ada-lovelace"},
        "facts_moved": 2,
        "statements_repointed": 1,
        "statements_retired": 1,
        "statements_left": 0,
        "not_announced": "",
    }


@pytest.mark.asyncio
async def test_merge_subjects_refusal_carries_the_merge_code_as_reason():
    trailing = grpc.aio.Metadata()
    trailing.add("grpc-status-details-bin", bytes.fromhex(_ALREADY_MERGED))
    err = grpc.aio.AioRpcError(
        code=grpc.StatusCode.FAILED_PRECONDITION,
        initial_metadata=grpc.aio.Metadata(),
        trailing_metadata=trailing,
        details="person:ada-king was already merged into another subject",
        debug_error_string="",
    )

    class _Refusing:
        async def MergeSubjects(self, req, metadata=None):
            raise err

    with pytest.raises(LoomcycleError) as ei:
        await _client(_Refusing()).merge_subjects("d1", "d2")
    assert ei.value.reason == "already_merged"
    assert "already merged" in str(ei.value)
