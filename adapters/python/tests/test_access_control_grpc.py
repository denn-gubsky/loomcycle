"""The access-control management RPCs (RFC EY / EZ): group_def / acl_def /
acl_binding / authz.

Each dispatches its JSON input to the matching stub RPC and decodes the
answer, like the substrate defs. What is particular to these four is the
refusal: its text is the HTTP route's JSON error body, which the client
decodes onto AclRefusedError (AclDeniedError for ``acl_denied``) rather than
surfacing raw JSON. The server-side path is covered by
internal/api/http/acl_twins_test.go and internal/api/grpc.
"""

from __future__ import annotations

import json
import re
from pathlib import Path
from typing import Any

import grpc
import grpc.aio
import pytest

from loomcycle import (
    AclDeniedError,
    AclRefusedError,
    LoomcycleClient,
    LoomcycleError,
    SubstrateToolRefusedError,
)
from loomcycle._generated import loomcycle_pb2 as pb


def _make_client() -> LoomcycleClient:
    return LoomcycleClient(channel=grpc.aio.insecure_channel("127.0.0.1:1"))


def _fake(result: Any):
    captured: dict = {}

    async def fn(req, metadata=None):
        captured["req"] = req
        return result

    return fn, captured


_CASES = [
    ("group_def", "GroupDef", {"op": "list"}),
    ("acl_def", "AclDef", {"op": "get", "name": "ops", "tenant": "t1"}),
    ("acl_binding", "AclBinding", {"op": "create", "acl": "ops", "subject_kind": "tenant"}),
    ("authz", "Authz", {"op": "evaluation", "action": {"name": "run"}, "resource": {"type": "agent", "id": "a"}}),
]


@pytest.mark.parametrize("method,rpc,inp", _CASES)
@pytest.mark.asyncio
async def test_acl_method_dispatches_input_to_its_rpc(method: str, rpc: str, inp: dict):
    client = _make_client()
    fake, captured = _fake(pb.SubstrateResponse(output_json=b'{"ok": true}', is_error=False))
    setattr(client._stub, rpc, fake)

    out = await getattr(client, method)(inp)
    assert out == {"ok": True}
    assert json.loads(captured["req"].input_json) == inp


def _refusal(body: Any, *, retryable: bool = False, category: str = "permission") -> pb.SubstrateResponse:
    text = body if isinstance(body, (bytes, str)) else json.dumps(body)
    if isinstance(text, str):
        text = text.encode()
    return pb.SubstrateResponse(
        output_json=text,
        is_error=True,
        error_info=pb.ErrorInfo(category=category, is_retryable=retryable, description="x"),
    )


@pytest.mark.asyncio
async def test_acl_denied_refusal_raises_acl_denied_error_with_its_rules():
    client = _make_client()
    body = {
        "code": "acl_denied",
        "error": "author def:acl/ops is denied by lockdown rule 0",
        "action": "author",
        "resource": "def:acl/ops",
        "reasons": [
            {"acl": "lockdown", "version": 3, "rule": 0, "effect": "deny"},
            {"acl": 7, "rule": "x", "effect": "deny"},  # malformed: dropped
        ],
    }
    fake, _ = _fake(_refusal(body))
    client._stub.AclDef = fake

    with pytest.raises(AclDeniedError) as exc:
        await client.acl_def({"op": "put", "name": "ops", "rules": []})
    e = exc.value
    # Still the classes a caller already catches.
    assert isinstance(e, AclRefusedError)
    assert isinstance(e, SubstrateToolRefusedError)
    assert e.tool == "AclDef"
    assert e.reason == "acl_denied"
    assert e.message == body["error"]
    assert e.action == "author"
    assert e.resource == "def:acl/ops"
    assert e.reasons == [{"acl": "lockdown", "version": 3, "rule": 0, "effect": "deny"}]
    assert e.error_info is not None and e.error_info.category == "permission"
    assert e.retryable is False


@pytest.mark.parametrize(
    "rpc,method,body",
    [
        ("GroupDef", "group_def", {"code": "insufficient_scope", "error": "groups … are the tenant operator's", "required_scope": "substrate:tenant"}),
        ("AclBinding", "acl_binding", {"code": "acl_self_elevation", "error": "refused: a binding to your own subject"}),
        ("GroupDef", "group_def", {"code": "group_external", "error": 'group "session-ops" is synced from an identity provider'}),
        ("Authz", "authz", {"code": "authz_subject", "error": "naming another subject needs a tenant operator"}),
        ("AclBinding", "acl_binding", {"code": "acl_binding_invalid", "error": "list \"x\" does not exist"}),
    ],
)
@pytest.mark.asyncio
async def test_other_refusals_raise_acl_refused_error_with_the_code_as_reason(rpc: str, method: str, body: dict):
    client = _make_client()
    fake, _ = _fake(_refusal(body))
    setattr(client._stub, rpc, fake)

    with pytest.raises(AclRefusedError) as exc:
        await getattr(client, method)({"op": "create"})
    e = exc.value
    assert not isinstance(e, AclDeniedError)
    assert e.reason == body["code"]
    assert e.message == body["error"]
    assert e.body == body
    assert e.required_scope == body.get("required_scope")


@pytest.mark.asyncio
async def test_retryable_follows_the_servers_classification():
    client = _make_client()
    fake, _ = _fake(_refusal({"code": "acl_unavailable", "error": "retry"}, retryable=True, category="transient"))
    client._stub.Authz = fake
    with pytest.raises(AclRefusedError) as exc:
        await client.authz({"op": "evaluation"})
    assert exc.value.retryable is True


@pytest.mark.asyncio
async def test_a_non_json_refusal_keeps_its_text():
    client = _make_client()
    fake, _ = _fake(_refusal(b"plain refusal"))
    client._stub.GroupDef = fake
    with pytest.raises(AclRefusedError) as exc:
        await client.group_def({"op": "list"})
    assert exc.value.message == "plain refusal"
    assert exc.value.reason is None
    assert exc.value.body == {}


class _RpcError(grpc.aio.AioRpcError):
    def __init__(self, code: grpc.StatusCode, details: str) -> None:
        super().__init__(code, grpc.aio.Metadata(), grpc.aio.Metadata(), details=details)


@pytest.mark.asyncio
async def test_the_interceptors_scope_refusal_surfaces_as_a_permission_denied_error():
    """The RPC's scope gate refuses before the operation runs, as a gRPC
    status rather than a tool result: a LoomcycleError carrying the code."""
    client = _make_client()

    async def deny(req, metadata=None):
        raise _RpcError(grpc.StatusCode.PERMISSION_DENIED, "insufficient scope: substrate:tenant required")

    client._stub.GroupDef = deny
    with pytest.raises(LoomcycleError) as exc:
        await client.group_def({"op": "list"})
    assert not isinstance(exc.value, AclRefusedError)
    assert exc.value.code == grpc.StatusCode.PERMISSION_DENIED


def test_every_acl_family_has_a_stub_rpc_and_a_client_method():
    """The four families named in the server's op table each reach the wire:
    a generated stub method and a client method dispatching to it."""
    repo = Path(__file__).resolve().parents[3]
    table = repo / "internal" / "connector" / "aclmgmt.go"
    if not table.exists():
        pytest.skip("standalone checkout: no Go tree to compare against")
    families = re.findall(r'ACLFamily\w+\s*=\s*"(\w+)"', table.read_text())
    from loomcycle._generated import loomcycle_pb2_grpc as pbg

    rpcs = {"groupdef": "GroupDef", "acldef": "AclDef", "aclbinding": "AclBinding", "authz": "Authz"}
    methods = {"groupdef": "group_def", "acldef": "acl_def", "aclbinding": "acl_binding", "authz": "authz"}
    # A family added server-side with no client method fails here.
    assert sorted(families) == sorted(rpcs)
    for fam in families:
        assert hasattr(pbg.LoomcycleServicer, rpcs[fam]), rpcs[fam]
        assert callable(getattr(LoomcycleClient, methods[fam]))
