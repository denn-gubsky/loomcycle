"""v0.8.18 — LoomcycleClient pause/resume/state + snapshot lifecycle tests.

These tests monkey-patch the client's gRPC stub methods so we can
assert on the request shape + response translation without spinning
up a real server. The server-side wire path is covered by
internal/api/grpc/pause_snapshot_test.go.
"""

from __future__ import annotations

from typing import Any

import grpc.aio
import pytest

from loomcycle import LoomcycleClient
from loomcycle._generated import loomcycle_pb2 as pb


def _make_client() -> LoomcycleClient:
    """Build a client without dialing. The fake channel is never
    used because we replace stub method bindings directly."""
    channel = grpc.aio.insecure_channel("127.0.0.1:1")
    return LoomcycleClient(channel=channel)


def _async_returning(result: Any):
    """Build an async function (matching the gRPC unary RPC signature)
    that records its single positional request arg + returns the
    supplied result."""
    captured: dict = {}

    async def fn(req, metadata=None):
        captured["req"] = req
        captured["metadata"] = metadata
        return result

    return fn, captured


# ---- Pause / Resume / State ----


@pytest.mark.asyncio
async def test_pause_runtime_round_trips_fields():
    client = _make_client()
    fake, _captured = _async_returning(pb.PauseRuntimeResponse(
        status="paused",
        duration_ms=42,
        force_cancelled_count=1,
        paused_runs_count=2,
        warnings=["flaky"],
    ))
    client._stub.PauseRuntime = fake  # type: ignore[attr-defined]

    result = await client.pause_runtime(timeout_ms=5000)
    assert result["status"] == "paused"
    assert result["duration_ms"] == 42
    assert result["force_cancelled_count"] == 1
    assert result["paused_runs_count"] == 2
    assert result["warnings"] == ["flaky"]
    assert _captured["req"].timeout_ms == 5000


@pytest.mark.asyncio
async def test_resume_runtime_round_trips_fields():
    client = _make_client()
    fake, _ = _async_returning(pb.ResumeRuntimeResponse(
        status="running",
        resumed_run_count=3,
    ))
    client._stub.ResumeRuntime = fake  # type: ignore[attr-defined]

    result = await client.resume_runtime()
    assert result["status"] == "running"
    assert result["resumed_run_count"] == 3
    assert result["warnings"] == []


@pytest.mark.asyncio
async def test_get_runtime_state_round_trips_fields():
    client = _make_client()
    fake, _ = _async_returning(pb.RuntimeStateResponse(
        status="paused",
        paused_run_count=4,
        snapshots_count=7,
    ))
    client._stub.GetRuntimeState = fake  # type: ignore[attr-defined]

    result = await client.get_runtime_state()
    assert result["status"] == "paused"
    assert result["paused_run_count"] == 4
    assert result["snapshots_count"] == 7


# ---- Snapshot lifecycle ----


@pytest.mark.asyncio
async def test_create_snapshot_round_trips_fields():
    client = _make_client()
    fake, captured = _async_returning(pb.SnapshotDescriptor(
        snapshot_id="snap_abc",
        size_bytes=1024,
        description="test",
        format_version="1",
    ))
    client._stub.CreateSnapshot = fake  # type: ignore[attr-defined]

    result = await client.create_snapshot(description="test", max_bytes=12345)
    assert result["snapshot_id"] == "snap_abc"
    assert result["size_bytes"] == 1024
    assert result["description"] == "test"
    assert captured["req"].description == "test"
    assert captured["req"].max_bytes == 12345


@pytest.mark.asyncio
async def test_list_snapshots_round_trips_each_descriptor():
    client = _make_client()
    resp = pb.ListSnapshotsResponse()
    resp.snapshots.add(snapshot_id="snap_a", size_bytes=10)
    resp.snapshots.add(snapshot_id="snap_b", size_bytes=20)
    fake, _ = _async_returning(resp)
    client._stub.ListSnapshots = fake  # type: ignore[attr-defined]

    result = await client.list_snapshots()
    assert len(result) == 2
    assert result[0]["snapshot_id"] == "snap_a"
    assert result[1]["snapshot_id"] == "snap_b"


@pytest.mark.asyncio
async def test_get_snapshot_returns_json_content_bytes():
    client = _make_client()
    envelope_bytes = b'{"schema_version":1,"sections":{}}'
    fake, captured = _async_returning(pb.SnapshotEnvelope(
        snapshot_id="snap_xyz",
        description="t",
        format_version="1",
        size_bytes=len(envelope_bytes),
        json_content=envelope_bytes,
    ))
    client._stub.GetSnapshot = fake  # type: ignore[attr-defined]

    result = await client.get_snapshot("snap_xyz")
    assert result["snapshot_id"] == "snap_xyz"
    assert result["json_content"] == envelope_bytes
    assert captured["req"].snapshot_id == "snap_xyz"


@pytest.mark.asyncio
async def test_export_snapshot_returns_raw_json_bytes():
    client = _make_client()
    envelope_bytes = b'{"schema_version":1,"sections":{}}'
    fake, captured = _async_returning(pb.ExportSnapshotResponse(
        snapshot_id="snap_xyz",
        size_bytes=len(envelope_bytes),
        raw_json=envelope_bytes,
    ))
    client._stub.ExportSnapshot = fake  # type: ignore[attr-defined]

    result = await client.export_snapshot("snap_xyz")
    assert result["raw_json"] == envelope_bytes
    assert captured["req"].snapshot_id == "snap_xyz"


@pytest.mark.asyncio
async def test_restore_snapshot_by_id():
    client = _make_client()
    fake, captured = _async_returning(pb.RestoreSnapshotResponse(
        memory_restored=3,
        paused_runs_restored=1,
    ))
    client._stub.RestoreSnapshot = fake  # type: ignore[attr-defined]

    result = await client.restore_snapshot(snapshot_id="snap_xyz")
    assert result["memory_restored"] == 3
    assert result["paused_runs_restored"] == 1
    assert captured["req"].snapshot_id == "snap_xyz"
    assert captured["req"].raw_json == b""


@pytest.mark.asyncio
async def test_restore_snapshot_by_raw_json():
    client = _make_client()
    fake, captured = _async_returning(pb.RestoreSnapshotResponse())
    client._stub.RestoreSnapshot = fake  # type: ignore[attr-defined]

    envelope = b'{"schema_version":1,"sections":{}}'
    await client.restore_snapshot(raw_json=envelope)
    assert captured["req"].raw_json == envelope
    assert captured["req"].snapshot_id == ""


@pytest.mark.asyncio
async def test_restore_snapshot_requires_one_of_id_or_raw_json():
    client = _make_client()
    with pytest.raises(Exception):
        await client.restore_snapshot()
    with pytest.raises(Exception):
        await client.restore_snapshot(snapshot_id="x", raw_json=b"y")


@pytest.mark.asyncio
async def test_delete_snapshot_returns_deleted_true():
    client = _make_client()
    fake, captured = _async_returning(pb.DeleteSnapshotResponse(deleted=True))
    client._stub.DeleteSnapshot = fake  # type: ignore[attr-defined]

    result = await client.delete_snapshot("snap_xyz")
    assert result is True
    assert captured["req"].snapshot_id == "snap_xyz"
