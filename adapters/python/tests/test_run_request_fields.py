"""Unit tests for the v0.8.x per-run policy fields on RunRequest
and ContinueRequest.

PR #150 closed the gap between the Go server's HTTP wire and the
TS adapter; this test family pins the equivalent gap-close on the
gRPC + Python side. Without these fields wired through, the
Python adapter couldn't pass per-tenant / per-tier / per-user-bearer
signals — gRPC consumers were stuck with the v0.4 baseline.

We don't exercise an end-to-end RPC; we inspect the stub's last
request to confirm the kwargs land on the proto message. The
proto regen + Python client wiring is what's being verified.
"""

from __future__ import annotations

from typing import List

import grpc.aio
import pytest

from loomcycle import LoomcycleClient
from loomcycle._generated import loomcycle_pb2 as pb


class _FakeStub:
    """Captures the most recent RunRequest / ContinueRequest so
    tests can assert the field map. Returns an empty async-iter
    so the public methods don't block."""

    def __init__(self) -> None:
        self.last_run_req: pb.RunRequest = None  # type: ignore[assignment]
        self.last_continue_req: pb.ContinueRequest = None  # type: ignore[assignment]

    def Run(self, req: pb.RunRequest, metadata=None) -> "_EmptyStream":
        self.last_run_req = req
        return _EmptyStream()

    def Continue(self, req: pb.ContinueRequest, metadata=None) -> "_EmptyStream":
        self.last_continue_req = req
        return _EmptyStream()


class _EmptyStream:
    def __aiter__(self) -> "_EmptyStream":
        return self

    async def __anext__(self):
        raise StopAsyncIteration


def _make_client_with_stub(stub: _FakeStub) -> LoomcycleClient:
    # Build the client without opening a real channel; swap the
    # stub out from under it. Mirrors the pattern used by the
    # hook tests.
    channel = grpc.aio.insecure_channel("127.0.0.1:1")
    client = LoomcycleClient(channel=channel)
    client._stub = stub  # type: ignore[assignment]
    return client


@pytest.mark.asyncio
async def test_run_streaming_threads_tenant_tier_bearer():
    """tenant_id / user_tier / user_bearer land on RunRequest."""
    stub = _FakeStub()
    client = _make_client_with_stub(stub)
    # Consume the (empty) stream to ensure Run() is actually called.
    async for _ in client.run_streaming(
        agent="default",
        segments=[],
        tenant_id="acme-corp",
        user_tier="pro",
        user_bearer="bearer_AbCdEfGhIjKlMnOpQrStUv0123456789",
        user_id="u_test",
    ):
        pass
    req = stub.last_run_req
    assert req is not None, "Run was not called on the stub"
    assert req.tenant_id == "acme-corp"
    assert req.user_tier == "pro"
    assert req.user_bearer == "bearer_AbCdEfGhIjKlMnOpQrStUv0123456789"
    # Regression guard for the v0.4 wiring we didn't touch:
    assert req.user_id == "u_test"


@pytest.mark.asyncio
async def test_run_streaming_omits_policy_fields_when_not_supplied():
    """Empty defaults — proto fields are empty strings, not unset
    sub-messages. This is the wire-shape contract: the server
    treats empty strings as "no policy override" per the HTTP
    surface's omitempty behaviour."""
    stub = _FakeStub()
    client = _make_client_with_stub(stub)
    async for _ in client.run_streaming(agent="default", segments=[]):
        pass
    req = stub.last_run_req
    assert req.tenant_id == ""
    assert req.user_tier == ""
    assert req.user_bearer == ""


@pytest.mark.asyncio
async def test_continue_session_threads_tier_bearer():
    """user_tier and user_bearer are per-call on ContinueRequest;
    tenant_id is NOT on Continue (server inherits from session)."""
    stub = _FakeStub()
    client = _make_client_with_stub(stub)
    async for _ in client.continue_session(
        session_id="s_existing",
        segments=[],
        user_tier="enterprise",
        user_bearer="bearer_ZyXwVuTsRqPoNmLkJiHgFeDcBa9876543210",
    ):
        pass
    req = stub.last_continue_req
    assert req is not None, "Continue was not called on the stub"
    assert req.session_id == "s_existing"
    assert req.user_tier == "enterprise"
    assert req.user_bearer == "bearer_ZyXwVuTsRqPoNmLkJiHgFeDcBa9876543210"
    # ContinueRequest does NOT have a tenant_id field — pin that
    # so a future "let's just mirror RunRequest" PR doesn't
    # accidentally widen the surface.
    assert not hasattr(req, "tenant_id") or pb.ContinueRequest.DESCRIPTOR.fields_by_name.get("tenant_id") is None
