"""Unit tests for the gRPC-error → typed-exception mapping.

We don't need a live gRPC server — we just synthesize an
``AioRpcError`` with the right code/details and verify the
right Python exception class lands.
"""

from __future__ import annotations

import grpc
import pytest

from loomcycle.client import _raise_from_grpc
from loomcycle.errors import (
    AgentIDInUseError,
    AgentNotFoundError,
    AuthError,
    BackpressureError,
    LoomcycleError,
    SessionBusyError,
    SessionNotFoundError,
    UnavailableError,
)


class _FakeAioRpcError(Exception):
    """Stand-in for ``grpc.aio.AioRpcError`` — only the surface
    ``_raise_from_grpc`` reads (``code()``, ``details()``)."""

    def __init__(self, code: grpc.StatusCode, details: str) -> None:
        super().__init__(details)
        self._code = code
        self._details = details

    def code(self) -> grpc.StatusCode:
        return self._code

    def details(self) -> str:
        return self._details


def test_not_found_with_agent_id_raises_agent_not_found():
    err = _FakeAioRpcError(grpc.StatusCode.NOT_FOUND, "no such agent")
    with pytest.raises(AgentNotFoundError) as ei:
        _raise_from_grpc(err, agent_id="ag-123")
    assert "no such agent" in ei.value.message
    assert ei.value.code == grpc.StatusCode.NOT_FOUND


def test_not_found_with_session_id_raises_session_not_found():
    err = _FakeAioRpcError(grpc.StatusCode.NOT_FOUND, "no such session")
    with pytest.raises(SessionNotFoundError) as ei:
        _raise_from_grpc(err, session_id="sess-abc")
    assert "no such session" in ei.value.message


def test_failed_precondition_session_busy_routes_to_session_busy():
    err = _FakeAioRpcError(
        grpc.StatusCode.FAILED_PRECONDITION,
        "session busy: another request is in flight",
    )
    with pytest.raises(SessionBusyError):
        _raise_from_grpc(err, session_id="sess-abc")


def test_already_exists_raises_agent_id_in_use():
    err = _FakeAioRpcError(
        grpc.StatusCode.ALREADY_EXISTS,
        "agent_id already in use",
    )
    with pytest.raises(AgentIDInUseError):
        _raise_from_grpc(err)


def test_resource_exhausted_raises_backpressure():
    err = _FakeAioRpcError(
        grpc.StatusCode.RESOURCE_EXHAUSTED,
        "concurrency limit reached",
    )
    with pytest.raises(BackpressureError):
        _raise_from_grpc(err)


def test_unauthenticated_raises_auth_error():
    err = _FakeAioRpcError(grpc.StatusCode.UNAUTHENTICATED, "invalid token")
    with pytest.raises(AuthError):
        _raise_from_grpc(err)


def test_unavailable_raises_unavailable_error():
    err = _FakeAioRpcError(
        grpc.StatusCode.UNAVAILABLE, "channel handshake failed"
    )
    with pytest.raises(UnavailableError):
        _raise_from_grpc(err)


def test_invalid_argument_raises_base_loomcycle_error():
    err = _FakeAioRpcError(
        grpc.StatusCode.INVALID_ARGUMENT, "missing required field 'agent'"
    )
    with pytest.raises(LoomcycleError) as ei:
        _raise_from_grpc(err)
    # Should NOT be one of the specific subclasses.
    assert not isinstance(ei.value, (AgentNotFoundError, SessionNotFoundError))
    assert ei.value.code == grpc.StatusCode.INVALID_ARGUMENT


def test_internal_falls_through_to_base_loomcycle_error():
    err = _FakeAioRpcError(grpc.StatusCode.INTERNAL, "panic in driveStream")
    with pytest.raises(LoomcycleError):
        _raise_from_grpc(err)
