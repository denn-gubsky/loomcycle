"""calc — the Calc RPC (RFC GY P4b).

Stub-mock pattern (no live server). The server-side path is covered by
internal/api/grpc/calc_test.go.
"""

from __future__ import annotations

import decimal
import json
from typing import Any

import grpc
import grpc.aio
import pytest

from loomcycle import (
    BackpressureError,
    CalcError,
    InvalidArgumentError,
    LoomcycleClient,
    LoomcycleError,
)
from loomcycle._generated import loomcycle_pb2 as pb
from loomcycle.client import _error_info, _error_reason


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


# Each value is a google.rpc.Status exactly as the SERVER serializes it into the
# grpc-status-details-bin trailer, captured from internal/api/grpc's Calc over
# the real Calc tool — not bytes this file encodes, so a reader wrong about the
# format cannot pass.
_DETAILS = {
    # {"expr":"2 ** 3"}: InvalidArgument "`**` is not an operator",
    # ErrorInfo{reason: syntax, domain: loomcycle, metadata: {pos: 2,
    # category: validation, is_retryable: false, hint: "power is `^`, e.g. `2^10`"}}
    "syntax": (
        "08031217602a2a60206973206e6f7420616e206f70657261746f721a9b010a28747970652e"
        "676f6f676c65617069732e636f6d2f676f6f676c652e7270632e4572726f72496e666f126f"
        "0a0673796e74617812096c6f6f6d6379636c651a080a03706f731201321a160a0863617465"
        "676f7279120a76616c69646174696f6e1a150a0c69735f726574727961626c65120566616c"
        "73651a210a0468696e741219706f77657220697320605e602c20652e672e2060325e313060"
    ),
    # {"expr":"sqrt(-1)"}: FailedPrecondition "sqrt(-1) is not a real number",
    # ErrorInfo{reason: domain, metadata: {category: business, is_retryable:
    # false, hint: "there are no complex numbers in this version", pos: 4}}
    "domain": (
        "0809121d73717274282d3129206973206e6f742061207265616c206e756d6265721aad010a"
        "28747970652e676f6f676c65617069732e636f6d2f676f6f676c652e7270632e4572726f72"
        "496e666f1280010a06646f6d61696e12096c6f6f6d6379636c651a140a0863617465676f72"
        "791208627573696e6573731a150a0c69735f726574727961626c65120566616c73651a340a"
        "0468696e74122c746865726520617265206e6f20636f6d706c6578206e756d626572732069"
        "6e20746869732076657273696f6e1a080a03706f73120134"
    ),
    # A spent run rate (from tests/test_crypto_grpc.py): ResourceExhausted,
    # ErrorInfo{reason: rate_limited, metadata: {category: transient,
    # is_retryable: true}} and a RetryInfo.
    "rate_limited": (
        "08081233726174655f6c696d697465643a207374617274696e672072756e7320746f6f20"
        "6f6674656e3b20726574727920696e203630731a720a28747970652e676f6f676c656170"
        "69732e636f6d2f676f6f676c652e7270632e4572726f72496e666f12460a0c726174655f"
        "6c696d6974656412096c6f6f6d6379636c651a150a0863617465676f727912097472616e"
        "7369656e741a140a0c69735f726574727961626c651204747275651a300a28747970652e"
        "676f6f676c65617069732e636f6d2f676f6f676c652e7270632e5265747279496e666f12"
        "040a02083c"
    ),
}


# ---- request encoding and the result ----

@pytest.mark.asyncio
async def test_calc_sends_the_input_as_json_and_returns_the_decoded_result():
    client = _make_client()
    result = {"result": 0.33333, "type": "number", "exact": True, "fraction": "1/3", "text": "0.33333"}
    fake, captured = _async_returning(pb.CalcResponse(result_json=json.dumps(result).encode()))
    client._stub.Calc = fake  # type: ignore[attr-defined]

    out = await client.calc("x / 3", vars={"x": "1"}, digits=5, seed=7, format="csv")

    assert out == result
    req = captured["req"]
    assert isinstance(req, pb.CalcRequest)
    assert json.loads(req.input_json) == {"expr": "x / 3", "vars": {"x": "1"}, "digits": 5, "seed": 7, "format": "csv"}
    assert ("authorization", "Bearer tok") in list(captured["metadata"])


@pytest.mark.asyncio
async def test_calc_sends_only_expr_when_no_option_is_given():
    client = _make_client()
    fake, captured = _async_returning(pb.CalcResponse(result_json=b'{"result":0.3,"type":"number","exact":true,"text":"0.3"}'))
    client._stub.Calc = fake  # type: ignore[attr-defined]

    await client.calc("0.1 + 0.2")

    assert json.loads(captured["req"].input_json) == {"expr": "0.1 + 0.2"}


@pytest.mark.asyncio
async def test_calc_sends_a_decimal_var_as_its_exact_numeric_string():
    client = _make_client()
    fake, captured = _async_returning(pb.CalcResponse(result_json=b"{}"))
    client._stub.Calc = fake  # type: ignore[attr-defined]

    await client.calc("x * 3", vars={"x": decimal.Decimal("0.1"), "xs": [decimal.Decimal("1.10"), 2]})

    assert json.loads(captured["req"].input_json)["vars"] == {"x": "0.1", "xs": ["1.10", 2]}


@pytest.mark.asyncio
async def test_calc_keeps_a_large_integer_result_exact():
    client = _make_client()
    raw = b'{"result":1180591620717411303424,"type":"number","exact":true,"text":"1180591620717411303424"}'
    fake, _ = _async_returning(pb.CalcResponse(result_json=raw))
    client._stub.Calc = fake  # type: ignore[attr-defined]

    out = await client.calc("2^70")

    assert out["result"] == 2**70


@pytest.mark.asyncio
async def test_calc_refuses_an_unserializable_var_before_any_call():
    client = _make_client()
    called = False

    async def fake(req, metadata=None):
        nonlocal called
        called = True

    client._stub.Calc = fake  # type: ignore[attr-defined]

    with pytest.raises(InvalidArgumentError):
        await client.calc("x", vars={"x": object()})
    with pytest.raises(InvalidArgumentError):
        await client.calc("x", vars={"x": float("nan")})
    assert not called


# ---- refusals ----

@pytest.mark.asyncio
async def test_calc_validation_refusal_raises_calc_error_with_code_hint_pos_and_category():
    client = _make_client()
    client._stub.Calc = _async_raising(  # type: ignore[attr-defined]
        _rpc_error(grpc.StatusCode.INVALID_ARGUMENT, "`**` is not an operator", _DETAILS["syntax"])
    )

    with pytest.raises(CalcError) as ei:
        await client.calc("2 ** 3")

    e = ei.value
    assert isinstance(e, LoomcycleError)
    assert e.reason == "syntax"
    assert e.message == e.detail == "`**` is not an operator"
    assert e.hint == "power is `^`, e.g. `2^10`"
    assert e.pos == 2
    assert e.category == "validation"
    assert e.code == grpc.StatusCode.INVALID_ARGUMENT
    assert e.retryable is False


@pytest.mark.asyncio
async def test_calc_business_refusal_raises_calc_error():
    client = _make_client()
    client._stub.Calc = _async_raising(  # type: ignore[attr-defined]
        _rpc_error(grpc.StatusCode.FAILED_PRECONDITION, "sqrt(-1) is not a real number", _DETAILS["domain"])
    )

    with pytest.raises(CalcError) as ei:
        await client.calc("sqrt(-1)")

    e = ei.value
    assert (e.reason, e.category, e.pos) == ("domain", "business", 4)
    assert e.hint == "there are no complex numbers in this version"
    assert e.retryable is False


@pytest.mark.asyncio
async def test_calc_spent_run_rate_raises_retryable_backpressure_not_calc_error():
    # Its ErrorInfo carries a category too; the code says it is not the tool's.
    client = _make_client()
    client._stub.Calc = _async_raising(  # type: ignore[attr-defined]
        _rpc_error(grpc.StatusCode.RESOURCE_EXHAUSTED, "rate_limited", _DETAILS["rate_limited"])
    )

    with pytest.raises(BackpressureError) as ei:
        await client.calc("1")

    assert not isinstance(ei.value, CalcError)
    assert ei.value.reason == "rate_limited"
    assert ei.value.retryable is True


@pytest.mark.asyncio
async def test_calc_refusal_before_the_tool_ran_maps_like_any_rpc():
    client = _make_client()
    client._stub.Calc = _async_raising(  # type: ignore[attr-defined]
        _rpc_error(grpc.StatusCode.INVALID_ARGUMENT, "Calc: empty input_json")
    )

    with pytest.raises(InvalidArgumentError) as ei:
        await client.calc("")

    assert not isinstance(ei.value, CalcError)


# ---- the ErrorInfo reader ----

def test_error_info_reads_reason_and_metadata():
    err = _rpc_error(grpc.StatusCode.INVALID_ARGUMENT, "x", _DETAILS["syntax"])
    assert _error_info(err) == (
        "syntax",
        {"pos": "2", "category": "validation", "is_retryable": "false", "hint": "power is `^`, e.g. `2^10`"},
    )
    assert _error_reason(err) == "syntax"


def test_error_info_is_none_without_details_or_on_garbage():
    assert _error_info(_rpc_error(grpc.StatusCode.INTERNAL, "x")) is None
    assert _error_info(_rpc_error(grpc.StatusCode.INTERNAL, "x", "ffff")) is None
