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


@pytest.mark.asyncio
async def test_memory_when_predicate_serialises_verbatim():
    """The observed-time predicate reaches the tool unchanged.

    There is no adapter-side schema for ``when`` and there should not be: the
    Memory RPC carries an opaque ``input_json`` to the same builtin dispatch HTTP
    and MCP use, so a typed mirror here would be a fourth hand-maintained copy of
    a predicate that decides which rows get DROPPED.

    What that design owes is proof the dict is not reshaped on the way out —
    "it is a pass-through" is an assumption until something asserts it.
    """
    client = _make_client()
    sent = {
        "op": "recall",
        "scope": "user",
        "query": "which city",
        "when": {
            "from": "2023-10-01T00:00:00Z",
            "to": "2023-10-04T00:00:00Z",
            "slack": "3d",
            "missing": "prefer",
        },
    }
    reply = {
        "memories": [],
        "time_filter": {
            "mode": "prefer",
            "slack_seconds": 259200,
            "in_window": 0,
            "out_of_window": 0,
            "untimed": 3,
        },
    }
    fn, captured = _async_returning(
        pb.SubstrateResponse(output_json=json.dumps(reply).encode(), is_error=False)
    )
    client._stub.Memory = fn  # type: ignore[attr-defined]

    got = await client.memory(sent)

    assert json.loads(captured["req"].input_json) == sent, (
        "the `when` predicate must reach the tool exactly as given — a reshaped "
        "window silently changes which rows are dropped"
    )
    # And the report survives the return trip, which is the half a caller acts on:
    # in_window 0 with untimed 3 is an UNDATED corpus, not an absent answer.
    assert got["time_filter"]["mode"] == "prefer"
    assert got["time_filter"]["untimed"] == 3


@pytest.mark.asyncio
async def test_memory_observed_at_on_set_serialises_verbatim():
    """Dating a row travels the same way."""
    client = _make_client()
    sent = {
        "op": "set",
        "scope": "user",
        "key": "turn-1",
        "value": "we spoke",
        "observed_at": "2023-10-04T14:00:00Z",
    }
    fn, captured = _async_returning(
        pb.SubstrateResponse(output_json=b'{"ok":true}', is_error=False)
    )
    client._stub.Memory = fn  # type: ignore[attr-defined]

    await client.memory(sent)

    assert json.loads(captured["req"].input_json) == sent
