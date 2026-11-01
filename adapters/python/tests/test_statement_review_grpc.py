"""Statement review over gRPC: list_review_queue, rule_statements,
revert_ruling and apply_review_policy send their arguments, return the result as a mapping, and a
refusal carries the code on ``.reason``."""

import datetime

import grpc
import pytest

from loomcycle import LoomcycleClient
from loomcycle._generated import loomcycle_pb2 as pb
from loomcycle.errors import LoomcycleError


def _field(number: int, payload: bytes) -> bytes:
    """One length-delimited protobuf field (payloads here are < 128 bytes)."""
    assert len(payload) < 128
    return bytes([number << 3 | 2, len(payload)]) + payload


def _status_details(code: int, message: str, reason: str) -> bytes:
    """A google.rpc.Status carrying ErrorInfo{reason, domain: loomcycle}, as
    the server sends it in grpc-status-details-bin."""
    info = _field(1, reason.encode()) + _field(2, b"loomcycle")
    any_ = _field(1, b"type.googleapis.com/google.rpc.ErrorInfo") + _field(2, info)
    return bytes([0x08, code]) + _field(2, message.encode()) + _field(3, any_)


class _Stub:
    def __init__(self):
        self.req = None

    async def ListReviewQueue(self, req, metadata=None):
        self.req = req
        return pb.ListReviewQueueResponse(
            tenant="acme",
            scope="tenant",
            statements_json=[b'{"id":"s1","kind":"cites"}'],
            next_cursor="c1",
            summary=[pb.ReviewQueueGroup(kind="cites", method="rule:citation", from_path_prefix="/board/reg", count=3,
                                         filter=pb.ReviewQueueFilter(kind_exact="cites", method_exact="rule:citation",
                                                                     from_path_prefix="/board/reg", has_judge_verdict=False))],
            total=3,
        )

    async def RuleStatements(self, req, metadata=None):
        self.req = req
        return pb.RuleStatementsResponse(
            tenant="acme", scope="tenant", action=req.action, dry_run=not req.HasField("dry_run") or req.dry_run,
            ruling_id="r1", matched=2, selected=2, ruled=2,
            sample_json=[b'{"id":"s1"}'],
            breakdown=[pb.RulingBreakdown(kind="cites", method="rule:citation", count=2)],
            items=[pb.RulingItem(id="s1", status="asserted")],
        )

    async def RevertRuling(self, req, metadata=None):
        self.req = req
        return pb.RevertRulingResponse(
            tenant="acme", scope="tenant", ruling_id=req.ruling_id, action="accept", reverted=1, changed=1,
            items=[pb.RulingItem(id="s1", status="proposed"), pb.RulingItem(id="s2", error="changed", message="m")],
            revert_id="v1",
        )

    async def ListRulings(self, req, metadata=None):
        self.req = req
        return pb.ListRulingsResponse(
            tenant="acme", scope="user", next_cursor="n1",
            rulings=[
                pb.RulingRecord(id="v1", action="revert", ruled_by="operator:ops", actor="ops",
                                selector=pb.RulingSelector(reverts="r1"), matched=2, ruled=2, created_at=9),
                pb.RulingRecord(id="r1", action="accept", ruled_by="operator:ops", actor="ops", reason="ok",
                                selector=pb.RulingSelector(has_filter=True, filter=pb.ReviewQueueFilter(kind_exact="cites"), max=50),
                                matched=3, ruled=2, failed=1, created_at=7, reverted=True, reverted_at=9, reverted_by="v1"),
                pb.RulingRecord(id="p1", action="policy", ruled_by="operator:ops",
                                selector=pb.RulingSelector(policy=pb.RulingPolicy(version=14, source="review")), matched=6, ruled=3),
            ],
        )


class _PolicyStub:
    def __init__(self):
        self.req = None

    async def ApplyReviewPolicy(self, req, metadata=None):
        self.req = req
        return pb.ApplyReviewPolicyResponse(
            tenant="acme", scope="tenant", dry_run=not req.HasField("dry_run") or req.dry_run, source="review", version=7,
            ruling_id="r2", matched=3, accept=2, judge=1, to_rule=2, ruled=2, marked=1,
            rules=[pb.PolicyRuleCount(rule=1, action="accept", text="accept kind:cites", count=2)],
            sample_json=[b'{"id":"s1","review_rule":1}'],
            items=[pb.RulingItem(id="s1", status="asserted")],
        )


def _client(stub) -> LoomcycleClient:
    client = LoomcycleClient(channel=grpc.aio.insecure_channel("127.0.0.1:1"))
    client._stub = stub  # type: ignore[assignment]
    return client


@pytest.mark.asyncio
async def test_list_review_queue_sends_the_filter_and_decodes_the_statements():
    stub = _Stub()
    after = datetime.datetime(2026, 10, 31, tzinfo=datetime.timezone.utc)
    got = await _client(stub).list_review_queue(
        filter={"method": "rule:", "kinds": ["cites"], "from_under_path": "/board", "min_confidence": 0.5,
                "created_after": after, "created_before": 1793481550963216000, "has_judge_verdict": False},
        summarize=True, limit=50, tenant="globex",
    )
    f = stub.req.filter
    assert (f.method, list(f.kinds), f.from_under_path, f.min_confidence) == ("rule:", ["cites"], "/board", 0.5)
    assert f.created_after == 1793404800000000000 and f.created_before == 1793481550963216000
    assert f.HasField("has_judge_verdict") and not f.has_judge_verdict
    assert (stub.req.summarize, stub.req.limit, stub.req.tenant) == (True, 50, "globex")
    assert got["statements"] == [{"id": "s1", "kind": "cites"}]
    assert got["summary"] == [{"kind": "cites", "method": "rule:citation", "from_path_prefix": "/board/reg", "judge_verdict": "", "count": 3,
                               "filter": {"kind_exact": "cites", "method_exact": "rule:citation", "from_path_prefix": "/board/reg",
                                          "has_judge_verdict": False}}]
    assert got["total"] == 3 and got["next_cursor"] == "c1"


@pytest.mark.asyncio
async def test_list_review_queue_refuses_an_unknown_filter_key():
    with pytest.raises(ValueError):
        await _client(_Stub()).list_review_queue(filter={"from_path": "/board"})


@pytest.mark.asyncio
async def test_rule_statements_is_a_dry_run_unless_told_otherwise():
    stub = _Stub()
    await _client(stub).rule_statements("accept", filter={})
    assert stub.req.has_filter and not stub.req.HasField("dry_run") and not stub.req.HasField("expect_count")

    got = await _client(stub).rule_statements("reject", ids=["s1", "s2"], reason="wrong act", dry_run=False, expect_count=2, max=10)
    assert (list(stub.req.ids), stub.req.has_filter, stub.req.reason, stub.req.max) == (["s1", "s2"], False, "wrong act", 10)
    assert stub.req.HasField("dry_run") and not stub.req.dry_run and stub.req.expect_count == 2
    assert got["ruling_id"] == "r1" and got["ruled"] == 2 and got["dry_run"] is False
    assert got["sample"] == [{"id": "s1"}]
    assert got["breakdown"] == [{"kind": "cites", "method": "rule:citation", "count": 2}]
    assert got["items"] == [{"id": "s1", "status": "asserted", "error": "", "message": ""}]


@pytest.mark.asyncio
async def test_rule_statements_needs_ids_or_a_filter():
    for kwargs in ({}, {"ids": ["s1"], "filter": {}}):
        with pytest.raises(ValueError):
            await _client(_Stub()).rule_statements("accept", **kwargs)


@pytest.mark.asyncio
async def test_revert_ruling_sends_the_ruling_and_returns_what_went_back():
    stub = _Stub()
    got = await _client(stub).revert_ruling("r1", tenant="globex")
    assert (stub.req.ruling_id, stub.req.tenant) == ("r1", "globex")
    assert (got["reverted"], got["changed"]) == (1, 1)
    assert got["items"][1] == {"id": "s2", "status": "", "error": "changed", "message": "m"}


@pytest.mark.asyncio
async def test_a_refusal_carries_the_review_code_as_reason():
    trailing = grpc.aio.Metadata()
    trailing.add("grpc-status-details-bin", _status_details(9, "the queue changed", "queue_changed"))
    err = grpc.aio.AioRpcError(
        code=grpc.StatusCode.FAILED_PRECONDITION,
        initial_metadata=grpc.aio.Metadata(),
        trailing_metadata=trailing,
        details="the selection now matches 3 statements, not the 2 expected",
        debug_error_string="",
    )

    class _Refusing:
        async def RuleStatements(self, req, metadata=None):
            raise err

    with pytest.raises(LoomcycleError) as ei:
        await _client(_Refusing()).rule_statements("accept", filter={}, dry_run=False, expect_count=2)
    assert ei.value.reason == "queue_changed"
    assert "3 statements" in str(ei.value)


@pytest.mark.asyncio
async def test_list_rulings_sends_the_page_and_names_ids_only_by_count():
    stub = _Stub()
    got = await _client(stub).list_rulings(limit=5, after="c", scope="user", scope_id="bob", tenant="globex")
    assert (stub.req.limit, stub.req.after, stub.req.scope, stub.req.scope_id, stub.req.tenant) == (5, "c", "user", "bob", "globex")
    assert got["next_cursor"] == "n1"
    revert, ruling, policy = got["rulings"]
    assert policy["action"] == "policy" and policy["selector"]["policy"] == {"version": 14, "source": "review", "run_id": ""}
    assert revert["action"] == "revert" and revert["selector"] == {"filter": None, "ids_count": 0, "max": 0, "reverts": "r1", "policy": None}
    assert ruling["selector"]["filter"] == {"kind_exact": "cites"} and ruling["selector"]["max"] == 50
    assert (ruling["reverted"], ruling["reverted_by"], ruling["failed"]) == (True, "v1", 1)


@pytest.mark.asyncio
async def test_scope_id_and_the_exact_selectors_reach_the_request():
    stub = _Stub()
    group = {"kind_exact": "cites", "method_exact": "rule:citation", "from_unfiled": True, "has_judge_verdict": False}
    await _client(stub).list_review_queue(filter=group, scope="user", scope_id="bob")
    f = stub.req.filter
    assert (stub.req.scope_id, f.kind_exact, f.method_exact, f.from_unfiled) == ("bob", "cites", "rule:citation", True)
    await _client(stub).rule_statements("accept", filter=group, scope="user", scope_id="bob")
    assert stub.req.scope_id == "bob" and stub.req.filter.from_unfiled
    got = await _client(stub).revert_ruling("r1", scope="user", scope_id="bob")
    assert stub.req.scope_id == "bob" and got["revert_id"] == "v1"


@pytest.mark.asyncio
async def test_apply_review_policy_is_a_dry_run_of_the_whole_queue_unless_told_otherwise():
    stub = _PolicyStub()
    got = await _client(stub).apply_review_policy()
    assert not stub.req.HasField("filter") and not stub.req.HasField("dry_run") and not stub.req.HasField("expect_count")
    assert got["dry_run"] is True and got["rules"] == [{"rule": 1, "action": "accept", "text": "accept kind:cites", "count": 2}]

    got = await _client(stub).apply_review_policy(filter={"kind": "cites"}, dry_run=False, expect_count=3, max=100, scope="user", scope_id="bob", tenant="globex")
    assert (stub.req.filter.kind, stub.req.max, stub.req.tenant, stub.req.scope_id) == ("cites", 100, "globex", "bob")
    assert stub.req.HasField("dry_run") and not stub.req.dry_run and stub.req.expect_count == 3
    assert (got["ruling_id"], got["ruled"], got["marked"], got["source"], got["version"]) == ("r2", 2, 1, "review", 7)
    assert got["sample"] == [{"id": "s1", "review_rule": 1}]
    assert got["items"] == [{"id": "s1", "status": "asserted", "error": "", "message": ""}]
