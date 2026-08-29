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
