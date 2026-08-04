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

// Tenant scope must reach the wire on every document read the viewer makes.
//
// This is the regression that made every tenant-scope document open EMPTY. The
// Path tree browses agent|user|tenant, but DocScope was "agent" | "user" — written
// when Documents refused tenant — so PathExplorer narrowed the browse scope to hand
// it to the viewer, "tenant" had nowhere to go, and it silently became "user". The
// document listed in the tree and its chunk query then asked the wrong store, which
// answers correctly with nothing.
//
// Nothing failed. No error, no empty-state distinction: a document with no chunks
// and a document you are looking for in the wrong scope render identically. So this
// asserts the scope on the wire rather than the absence of an exception.
describe("tenant document scope reaches the wire", () => {
  const reads: Array<[string, (dl: ReturnType<typeof dataLayerFromClient>) => Promise<unknown>]> = [
    ["get_document", (dl) => dl.documentGet("d1", "tenant")],
    ["query_chunks", (dl) => dl.documentQueryChunks("d1", "tenant")],
    ["get_chunk", (dl) => dl.documentGetChunk("c1", "tenant")],
    ["export_md", (dl) => dl.documentExportMd("d1", "tenant", false)],
  ];

  for (const [op, call] of reads) {
    it(`${op} carries scope=tenant`, async () => {
      const s = stubClient();
      await call(dataLayerFromClient(s.client));
      expect(s.document).toHaveBeenCalled();
      const sent = s.document.mock.calls[0][0] as { op: string; scope?: string };
      expect(sent.op).toBe(op);
      // The assertion that matters: NOT "user". A silent fold to user scope is the
      // bug, and it looks exactly like a document that has no chunks.
      expect(sent.scope).toBe("tenant");
    });
  }

  it("writes carry it too — a tenant document must be editable, not just readable", async () => {
    const s = stubClient();
    const dl = dataLayerFromClient(s.client);
    await dl.documentUpdateChunk("c1", 3, { body: "x" }, "tenant");
    const sent = s.document.mock.calls[0][0] as { scope?: string };
    expect(sent.scope).toBe("tenant");
  });
});

// RFC BS document-viewer ops: connections (backlinks / related / unlinked
// mentions), per-chunk history (history / get_version / diff), and the canvas
// export. Locks the op name + arg mapping + browse passthrough, same as the ops
// above — these route through the `as unknown as DocumentToolInput` passthrough,
// so the wire shape is what the assertion guards.
describe("RFC BS connections / history / canvas ops → wire mapping", () => {
  it("documentBacklinks sends the chunk id", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).documentBacklinks("c1", "user", BROWSE);
    expect(s.document).toHaveBeenCalledWith({ op: "backlinks", id: "c1", scope: "user" }, BROWSE);
  });

  it("documentRelated carries the limit when given", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).documentRelated("c1", "user", BROWSE, 8);
    expect(s.document).toHaveBeenCalledWith(
      { op: "related", id: "c1", scope: "user", limit: 8 },
      BROWSE,
    );
  });

  it("documentRelated omits the limit when unset", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).documentRelated("c1", "user");
    expect(s.document).toHaveBeenCalledWith({ op: "related", id: "c1", scope: "user" }, undefined);
  });

  it("documentUnlinkedMentions carries id + limit", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).documentUnlinkedMentions("c1", "tenant", BROWSE, 20);
    expect(s.document).toHaveBeenCalledWith(
      { op: "unlinked_mentions", id: "c1", scope: "tenant", limit: 20 },
      BROWSE,
    );
  });

  it("documentHistory carries id + limit", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).documentHistory("c1", "user", BROWSE, 50);
    expect(s.document).toHaveBeenCalledWith(
      { op: "history", id: "c1", scope: "user", limit: 50 },
      BROWSE,
    );
  });

  it("documentGetVersion carries the revision", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).documentGetVersion("c1", 3, "user", BROWSE);
    expect(s.document).toHaveBeenCalledWith(
      { op: "get_version", id: "c1", revision: 3, scope: "user" },
      BROWSE,
    );
  });

  it("documentDiff carries from_revision + to_revision", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).documentDiff("c1", 2, 5, "user", BROWSE);
    expect(s.document).toHaveBeenCalledWith(
      { op: "diff", id: "c1", from_revision: 2, to_revision: 5, scope: "user" },
      BROWSE,
    );
  });

  it("documentExportCanvas carries the document_id", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).documentExportCanvas("d1", "user", BROWSE);
    expect(s.document).toHaveBeenCalledWith(
      { op: "export_canvas", document_id: "d1", scope: "user" },
      BROWSE,
    );
  });
});
