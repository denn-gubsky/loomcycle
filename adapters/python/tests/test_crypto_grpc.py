"""The Crypto tool outside a run over gRPC (RFC EP, Python parity).

Same substrate-dispatch shape as CredentialDef: a JSON ``input`` dict serialised
to ``SubstrateRequest.input_json``, dispatched to the ``Crypto`` stub RPC,
decoded from ``SubstrateResponse``. A tool refusal (is_error=True) raises
SubstrateToolRefusedError; a spent token rate (RESOURCE_EXHAUSTED) raises
BackpressureError with reason ``rate_limited``. The server-side scope gate and
rate mapping are covered by internal/api/grpc/crypto_test.go.
"""

from __future__ import annotations

import json
from typing import Any

import grpc
import grpc.aio
import pytest

from loomcycle import BackpressureError, LoomcycleClient, SubstrateToolRefusedError
from loomcycle._generated import loomcycle_pb2 as pb


def _make_client() -> LoomcycleClient:
    channel = grpc.aio.insecure_channel("127.0.0.1:1")
    return LoomcycleClient(channel=channel)


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


# google.rpc.Status as the server sends it for a spent rate: ResourceExhausted,
# ErrorInfo{reason: rate_limited, domain: loomcycle, metadata: {category:
# transient, is_retryable: true}} and RetryInfo{retry_delay: 60s}.
_RATE_LIMITED_DETAILS = (
    "08081233726174655f6c696d697465643a207374617274696e672072756e7320746f6f20"
    "6f6674656e3b20726574727920696e203630731a720a28747970652e676f6f676c656170"
    "69732e636f6d2f676f6f676c652e7270632e4572726f72496e666f12460a0c726174655f"
    "6c696d6974656412096c6f6f6d6379636c651a150a0863617465676f727912097472616e"
    "7369656e741a140a0c69735f726574727961626c651204747275651a300a28747970652e"
    "676f6f676c65617069732e636f6d2f676f6f676c652e7270632e5265747279496e666f12"
    "040a02083c"
)


@pytest.mark.asyncio
async def test_crypto_dispatches_to_the_rpc():
    client = _make_client()
    out_json = json.dumps({"ciphertext": "lc1:abc", "key": "tenant:fields"}).encode()
    fake, captured = _async_returning(pb.SubstrateResponse(output_json=out_json, is_error=False))
    client._stub.Crypto = fake  # type: ignore[attr-defined]

    result = await client.crypto(
        {"op": "encrypt", "key": "tenant:fields", "plaintext": "hi", "tenant": "acme"}
    )
    assert result["ciphertext"] == "lc1:abc"
    # The input, operator-only "tenant" included, reaches the RPC as JSON bytes.
    assert isinstance(captured["req"].input_json, bytes)
    assert json.loads(captured["req"].input_json) == {
        "op": "encrypt",
        "key": "tenant:fields",
        "plaintext": "hi",
        "tenant": "acme",
    }


@pytest.mark.asyncio
async def test_crypto_refusal_raises_with_tool():
    client = _make_client()
    fake, _ = _async_returning(
        pb.SubstrateResponse(output_json=b"only an admin may use another tenant's keys", is_error=True)
    )
    client._stub.Crypto = fake  # type: ignore[attr-defined]

    with pytest.raises(SubstrateToolRefusedError) as ei:
        await client.crypto({"op": "random", "bytes": 16, "tenant": "other"})
    assert ei.value.tool == "Crypto"


@pytest.mark.asyncio
async def test_crypto_rate_limited_raises_retryable_backpressure():
    client = _make_client()
    trailing = grpc.aio.Metadata()
    trailing.add("grpc-status-details-bin", bytes.fromhex(_RATE_LIMITED_DETAILS))
    err = grpc.aio.AioRpcError(
        code=grpc.StatusCode.RESOURCE_EXHAUSTED,
        initial_metadata=grpc.aio.Metadata(),
        trailing_metadata=trailing,
        details="rate_limited: starting runs too often; retry in 60s",
        debug_error_string="",
    )
    client._stub.Crypto = _async_raising(err)  # type: ignore[attr-defined]

    with pytest.raises(BackpressureError) as ei:
        await client.crypto({"op": "random", "bytes": 16})
    assert ei.value.reason == "rate_limited"
    assert ei.value.retryable is True
