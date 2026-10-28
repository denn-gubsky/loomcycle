"""The usage report over gRPC: a report grouped by purpose returns each row's
purpose and the audio seconds its speech-to-text calls transcribed, and the
group_by sent is the caller's."""

import grpc
import pytest

from loomcycle import LoomcycleClient
from loomcycle._generated import loomcycle_pb2 as pb


class _Stub:
    def __init__(self):
        self.req = None

    async def UsageReport(self, req, metadata=None):
        self.req = req
        return pb.UsageReportResponse(
            group_by=list(req.group_by),
            rows=[
                pb.UsageAggregate(purpose="ocr", input_tokens=300, cost=0.3, currency="USD", call_count=1),
                pb.UsageAggregate(purpose="stt", cost=0.5, currency="USD", call_count=1, audio_seconds=75.0),
            ],
        )


@pytest.mark.asyncio
async def test_a_report_by_purpose_returns_purpose_and_audio_seconds():
    stub = _Stub()
    client = LoomcycleClient(channel=grpc.aio.insecure_channel("127.0.0.1:1"))
    client._stub = stub  # type: ignore[assignment]
    got = await client.usage_report(group_by=["purpose"])
    assert list(stub.req.group_by) == ["purpose"]
    assert got["group_by"] == ["purpose"]
    ocr, stt = got["rows"]
    assert (ocr["purpose"], ocr["input_tokens"], ocr["audio_seconds"]) == ("ocr", 300, 0.0)
    assert (stt["purpose"], stt["call_count"], stt["audio_seconds"]) == ("stt", 1, 75.0)


class _ChartStub:
    def __init__(self):
        self.req = None

    async def UsageReport(self, req, metadata=None):
        self.req = req
        big = pb.UsageAggregate(model="big", period="2026-10-01T00:00:00Z", cost=9.0, call_count=1)
        rest = pb.UsageAggregate(period="2026-10-01T00:00:00Z", cost=4.0, call_count=2, other=True, input_chars=40)
        setattr(rest, "class", "background")
        return pb.UsageReportResponse(
            group_by=list(req.group_by), bucket=req.bucket, detail_since="2026-09-28T00:00:00Z", rows=[big, rest],
        )


@pytest.mark.asyncio
async def test_a_chart_report_sends_bucket_top_and_filters_and_reads_the_new_fields():
    stub = _ChartStub()
    client = LoomcycleClient(channel=grpc.aio.insecure_channel("127.0.0.1:1"))
    client._stub = stub  # type: ignore[assignment]
    got = await client.usage_report(
        group_by=["model"], bucket="day", top=1, top_by="tokens",
        filters={"class": ["background"], "tier": ["high", "middle"], "user": []},
    )
    assert (stub.req.bucket, stub.req.top, stub.req.top_by) == ("day", 1, "tokens")
    assert {f.dimension: list(f.values) for f in stub.req.filters} == {"class": ["background"], "tier": ["high", "middle"]}
    assert (got["bucket"], got["detail_since"]) == ("day", "2026-09-28T00:00:00Z")
    big, rest = got["rows"]
    assert (big["model"], big["period"], big["other"]) == ("big", "2026-10-01T00:00:00Z", False)
    assert (rest["other"], rest["class"], rest["input_chars"]) == (True, "background", 40)
