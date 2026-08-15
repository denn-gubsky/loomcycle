"""RFC CD Part D — the Memory tool over gRPC (Python/gRPC memory parity).

Same substrate-dispatch shape as Path/Document: a JSON ``input`` dict serialised
to ``SubstrateRequest.input_json``, dispatched to the ``Memory`` stub RPC,
decoded from ``SubstrateResponse``. A tool refusal (is_error=True) raises
SubstrateToolRefusedError. The server-side path is covered by
internal/api/grpc/substrate_test.go (TestGrpcMemory_HappyPath).
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
async def test_memory_set_get_roundtrip():
    client = _make_client()
    out_json = json.dumps({"scope": "user", "key": "tone", "value": "concise"}).encode()
    fake, captured = _async_returning(
        pb.SubstrateResponse(output_json=out_json, is_error=False)
    )
    client._stub.Memory = fake  # type: ignore[attr-defined]

    result = await client.memory({"op": "get", "scope": "user", "key": "tone"})
    assert result["value"] == "concise"
    # Input was JSON-serialised to bytes and dispatched to the Memory RPC.
    assert isinstance(captured["req"].input_json, bytes)
    assert json.loads(captured["req"].input_json) == {
        "op": "get",
        "scope": "user",
        "key": "tone",
    }


@pytest.mark.asyncio
async def test_memory_search_refusal_raises_with_tool():
    client = _make_client()
    fake, _ = _async_returning(
        pb.SubstrateResponse(
            output_json=b"memory: vector search unsupported (no embedder)",
            is_error=True,
        )
    )
    client._stub.Memory = fake  # type: ignore[attr-defined]

    with pytest.raises(SubstrateToolRefusedError) as exc:
        await client.memory({"op": "search", "scope": "user", "query": "x"})
    assert exc.value.tool == "Memory"
