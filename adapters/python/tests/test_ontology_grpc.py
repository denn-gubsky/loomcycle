"""Ontology subject operations over gRPC: merge_subjects and
bind_subject_principal send their arguments and an admin's tenant focus,
return the result as a mapping, and a refusal carries the code on
``.reason``."""

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


# A FailedPrecondition status whose details hold
# ErrorInfo{reason: "binding_conflict", domain: "loomcycle"}, as the server sends it.
_BINDING_CONFLICT = (
    "080912667072696e636970616c2022752d6164612220697320616c726561647920626f756e64"
    "20746f207375626a65637420706572736f6e3a6164612d6b696e672028646f63756d656e7420"
    "6431293b206f6e65207375626a65637420706572207072696e636970616c1a490a2874797065"
    "2e676f6f676c65617069732e636f6d2f676f6f676c652e7270632e4572726f72496e666f121d"
    "0a1062696e64696e675f636f6e666c69637412096c6f6f6d6379636c65"
)


class _BindStub:
    def __init__(self):
        self.req = None

    async def BindSubjectPrincipal(self, req, metadata=None):
        self.req = req
        return pb.BindSubjectPrincipalResponse(
            tenant="acme",
            document_id="d1",
            root_chunk_id="r1",
            natural_key="person:ada",
            path="/facts/ada",
            principal="u-ada",
            changed=True,
        )


@pytest.mark.asyncio
async def test_bind_subject_principal_sends_the_binding_and_returns_the_result():
    stub = _BindStub()
    got = await _client(stub).bind_subject_principal("person:ada", "u-ada", tenant="globex")
    assert (stub.req.subject, stub.req.principal, stub.req.tenant) == ("person:ada", "u-ada", "globex")
    assert got == {
        "tenant": "acme",
        "document_id": "d1",
        "root_chunk_id": "r1",
        "natural_key": "person:ada",
        "path": "/facts/ada",
        "principal": "u-ada",
        "previous": "",
        "changed": True,
    }


@pytest.mark.asyncio
async def test_bind_subject_principal_clears_with_an_empty_principal():
    stub = _BindStub()
    await _client(stub).bind_subject_principal("person:ada", "")
    assert (stub.req.subject, stub.req.principal) == ("person:ada", "")


@pytest.mark.asyncio
async def test_bind_subject_principal_refusal_carries_the_code_as_reason():
    trailing = grpc.aio.Metadata()
    trailing.add("grpc-status-details-bin", bytes.fromhex(_BINDING_CONFLICT))
    err = grpc.aio.AioRpcError(
        code=grpc.StatusCode.FAILED_PRECONDITION,
        initial_metadata=grpc.aio.Metadata(),
        trailing_metadata=trailing,
        details='principal "u-ada" is already bound to subject person:ada-king (document d1); one subject per principal',
        debug_error_string="",
    )

    class _Refusing:
        async def BindSubjectPrincipal(self, req, metadata=None):
            raise err

    with pytest.raises(LoomcycleError) as ei:
        await _client(_Refusing()).bind_subject_principal("person:ada-lovelace", "u-ada")
    assert ei.value.reason == "binding_conflict"
    assert "person:ada-king" in str(ei.value)


class _PlanStub:
    def __init__(self):
        self.req = None

    async def PlanOntology(self, req, metadata=None):
        self.req = req
        return pb.PlanOntologyResponse(
            tenant="acme",
            fingerprint="f" * 64,
            changes=[
                pb.OntologyPlannedChange(
                    op="delete",
                    entry=pb.OntologyPlanEntry(chunk_id="c1", name="Provision", section="classes", kind="class"),
                    risk="destroys",
                    counts={
                        "statements": pb.OntologyPlanCount(
                            n=3, effect="destroyed", tenant_scope=2, other_scopes=1,
                            by_status={"asserted": 3}, sample=["s1"],
                        )
                    },
                    subtree=[pb.OntologyPlanEntry(chunk_id="c2", name="Article", section="classes", kind="class")],
                    notes=["n"],
                )
            ],
            issues=["i"],
            scopes_counted=2,
            complete=True,
        )


@pytest.mark.asyncio
async def test_plan_ontology():
    stub = _PlanStub()
    got = await _client(stub).plan_ontology(
        [{"op": "delete", "entry": "c1"}, {"op": "rename", "entry": "c3", "name": "refers_to"}],
        tenant="globex",
    )
    assert stub.req.tenant == "globex"
    assert [(c.op, c.entry, c.name) for c in stub.req.changes] == [
        ("delete", "c1", ""),
        ("rename", "c3", "refers_to"),
    ]
    assert got == {
        "tenant": "acme",
        "fingerprint": "f" * 64,
        "changes": [
            {
                "op": "delete",
                "entry": {"chunk_id": "c1", "name": "Provision", "section": "classes", "kind": "class"},
                "risk": "destroys",
                "counts": {
                    "statements": {
                        "n": 3,
                        "effect": "destroyed",
                        "tenant_scope": 2,
                        "other_scopes": 1,
                        "by_status": {"asserted": 3},
                        "sample": ["s1"],
                    }
                },
                "subtree": [{"chunk_id": "c2", "name": "Article", "section": "classes", "kind": "class"}],
                "notes": ["n"],
            }
        ],
        "issues": ["i"],
        "new_issues": [],
        "scopes_counted": 2,
        "scopes_skipped": 0,
        "complete": True,
    }


def _status_details(code: int, message: str, reason: str) -> bytes:
    """A serialized google.rpc.Status carrying ErrorInfo{reason, domain:
    loomcycle}, as the server sends it in grpc-status-details-bin."""

    def field(num: int, payload: bytes) -> bytes:
        # Length-delimited fields only; every length here is under 128.
        assert len(payload) < 128
        return bytes([num << 3 | 2, len(payload)]) + payload

    info = field(1, reason.encode()) + field(2, b"loomcycle")
    any_ = field(1, b"type.googleapis.com/google.rpc.ErrorInfo") + field(2, info)
    return bytes([0x08, code]) + field(2, message.encode()) + field(3, any_)


@pytest.mark.asyncio
async def test_plan_ontology_refusal_carries_the_code_as_reason():
    trailing = grpc.aio.Metadata()
    trailing.add("grpc-status-details-bin", _status_details(5, "no in-force entry c9", "entry_not_found"))
    err = grpc.aio.AioRpcError(
        code=grpc.StatusCode.NOT_FOUND,
        initial_metadata=grpc.aio.Metadata(),
        trailing_metadata=trailing,
        details="changes[0]: no in-force entry c9",
        debug_error_string="",
    )

    class _Refusing:
        async def PlanOntology(self, req, metadata=None):
            raise err

    with pytest.raises(LoomcycleError) as ei:
        await _client(_Refusing()).plan_ontology([{"op": "retire", "entry": "c9"}])
    assert ei.value.reason == "entry_not_found"
