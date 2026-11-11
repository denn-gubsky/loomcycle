"""A tenant's own trace export: get, set, delete and test over the
TenantTelemetry RPC. These tests pin the request each method builds and the
shape it returns."""

import grpc
import pytest

from loomcycle import LoomcycleClient
from loomcycle._generated import loomcycle_pb2 as pb


class _Stub:
    def __init__(self, resp: pb.TenantTelemetryResponse) -> None:
        self.resp = resp
        self.last: pb.TenantTelemetryRequest = None  # type: ignore[assignment]

    async def TenantTelemetry(self, req, metadata=None):
        self.last = req
        return self.resp


def _client(stub: _Stub) -> LoomcycleClient:
    client = LoomcycleClient(channel=grpc.aio.insecure_channel("127.0.0.1:1"))
    client._stub = stub  # type: ignore[assignment]
    return client


_POLICY = pb.TenantTelemetryPolicy(
    endpoint="https://otel.acme.example",
    protocol="http/protobuf",
    signals=["traces"],
    sampler_ratio=0.25,
    updated_at="2026-11-10T09:00:00Z",
    updated_by="ops",
)


@pytest.mark.asyncio
async def test_get_sends_op_and_tenant_and_returns_the_result_dict():
    stub = _Stub(
        pb.TenantTelemetryResponse(
            tenant="acme",
            policy=_POLICY,
            status=pb.TenantTelemetryStatus(replica="r1", exporting=True, exported_spans=12, export_failures=1),
        )
    )
    got = await _client(stub).get_telemetry(tenant="globex")
    assert (stub.last.op, stub.last.tenant, stub.last.HasField("policy")) == ("get", "globex", False)
    assert got == {
        "tenant": "acme",
        "policy": {
            "endpoint": "https://otel.acme.example",
            "protocol": "http/protobuf",
            "headers_credential": "",
            "signals": ["traces"],
            "sampler_ratio": 0.25,
            "updated_at": "2026-11-10T09:00:00Z",
            "updated_by": "ops",
        },
        "status": {
            "replica": "r1",
            "exporting": True,
            "exported_spans": 12,
            "dropped_spans": 0,
            "export_failures": 1,
            "last_success_at": "",
            "last_error": "",
            "last_error_at": "",
        },
        "deleted": False,
        "test": None,
    }


@pytest.mark.asyncio
async def test_get_without_a_policy_returns_none():
    stub = _Stub(pb.TenantTelemetryResponse(tenant="acme"))
    got = await _client(stub).get_telemetry()
    assert stub.last.tenant == ""
    assert (got["policy"], got["status"], got["test"]) == (None, None, None)


@pytest.mark.asyncio
async def test_set_leaves_sampler_ratio_unset_unless_given_and_sends_zero():
    stub = _Stub(pb.TenantTelemetryResponse(tenant="acme", policy=_POLICY))
    client = _client(stub)
    got = await client.set_telemetry("https://otel.acme.example")
    assert stub.last.op == "set"
    assert stub.last.policy.endpoint == "https://otel.acme.example"
    assert not stub.last.policy.HasField("sampler_ratio")
    assert list(stub.last.policy.signals) == []
    assert got["policy"]["endpoint"] == "https://otel.acme.example"

    await client.set_telemetry(
        "https://otel.globex.example",
        protocol="grpc",
        headers_credential="otel-headers",
        signals=["traces"],
        sampler_ratio=0.0,
        content_capture="off",
        tenant="globex",
    )
    p = stub.last.policy
    assert stub.last.tenant == "globex"
    assert (p.protocol, p.headers_credential, list(p.signals), p.content_capture) == (
        "grpc",
        "otel-headers",
        ["traces"],
        "off",
    )
    # 0.0 is a real ratio (send nothing), not "unset".
    assert p.HasField("sampler_ratio") and p.sampler_ratio == 0.0


@pytest.mark.asyncio
async def test_delete_reports_whether_there_was_one():
    stub = _Stub(pb.TenantTelemetryResponse(tenant="acme", deleted=True))
    got = await _client(stub).delete_telemetry()
    assert (stub.last.op, got["deleted"], got["policy"]) == ("delete", True, None)
    stub.resp = pb.TenantTelemetryResponse(tenant="globex")
    got = await _client(stub).delete_telemetry(tenant="globex")
    assert (stub.last.tenant, got["deleted"]) == ("globex", False)


@pytest.mark.asyncio
async def test_test_returns_the_outcome():
    stub = _Stub(
        pb.TenantTelemetryResponse(
            tenant="acme",
            policy=_POLICY,
            test=pb.TenantTelemetryTest(ok=False, error="connection refused", duration_ms=31),
        )
    )
    got = await _client(stub).test_telemetry(tenant="acme")
    assert (stub.last.op, stub.last.tenant) == ("test", "acme")
    assert got["test"] == {"ok": False, "error": "connection refused", "duration_ms": 31}
