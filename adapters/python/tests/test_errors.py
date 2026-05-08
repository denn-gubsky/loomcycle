"""Unit tests for the gRPC-error → typed-exception mapping.

We don't need a live gRPC server — we synthesize an
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
    """Stand-in for ``grpc.aio.AioRpcError`` for the unit tests
    that only need ``code()`` / ``details()``. The full-stack flow
    through ``_drive_stream`` is exercised separately with a real
    ``grpc.aio.AioRpcError`` (see test_drive_stream)."""

    def __init__(self, code: grpc.StatusCode, details: str) -> None:
        super().__init__(details)
        self._code = code
        self._details = details

    def code(self) -> grpc.StatusCode:
        return self._code

    def details(self) -> str:
        return self._details


# ---- NOT_FOUND discrimination by message text ----
#
# The server's wire-stable strings:
#   "session not found"                   (Continue / GetTranscript)
#   "no live run for %q ..."              (GetAgent / CancelAgent)
#   "no run found for agent_id %q"        (GetAgent / CancelAgent)


def test_not_found_session_message_routes_to_session_not_found():
    err = _FakeAioRpcError(grpc.StatusCode.NOT_FOUND, "session not found")
    with pytest.raises(SessionNotFoundError) as ei:
        _raise_from_grpc(err)
    assert ei.value.code == grpc.StatusCode.NOT_FOUND


def test_not_found_session_quoted_message_routes_to_session_not_found():
    err = _FakeAioRpcError(grpc.StatusCode.NOT_FOUND, 'session "abc-123" not found')
    with pytest.raises(SessionNotFoundError):
        _raise_from_grpc(err)


def test_not_found_agent_message_routes_to_agent_not_found():
    err = _FakeAioRpcError(
        grpc.StatusCode.NOT_FOUND, 'no run found for agent_id "ag-99"'
    )
    with pytest.raises(AgentNotFoundError) as ei:
        _raise_from_grpc(err)
    assert ei.value.code == grpc.StatusCode.NOT_FOUND


def test_not_found_no_live_run_message_routes_to_agent_not_found():
    err = _FakeAioRpcError(
        grpc.StatusCode.NOT_FOUND,
        'no live run for "ag-77" (no store configured)',
    )
    with pytest.raises(AgentNotFoundError):
        _raise_from_grpc(err)


def test_failed_precondition_session_busy_routes_to_session_busy():
    err = _FakeAioRpcError(
        grpc.StatusCode.FAILED_PRECONDITION,
        "session busy: another request is in flight",
    )
    with pytest.raises(SessionBusyError):
        _raise_from_grpc(err)


def test_failed_precondition_session_required_routes_to_base_error():
    # ErrSessionRequired also maps to FAILED_PRECONDITION but the
    # message doesn't match the session-busy heuristic — it should
    # surface as the base LoomcycleError (caller saw a code-precondition
    # the busy-session subclass wouldn't apply to).
    err = _FakeAioRpcError(
        grpc.StatusCode.FAILED_PRECONDITION,
        "session-bound action requires store",
    )
    with pytest.raises(LoomcycleError) as ei:
        _raise_from_grpc(err)
    assert not isinstance(ei.value, SessionBusyError)


def test_already_exists_raises_agent_id_in_use():
    err = _FakeAioRpcError(
        grpc.StatusCode.ALREADY_EXISTS, "agent_id already in use"
    )
    with pytest.raises(AgentIDInUseError):
        _raise_from_grpc(err)


def test_resource_exhausted_raises_backpressure():
    err = _FakeAioRpcError(
        grpc.StatusCode.RESOURCE_EXHAUSTED, "concurrency limit reached"
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
    assert not isinstance(ei.value, (AgentNotFoundError, SessionNotFoundError))
    assert ei.value.code == grpc.StatusCode.INVALID_ARGUMENT


def test_internal_falls_through_to_base_loomcycle_error():
    err = _FakeAioRpcError(grpc.StatusCode.INTERNAL, "panic in driveStream")
    with pytest.raises(LoomcycleError):
        _raise_from_grpc(err)
