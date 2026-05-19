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


class HookNotFoundError(LoomcycleError):
    """Raised by ``delete_hook`` when no hook is registered with the
    supplied id. Maps from gRPC ``codes.NOT_FOUND`` whose message
    mentions ``"hook"`` — the dispatcher in ``_raise_from_grpc``
    checks for the keyword BEFORE falling through to
    ``AgentNotFoundError``."""
