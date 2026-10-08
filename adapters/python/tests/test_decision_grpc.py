"""decide / list_decision_models — the Decide and ListDecisionModels RPCs.

Stub-mock pattern (no live server). The server-side path is covered by
internal/api/grpc/decide_test.go.
"""

from __future__ import annotations

import json
from typing import Any

import grpc
import grpc.aio
import pytest

from loomcycle import (
    BackpressureError,
    InvalidArgumentError,
    LoomcycleClient,
    LoomcycleError,
)
from loomcycle._generated import loomcycle_pb2 as pb
from loomcycle.client import _error_reason, _raise_from_grpc


def _make_client() -> LoomcycleClient:
    channel = grpc.aio.insecure_channel("127.0.0.1:1")
    return LoomcycleClient(channel=channel, auth_token="tok")


def _async_returning(result: Any):
    captured: dict = {}

    async def fn(req, metadata=None):
        captured["req"] = req
        captured["metadata"] = metadata
        return result

    return fn, captured


def _async_raising(err: Exception):
    async def fn(req, metadata=None):
        raise err

    return fn


STATE = {
    "ticket": "My invoice for March was charged twice and I want my money back.",
    "customer_tier": "gold",
    "attempts": 2,
    "weight": 1.0,
    "nested": {"tags": ["billing", "ü"], "open": True, "owner": None},
}

QUESTIONS = {
    "route": {
        "type": "choice",
        "instructions": "Which team should handle this ticket?",
        "criteria": {"billing": "invoices, refunds, charges", "support": None},
    },
    "urgent": {"type": "noul", "instructions": "Does this ticket need a reply within the hour?"},
    "detail": {
        "type": "score",
        "instructions": "How complete is the problem report?",
        "criteria": ["no detail", "some detail", "everything needed"],
    },
}


# ---- request encoding ----

@pytest.mark.asyncio
async def test_decide_encodes_state_and_each_criteria_as_json_bytes():
    client = _make_client()
    fake, captured = _async_returning(pb.DecideResponse())
    client._stub.Decide = fake  # type: ignore[attr-defined]

    await client.decide(STATE, QUESTIONS)

    req = captured["req"]
    assert isinstance(req, pb.DecideRequest)
    assert req.model == ""
    assert isinstance(req.state_json, bytes)
    assert json.loads(req.state_json) == STATE
    # An integer stays an integer and a float a float on the wire: the model is
    # shown the number the caller wrote.
    assert b'"attempts": 2,' in req.state_json
    assert b'"weight": 1.0' in req.state_json
    assert set(req.questions) == {"route", "urgent", "detail"}
    route = req.questions["route"]
    assert (route.type, route.instructions) == ("choice", "Which team should handle this ticket?")
    assert json.loads(route.criteria_json) == {"billing": "invoices, refunds, charges", "support": None}
    detail = req.questions["detail"]
    assert detail.type == "score"
    assert json.loads(detail.criteria_json) == ["no detail", "some detail", "everything needed"]
    assert ("authorization", "Bearer tok") in list(captured["metadata"])


@pytest.mark.asyncio
async def test_decide_sends_no_criteria_for_a_question_without_one():
    client = _make_client()
    fake, captured = _async_returning(pb.DecideResponse())
    client._stub.Decide = fake  # type: ignore[attr-defined]

    await client.decide(
        {},
        {
            "absent": {"type": "noul", "instructions": "q"},
            "none": {"type": "noul", "instructions": "q", "criteria": None},
            "given": {"type": "noul", "instructions": "q", "criteria": {"true": "yes"}},
        },
    )

    req = captured["req"]
    assert req.state_json == b"{}"
    # Empty bytes, not an encoded `null`.
    assert req.questions["absent"].criteria_json == b""
    assert req.questions["none"].criteria_json == b""
    assert json.loads(req.questions["given"].criteria_json) == {"true": "yes"}


@pytest.mark.asyncio
async def test_decide_names_the_model_when_given():
    client = _make_client()
    fake, captured = _async_returning(pb.DecideResponse())
    client._stub.Decide = fake  # type: ignore[attr-defined]

    await client.decide({}, {"q": {"type": "noul", "instructions": "q"}}, model="decide-deep")

    assert captured["req"].model == "decide-deep"


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "state,questions,names",
    [
        ({"when": object()}, {"q": {"type": "noul", "instructions": "q"}}, "state"),
        ({"x": float("nan")}, {"q": {"type": "noul", "instructions": "q"}}, "state"),
        ({}, {"q": {"type": "choice", "instructions": "q", "criteria": {"a": {1, 2}}}}, "question 'q': criteria"),
    ],
)
async def test_decide_refuses_a_value_that_is_not_json_before_any_call(state, questions, names):
    client = _make_client()
    fake, captured = _async_returning(pb.DecideResponse())
    client._stub.Decide = fake  # type: ignore[attr-defined]

    with pytest.raises(InvalidArgumentError) as ei:
        await client.decide(state, questions)

    assert names in str(ei.value)
    assert ei.value.code is None  # client-side: nothing was sent
    assert "req" not in captured


# ---- answer decoding ----

@pytest.mark.asyncio
async def test_decide_decodes_each_answer_type_to_a_dict():
    client = _make_client()
    resp = pb.DecideResponse(
        model="decide",
        provider="ollama-local",
        served_model="nimble",
        answers={
            "route": b'{"type":"choice","choice":"billing","probabilities":{"billing":0.983,"support":0.017},"confidence":0.911}',
            "urgent": b'{"type":"noul","noul":0.316}',
            "detail": b'{"type":"score","score":1.874,"legend":{"0":"no detail","1":"some detail","2":"everything needed"},"probabilities":{"0":0.039,"1":0.048,"2":0.913},"confidence":0.677}',
        },
        usage=pb.DecisionUsage(input_tokens=1059, output_tokens=4),
    )
    fake, _ = _async_returning(resp)
    client._stub.Decide = fake  # type: ignore[attr-defined]

    out = await client.decide(STATE, QUESTIONS)

    assert out == {
        "model": "decide",
        "provider": "ollama-local",
        "served_model": "nimble",
        "answers": {
            "route": {
                "type": "choice",
                "choice": "billing",
                "probabilities": {"billing": 0.983, "support": 0.017},
                "confidence": 0.911,
            },
            "urgent": {"type": "noul", "noul": 0.316},
            "detail": {
                "type": "score",
                "score": 1.874,
                "legend": {"0": "no detail", "1": "some detail", "2": "everything needed"},
                "probabilities": {"0": 0.039, "1": 0.048, "2": 0.913},
                "confidence": 0.677,
            },
        },
        "usage": {"input_tokens": 1059, "output_tokens": 4},
    }
    # The caller never handles bytes.
    assert all(isinstance(a, dict) for a in out["answers"].values())


@pytest.mark.asyncio
async def test_decide_keeps_a_noul_of_zero_and_integer_numbers_unchanged():
    client = _make_client()
    resp = pb.DecideResponse(
        answers={
            "urgent": b'{"type":"noul","noul":0}',
            "detail": b'{"type":"score","score":2,"legend":{"2":"all"},"probabilities":{"2":1},"confidence":1.0,"margin":0.25}',
        },
    )
    fake, _ = _async_returning(resp)
    client._stub.Decide = fake  # type: ignore[attr-defined]

    out = await client.decide({}, {"urgent": {"type": "noul", "instructions": "q"}})

    urgent = out["answers"]["urgent"]
    assert "noul" in urgent
    assert urgent["noul"] == 0 and type(urgent["noul"]) is int
    detail = out["answers"]["detail"]
    assert type(detail["score"]) is int and detail["score"] == 2
    assert type(detail["probabilities"]["2"]) is int
    assert type(detail["confidence"]) is float
    # A field this client does not know about arrives as the model wrote it.
    assert detail["margin"] == 0.25
    # No usage on the wire reads as zero, not a missing key.
    assert out["usage"] == {"input_tokens": 0, "output_tokens": 0}


# ---- the model list ----

@pytest.mark.asyncio
async def test_list_decision_models_returns_default_and_limits():
    client = _make_client()
    resp = pb.ListDecisionModelsResponse(
        default_model="decide",
        models=[
            pb.DecisionModel(
                name="decide", provider="ollama-local", model="nimble",
                limits=pb.DecisionLimits(max_questions=64, min_options=2, max_options=26),
            ),
            pb.DecisionModel(
                name="decide-deep", provider="ollama-local", model="nimble-xl",
                limits=pb.DecisionLimits(max_questions=32, min_options=2, max_options=16),
            ),
        ],
    )
    fake, captured = _async_returning(resp)
    client._stub.ListDecisionModels = fake  # type: ignore[attr-defined]

    out = await client.list_decision_models()

    assert isinstance(captured["req"], pb.ListDecisionModelsRequest)
    assert ("authorization", "Bearer tok") in list(captured["metadata"])
    assert out == {
        "default": "decide",
        "models": [
            {"name": "decide", "provider": "ollama-local", "model": "nimble",
             "limits": {"max_questions": 64, "min_options": 2, "max_options": 26}},
            {"name": "decide-deep", "provider": "ollama-local", "model": "nimble-xl",
             "limits": {"max_questions": 32, "min_options": 2, "max_options": 16}},
        ],
    }


# ---- errors: the decision code is on .reason ----
#
# Each value is a google.rpc.Status exactly as the SERVER serializes it into the
# grpc-status-details-bin trailer (captured from internal/api/grpc's
# decideStatus / mapRunnerErr), not bytes this file encodes: a test that built
# them with the reader's own idea of the format could not catch the reader
# being wrong about it.
_DETAILS = {
    # InvalidArgument, ErrorInfo{reason: model_not_allowed, domain: loomcycle,
    # metadata: {category: validation, is_retryable: false}}
    "model_not_allowed": (
        "0803124d4465636973696f6e3a206d6f64656c5f6e6f745f616c6c6f7765643a2022782220"
        "6973206e6f742061206d6f64656c20796f75206d61792061736b2028616c6c6f7765643a20"
        "646563696465291a790a28747970652e676f6f676c65617069732e636f6d2f676f6f676c65"
        "2e7270632e4572726f72496e666f124d0a116d6f64656c5f6e6f745f616c6c6f7765641209"
        "6c6f6f6d6379636c651a160a0863617465676f7279120a76616c69646174696f6e1a150a0c"
        "69735f726574727961626c65120566616c7365"
    ),
    # FailedPrecondition, ErrorInfo{reason: decision_not_configured}, no metadata
    "decision_not_configured": (
        "0809122b74686973206465706c6f796d656e74206465636c61726573206e6f206465636973"
        "696f6e206d6f64656c731a500a28747970652e676f6f676c65617069732e636f6d2f676f6f"
        "676c652e7270632e4572726f72496e666f12240a176465636973696f6e5f6e6f745f636f6e"
        "6669677572656412096c6f6f6d6379636c65"
    ),
    # ResourceExhausted, ErrorInfo{reason: token_limit_exceeded, …}
    "token_limit_exceeded": (
        "08081223746f6b656e5f6c696d69745f65786365656465643a2074656e616e742062756467"
        "65741a7a0a28747970652e676f6f676c65617069732e636f6d2f676f6f676c652e7270632e"
        "4572726f72496e666f124e0a14746f6b656e5f6c696d69745f657863656564656412096c6f"
        "6f6d6379636c651a140a0863617465676f72791208627573696e6573731a150a0c69735f72"
        "6574727961626c65120566616c7365"
    ),
    # ResourceExhausted with TWO details: ErrorInfo{reason: backpressure, …}
    # then a RetryInfo.
    "backpressure": (
        "0808120c6261636b70726573737572651a720a28747970652e676f6f676c65617069732e63"
        "6f6d2f676f6f676c652e7270632e4572726f72496e666f12460a0c6261636b707265737375"
        "726512096c6f6f6d6379636c651a150a0863617465676f727912097472616e7369656e741a"
        "140a0c69735f726574727961626c651204747275651a300a28747970652e676f6f676c6561"
        "7069732e636f6d2f676f6f676c652e7270632e5265747279496e666f12040a020805"
    ),
}


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


@pytest.mark.asyncio
async def test_decide_refusal_carries_the_decision_code_as_reason():
    client = _make_client()
    client._stub.Decide = _async_raising(  # type: ignore[attr-defined]
        _rpc_error(
            grpc.StatusCode.INVALID_ARGUMENT,
            'Decision: model_not_allowed: "x" is not a model you may ask (allowed: decide)',
            _DETAILS["model_not_allowed"],
        )
    )

    with pytest.raises(InvalidArgumentError) as ei:
        await client.decide({}, {"q": {"type": "noul", "instructions": "q"}}, model="x")

    assert ei.value.reason == "model_not_allowed"
    assert ei.value.code == grpc.StatusCode.INVALID_ARGUMENT
    assert "is not a model you may ask" in ei.value.message


@pytest.mark.asyncio
async def test_decide_over_a_hard_budget_is_backpressure_with_the_token_limit_reason():
    client = _make_client()
    client._stub.Decide = _async_raising(  # type: ignore[attr-defined]
        _rpc_error(
            grpc.StatusCode.RESOURCE_EXHAUSTED,
            "token_limit_exceeded: tenant budget",
            _DETAILS["token_limit_exceeded"],
        )
    )

    with pytest.raises(BackpressureError) as ei:
        await client.decide({}, {"q": {"type": "noul", "instructions": "q"}})

    # The class is the one plain backpressure raises; the reason is what tells
    # a budget (do not retry) from a full queue (retry).
    assert ei.value.reason == "token_limit_exceeded"


@pytest.mark.asyncio
async def test_list_decision_models_unconfigured_carries_its_reason():
    client = _make_client()
    client._stub.ListDecisionModels = _async_raising(  # type: ignore[attr-defined]
        _rpc_error(
            grpc.StatusCode.FAILED_PRECONDITION,
            "this deployment declares no decision models",
            _DETAILS["decision_not_configured"],
        )
    )

    with pytest.raises(LoomcycleError) as ei:
        await client.list_decision_models()

    assert ei.value.reason == "decision_not_configured"
    assert ei.value.code == grpc.StatusCode.FAILED_PRECONDITION


def test_reason_is_found_among_other_details_in_either_order():
    served = _DETAILS["backpressure"]
    err = _rpc_error(grpc.StatusCode.RESOURCE_EXHAUSTED, "backpressure", served)
    assert _error_reason(err) == "backpressure"

    # The same two details the server wrote, RetryInfo first: the order of a
    # repeated field is the server's to choose.
    retry_info = served[-100:]
    assert bytes.fromhex(retry_info).endswith(b"google.rpc.RetryInfo\x12\x04\x0a\x02\x08\x05")
    head, error_info = served[:32], served[32:-100]
    assert bytes.fromhex(head).endswith(b"backpressure")
    err = _rpc_error(grpc.StatusCode.RESOURCE_EXHAUSTED, "backpressure", head + retry_info + error_info)
    assert _error_reason(err) == "backpressure"


@pytest.mark.parametrize(
    "trailer",
    [
        "",  # no status details at all
        "0803",  # a Status with no details
        _DETAILS["model_not_allowed"][:-6],  # truncated mid-field
        "ffffffffffffffffffffff",  # not a protobuf message
        "1a0208",  # a detail that is not a well-formed Any
    ],
)
def test_an_error_without_readable_details_is_still_the_typed_error(trailer):
    err = _rpc_error(grpc.StatusCode.INVALID_ARGUMENT, "bad request", trailer)
    with pytest.raises(InvalidArgumentError) as ei:
        _raise_from_grpc(err)
    assert ei.value.reason is None
    assert ei.value.message == "bad request"
