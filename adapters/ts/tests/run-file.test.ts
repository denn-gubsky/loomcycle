/**
 * The File twin over HTTP and the inbox download:
 *   - runFile()              → POST /v1/runs/{run_id}/file
 *   - downloadRunInboxFile() → GET (or HEAD) /v1/runs/{run_id}/inbox/{path}
 */

import { describe, it, expect } from "vitest";
import { makeClient, jsonResponse, errorResponse } from "./helpers.js";
import { NotFoundError, SubstrateToolRefusedError } from "../src/errors.js";

describe("runFile", () => {
  it("POSTs the op to the run's file route, without run_id in the body", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ ref: "volume:inbox:documents/a.pdf", sha256: "ab", origin: "http" }),
    ]);
    const res = await client.runFile<{ sha256: string }>("run_1", { op: "stat", path: "volume:inbox:documents/a.pdf" });
    expect(res.sha256).toBe("ab");
    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/runs/run_1/file");
    expect(call[1]!.method).toBe("POST");
    expect(JSON.parse(call[1]!.body as string)).toEqual({ op: "stat", path: "volume:inbox:documents/a.pdf" });
  });

  it("sends import's source and headings as the tool takes them", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ document_id: "d1", chunks: 3 })]);
    await client.runFile("run_1", {
      op: "import",
      path: "volume:inbox:extracted/NBU-064.md",
      source: { url: "https://tenant.sharepoint.com/sites/board/reg.pdf", kind: "sharepoint" },
      headings: [
        { line: 593, level: 2, as: "heading" },
        { line: 594, level: 3, as: "marker", title: "1. Це Положення розроблено…" },
        { line: 610, level: 3, as: "level" },
      ],
    });
    const body = JSON.parse(fetchMock.mock.calls[0]![1]!.body as string);
    expect(body.headings).toEqual([
      { line: 593, level: 2, as: "heading" },
      { line: 594, level: 3, as: "marker", title: "1. Це Положення розроблено…" },
      { line: 610, level: 3, as: "level" },
    ]);
    expect(body.source.kind).toBe("sharepoint");
  });

  it("raises SubstrateToolRefusedError on the 422 tool refusal", async () => {
    const { client } = makeClient([
      jsonResponse(
        { code: "tool_refused", error: "file: only the run's owner or an admin may rm in its inbox", tool: "File" },
        422,
      ),
    ]);
    await expect(client.runFile("run_1", { op: "rm", path: "documents/a.pdf" })).rejects.toBeInstanceOf(
      SubstrateToolRefusedError,
    );
  });
});

describe("downloadRunInboxFile", () => {
  const served = (body: string | null, status = 200) => () =>
    new Response(body, {
      status,
      headers: {
        "Content-Type": "application/pdf",
        "Content-Disposition": "attachment; filename*=UTF-8''board%20report.pdf",
        "X-Loomcycle-SHA256": "deadbeef",
        "X-Loomcycle-Origin": "mcp:ms365/get-attachment",
      },
    });

  it("GETs the file by its path, each segment encoded, and returns bytes and headers", async () => {
    const { client, fetchMock } = makeClient([served("%PDF-1.7")]);
    const res = await client.downloadRunInboxFile("run_1", "volume:inbox:documents/board report.pdf");
    expect(new TextDecoder().decode(res.bytes)).toBe("%PDF-1.7");
    expect(res).toMatchObject({
      status: 200,
      contentType: "application/pdf",
      filename: "board report.pdf",
      sha256: "deadbeef",
      origin: "mcp:ms365/get-attachment",
    });
    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/runs/run_1/inbox/documents/board%20report.pdf");
    expect(call[1]!.method).toBe("GET");
  });

  it("sends Range and HEAD when asked", async () => {
    const { client, fetchMock } = makeClient([served("PDF", 206), served(null)]);
    const part = await client.downloadRunInboxFile("run_1", "documents/a.pdf", { range: "bytes=1-3" });
    expect(part.status).toBe(206);
    expect((fetchMock.mock.calls[0]![1]!.headers as Record<string, string>).Range).toBe("bytes=1-3");
    const head = await client.downloadRunInboxFile("run_1", "documents/a.pdf", { head: true });
    expect(fetchMock.mock.calls[1]![1]!.method).toBe("HEAD");
    expect(head.bytes.length).toBe(0);
    expect(head.sha256).toBe("deadbeef");
  });

  it("raises NotFoundError for a missing, finished or another tenant's run", async () => {
    const { client } = makeClient([errorResponse(404, '{"code":"not_found","error":"file: run run_x not found"}')]);
    await expect(client.downloadRunInboxFile("run_x", "documents/a.pdf")).rejects.toBeInstanceOf(NotFoundError);
  });
});

describe("putInboxFile", () => {
  it("PUTs the raw bytes to the run's inbox under the encoded name, typed", async () => {
    const kept = {
      ref: "volume:inbox:documents/Pasport.docx",
      name: "Pasport.docx",
      bytes: 4,
      sha256: "ab",
      media_type: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
    };
    const { client, fetchMock } = makeClient([jsonResponse(kept, 201)]);
    const bytes = new Uint8Array([0x50, 0x4b, 0x03, 0x04]);
    const res = await client.putInboxFile("run_1", "Паспорт рішення.docx", bytes, {
      contentType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
    });
    expect(res).toEqual(kept);
    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe(
      `http://test-loomcycle:8787/v1/runs/run_1/inbox/${encodeURIComponent("Паспорт рішення.docx")}`,
    );
    expect(call[1]!.method).toBe("PUT");
    expect((call[1]!.headers as Record<string, string>)["Content-Type"]).toBe(
      "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
    );
    expect(call[1]!.body).toBe(bytes);
  });

  it("encodes a slash in the name, so the name stays one path segment", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ ref: "r", name: "n", bytes: 1, sha256: "s", media_type: "t" }, 201),
    ]);
    await client.putInboxFile("run_1", "../a.pdf", new Uint8Array([1]));
    expect(fetchMock.mock.calls[0]![0]).toBe("http://test-loomcycle:8787/v1/runs/run_1/inbox/..%2Fa.pdf");
  });

  it("raises with the status and code of a refusal", async () => {
    const { client } = makeClient([
      errorResponse(413, '{"code":"client_put_max_bytes","error":"file not kept: over the per-file limit"}'),
      errorResponse(404, '{"code":"not_found","error":"file: run run_x not found"}'),
    ]);
    await expect(client.putInboxFile("run_1", "a.pdf", new Uint8Array([1]))).rejects.toMatchObject({
      status: 413,
      code: "client_put_max_bytes",
    });
    await expect(client.putInboxFile("run_x", "a.pdf", new Uint8Array([1]))).rejects.toBeInstanceOf(NotFoundError);
  });
});
