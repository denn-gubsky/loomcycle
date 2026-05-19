"""v0.8.18 — typed-error tests for the new pause/snapshot gRPC codes.

Mirrors the test_errors.py pattern: we synthesize an AioRpcError-like
stub with the right code/details and verify the right Python
exception class lands.
"""

from __future__ import annotations

import grpc
import pytest

from loomcycle.client import _raise_from_grpc
from loomcycle.errors import (
    AlreadyPausingError,
    NotPausedError,
    PauseNotConfiguredError,
    SnapshotNotFoundError,
    SnapshotTooLargeError,
    SnapshotVersionError,
    UnavailableError,
)


class _FakeAioRpcError(Exception):
    def __init__(self, code: grpc.StatusCode, details: str) -> None:
        super().__init__(details)
        self._code = code
        self._details = details

    def code(self) -> grpc.StatusCode:
        return self._code

    def details(self) -> str:
        return self._details


# ---- AlreadyPausing / NotPaused (FailedPrecondition) ----

def test_failed_precondition_already_pausing_routes_to_already_pausing():
    err = _FakeAioRpcError(
        grpc.StatusCode.FAILED_PRECONDITION,
        "connector: runtime is already pausing or paused",
    )
    with pytest.raises(AlreadyPausingError):
        _raise_from_grpc(err)


def test_failed_precondition_not_paused_routes_to_not_paused():
    err = _FakeAioRpcError(
        grpc.StatusCode.FAILED_PRECONDITION,
        "connector: runtime is not paused",
    )
    with pytest.raises(NotPausedError):
        _raise_from_grpc(err)


def test_failed_precondition_snapshot_version_too_new_routes_to_version_error():
    err = _FakeAioRpcError(
        grpc.StatusCode.FAILED_PRECONDITION,
        "connector: snapshot section version newer than reader supports: memory v9.99",
    )
    with pytest.raises(SnapshotVersionError):
        _raise_from_grpc(err)


def test_failed_precondition_snapshot_version_unknown_routes_to_version_error():
    err = _FakeAioRpcError(
        grpc.StatusCode.FAILED_PRECONDITION,
        "connector: snapshot section version unknown: memory v0.1",
    )
    with pytest.raises(SnapshotVersionError):
        _raise_from_grpc(err)


# ---- SnapshotNotFound (NotFound) ----

def test_not_found_snapshot_routes_to_snapshot_not_found():
    err = _FakeAioRpcError(
        grpc.StatusCode.NOT_FOUND,
        "connector: snapshot not found: snap_does_not_exist",
    )
    with pytest.raises(SnapshotNotFoundError):
        _raise_from_grpc(err)


def test_not_found_snapshot_wins_over_session_in_overlapping_message():
    """Pins the priority order documented in _raise_from_grpc: when a
    message contains both "snapshot" and "session" (e.g., a restore
    diagnostic referencing the synthesized session_id for a missing
    snapshot), the snapshot keyword wins. This is a deliberate
    choice — the snapshot is the operation that failed; the session
    reference is incidental."""
    err = _FakeAioRpcError(
        grpc.StatusCode.NOT_FOUND,
        "connector: snapshot not found (session snap_sess_X for run Y was incidentally referenced)",
    )
    with pytest.raises(SnapshotNotFoundError):
        _raise_from_grpc(err)


# ---- SnapshotTooLarge (ResourceExhausted) ----
#
# ResourceExhausted is overloaded: BackpressureError for concurrency
# rejections, SnapshotTooLargeError when the message mentions snapshot.
# The discriminator is the message text.

def test_resource_exhausted_snapshot_routes_to_snapshot_too_large():
    err = _FakeAioRpcError(
        grpc.StatusCode.RESOURCE_EXHAUSTED,
        "connector: snapshot exceeds size cap: serialised size 600M bytes",
    )
    with pytest.raises(SnapshotTooLargeError):
        _raise_from_grpc(err)


# ---- PauseNotConfigured (Unavailable) ----
#
# Unavailable is overloaded: PauseNotConfiguredError when the
# message mentions pause-not-configured, else generic UnavailableError.

def test_unavailable_pause_not_configured_routes_to_typed_error():
    err = _FakeAioRpcError(
        grpc.StatusCode.UNAVAILABLE,
        "connector: pause manager not configured on this server",
    )
    with pytest.raises(PauseNotConfiguredError):
        _raise_from_grpc(err)


def test_unavailable_generic_falls_through_to_base_unavailable():
    err = _FakeAioRpcError(
        grpc.StatusCode.UNAVAILABLE,
        "channel closed unexpectedly",
    )
    with pytest.raises(UnavailableError) as excinfo:
        _raise_from_grpc(err)
    # Confirm we didn't accidentally upgrade to PauseNotConfiguredError.
    assert not isinstance(excinfo.value, PauseNotConfiguredError)


# ---- Inheritance check: PauseNotConfiguredError IS-A UnavailableError ----

def test_pause_not_configured_inherits_from_unavailable():
    """Callers that broadly catch UnavailableError get the
    pause-not-configured case too — preserves backwards compat for
    code that doesn't yet know about the more specific error."""
    err = PauseNotConfiguredError("test", code=grpc.StatusCode.UNAVAILABLE)
    assert isinstance(err, UnavailableError)
