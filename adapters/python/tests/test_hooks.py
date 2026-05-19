"""Unit tests for the hook-management client methods.

These exercise the request shape, response unwrap, and error
mapping for ``register_hook`` / ``list_hooks`` / ``delete_hook``.

We don't need a live gRPC server — we mock the stub directly,
mirroring the pattern in ``test_errors.py``.
"""

from __future__ import annotations

import grpc
import pytest
from google.protobuf.timestamp_pb2 import Timestamp

from loomcycle._generated import loomcycle_pb2 as pb
from loomcycle.client import _hook_to_dict
from loomcycle.errors import HookNotFoundError, LoomcycleError


class _FakeAioRpcError(grpc.aio.AioRpcError):
    """Stand-in for a real ``grpc.aio.AioRpcError``.

    Subclasses the real class so the ``except grpc.aio.AioRpcError``
    blocks inside ``client.py`` catch it — ``test_errors.py``'s
    private ``_FakeAioRpcError`` only needs ``code()`` / ``details()``
    because it calls ``_raise_from_grpc`` directly, but here the
    error has to travel through the client method's try/except,
    which uses the real type for the catch clause.

    We bypass ``AioRpcError.__init__`` (which expects a full set of
    real metadata) — the methods that ``_raise_from_grpc`` calls
    (``code()`` / ``details()``) are overridden below."""

    def __init__(self, code: grpc.StatusCode, details: str) -> None:
        # Skip AioRpcError.__init__ — its signature wants metadata
        # we don't have. Go straight to Exception.
        Exception.__init__(self, details)
        self._code = code
        self._details = details

    def code(self) -> grpc.StatusCode:  # type: ignore[override]
        return self._code

    def details(self) -> str:  # type: ignore[override]
        return self._details


class _FakeStub:
    """Records the last request handed to each hook RPC and returns
    a canned response — or raises a canned AioRpcError."""

    def __init__(self) -> None:
        self.register_req = None
        self.list_called = False
        self.delete_req = None
        self.register_resp = pb.RegisterHookResponse(id="hook_test")
        self.list_resp = pb.ListHooksResponse()
        self.delete_resp = pb.DeleteHookResponse(deleted="hook_test")
        self.raise_on_register = None
        self.raise_on_list = None
        self.raise_on_delete = None

    async def RegisterHook(self, req, metadata=None):
        if self.raise_on_register is not None:
            raise self.raise_on_register
        self.register_req = req
        return self.register_resp

    async def ListHooks(self, req, metadata=None):
        if self.raise_on_list is not None:
            raise self.raise_on_list
        self.list_called = True
        return self.list_resp

    async def DeleteHook(self, req, metadata=None):
        if self.raise_on_delete is not None:
            raise self.raise_on_delete
        self.delete_req = req
        return self.delete_resp


def _client_with_stub(stub: _FakeStub):
    """Build a LoomcycleClient bypassing __init__'s channel
    creation. Only the stub is exercised; auth metadata is empty."""
    from loomcycle.client import LoomcycleClient

    c = LoomcycleClient.__new__(LoomcycleClient)
    c._stub = stub
    c._auth_token = ""
    return c


@pytest.mark.asyncio
async def test_register_hook_sends_full_request_shape():
    stub = _FakeStub()
    stub.register_resp = pb.RegisterHookResponse(id="hook_abc")
    c = _client_with_stub(stub)

    resp = await c.register_hook(
        owner="jobs-search-web",
        name="scan",
        phase="pre",
        tools=["WebFetch"],
        agents=["*"],
        callback_url="https://callback.local/h",
        fail_mode="closed",
        timeout_ms=2500,
    )
    assert resp == {"id": "hook_abc"}
    req = stub.register_req
    assert req.owner == "jobs-search-web"
    assert req.name == "scan"
    assert req.phase == "pre"
    assert list(req.tools) == ["WebFetch"]
    assert list(req.agents) == ["*"]
    assert req.callback_url == "https://callback.local/h"
    assert req.fail_mode == "closed"
    assert req.timeout_ms == 2500


@pytest.mark.asyncio
async def test_register_hook_defaults_fail_mode_open():
    stub = _FakeStub()
    c = _client_with_stub(stub)
    await c.register_hook(
        owner="x", name="y", phase="post", callback_url="https://e.test/h"
    )
    assert stub.register_req.fail_mode == "open"
    # Optional fields default to empty/zero on the wire.
    assert list(stub.register_req.tools) == []
    assert list(stub.register_req.agents) == []
    assert stub.register_req.timeout_ms == 0


@pytest.mark.asyncio
async def test_register_hook_invalid_argument_raises_loomcycle_error():
    """INVALID_ARGUMENT does not currently get a typed subclass —
    it falls through to the base LoomcycleError. Confirm that contract."""
    stub = _FakeStub()
    stub.raise_on_register = _FakeAioRpcError(
        grpc.StatusCode.INVALID_ARGUMENT,
        "invalid_registration: callback_url required",
    )
    c = _client_with_stub(stub)
    with pytest.raises(LoomcycleError) as ei:
        await c.register_hook(
            owner="x", name="y", phase="pre", callback_url=""
        )
    # The typed subclass is just LoomcycleError, NOT one of the more
    # specific subclasses.
    assert type(ei.value) is LoomcycleError
    assert ei.value.code == grpc.StatusCode.INVALID_ARGUMENT


@pytest.mark.asyncio
async def test_delete_hook_not_found_raises_hook_not_found():
    stub = _FakeStub()
    stub.raise_on_delete = _FakeAioRpcError(
        grpc.StatusCode.NOT_FOUND, 'no hook with id "hook_gone"'
    )
    c = _client_with_stub(stub)
    with pytest.raises(HookNotFoundError) as ei:
        await c.delete_hook("hook_gone")
    assert ei.value.code == grpc.StatusCode.NOT_FOUND


@pytest.mark.asyncio
async def test_delete_hook_success_returns_true():
    stub = _FakeStub()
    stub.delete_resp = pb.DeleteHookResponse(deleted="hook_xyz")
    c = _client_with_stub(stub)
    ok = await c.delete_hook("hook_xyz")
    assert ok is True
    assert stub.delete_req.id == "hook_xyz"


@pytest.mark.asyncio
async def test_list_hooks_converts_proto_to_dicts():
    stub = _FakeStub()
    ts = Timestamp()
    ts.FromSeconds(1715948400)  # 2026-05-17ish
    stub.list_resp = pb.ListHooksResponse(
        hooks=[
            pb.Hook(
                id="hook_a",
                owner="o",
                name="n",
                phase="pre",
                agents=["*"],
                tools=["WebFetch"],
                callback_url="https://e.test/h",
                fail_mode="open",
                timeout_ms=3000,
                registered_at=ts,
            )
        ]
    )
    c = _client_with_stub(stub)
    rows = await c.list_hooks()
    assert isinstance(rows, list)
    assert len(rows) == 1
    row = rows[0]
    # Dict keys mirror hooks.Hook JSON tags exactly.
    assert row["id"] == "hook_a"
    assert row["phase"] == "pre"
    assert row["callback_url"] == "https://e.test/h"
    assert row["fail_mode"] == "open"
    assert row["timeout_ms"] == 3000
    assert row["agents"] == ["*"]
    assert row["registered_at"]  # non-empty ISO 8601


def test_hook_to_dict_handles_unset_timestamp():
    """When the proto Timestamp is the zero value, _ts_to_iso
    should produce an empty string (no crash)."""
    h = pb.Hook(id="h", owner="o", name="n", phase="pre")
    d = _hook_to_dict(h)
    # Zero Timestamp is treated as "no value" by _ts_to_iso.
    assert d["registered_at"] in ("", "1970-01-01T00:00:00Z")
