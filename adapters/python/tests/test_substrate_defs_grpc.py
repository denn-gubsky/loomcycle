"""v0.8.0 — the 7 substrate-def methods added for gRPC parity
(mcp_server_def / schedule_def / a2a_server_card_def / a2a_agent_def /
webhook_def / memory_backend_def / operator_token_def).

All seven share the AgentDef/SkillDef shape: a JSON ``input`` dict
serialised to ``SubstrateRequest.input_json``, dispatched to the
matching stub RPC (the stub attribute name equals the proto RPC name),
decoded from ``SubstrateResponse``. A tool refusal (is_error=True)
raises SubstrateToolRefusedError carrying the tool name. The server-side
path is covered by internal/api/grpc/substrate_test.go.
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


# (python method name, stub RPC attribute) for the 7 new substrate defs.
_CASES = [
    ("mcp_server_def", "MCPServerDef"),
    ("schedule_def", "ScheduleDef"),
    ("a2a_server_card_def", "A2AServerCardDef"),
    ("a2a_agent_def", "A2AAgentDef"),
    ("webhook_def", "WebhookDef"),
    ("memory_backend_def", "MemoryBackendDef"),
    ("operator_token_def", "OperatorTokenDef"),
]


@pytest.mark.parametrize("method,rpc", _CASES)
@pytest.mark.asyncio
async def test_substrate_def_happy_path(method: str, rpc: str):
    client = _make_client()
    out_json = json.dumps({"def_id": "def_1", "name": "x", "version": 1}).encode()
    fake, captured = _async_returning(
        pb.SubstrateResponse(output_json=out_json, is_error=False)
    )
    setattr(client._stub, rpc, fake)  # type: ignore[arg-type]

    result = await getattr(client, method)({"op": "create", "name": "x"})
    assert result["def_id"] == "def_1"
    # Input was JSON-serialised to bytes and dispatched to the right RPC.
    assert isinstance(captured["req"].input_json, bytes)
    assert json.loads(captured["req"].input_json) == {"op": "create", "name": "x"}


@pytest.mark.parametrize("method,rpc", _CASES)
@pytest.mark.asyncio
async def test_substrate_def_refusal_raises_with_tool(method: str, rpc: str):
    client = _make_client()
    fake, _ = _async_returning(
        pb.SubstrateResponse(output_json=b"scope deny", is_error=True)
    )
    setattr(client._stub, rpc, fake)  # type: ignore[arg-type]

    with pytest.raises(SubstrateToolRefusedError) as exc:
        await getattr(client, method)({"op": "create", "name": "blocked"})
    assert exc.value.tool == rpc
