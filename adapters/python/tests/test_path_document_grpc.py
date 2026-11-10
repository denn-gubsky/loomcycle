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


@pytest.mark.asyncio
async def test_copy_documents_sends_only_the_arguments_given():
    client = _make_client()
    fake, captured = _async_returning(
        pb.SubstrateResponse(output_json=json.dumps({"dry_run": True, "documents": []}).encode(), is_error=False)
    )
    client._stub.Document = fake  # type: ignore[attr-defined]

    result = await client.copy_documents(to_scope="tenant", under_path="/proj", path_map={"/proj": "/kb/proj"})
    assert result["dry_run"] is True
    assert json.loads(captured["req"].input_json) == {
        "op": "copy_documents",
        "to_scope": "tenant",
        "dry_run": True,
        "under_path": "/proj",
        "path_map": {"/proj": "/kb/proj"},
    }


@pytest.mark.asyncio
async def test_move_documents_carries_expect_count_and_dangling():
    client = _make_client()
    fake, captured = _async_returning(
        pb.SubstrateResponse(output_json=json.dumps({"dry_run": False, "moved": ["d1"]}).encode(), is_error=False)
    )
    client._stub.Document = fake  # type: ignore[attr-defined]

    result = await client.move_documents(
        to_scope="tenant", scope="user", document_ids=["d1"], dangling="drop", dry_run=False, expect_count=1
    )
    assert result["moved"] == ["d1"]
    assert json.loads(captured["req"].input_json) == {
        "op": "move_documents",
        "to_scope": "tenant",
        "scope": "user",
        "dry_run": False,
        "document_ids": ["d1"],
        "dangling": "drop",
        "expect_count": 1,
    }


@pytest.mark.asyncio
async def test_move_documents_refusal_raises():
    client = _make_client()
    fake, _ = _async_returning(
        pb.SubstrateResponse(
            output_json=b"move_documents: expect_count is required with dry_run false", is_error=True
        )
    )
    client._stub.Document = fake  # type: ignore[attr-defined]

    with pytest.raises(SubstrateToolRefusedError):
        await client.move_documents(to_scope="tenant", under_path="/proj", dry_run=False)



@pytest.mark.asyncio
async def test_find_duplicates_sends_the_set_basis_and_propose():
    client = _make_client()
    answer = {
        "pairs": [{"copy_id": "d2", "original_id": "d1", "basis": "title", "confidence": 0.7, "statement_id": "s1"}],
        "counts": {"documents": 2, "pairs": 1, "proposed": 1},
        "truncated": False,
    }
    fake, captured = _async_returning(pb.SubstrateResponse(output_json=json.dumps(answer).encode(), is_error=False))
    client._stub.Document = fake  # type: ignore[attr-defined]

    result = await client.find_duplicates(scope="user", under_path="/notes", basis=["title"], propose=True)
    assert result["pairs"][0]["statement_id"] == "s1"
    assert json.loads(captured["req"].input_json) == {
        "op": "find_duplicates",
        "propose": True,
        "scope": "user",
        "under_path": "/notes",
        "basis": ["title"],
    }


@pytest.mark.asyncio
async def test_find_duplicates_reports_by_default():
    client = _make_client()
    fake, captured = _async_returning(
        pb.SubstrateResponse(output_json=json.dumps({"pairs": [], "counts": {}, "truncated": False}).encode(), is_error=False)
    )
    client._stub.Document = fake  # type: ignore[attr-defined]

    await client.find_duplicates(document_ids=["d1", "d2"])
    assert json.loads(captured["req"].input_json) == {
        "op": "find_duplicates",
        "propose": False,
        "document_ids": ["d1", "d2"],
    }


@pytest.mark.asyncio
async def test_annotate_without_a_hash_is_manual():
    client = _make_client()
    fake, captured = _async_returning(
        pb.SubstrateResponse(output_json=json.dumps({"annotation": {"chunk_id": "c1"}}).encode(), is_error=False)
    )
    client._stub.Document = fake  # type: ignore[attr-defined]

    await client.annotate(brief="b", abstract="a", level="path", path="/kb")
    assert json.loads(captured["req"].input_json) == {
        "op": "set_annotation",
        "scope": "tenant",
        "brief": "b",
        "abstract": "a",
        "level": "path",
        "path": "/kb",
        "manual": True,
    }


@pytest.mark.asyncio
async def test_list_annotations_sends_only_the_filters_given():
    client = _make_client()
    fake, captured = _async_returning(
        pb.SubstrateResponse(output_json=json.dumps({"annotations": []}).encode(), is_error=False)
    )
    client._stub.Document = fake  # type: ignore[attr-defined]

    await client.list_annotations(level="document", stale=True)
    assert json.loads(captured["req"].input_json) == {
        "op": "list_annotations",
        "scope": "tenant",
        "level": "document",
        "stale": True,
    }
