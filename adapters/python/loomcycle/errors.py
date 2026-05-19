"""Typed exceptions raised by ``LoomcycleClient``.

The gRPC server returns standard ``codes.*`` values which we map to
typed Python exceptions so callers can ``except`` on the specific
class instead of switching on ``e.code()``. The mapping mirrors
internal/api/grpc/server.go's ``mapRunnerErr``:

    InvalidArgument    → LoomcycleError (default for bad-request shape)
    FailedPrecondition → LoomcycleError (configuration/state mismatch)
    NotFound           → AgentNotFoundError or SessionNotFoundError
    AlreadyExists      → AgentIDInUseError
    ResourceExhausted  → BackpressureError
    Unauthenticated    → AuthError
    Unavailable        → UnavailableError
    Internal / other   → LoomcycleError (with the original message)

The ``code`` and ``message`` attributes preserve the gRPC payload
for log correlation when the typed class doesn't carry enough.
"""

from __future__ import annotations
from typing import Any, Optional


class LoomcycleError(Exception):
    """Base class for every error LoomcycleClient raises."""

    def __init__(self, message: str, code: Optional[Any] = None) -> None:
        super().__init__(message)
        self.message = message
        # ``code`` is a ``grpc.StatusCode`` when this came from the
        # gRPC layer; ``None`` for client-side validation errors.
        self.code = code


class AgentNotFoundError(LoomcycleError):
    """Raised by ``get_agent`` / ``cancel_agent`` when the agent_id
    has no matching run in the cancel registry or store."""


class SessionNotFoundError(LoomcycleError):
    """Raised by ``continue_session`` / ``get_transcript`` when the
    session_id has no matching row in the store."""


class SessionBusyError(LoomcycleError):
    """Raised by ``continue_session`` when another request is already
    in flight on the same session_id. Wait + retry, or cancel the
    in-flight one via ``cancel_agent``."""


class AgentIDInUseError(LoomcycleError):
    """Raised by ``run_streaming`` / ``continue_session`` when a
    caller-supplied agent_id is already mapped to an active run.
    The caller should pick a fresh agent_id (or omit it and let the
    server generate one)."""


class BackpressureError(LoomcycleError):
    """Raised when the concurrency semaphore rejected the run. Wait
    until in-flight runs drain, or raise the operator's
    ``max_concurrent_runs``."""


class AuthError(LoomcycleError):
    """Raised on bad/missing bearer token. Re-issue with a valid
    ``auth_token=`` constructor arg."""


class UnavailableError(LoomcycleError):
    """Raised when the gRPC channel can't reach the server (network
    error, server down, TLS handshake failure). Adapters should
    retry with backoff."""


# v0.8.18 — Pause/Resume/Snapshot typed errors. Each maps from a
# specific gRPC status code on the new Pause/Snapshot RPCs.

class PauseNotConfiguredError(UnavailableError):
    """Raised when the server has no pause Manager wired (no Store
    backend on the deployment). Operator config issue; not a transient
    failure. Server returns gRPC Unavailable for this case."""


class AlreadyPausingError(LoomcycleError):
    """Raised by ``pause_runtime`` when the runtime is already pausing
    or paused. Server returns gRPC FailedPrecondition. Idempotent —
    a scripted ``pause if not paused`` loop can swallow this."""


class NotPausedError(LoomcycleError):
    """Raised by ``resume_runtime`` when the runtime is not paused.
    Server returns gRPC FailedPrecondition. Symmetric with
    ``AlreadyPausingError``."""


class SnapshotNotFoundError(LoomcycleError):
    """Raised by ``get_snapshot`` / ``export_snapshot`` /
    ``restore_snapshot`` / ``delete_snapshot`` when no snapshot has
    the supplied id. Server returns gRPC NotFound."""


class SnapshotTooLargeError(LoomcycleError):
    """Raised by ``create_snapshot`` when the serialised envelope
    exceeds ``LOOMCYCLE_SNAPSHOT_MAX_BYTES`` (default 512 MiB).
    Server returns gRPC ResourceExhausted."""


class SnapshotVersionError(LoomcycleError):
    """Raised by ``restore_snapshot`` when a section's declared
    version is newer than the reader supports OR unknown to the
    migration registry. Operator upgrades loomcycle before restoring.
    Server returns gRPC FailedPrecondition for both subcases."""
