"""v0.8.22 — LoomcycleClient substrate admin tests (agent_def +
skill_def).

Mirror of the pause/snapshot client test pattern: monkey-patch the
gRPC stub methods to assert request shape + response translation
without dialing a real server. The server-side wire path is
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
    """Build a client without dialing — the channel is unused
    because we replace stub methods directly."""
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
async def test_agent_def_happy_path():
    client = _make_client()
    out_json = json.dumps({"def_id": "def_abc", "name": "reviewer", "version": 1}).encode()
    fake, captured = _async_returning(
        pb.SubstrateResponse(output_json=out_json, is_error=False),
    )
    client._stub.AgentDef = fake  # type: ignore[attr-defined]

    result = await client.agent_def({
        "op": "create",
        "name": "reviewer",
        "overlay": {"system_prompt": "hi"},
    })
    assert result["def_id"] == "def_abc"
    assert result["name"] == "reviewer"
    assert result["version"] == 1

    sent = json.loads(captured["req"].input_json)
    assert sent["op"] == "create"
    assert sent["overlay"]["system_prompt"] == "hi"


# v0.9.x — the overlay field is server-side opaque (the AgentDef
# tool owns the schema). The adapter must pass it through verbatim
# so new fields like max_iterations work without an adapter version
# bump. This test pins the contract.
@pytest.mark.asyncio
async def test_agent_def_passes_max_iterations_through_overlay():
    client = _make_client()
    out_json = json.dumps({"def_id": "def_xyz", "name": "discovery", "version": 1}).encode()
    fake, captured = _async_returning(
        pb.SubstrateResponse(output_json=out_json, is_error=False),
    )
    client._stub.AgentDef = fake  # type: ignore[attr-defined]

    await client.agent_def({
        "op": "create",
        "name": "discovery",
        "overlay": {"system_prompt": "explore", "max_iterations": 64},
    })
    sent = json.loads(captured["req"].input_json)
    assert sent["overlay"]["max_iterations"] == 64
    assert sent["overlay"]["system_prompt"] == "explore"


@pytest.mark.asyncio
async def test_skill_def_happy_path():
    client = _make_client()
    out_json = json.dumps({"def_id": "sdf_abc", "name": "voice-applier", "version": 1}).encode()
    fake, captured = _async_returning(
        pb.SubstrateResponse(output_json=out_json, is_error=False),
    )
    client._stub.SkillDef = fake  # type: ignore[attr-defined]

    result = await client.skill_def({
        "op": "create",
        "name": "voice-applier",
        "overlay": {"body": "VOICE BODY"},
    })
    assert result["def_id"] == "sdf_abc"
    assert result["name"] == "voice-applier"


@pytest.mark.asyncio
async def test_skill_def_tool_refusal_raises_typed_error():
    """A tool-level refusal comes back as is_error=True; the client
    raises SubstrateToolRefusedError with the tool field set."""
    client = _make_client()
    err_text = b"overlay.body is required and must contain non-whitespace content"
    fake, _ = _async_returning(
        pb.SubstrateResponse(output_json=err_text, is_error=True),
    )
    client._stub.SkillDef = fake  # type: ignore[attr-defined]

    with pytest.raises(SubstrateToolRefusedError) as exc_info:
        await client.skill_def({"op": "create", "name": "x", "overlay": {"body": ""}})
    assert exc_info.value.tool == "SkillDef"
    assert "body is required" in str(exc_info.value)


@pytest.mark.asyncio
async def test_agent_def_tool_refusal_carries_agentdef_tool_field():
    """Same refusal path for AgentDef — the `tool` attribute on the
    typed error distinguishes which substrate refused."""
    client = _make_client()
    fake, _ = _async_returning(
        pb.SubstrateResponse(output_json=b"scope deny", is_error=True),
    )
    client._stub.AgentDef = fake  # type: ignore[attr-defined]

    with pytest.raises(SubstrateToolRefusedError) as exc_info:
        await client.agent_def({"op": "create", "name": "blocked"})
    assert exc_info.value.tool == "AgentDef"


@pytest.mark.asyncio
async def test_skill_def_serialises_input_to_bytes():
    """Verify input is JSON-encoded as bytes for the proto wire."""
    client = _make_client()
    fake, captured = _async_returning(
        pb.SubstrateResponse(output_json=b"{}", is_error=False),
    )
    client._stub.SkillDef = fake  # type: ignore[attr-defined]
    await client.skill_def({"op": "list", "name": "voice-applier"})
    assert isinstance(captured["req"].input_json, bytes)
    parsed = json.loads(captured["req"].input_json)
    assert parsed == {"op": "list", "name": "voice-applier"}
