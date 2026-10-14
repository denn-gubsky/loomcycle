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

Every error has a read-only ``retryable``: True only for a refusal that
clears on its own, judged from the class and ``reason``.

The ``code`` and ``message`` attributes preserve the gRPC payload
for log correlation when the typed class doesn't carry enough.
The ``reason`` attribute names the condition when the server said which
one it is (see ``LoomcycleError``).
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
        # ``reason`` is the server's machine-readable name for the condition
        # (``token_limit_exceeded``, ``model_not_allowed``, …): the reason of
        # the ``google.rpc.ErrorInfo`` in the status details, and the same
        # string the HTTP surface puts in an error body's ``code``. One gRPC
        # code covers several conditions, so branch on this, not on the
        # message. ``None`` when the server attached none.
        self.reason: Optional[str] = None

    @property
    def retryable(self) -> bool:
        """Whether the same call, sent again unchanged, can succeed once time
        has passed: True only for a refusal that clears on its own (see
        ``BackpressureError``, ``UnavailableError`` and ``_TRANSIENT_REASONS``),
        False otherwise.

        A read-only property rather than an attribute because it follows
        ``reason``, which is filled in after the error is built. False is the
        default because a wrong True turns a refusal that never clears into a
        retry loop that never ends, while a wrong False only surfaces an error
        the caller could have waited out. The client never retries on its
        own; bound your retries even when this is True."""
        return self.reason in _TRANSIENT_REASONS


# Reasons that clear on their own although their gRPC code, which also covers
# conditions that never do, maps to a class whose answer is otherwise False.
# The server classifies each as transient:
#   runtime_paused (UNAVAILABLE)      new runs refused until the operator resumes
#   tier_unavailable, pin_unavailable no provider for the tier/pin is up right now
#   call_failed (UNAVAILABLE)         a decision-model call did not complete
#   timeout (DEADLINE_EXCEEDED)       a decision-model call did not answer in time
_TRANSIENT_REASONS = frozenset(
    {"runtime_paused", "tier_unavailable", "pin_unavailable", "call_failed", "timeout"}
)

# RESOURCE_EXHAUSTED reasons that do NOT clear by waiting: an operator or the
# caller has to act first, so a retry loop on them never ends.
#   token_limit_exceeded  a hard token budget (until raised or the month rolls)
#   configured_run_cap    the user holds as many configured runs as allowed;
#                         start or discard one first
_CALLER_ACTION_RESOURCE_REASONS = frozenset({"token_limit_exceeded", "configured_run_cap"})

# The server sends configured_run_cap over gRPC with no ErrorInfo (its
# configured-run refusals map by HTTP status alone), so until it does, the
# message is the only discriminator: the stable tail of the refusal's text in
# internal/api/http/configured_runs.go.
_CONFIGURED_RUN_CAP_MESSAGE = "start or discard one first"


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
    """Raised for every gRPC ``RESOURCE_EXHAUSTED`` except a snapshot over
    its size cap. ``reason`` says which condition it is:

    * ``backpressure`` — the run queue is full; ``per_user_quota_exhausted``
      — the caller's own in-flight runs are at their cap; ``rate_limited``
      — the caller is starting runs too fast. These clear on their own:
      ``retryable`` is True.
    * ``token_limit_exceeded`` — a hard token budget. It does not clear
      until an operator raises the budget or the month rolls over, so
      ``retryable`` is False. Test ``retryable`` (or ``reason``) before
      retrying a BackpressureError; ``except BackpressureError: retry()``
      alone spins on the budget for the rest of the month.
    * the configured-run cap — the user already holds as many configured
      (not yet started) runs as allowed; only starting or discarding one
      makes room, so ``retryable`` is False. The server sends no reason for
      it over gRPC yet; it is recognised by its message."""

    @property
    def retryable(self) -> bool:
        if self.reason in _CALLER_ACTION_RESOURCE_REASONS:
            return False
        return _CONFIGURED_RUN_CAP_MESSAGE not in (self.message or "")


class AuthError(LoomcycleError):
    """Raised on bad/missing bearer token. Re-issue with a valid
    ``auth_token=`` constructor arg."""


class UnavailableError(LoomcycleError):
    """Raised for gRPC ``UNAVAILABLE``: the channel can't reach the server
    (network error, server down, TLS handshake failure), the runtime is
    paused, no provider in the tier is up, or a feature the deployment does
    not have (``decision_not_configured``, an unwired connector).

    ``retryable`` (inherited) is True only where the server's ``reason``
    names a condition that clears on its own: ``runtime_paused`` (until the
    operator resumes), ``tier_unavailable`` and ``pin_unavailable`` (until a
    provider recovers), ``call_failed`` (a decision-model call that did not
    complete). A transport failure carries no reason and neither does an
    unwired connector, so ``retryable`` cannot tell them apart and stays
    False; retry a reason-less UnavailableError on your own judgement, with
    backoff and a bound."""


class InvalidArgumentError(LoomcycleError):
    """Raised on caller-supplied input validation failures.

    Two sources, distinguished by the ``code`` attribute:

    * Client-side validation (v0.8.18+): a caller-side
      precondition fails before any wire call is made — e.g.
      ``restore_snapshot`` invoked with neither ``snapshot_id`` nor
      ``raw_json``, or ``create_snapshot`` given an invalid RFC3339
      ``since_ts``. ``code`` is ``None`` for these cases.
    * Server-side validation (v0.8.22+): the server returns gRPC
      ``INVALID_ARGUMENT`` — typically on substrate admin RPCs with
      malformed JSON in ``input_json``. ``code`` is
      ``grpc.StatusCode.INVALID_ARGUMENT`` for these cases.

    Callers wanting to distinguish should inspect ``e.code``."""


# v0.8.18 — Pause/Resume/Snapshot typed errors. Each maps from a
# specific gRPC status code on the new Pause/Snapshot RPCs.

class PauseNotConfiguredError(UnavailableError):
    """Raised when the server has no pause Manager wired (no Store
    backend on the deployment). Operator config issue; not a transient
    failure. Server returns gRPC Unavailable for this case."""

    @property
    def retryable(self) -> bool:
        return False


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


class SubstrateToolRefusedError(LoomcycleError):
    """Raised by ``agent_def`` / ``skill_def`` when the in-process
    substrate tool refused the call (scope deny, empty body,
    allowed-tools widening, etc.). Distinct from transport
    failures: the request reached the server, the substrate tool
    ran, and the tool itself returned is_error=True with a
    human-readable reason in ``message``.

    The ``tool`` attribute identifies which substrate tool refused
    ("AgentDef" or "SkillDef"). Operators catching this error
    should surface ``message`` to the calling agent / user rather
    than retrying."""

    def __init__(self, message: str, *, tool: str = "", error_info=None) -> None:
        super().__init__(message)
        self.tool = tool
        # The refusal's structure (a loomcycle.events.ErrorInfo): category,
        # retryability, next step, and the correct call format. None when the
        # runtime did not classify it.
        self.error_info = error_info
