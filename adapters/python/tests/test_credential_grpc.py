"""RFC CN P4 — the RFC AR credential store over gRPC (Python parity).

Same substrate-dispatch shape as Path/Document/Memory: a JSON ``input`` dict
serialised to ``SubstrateRequest.input_json``, dispatched to the ``CredentialDef``
stub RPC, decoded from ``SubstrateResponse``. get/list return metadata only. A
tool refusal (is_error=True — e.g. an isolated user requesting scope=tenant, which
the server refuses per RFC CN) raises SubstrateToolRefusedError. The server-side
scope gate + isolated-user confinement are covered by
internal/api/grpc/substrate_test.go.
"""

from __future__ import annotations

import json
from typing import Any

import grpc.aio
import pytest

from loomcycle import LoomcycleClient, SubstrateToolRefusedError
from loomcycle._generated import loomcycle_pb2 as pb


def _make_client() -> LoomcycleClient:
    channel = grpc.aio.insecure_channel("127.0.0.1:1")
    return LoomcycleClient(channel=channel)


def _async_returning(result: Any):
    captured: dict = {}

    async def fn(req, metadata=None):
        captured["req"] = req
        captured["metadata"] = metadata
        return result

    return fn, captured


@pytest.mark.asyncio
async def test_credential_def_create_dispatches_to_the_rpc():
    client = _make_client()
    out_json = json.dumps({"name": "telegram", "scope": "user", "status": "stored"}).encode()
    fake, captured = _async_returning(
        pb.SubstrateResponse(output_json=out_json, is_error=False)
    )
    client._stub.CredentialDef = fake  # type: ignore[attr-defined]

    result = await client.credential_def(
        {"op": "create", "scope": "user", "name": "telegram", "value": "secret-123"}
    )
    assert result["status"] == "stored"
    # Input serialised to bytes on the CredentialDef RPC; the plaintext value is
    # sent (write-only) but never returned.
    assert isinstance(captured["req"].input_json, bytes)
    assert json.loads(captured["req"].input_json) == {
        "op": "create",
        "scope": "user",
        "name": "telegram",
        "value": "secret-123",
    }


@pytest.mark.asyncio
async def test_credential_def_refusal_raises_with_tool():
    client = _make_client()
    msg = b'"an isolated user token may only manage scope=user credentials (its own)"'
    fake, _ = _async_returning(pb.SubstrateResponse(output_json=msg, is_error=True))
    client._stub.CredentialDef = fake  # type: ignore[attr-defined]

    with pytest.raises(SubstrateToolRefusedError) as ei:
        await client.credential_def({"op": "create", "scope": "tenant", "name": "x", "value": "y"})
    assert ei.value.tool == "CredentialDef"


# --- OAuth credentials: typed twins over the same CredentialDef RPC. The
# server's answers carry the user code, verification URI, expiry, interval,
# state and connected_as; never a token.


@pytest.mark.asyncio
async def test_create_oauth_credential_sends_usage_oauth_and_the_oauth_object():
    client = _make_client()
    out = {
        "name": "docs-server",
        "scope": "user",
        "usage": "oauth",
        "oauth": {"resource": "https://docs.example.com/mcp", "registration": "dynamic", "state": "pending_connect"},
        "status": "stored",
    }
    fake, captured = _async_returning(pb.SubstrateResponse(output_json=json.dumps(out).encode()))
    client._stub.CredentialDef = fake  # type: ignore[attr-defined]

    result = await client.create_oauth_credential(
        scope="user",
        name="docs-server",
        resource="https://docs.example.com/mcp",
        scopes=["openid", "offline_access"],
        token_endpoint_auth="none",
    )
    assert result["oauth"]["state"] == "pending_connect"
    assert json.loads(captured["req"].input_json) == {
        "op": "create",
        "usage": "oauth",
        "scope": "user",
        "name": "docs-server",
        "oauth": {
            "resource": "https://docs.example.com/mcp",
            "scopes": ["openid", "offline_access"],
            "client": {"token_endpoint_auth": "none"},
        },
    }


@pytest.mark.asyncio
async def test_create_oauth_credential_leaves_unset_options_out():
    client = _make_client()
    fake, captured = _async_returning(pb.SubstrateResponse(output_json=b'{"name":"d","scope":"tenant"}'))
    client._stub.CredentialDef = fake  # type: ignore[attr-defined]

    await client.create_oauth_credential(
        scope="tenant",
        name="d",
        resource="https://docs.example.com/mcp",
        issuer="https://auth.example.com",
        allowed_urls=["https://docs.example.com/mcp"],
    )
    assert json.loads(captured["req"].input_json) == {
        "op": "create",
        "usage": "oauth",
        "scope": "tenant",
        "name": "d",
        "oauth": {"resource": "https://docs.example.com/mcp", "issuer": "https://auth.example.com"},
        "allowed_urls": ["https://docs.example.com/mcp"],
    }


@pytest.mark.asyncio
async def test_oauth_connect_start_returns_the_code_to_approve():
    client = _make_client()
    out = {
        "name": "docs-server",
        "scope": "user",
        "status": "pending_connect",
        "user_code": "ABCD-EFGH",
        "verification_uri": "https://auth.example.com/device",
        "expires_in": 299,
        "interval": 5,
    }
    fake, captured = _async_returning(pb.SubstrateResponse(output_json=json.dumps(out).encode()))
    client._stub.CredentialDef = fake  # type: ignore[attr-defined]

    start = await client.oauth_connect_start(scope="user", name="docs-server")
    assert start["user_code"] == "ABCD-EFGH"
    assert start["verification_uri"] == "https://auth.example.com/device"
    assert start["interval"] == 5
    assert json.loads(captured["req"].input_json) == {
        "op": "oauth_connect_start",
        "scope": "user",
        "name": "docs-server",
    }


@pytest.mark.asyncio
async def test_oauth_connect_status_returns_the_connection_state():
    client = _make_client()
    out = {
        "name": "docs-server",
        "scope": "tenant",
        "usage": "oauth",
        "oauth": {"state": "connected", "connected_as": "owner@example.com"},
    }
    fake, captured = _async_returning(pb.SubstrateResponse(output_json=json.dumps(out).encode()))
    client._stub.CredentialDef = fake  # type: ignore[attr-defined]

    meta = await client.oauth_connect_status(scope="tenant", name="docs-server")
    assert meta["oauth"]["state"] == "connected"
    assert meta["oauth"]["connected_as"] == "owner@example.com"
    assert json.loads(captured["req"].input_json) == {
        "op": "oauth_connect_status",
        "scope": "tenant",
        "name": "docs-server",
    }


@pytest.mark.asyncio
async def test_oauth_disconnect_returns_the_revoked_state():
    client = _make_client()
    out = {"name": "docs-server", "scope": "user", "usage": "oauth", "oauth": {"state": "revoked"}}
    fake, captured = _async_returning(pb.SubstrateResponse(output_json=json.dumps(out).encode()))
    client._stub.CredentialDef = fake  # type: ignore[attr-defined]

    meta = await client.oauth_disconnect(scope="user", name="docs-server")
    assert meta["oauth"]["state"] == "revoked"
    assert json.loads(captured["req"].input_json) == {
        "op": "oauth_disconnect",
        "scope": "user",
        "name": "docs-server",
    }


@pytest.mark.asyncio
async def test_oauth_connect_start_refusal_raises_with_tool():
    client = _make_client()
    msg = b'"oauth_connect_start: a tenant OAuth credential is managed by a tenant operator"'
    fake, _ = _async_returning(pb.SubstrateResponse(output_json=msg, is_error=True))
    client._stub.CredentialDef = fake  # type: ignore[attr-defined]

    with pytest.raises(SubstrateToolRefusedError) as ei:
        await client.oauth_connect_start(scope="tenant", name="docs-server")
    assert ei.value.tool == "CredentialDef"
