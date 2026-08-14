import { describe, it, expect, vi } from "vitest";
import type { LoomcycleClient } from "@loomcycle/client";
import { dataLayerFromClient } from "./dataLayer";
import type { ViewLayout, ViewQuery } from "../types";

// A recording stub for the single `document` meta-tool the data layer routes
// through. It returns a superset shape so both the create_document leg (needs
// root_chunk_id) and the query legs narrow cleanly; we assert only routing +
// the exact wire body per call.
function stubClient() {
  const document = vi.fn().mockResolvedValue({
    document_id: "doc-1",
    root_chunk_id: "root-1",
    title: "t",
    documents: [],
    chunks: [],
    types: [],
    id: "root-1",
    revision: 1,
  });
  const client = { document } as unknown as LoomcycleClient;
  return { client, document };
}

const LAYOUT: ViewLayout = { kind: "kanban", groupBy: "status" };

describe("dataLayerFromClient — @loomcycle/client → Document wire mapping", () => {
  it("queryDocuments sends only the set axes (camelCase → snake_case)", async () => {
    const s = stubClient();
    const q: ViewQuery = { source: "documents", type: "rfc", status: "draft", tag: "area/ui", underPath: "/loomcycle/rfcs", limit: 50 };
    await dataLayerFromClient(s.client).queryDocuments("user", q);
    expect(s.document).toHaveBeenCalledWith({
      op: "query_documents",
      scope: "user",
      type: "rfc",
      status: "draft",
      tag: "area/ui",
      under_path: "/loomcycle/rfcs",
      limit: 50,
    });
  });

  it("queryDocuments omits unset axes", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).queryDocuments("tenant", { source: "documents", type: "task" });
    expect(s.document).toHaveBeenCalledWith({ op: "query_documents", scope: "tenant", type: "task" });
  });

  it("queryChunks carries document_id + tag_prefix", async () => {
    const s = stubClient();
    const q: ViewQuery = { source: "chunks", documentId: "d9", type: "step", tagPrefix: "area", limit: 100 };
    await dataLayerFromClient(s.client).queryChunks("user", q);
    expect(s.document).toHaveBeenCalledWith({
      op: "query_chunks",
      scope: "user",
      document_id: "d9",
      type: "step",
      tag_prefix: "area",
      limit: 100,
    });
  });

  it("getChunk addresses by id", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).getChunk("user", "c1");
    expect(s.document).toHaveBeenCalledWith({ op: "get_chunk", scope: "user", id: "c1" });
  });

  it("listTypes passes the scope", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).listTypes("tenant");
    expect(s.document).toHaveBeenCalledWith({ op: "list_types", scope: "tenant" });
  });

  it("listViews filters query_documents to type:view", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).listViews("user");
    expect(s.document).toHaveBeenCalledWith({ op: "query_documents", scope: "user", type: "view" });
  });

  it("saveView creates a type=view Document then seeds root fields at revision 1", async () => {
    const s = stubClient();
    const query: ViewQuery = { source: "documents", type: "rfc" };
    const out = await dataLayerFromClient(s.client).saveView("user", {
      title: "RFCs by status",
      path: "/loomcycle/views/rfcs",
      query,
      layout: LAYOUT,
    });
    expect(s.document).toHaveBeenNthCalledWith(1, {
      op: "create_document",
      scope: "user",
      title: "RFCs by status",
      type: "view",
      path: "/loomcycle/views/rfcs",
    });
    expect(s.document).toHaveBeenNthCalledWith(2, {
      op: "update_chunk",
      scope: "user",
      id: "root-1",
      revision: 1,
      fields: { query, layout: LAYOUT },
    });
    expect(out).toEqual({ documentId: "doc-1", rootChunkId: "root-1" });
  });

  it("updateView patches only the given fields on the root chunk", async () => {
    const s = stubClient();
    const query: ViewQuery = { source: "documents", status: "done" };
    await dataLayerFromClient(s.client).updateView("user", "root-1", 4, { query });
    expect(s.document).toHaveBeenCalledWith({
      op: "update_chunk",
      scope: "user",
      id: "root-1",
      revision: 4,
      fields: { query },
    });
  });

  it("deleteView removes the whole view Document", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).deleteView("tenant", "doc-1");
    expect(s.document).toHaveBeenCalledWith({ op: "delete_document", scope: "tenant", id: "doc-1" });
  });

  it("setStatus writes a chunk status at its revision (kanban move)", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).setStatus("user", "c7", 2, "in-progress");
    expect(s.document).toHaveBeenCalledWith({
      op: "update_chunk",
      scope: "user",
      id: "c7",
      revision: 2,
      status: "in-progress",
    });
  });
});
