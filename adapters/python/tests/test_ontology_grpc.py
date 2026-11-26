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
                    proposal="p1",
                )
            ],
            issues=["i"],
            scopes_counted=2,
            complete=True,
            content_version="0123456789abcdef",
        )


@pytest.mark.asyncio
async def test_plan_ontology():
    stub = _PlanStub()
    got = await _client(stub).plan_ontology(
        [
            {"op": "delete", "entry": "c1"},
            {"op": "rename", "entry": "c3", "name": "refers_to"},
            {"op": "proposal", "id": "p1"},
        ],
        tenant="globex",
    )
    assert stub.req.tenant == "globex"
    assert [(c.op, c.entry, c.name, c.id) for c in stub.req.changes] == [
        ("delete", "c1", "", ""),
        ("rename", "c3", "refers_to", ""),
        ("proposal", "", "", "p1"),
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
                "proposal": "p1",
                "job": None,
                "release": 0,
            }
        ],
        "issues": ["i"],
        "new_issues": [],
        "scopes_counted": 2,
        "scopes_skipped": 0,
        "complete": True,
        "content_version": "0123456789abcdef",
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


class _ReleasesStub:
    def __init__(self):
        self.req = None

    async def ListOntologyReleases(self, req, metadata=None):
        self.req = req
        return pb.ListOntologyReleasesResponse(
            tenant="acme",
            releases=[
                pb.OntologyRelease(
                    seq=2, content_version="0123456789abcdef", fingerprint="f" * 64,
                    reason="confirm", actor="u-op", created_at=1764000000000000000, entries=12,
                ),
                pb.OntologyRelease(seq=1, content_version="0", fingerprint="e", reason="catch_up", entries=10),
            ],
        )

    async def DiffOntologyReleases(self, req, metadata=None):
        self.req = req
        return pb.DiffOntologyReleasesResponse(
            tenant="acme",
            to=pb.OntologyDiffSide(current=True, content_version="fedcba9876543210"),
            changes=[
                pb.OntologyEntryDiff(
                    chunk_id="c1", kind="class", change="changed", changed=["renamed", "directives"],
                    before=pb.OntologyEntryState(name="event", directives_json=b'{"identity":["title"]}'),
                    after=pb.OntologyEntryState(
                        name="incident", parent="c0", parent_name="thing", status="", meaning="m",
                        directives_json=b'{"identity":["title","date"]}',
                    ),
                ),
                pb.OntologyEntryDiff(
                    chunk_id="c2", kind="relation", change="added",
                    after=pb.OntologyEntryState(name="caused_by", directives_json=b"{}"),
                ),
            ],
            issues=["i"],
            **{"from": pb.OntologyDiffSide(seq=2, content_version="0123456789abcdef")},
        )


@pytest.mark.asyncio
async def test_list_ontology_releases_returns_every_header_field():
    stub = _ReleasesStub()
    got = await _client(stub).list_ontology_releases(tenant="globex")
    assert stub.req.tenant == "globex"
    assert got == {
        "tenant": "acme",
        "releases": [
            {
                "seq": 2, "content_version": "0123456789abcdef", "fingerprint": "f" * 64,
                "reason": "confirm", "actor": "u-op", "created_at": 1764000000000000000, "entries": 12,
            },
            {
                "seq": 1, "content_version": "0", "fingerprint": "e",
                "reason": "catch_up", "actor": "", "created_at": 0, "entries": 10,
            },
        ],
    }


@pytest.mark.asyncio
async def test_diff_ontology_releases_sends_from_and_to_and_keeps_an_absent_side_none():
    stub = _ReleasesStub()
    got = await _client(stub).diff_ontology_releases(2, tenant="globex")
    assert (getattr(stub.req, "from"), stub.req.to, stub.req.tenant) == (2, 0, "globex")
    assert got == {
        "tenant": "acme",
        "from": {"seq": 2, "current": False, "content_version": "0123456789abcdef"},
        "to": {"seq": 0, "current": True, "content_version": "fedcba9876543210"},
        "changes": [
            {
                "chunk_id": "c1", "kind": "class", "change": "changed", "changed": ["renamed", "directives"],
                "before": {
                    "name": "event", "parent": "", "parent_name": "", "status": "", "meaning": "",
                    "directives": {"identity": ["title"]},
                },
                "after": {
                    "name": "incident", "parent": "c0", "parent_name": "thing", "status": "", "meaning": "m",
                    "directives": {"identity": ["title", "date"]},
                },
            },
            {
                "chunk_id": "c2", "kind": "relation", "change": "added", "changed": [],
                "before": None,
                "after": {
                    "name": "caused_by", "parent": "", "parent_name": "", "status": "", "meaning": "",
                    "directives": {},
                },
            },
        ],
        "issues": ["i"],
    }


@pytest.mark.asyncio
async def test_diff_ontology_releases_sends_a_release_number_as_to():
    stub = _ReleasesStub()
    await _client(stub).diff_ontology_releases(1, 3)
    assert (getattr(stub.req, "from"), stub.req.to, stub.req.tenant) == (1, 3, "")


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("method", "rpc", "code", "status", "reason"),
    [
        ("list", "ListOntologyReleases", grpc.StatusCode.PERMISSION_DENIED, 7, "forbidden"),
        ("diff", "DiffOntologyReleases", grpc.StatusCode.NOT_FOUND, 5, "release_not_found"),
        ("diff", "DiffOntologyReleases", grpc.StatusCode.INVALID_ARGUMENT, 3, "invalid_request"),
        ("diff", "DiffOntologyReleases", grpc.StatusCode.UNAVAILABLE, 14, "unavailable"),
    ],
)
async def test_ontology_release_refusal_carries_the_code_as_reason(method, rpc, code, status, reason):
    trailing = grpc.aio.Metadata()
    trailing.add("grpc-status-details-bin", _status_details(status, "refused", reason))
    err = grpc.aio.AioRpcError(
        code=code,
        initial_metadata=grpc.aio.Metadata(),
        trailing_metadata=trailing,
        details="refused",
        debug_error_string="",
    )

    async def _raise(req, metadata=None):
        raise err

    stub = type("_Refusing", (), {})()
    setattr(stub, rpc, _raise)
    client = _client(stub)
    call = client.list_ontology_releases() if method == "list" else client.diff_ontology_releases(9)
    with pytest.raises(LoomcycleError) as ei:
        await call
    assert ei.value.reason == reason


def _pb_job(**kw):
    return pb.OntologyJobStatus(
        id="j1", kind="concept_retag", entry="c1", to="topics/reporting/risk",
        to_tags=["topics/reporting/risk"], state="running", step="document_tags", done={"chunk_tags": 4},
        error="", remaining=7, scope="user", scopes_done=1, scopes_total=3,
        created_at=1764000000123456789, updated_at=1764000000123456790,
        **{"from": ["topics/risk"]}, **kw,
    )


_JOB = {
    "id": "j1", "kind": "concept_retag", "entry": "c1", "from": ["topics/risk"], "to": "topics/reporting/risk",
    "to_tags": ["topics/reporting/risk"], "state": "running", "step": "document_tags", "done": {"chunk_tags": 4},
    "error": "", "remaining": 7, "scope": "user", "scopes_done": 1, "scopes_total": 3,
    "created_at": 1764000000123456789, "updated_at": 1764000000123456790,
}


class _JobStub:
    def __init__(self, job=None):
        self.req = None
        self.job = job

    async def GetOntologyJob(self, req, metadata=None):
        self.req = req
        return pb.OntologyJobResponse(tenant="acme", job=self.job)

    async def ResumeOntologyJob(self, req, metadata=None):
        self.req = req
        return pb.OntologyJobResponse(tenant="acme", job=self.job)


@pytest.mark.asyncio
async def test_get_ontology_job_returns_every_field_of_the_job():
    stub = _JobStub(_pb_job())
    got = await _client(stub).get_ontology_job(tenant="globex")
    assert stub.req.tenant == "globex"
    assert got == {"tenant": "acme", "job": _JOB}


@pytest.mark.asyncio
async def test_get_ontology_job_reads_no_job_as_none():
    got = await _client(_JobStub()).get_ontology_job()
    assert got == {"tenant": "acme", "job": None}


@pytest.mark.asyncio
async def test_resume_ontology_job_sends_the_id_and_returns_the_job():
    stub = _JobStub(_pb_job())
    got = await _client(stub).resume_ontology_job(id="j1", tenant="globex")
    assert (stub.req.id, stub.req.tenant) == ("j1", "globex")
    assert got["job"] == _JOB


@pytest.mark.asyncio
async def test_plan_ontology_carries_a_class_renames_job_block():
    class _RenamePlanStub:
        async def PlanOntology(self, req, metadata=None):
            return pb.PlanOntologyResponse(
                tenant="acme",
                changes=[
                    pb.OntologyPlannedChange(
                        op="rename",
                        entry=pb.OntologyPlanEntry(chunk_id="c1", name="Bulletin", section="classes", kind="class"),
                        risk="stales",
                        job=pb.OntologyRenameJobPlan(
                            steps=[pb.OntologyRenameJobStep(name="documents_type", tenant_scope=2, other_scopes=5)],
                            blocked=["the plan could not count every scope"],
                            collisions=[pb.OntologyRenameCollision(column="identity_key", key="bulletin:a", into="circular:a", holder="c9")],
                            collisions_total=3, collisions_other_scopes=2, scopes_not_counted=1,
                            running=_pb_job(), notes=["n"], kind="class_rename",
                        ),
                    )
                ],
            )

    got = await _client(_RenamePlanStub()).plan_ontology([{"op": "rename", "entry": "c1", "name": "Circular"}])
    assert got["changes"][0]["job"] == {
        "steps": [{"name": "documents_type", "tenant_scope": 2, "other_scopes": 5}],
        "runnable": False,
        "blocked": ["the plan could not count every scope"],
        "collisions": [{"column": "identity_key", "key": "bulletin:a", "into": "circular:a", "holder": "c9"}],
        "collisions_total": 3,
        "collisions_other_scopes": 2,
        "scopes_not_counted": 1,
        "running": _JOB,
        "notes": ["n"],
        "kind": "class_rename",
        "tag_moves": [],
    }


@pytest.mark.asyncio
async def test_plan_ontology_carries_a_concept_retag_job_block():
    class _RetagPlanStub:
        async def PlanOntology(self, req, metadata=None):
            return pb.PlanOntologyResponse(
                tenant="acme",
                changes=[
                    pb.OntologyPlannedChange(
                        op="move",
                        entry=pb.OntologyPlanEntry(chunk_id="c1", name="Risk", section="schemes", kind="concept"),
                        risk="stales",
                        job=pb.OntologyRenameJobPlan(
                            kind="concept_retag",
                            tag_moves=[pb.OntologyTagMove(**{"from": "topics/risk", "to": "topics/reporting/risk"})],
                            steps=[pb.OntologyRenameJobStep(name="document_tags", tenant_scope=1, other_scopes=2)],
                            runnable=True,
                        ),
                    )
                ],
            )

    got = await _client(_RetagPlanStub()).plan_ontology([{"op": "move", "entry": "c1", "parent": "c2"}])
    job = got["changes"][0]["job"]
    assert job["kind"] == "concept_retag"
    assert job["tag_moves"] == [{"from": "topics/risk", "to": "topics/reporting/risk"}]
    assert job["steps"] == [{"name": "document_tags", "tenant_scope": 1, "other_scopes": 2}]
    assert job["running"] is None


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("rpc", "code", "status", "reason"),
    [
        ("GetOntologyJob", grpc.StatusCode.PERMISSION_DENIED, 7, "forbidden"),
        ("ResumeOntologyJob", grpc.StatusCode.NOT_FOUND, 5, "job_not_found"),
        ("ResumeOntologyJob", grpc.StatusCode.ABORTED, 10, "job_busy"),
        ("ResumeOntologyJob", grpc.StatusCode.UNAVAILABLE, 14, "unavailable"),
    ],
)
async def test_ontology_job_refusal_carries_the_code_as_reason(rpc, code, status, reason):
    trailing = grpc.aio.Metadata()
    trailing.add("grpc-status-details-bin", _status_details(status, "refused", reason))
    err = grpc.aio.AioRpcError(
        code=code,
        initial_metadata=grpc.aio.Metadata(),
        trailing_metadata=trailing,
        details="refused",
        debug_error_string="",
    )

    async def _raise(req, metadata=None):
        raise err

    stub = type("_Refusing", (), {})()
    setattr(stub, rpc, _raise)
    client = _client(stub)
    call = client.get_ontology_job() if rpc == "GetOntologyJob" else client.resume_ontology_job()
    with pytest.raises(LoomcycleError) as ei:
        await call
    assert ei.value.reason == reason


class _RollbackStub:
    def __init__(self):
        self.req = None
        self.plan_req = None

    async def PlanOntology(self, req, metadata=None):
        self.plan_req = req
        return pb.PlanOntologyResponse(
            tenant="acme",
            changes=[
                pb.OntologyPlannedChange(
                    op="unretire",
                    entry=pb.OntologyPlanEntry(chunk_id="c1", name="Memo", section="classes", kind="class"),
                    risk="none",
                    release=2,
                )
            ],
        )

    async def ApplyOntologyRollback(self, req, metadata=None):
        self.req = req
        return pb.ApplyOntologyRollbackResponse(
            tenant="acme",
            rollback=pb.OntologyRollbackReport(
                release=2, release_content_version="aaaa", content_version="aaaa", matches=True, risk="none",
                applied=[pb.OntologyRollbackStep(op="add", entry="c2", name="Ledger", new_id="c9")],
                rename_jobs=[_pb_job()], rollback_release=3, notes=["n"], warnings=["w"],
            ),
        )


@pytest.mark.asyncio
async def test_plan_ontology_sends_a_release_change_and_reads_its_release():
    stub = _RollbackStub()
    got = await _client(stub).plan_ontology([{"op": "release", "seq": 2}, {"op": "add", "seq": 2, "entry": "c5"}])
    assert [(c.op, c.seq, c.entry) for c in stub.plan_req.changes] == [("release", 2, ""), ("add", 2, "c5")]
    assert got["changes"][0]["release"] == 2


@pytest.mark.asyncio
async def test_rollback_ontology_sends_seq_and_fingerprint_and_returns_the_report():
    stub = _RollbackStub()
    got = await _client(stub).rollback_ontology(2, "f" * 64, tenant="globex")
    assert (stub.req.seq, stub.req.plan_fingerprint, stub.req.tenant) == (2, "f" * 64, "globex")
    assert got == {
        "tenant": "acme",
        "rollback": {
            "release": 2, "release_content_version": "aaaa", "content_version": "aaaa", "matches": True,
            "risk": "none", "applied": [{"op": "add", "entry": "c2", "name": "Ledger", "new_id": "c9"}],
            "rename_jobs": [_JOB], "rollback_release": 3, "notes": ["n"], "warnings": ["w"],
        },
    }


def _status_with_metadata(code: int, message: str, reason: str, meta: dict) -> bytes:
    """A serialized google.rpc.Status carrying ErrorInfo{reason, domain,
    metadata}."""

    def varint(n: int) -> bytes:
        out = b""
        while True:
            b, n = n & 0x7F, n >> 7
            if n:
                out += bytes([b | 0x80])
            else:
                return out + bytes([b])

    def field(num: int, payload: bytes) -> bytes:
        return bytes([num << 3 | 2]) + varint(len(payload)) + payload

    info = field(1, reason.encode()) + field(2, b"loomcycle")
    for k, v in meta.items():
        info += field(3, field(1, k.encode()) + field(2, v.encode()))
    any_ = field(1, b"type.googleapis.com/google.rpc.ErrorInfo") + field(2, info)
    return bytes([0x08, code]) + field(2, message.encode()) + field(3, any_)


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("code", "status", "reason", "meta", "fingerprint", "applied"),
    [
        (grpc.StatusCode.FAILED_PRECONDITION, 9, "plan_stale", {"fingerprint": "e" * 64}, "e" * 64, []),
        (grpc.StatusCode.FAILED_PRECONDITION, 9, "plan_required", {"fingerprint": "e" * 64}, "e" * 64, []),
        (
            grpc.StatusCode.ABORTED, 10, "partially_applied",
            {"applied": '[{"op":"retire","entry":"c1","name":"Memo"}]'}, "",
            [{"op": "retire", "entry": "c1", "name": "Memo"}],
        ),
        (grpc.StatusCode.FAILED_PRECONDITION, 9, "rename_job_pending", {}, "", []),
    ],
)
async def test_rollback_ontology_refusal_carries_the_code_and_its_metadata(code, status, reason, meta, fingerprint, applied):
    trailing = grpc.aio.Metadata()
    trailing.add("grpc-status-details-bin", _status_with_metadata(status, "refused", reason, meta))
    err = grpc.aio.AioRpcError(
        code=code,
        initial_metadata=grpc.aio.Metadata(),
        trailing_metadata=trailing,
        details="refused",
        debug_error_string="",
    )

    class _Refusing:
        async def ApplyOntologyRollback(self, req, metadata=None):
            raise err

    with pytest.raises(LoomcycleError) as ei:
        await _client(_Refusing()).rollback_ontology(2, "f" * 64)
    assert ei.value.reason == reason
    assert ei.value.fingerprint == fingerprint
    assert ei.value.applied == applied
