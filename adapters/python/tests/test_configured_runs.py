"""RFC DI configured runs over the Python adapter: each method builds the wire
request the server expects and decodes what it returns."""

from __future__ import annotations

import json
from typing import Any

import grpc.aio
import pytest

from loomcycle import LoomcycleClient
from loomcycle._generated import loomcycle_pb2 as pb


class _EmptyStream:
    def __aiter__(self):
        return self

    async def __anext__(self):
        raise StopAsyncIteration


class _Stub:
    """Records each configured-run request and answers with a fixed reply."""

    def __init__(self) -> None:
        self.calls: dict[str, Any] = {}
        self.reply = pb.ConfiguredRun(
            run_id="r_1", agent_id="a_1", session_id="s_1", status="configured",
            draft=b'{"agent": "qa", "segments": []}',
        )

    async def CreateConfiguredRun(self, req, metadata=None):
        self.calls["create"] = req
        return self.reply

    async def UpdateConfiguredRun(self, req, metadata=None):
        self.calls["update"] = req
        return self.reply

    def StartConfiguredRun(self, req, metadata=None):
        self.calls["start"] = req
        return _EmptyStream()

    async def DeleteConfiguredRun(self, req, metadata=None):
        self.calls["delete"] = req
        return pb.DeleteConfiguredRunResponse(deleted=True)


def _client() -> tuple[LoomcycleClient, _Stub]:
    client = LoomcycleClient(channel=grpc.aio.insecure_channel("127.0.0.1:1"))
    stub = _Stub()
    client._stub = stub  # type: ignore[assignment]
    return client, stub


@pytest.mark.asyncio
async def test_create_configured_run_sends_a_run_request_and_decodes_the_draft():
    client, stub = _client()
    out = await client.create_configured_run(
        agent="qa",
        segments=[{"role": "user", "content": [{"type": "trusted-text", "text": "hi"}]}],
        tool_choice={"mode": "none"},
        max_iterations=5,
    )
    req = stub.calls["create"]
    assert req.agent == "qa" and req.max_iterations == 5
    assert req.tool_choice.mode == "none"
    assert out["status"] == "configured" and out["run_id"] == "r_1"
    assert out["draft"] == {"agent": "qa", "segments": []}


@pytest.mark.asyncio
async def test_update_configured_run_sends_the_patch_as_json_with_null_for_removal():
    client, stub = _client()
    await client.update_configured_run("r_1", {"sampling": None, "max_tokens": 100})
    req = stub.calls["update"]
    assert req.run_id == "r_1"
    assert json.loads(req.patch) == {"sampling": None, "max_tokens": 100}


@pytest.mark.asyncio
async def test_start_configured_run_passes_the_secrets_to_the_start():
    client, stub = _client()
    async for _ in client.start_configured_run(
        "r_1", user_bearer="b" * 20, user_credentials={"github": "tok"}
    ):
        pass
    req = stub.calls["start"]
    assert req.run_id == "r_1" and req.user_bearer == "b" * 20
    assert dict(req.user_credentials) == {"github": "tok"}


@pytest.mark.asyncio
async def test_delete_configured_run_returns_deleted():
    client, stub = _client()
    assert await client.delete_configured_run("r_1") is True
    assert stub.calls["delete"].run_id == "r_1"
