import { describe, it, expect, vi } from "vitest";
import type { LoomcycleClient } from "@loomcycle/client";
import { dataLayerFromClient } from "./dataLayer";

// A recording stub for the two op-discriminated tool methods the explorer data
// layer uses. Each returns a marker so we can assert routing + arg mapping,
// including the RFC AS browse opts passed as the 2nd argument.
function stubClient() {
  const path = vi.fn().mockResolvedValue({ path: "/", entries: [] });
  const document = vi.fn().mockResolvedValue({ ok: true });
  const client = { path, document } as unknown as LoomcycleClient;
  return { client, path, document };
}

const BROWSE = { scopeId: "u1", tenant: "t1" };

describe("dataLayerFromClient — @loomcycle/client → wire mapping", () => {
  it("routes path ops to client.path with op + browse opts", async () => {
    const s = stubClient();
    const dl = dataLayerFromClient(s.client);
    await dl.pathLs("/docs", "user", true, BROWSE);
    expect(s.path).toHaveBeenCalledWith(
      { op: "ls", path: "/docs", scope: "user", recursive: true },
      BROWSE,
    );
  });

  it("pathMkdir / pathMv / pathRm map args + carry browse", async () => {
    const s = stubClient();
    const dl = dataLayerFromClient(s.client);
    await dl.pathMkdir("/docs/launches", "user", BROWSE);
    await dl.pathMv("/a", "/b", "agent", BROWSE);
    await dl.pathRm("/a", "tenant", true, BROWSE);
    expect(s.path).toHaveBeenNthCalledWith(
      1,
      { op: "mkdir", path: "/docs/launches", scope: "user" },
      BROWSE,
    );
    expect(s.path).toHaveBeenNthCalledWith(
      2,
      { op: "mv", path: "/a", to: "/b", scope: "agent" },
      BROWSE,
    );
    expect(s.path).toHaveBeenNthCalledWith(
      3,
      { op: "rm", path: "/a", scope: "tenant", recursive: true },
      BROWSE,
    );
  });

  it("omits browse (undefined 2nd arg) when none is given → own subject", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).pathLs("/", "user", false);
    expect(s.path).toHaveBeenCalledWith(
      { op: "ls", path: "/", scope: "user", recursive: false },
      undefined,
    );
  });

  it("documentCreate / documentDelete route to client.document", async () => {
    const s = stubClient();
    const dl = dataLayerFromClient(s.client);
    await dl.documentCreate("Launch Plan", "/docs/launch", "user", BROWSE);
    await dl.documentDelete("doc1", "agent", BROWSE);
    expect(s.document).toHaveBeenNthCalledWith(
      1,
      { op: "create_document", title: "Launch Plan", path: "/docs/launch", scope: "user" },
      BROWSE,
    );
    expect(s.document).toHaveBeenNthCalledWith(
      2,
      { op: "delete_document", id: "doc1", scope: "agent" },
      BROWSE,
    );
  });

  it("documentQueryChunks caps limit at 1000 and carries document_id", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).documentQueryChunks("doc1", "user", BROWSE);
    expect(s.document).toHaveBeenCalledWith(
      { op: "query_chunks", document_id: "doc1", scope: "user", limit: 1000 },
      BROWSE,
    );
  });

  it("documentGetChunk sends the chunk id", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).documentGetChunk("chunk1", "user", BROWSE);
    expect(s.document).toHaveBeenCalledWith(
      { op: "get_chunk", id: "chunk1", scope: "user" },
      BROWSE,
    );
  });

  it("documentUpdateChunk carries revision + spreads the patch (optimistic concurrency)", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).documentUpdateChunk(
      "chunk1",
      7,
      { body: "b", title: "t", type: "section", status: "draft", fields: { k: "v" } },
      "user",
      BROWSE,
    );
    expect(s.document).toHaveBeenCalledWith(
      {
        op: "update_chunk",
        id: "chunk1",
        revision: 7,
        scope: "user",
        body: "b",
        title: "t",
        type: "section",
        status: "draft",
        fields: { k: "v" },
      },
      BROWSE,
    );
  });

  it("documentExportMd sends include_metadata", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).documentExportMd("doc1", "user", true, BROWSE);
    expect(s.document).toHaveBeenCalledWith(
      { op: "export_md", document_id: "doc1", scope: "user", include_metadata: true },
      BROWSE,
    );
  });
});
