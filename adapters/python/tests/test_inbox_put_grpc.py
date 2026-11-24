"""RFC HE F1 — put_inbox_file over the client-streaming PutInboxFile RPC.

Stub-mock pattern (no live server). The server side is covered by
internal/api/grpc/inbox_put_test.go and internal/api/http/inbox_put_test.go.
"""

from __future__ import annotations

import grpc
import grpc.aio
import pytest

from loomcycle import LoomcycleClient
from loomcycle._generated import loomcycle_pb2 as pb


def _make_client() -> LoomcycleClient:
    channel = grpc.aio.insecure_channel("127.0.0.1:1")
    return LoomcycleClient(channel=channel)


@pytest.mark.asyncio
async def test_put_inbox_file_streams_the_bytes_after_a_first_message_naming_the_file():
    client = _make_client()
    captured: dict = {}

    async def fake(reqs, metadata=None):
        captured["reqs"] = list(reqs)
        return pb.PutInboxFileResponse(
            ref="volume:inbox:documents/Pasport.docx",
            name="Pasport.docx",
            bytes=10,
            sha256="ab",
            media_type="application/vnd.openxmlformats-officedocument.wordprocessingml.document",
        )

    client._stub.PutInboxFile = fake  # type: ignore[attr-defined]
    out = await client.put_inbox_file(
        "r_1", "Паспорт.docx", b"0123456789", media_type="application/x-test", chunk_size=4
    )

    reqs = captured["reqs"]
    assert [r.data for r in reqs] == [b"0123", b"4567", b"89"]
    assert (reqs[0].run_id, reqs[0].name, reqs[0].media_type) == ("r_1", "Паспорт.docx", "application/x-test")
    assert all(r.run_id == "" and r.name == "" for r in reqs[1:])
    assert out == {
        "ref": "volume:inbox:documents/Pasport.docx",
        "name": "Pasport.docx",
        "bytes": 10,
        "sha256": "ab",
        "media_type": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
    }


@pytest.mark.asyncio
async def test_put_inbox_file_sends_an_empty_file_as_one_message():
    client = _make_client()
    captured: dict = {}

    async def fake(reqs, metadata=None):
        captured["reqs"] = list(reqs)
        return pb.PutInboxFileResponse(ref="volume:inbox:documents/e.txt", name="e.txt")

    client._stub.PutInboxFile = fake  # type: ignore[attr-defined]
    await client.put_inbox_file("r_1", "e.txt", b"")
    assert len(captured["reqs"]) == 1 and captured["reqs"][0].data == b""


@pytest.mark.asyncio
async def test_put_inbox_file_refuses_a_non_positive_chunk_size():
    client = _make_client()
    with pytest.raises(ValueError):
        await client.put_inbox_file("r_1", "a.pdf", b"x", chunk_size=0)
