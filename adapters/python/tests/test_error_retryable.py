"""``retryable`` on the typed errors: can the same call succeed later?

Every gRPC RESOURCE_EXHAUSTED is a BackpressureError, so the class alone
cannot tell a full queue (retry) from a hard token budget (do not). The
server's ErrorInfo reason can, and ``retryable`` reads it.

Each trailer below is a google.rpc.Status exactly as the SERVER serializes it
(captured from internal/api/grpc mapRunnerErr), not bytes this file encodes:
a test that built them with the reader's own idea of the format could not
catch the reader being wrong about it.
"""

from __future__ import annotations

import grpc
import grpc.aio
import pytest

from loomcycle import (
    AgentIDInUseError,
    AuthError,
    BackpressureError,
    InvalidArgumentError,
    LoomcycleError,
    PauseNotConfiguredError,
    SessionBusyError,
    SnapshotTooLargeError,
    UnavailableError,
)
from loomcycle.client import _raise_from_grpc

_SERVED = {
    # ResourceExhausted, ErrorInfo{reason: token_limit_exceeded, category:
    # business, is_retryable: false}
    "token_limit_exceeded": (
        "08081223746f6b656e5f6c696d69745f65786365656465643a2074656e616e742062756467"
        "65741a7a0a28747970652e676f6f676c65617069732e636f6d2f676f6f676c652e7270632e"
        "4572726f72496e666f124e0a14746f6b656e5f6c696d69745f657863656564656412096c6f"
        "6f6d6379636c651a140a0863617465676f72791208627573696e6573731a150a0c69735f72"
        "6574727961626c65120566616c7365"
    ),
    # ResourceExhausted, ErrorInfo{reason: backpressure} then a RetryInfo.
    "backpressure": (
        "0808120c6261636b70726573737572651a720a28747970652e676f6f676c65617069732e63"
        "6f6d2f676f6f676c652e7270632e4572726f72496e666f12460a0c6261636b707265737375"
        "726512096c6f6f6d6379636c651a150a0863617465676f727912097472616e7369656e741a"
        "140a0c69735f726574727961626c651204747275651a300a28747970652e676f6f676c6561"
        "7069732e636f6d2f676f6f676c652e7270632e5265747279496e666f12040a020805"
    ),
    # ResourceExhausted, ErrorInfo{reason: rate_limited}, no RetryInfo.
    "rate_limited": (
        "08081217726174655f6c696d697465643a20736c6f7720646f776e1a720a28747970652e67"
        "6f6f676c65617069732e636f6d2f676f6f676c652e7270632e4572726f72496e666f12460a"
        "0c726174655f6c696d6974656412096c6f6f6d6379636c651a150a0863617465676f727912"
        "097472616e7369656e741a140a0c69735f726574727961626c65120474727565"
    ),
    # ResourceExhausted, ErrorInfo{reason: per_user_quota_exhausted} then a
    # RetryInfo.
    "per_user_quota_exhausted": (
        "080812267065725f757365725f71756f74615f6578686175737465643a20757365723d7520"
        "6361703d321a7e0a28747970652e676f6f676c65617069732e636f6d2f676f6f676c652e72"
        "70632e4572726f72496e666f12520a187065725f757365725f71756f74615f657868617573"
        "74656412096c6f6f6d6379636c651a150a0863617465676f727912097472616e7369656e74"
        "1a140a0c69735f726574727961626c651204747275651a300a28747970652e676f6f676c65"
        "617069732e636f6d2f676f6f676c652e7270632e5265747279496e666f12040a020805"
    ),
    # Unavailable, ErrorInfo{reason: runtime_paused} then a RetryInfo.
    "runtime_paused": (
        "080e121672756e74696d65207061757365643a207061757365641a740a28747970652e676f"
        "6f676c65617069732e636f6d2f676f6f676c652e7270632e4572726f72496e666f12480a0e"
        "72756e74696d655f70617573656412096c6f6f6d6379636c651a150a0863617465676f7279"
        "12097472616e7369656e741a140a0c69735f726574727961626c651204747275651a300a28"
        "747970652e676f6f676c65617069732e636f6d2f676f6f676c652e7270632e526574727949"
        "6e666f12040a02080f"
    ),
    # Unavailable, ErrorInfo{reason: call_failed, category: transient,
    # is_retryable: true} (internal/api/grpc decideStatus).
    "call_failed": (
        "080e123c4465636973696f6e3a2063616c6c5f6661696c65643a207468652063616c6c2074"
        "6f20746865206465636973696f6e206d6f64656c206661696c65641a710a28747970652e67"
        "6f6f676c65617069732e636f6d2f676f6f676c652e7270632e4572726f72496e666f12450a"
        "0b63616c6c5f6661696c656412096c6f6f6d6379636c651a150a0863617465676f72791209"
        "7472616e7369656e741a140a0c69735f726574727961626c65120474727565"
    ),
    # DeadlineExceeded, ErrorInfo{reason: timeout, …} (decideStatus).
    "timeout": (
        "0804123c4465636973696f6e3a2074696d656f75743a20746865206465636973696f6e206d"
        "6f64656c20646964206e6f7420616e7377657220696e2074696d651a6d0a28747970652e67"
        "6f6f676c65617069732e636f6d2f676f6f676c652e7270632e4572726f72496e666f12410a"
        "0774696d656f757412096c6f6f6d6379636c651a140a0c69735f726574727961626c651204"
        "747275651a150a0863617465676f727912097472616e7369656e74"
    ),
    # ResourceExhausted, ErrorInfo{reason: configured_run_cap} and no metadata
    # or RetryInfo (internal/api/grpc configuredErrToStatus, CreateConfiguredRun
    # over the per-user cap of 2).
    "configured_run_cap": (
        "0808125474686973207573657220616c726561647920686f6c6473203220636f6e666967"
        "757265642072756e732028746865206361702069732032293b207374617274206f722064"
        "697363617264206f6e652066697273741a4b0a28747970652e676f6f676c65617069732e"
        "636f6d2f676f6f676c652e7270632e4572726f72496e666f121f0a12636f6e6669677572"
        "65645f72756e5f63617012096c6f6f6d6379636c65"
    ),
}

# What an older server's configuredErrToStatus sent for the per-user
# configured-run cap: a bare ResourceExhausted status with no details, so no
# reason. The message is the refusal's text in
# internal/api/http/configured_runs.go.
_CONFIGURED_RUN_CAP_MSG = (
    "this user already holds 2 configured runs (the cap is 2); start or discard one first"
)


def _rpc_error(code: grpc.StatusCode, details: str, status_details_hex: str = "") -> grpc.aio.AioRpcError:
    trailing = grpc.aio.Metadata()
    if status_details_hex:
        trailing.add("grpc-status-details-bin", bytes.fromhex(status_details_hex))
    return grpc.aio.AioRpcError(
        code=code,
        initial_metadata=grpc.aio.Metadata(),
        trailing_metadata=trailing,
        details=details,
        debug_error_string="",
    )


def _raised(err: grpc.aio.AioRpcError) -> LoomcycleError:
    with pytest.raises(LoomcycleError) as ei:
        _raise_from_grpc(err)
    return ei.value


def test_a_token_budget_refusal_is_not_retryable():
    e = _raised(
        _rpc_error(
            grpc.StatusCode.RESOURCE_EXHAUSTED,
            "token_limit_exceeded: tenant budget",
            _SERVED["token_limit_exceeded"],
        )
    )
    # Still the class every RESOURCE_EXHAUSTED raises; retryable is what
    # stops an `except BackpressureError: retry()` loop from spinning on it.
    assert isinstance(e, BackpressureError)
    assert e.reason == "token_limit_exceeded"
    assert e.retryable is False


@pytest.mark.parametrize("reason", ["backpressure", "rate_limited", "per_user_quota_exhausted"])
def test_a_resource_exhausted_that_clears_on_its_own_is_retryable(reason):
    e = _raised(_rpc_error(grpc.StatusCode.RESOURCE_EXHAUSTED, reason, _SERVED[reason]))
    assert isinstance(e, BackpressureError)
    assert e.reason == reason
    assert e.retryable is True


def test_the_configured_run_cap_is_backpressure_but_not_retryable():
    # Only starting or discarding a configured run makes room; waiting never
    # does, so a retry loop on it would not end.
    e = _raised(_rpc_error(grpc.StatusCode.RESOURCE_EXHAUSTED, _CONFIGURED_RUN_CAP_MSG))
    assert isinstance(e, BackpressureError)
    assert e.reason is None
    assert e.retryable is False


def test_the_configured_run_cap_arrives_as_its_reason():
    # The server names the cap in ErrorInfo, so the reason (not the message
    # text) is what makes it not retryable.
    e = _raised(
        _rpc_error(
            grpc.StatusCode.RESOURCE_EXHAUSTED,
            _CONFIGURED_RUN_CAP_MSG,
            _SERVED["configured_run_cap"],
        )
    )
    assert isinstance(e, BackpressureError)
    assert e.reason == "configured_run_cap"
    assert e.retryable is False


def test_the_configured_run_cap_reason_is_not_retryable_either():
    # The HTTP code, once the server attaches it as the gRPC reason too.
    e = BackpressureError("configured run cap")
    e.reason = "configured_run_cap"
    assert e.retryable is False


@pytest.mark.parametrize(
    "code,reason,cls",
    [
        (grpc.StatusCode.UNAVAILABLE, "call_failed", UnavailableError),
        (grpc.StatusCode.DEADLINE_EXCEEDED, "timeout", LoomcycleError),
    ],
)
def test_a_decision_call_that_failed_or_timed_out_is_retryable(code, reason, cls):
    # The server classifies both as transient and says to send the call again.
    e = _raised(_rpc_error(code, "Decision: " + reason, _SERVED[reason]))
    assert type(e) is cls
    assert e.reason == reason
    assert e.retryable is True


def test_a_resource_exhausted_without_a_reason_is_retryable_backpressure():
    # An older server, or a refusal the server did not classify (a full
    # steering input queue): the semaphore's meaning, transient.
    e = _raised(_rpc_error(grpc.StatusCode.RESOURCE_EXHAUSTED, "concurrency limit reached"))
    assert isinstance(e, BackpressureError)
    assert e.reason is None
    assert e.retryable is True


def test_a_snapshot_over_its_cap_is_not_retryable():
    e = _raised(_rpc_error(grpc.StatusCode.RESOURCE_EXHAUSTED, "snapshot exceeds max bytes"))
    assert isinstance(e, SnapshotTooLargeError)
    assert e.retryable is False


def test_a_paused_runtime_is_retryable():
    e = _raised(_rpc_error(grpc.StatusCode.UNAVAILABLE, "runtime paused: paused", _SERVED["runtime_paused"]))
    assert isinstance(e, UnavailableError)
    assert e.reason == "runtime_paused"
    assert e.retryable is True


@pytest.mark.parametrize(
    "code,details,cls",
    [
        (grpc.StatusCode.UNAVAILABLE, "connector not wired", UnavailableError),
        (grpc.StatusCode.UNAVAILABLE, "pause manager not configured", PauseNotConfiguredError),
        (grpc.StatusCode.INVALID_ARGUMENT, "missing agent", InvalidArgumentError),
        (grpc.StatusCode.UNAUTHENTICATED, "invalid token", AuthError),
        (grpc.StatusCode.ALREADY_EXISTS, "agent_id already in use", AgentIDInUseError),
        (grpc.StatusCode.FAILED_PRECONDITION, "session busy", SessionBusyError),
        (grpc.StatusCode.INTERNAL, "boom", LoomcycleError),
    ],
)
def test_other_errors_are_not_retryable(code, details, cls):
    e = _raised(_rpc_error(code, details))
    assert type(e) is cls
    assert e.retryable is False


def test_retryable_is_read_only_and_false_on_a_bare_error():
    e = LoomcycleError("x")
    assert e.retryable is False
    with pytest.raises(AttributeError):
        e.retryable = True  # type: ignore[misc]
