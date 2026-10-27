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
