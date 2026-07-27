"""GET /v1/config parity over gRPC: get_config decodes the JSON report.

Stub-mock pattern (no live server); the server-side path is covered by
internal/api/grpc/config_test.go.
"""

from __future__ import annotations

import json
from typing import Any

import grpc.aio
import pytest

from loomcycle import LoomcycleClient
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
async def test_get_config_decodes_the_json_report():
    """The RPC carries the report as JSON because its ``features`` values change
    shape by disclosure level; the client's job is to hand back a plain dict."""
    client = _make_client()
    report = {
        "generated_at": "2026-07-27T10:00:00Z",
        "view": "admin",
        "instance": {"version": "v1.38.0", "commit": "abc1234"},
        "features": {"bash": {"available": True}, "storage": {"backend": "postgres"}},
        "providers": [
            {"provider": "deepseek", "active": True},
            {"provider": "anthropic", "active": False},
        ],
        "models": [
            {
                "provider": "deepseek",
                "model": "deepseek-v4-pro",
                "tiers": ["middle"],
                "active": True,
                "selected": True,
            }
        ],
        "search": [{"provider": "brave", "active": True, "primary": True}],
        "limits": {"max_request_bytes": 16777216},
    }
    fn, captured = _async_returning(
        pb.ConfigResponse(config_json=json.dumps(report).encode())
    )
    client._stub.Config = fn  # type: ignore[assignment]

    got = await client.get_config()

    assert isinstance(captured["req"], pb.ConfigRequest)
    assert got["view"] == "admin"
    assert got["instance"]["version"] == "v1.38.0"
    # The live cascade is what a consumer actually renders.
    assert [p["provider"] for p in got["providers"]] == ["deepseek", "anthropic"]
    assert got["providers"][0]["active"] is True
    assert got["providers"][1]["active"] is False
    assert got["models"][0]["selected"] is True
    assert got["search"][0]["primary"] is True
    # Nested detail survives intact rather than being flattened by the client.
    assert got["features"]["storage"]["backend"] == "postgres"
    assert got["limits"]["max_request_bytes"] == 16777216


@pytest.mark.asyncio
async def test_get_config_public_view_is_not_a_grpc_shape():
    """gRPC authenticates before dispatch, so the narrower ``public`` level — an
    unauthenticated HTTP read under LOOMCYCLE_PUBLIC_CONFIG — never arrives here.
    A caller gets ``authenticated`` or ``admin``. This pins the documented
    contract so a future change that started emitting ``public`` over gRPC is
    caught rather than silently widening what the transport discloses.
    """
    client = _make_client()
    fn, _ = _async_returning(
        pb.ConfigResponse(
            config_json=json.dumps({"view": "authenticated", "features": {}}).encode()
        )
    )
    client._stub.Config = fn  # type: ignore[assignment]

    got = await client.get_config()
    assert got["view"] in ("authenticated", "admin")
