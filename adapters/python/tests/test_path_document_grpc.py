"""v1.4.0 — the RFC AL Path VFS + RFC AK Document tools over gRPC.

Both share the substrate dispatch shape: a JSON ``input`` dict serialised to
``SubstrateRequest.input_json``, dispatched to the matching stub RPC (the stub
attribute name equals the proto RPC name — ``Path`` / ``Document``), decoded
from ``SubstrateResponse``. A tool refusal (is_error=True) raises
SubstrateToolRefusedError carrying the tool name. The server-side path is
covered by internal/api/grpc/substrate_test.go.
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
async def test_path_happy_path():
    client = _make_client()
    out_json = json.dumps({"path": "/", "entries": []}).encode()
    fake, captured = _async_returning(
        pb.SubstrateResponse(output_json=out_json, is_error=False)
    )
    client._stub.Path = fake  # type: ignore[attr-defined]

    result = await client.path({"op": "ls", "scope": "user", "path": "/"})
    assert result["entries"] == []
    # Input was JSON-serialised to bytes and dispatched to the Path RPC.
    assert isinstance(captured["req"].input_json, bytes)
    assert json.loads(captured["req"].input_json) == {
        "op": "ls",
        "scope": "user",
        "path": "/",
    }


@pytest.mark.asyncio
async def test_path_refusal_raises_with_tool():
    client = _make_client()
    fake, _ = _async_returning(
        pb.SubstrateResponse(output_json=b"rm: path has descendants", is_error=True)
    )
    client._stub.Path = fake  # type: ignore[attr-defined]

    with pytest.raises(SubstrateToolRefusedError) as exc:
        await client.path({"op": "rm", "path": "/docs"})
    assert exc.value.tool == "Path"


@pytest.mark.asyncio
async def test_document_happy_path():
    client = _make_client()
    out_json = json.dumps({"document_id": "d1", "root_chunk_id": "c0"}).encode()
    fake, captured = _async_returning(
        pb.SubstrateResponse(output_json=out_json, is_error=False)
    )
    client._stub.Document = fake  # type: ignore[attr-defined]

    result = await client.document(
        {"op": "create_document", "scope": "user", "title": "Launch"}
    )
    assert result["document_id"] == "d1"
    assert result["root_chunk_id"] == "c0"
    assert json.loads(captured["req"].input_json)["op"] == "create_document"


@pytest.mark.asyncio
async def test_document_refusal_raises_with_tool():
    client = _make_client()
    fake, _ = _async_returning(
        pb.SubstrateResponse(
            output_json=b"document: SQL Memory not enabled", is_error=True
        )
    )
    client._stub.Document = fake  # type: ignore[attr-defined]

    with pytest.raises(SubstrateToolRefusedError) as exc:
        await client.document({"op": "create_document", "title": "x"})
    assert exc.value.tool == "Document"
