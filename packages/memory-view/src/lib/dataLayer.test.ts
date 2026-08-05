import { describe, it, expect, vi } from "vitest";
import type { LoomcycleClient, MemorySearchInput } from "@loomcycle/client";
import { dataLayerFromClient } from "./dataLayer";

// A recording stub for the eight off-run memory admin methods the data layer
// uses. Each returns a benign marker so we can assert routing + arg mapping —
// the value doesn't matter, only that the right client method was called with
// the right positional args.
function stubClient() {
  const listMemoryScopes = vi.fn().mockResolvedValue({ scopes: [] });
  const listMemoryScopeIDs = vi.fn().mockResolvedValue({ scope: "user", scope_ids: [] });
  const listMemoryEntries = vi
    .fn()
    .mockResolvedValue({ scope: "user", scope_id: "alice", entries: [], truncated: false });
  const getMemoryEntry = vi.fn().mockResolvedValue({ scope: "user", scope_id: "alice", entry: {} });
  const setMemoryEntry = vi
    .fn()
    .mockResolvedValue({ scope: "user", scope_id: "alice", key: "k", embedded: false });
  const deleteMemoryEntry = vi.fn().mockResolvedValue(undefined);
  const memoryEmbedStats = vi
    .fn()
    .mockResolvedValue({ scope: "user", models: [], total_embedding_bytes: 0 });
  const reembedMemory = vi.fn().mockResolvedValue({ scope: "user", scope_id: "alice", dry_run: true });
  // P4b — the unified search + Document (list_facts / get_chunk / get_edges) legs.
  const memorySearch = vi
    .fn()
    .mockResolvedValue({ scope: "user", scope_id: "alice", entries: [], query_embedding_dim: 0, truncated: false });
  const document = vi.fn().mockResolvedValue({ facts: [], count: 0, truncated: false });
  const client = {
    listMemoryScopes,
    listMemoryScopeIDs,
    listMemoryEntries,
    getMemoryEntry,
    setMemoryEntry,
    deleteMemoryEntry,
    memoryEmbedStats,
    reembedMemory,
    memorySearch,
    document,
  } as unknown as LoomcycleClient;
  return {
    client,
    listMemoryScopes,
    listMemoryScopeIDs,
    listMemoryEntries,
    getMemoryEntry,
    setMemoryEntry,
    deleteMemoryEntry,
    memoryEmbedStats,
    reembedMemory,
    memorySearch,
    document,
  };
}

describe("dataLayerFromClient — @loomcycle/client → memory wire mapping", () => {
  it("listScopes routes to client.listMemoryScopes with no args", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).listScopes();
    expect(s.listMemoryScopes).toHaveBeenCalledWith();
  });

  it("listScopeIDs passes the scope", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).listScopeIDs("user");
    expect(s.listMemoryScopeIDs).toHaveBeenCalledWith("user");
  });

  it("listEntries passes scope + scope_id + the prefix/limit opts", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).listEntries("user", "alice", {
      prefix: "policy",
      limit: 200,
    });
    expect(s.listMemoryEntries).toHaveBeenCalledWith("user", "alice", {
      prefix: "policy",
      limit: 200,
    });
  });

  it("listEntries forwards an omitted opts as undefined (client applies defaults)", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).listEntries("user", "alice");
    expect(s.listMemoryEntries).toHaveBeenCalledWith("user", "alice", undefined);
  });

  it("getEntry passes scope + scope_id + key", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).getEntry("agent", "researcher", "note");
    expect(s.getMemoryEntry).toHaveBeenCalledWith("agent", "researcher", "note");
  });

  it("setEntry passes the identifier tuple + the SetMemoryEntryOptions", async () => {
    const s = stubClient();
    const input = { value: { a: 1 }, embed: true, ttl_seconds: 3600 };
    await dataLayerFromClient(s.client).setEntry("user", "alice", "company-policy", input);
    expect(s.setMemoryEntry).toHaveBeenCalledWith("user", "alice", "company-policy", input);
  });

  it("deleteEntry passes scope + scope_id + key", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).deleteEntry("user", "alice", "stale");
    expect(s.deleteMemoryEntry).toHaveBeenCalledWith("user", "alice", "stale");
  });

  it("embedStats passes the scope", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).embedStats("user");
    expect(s.memoryEmbedStats).toHaveBeenCalledWith("user");
  });

  it("reembed passes scope + scope_id + the {dryRun,limit} opts (dry run plans, never writes)", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).reembed("user", "alice", { dryRun: true });
    expect(s.reembedMemory).toHaveBeenCalledWith("user", "alice", { dryRun: true });
  });

  it("reembed carries dryRun:false to commit", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).reembed("user", "alice", { dryRun: false, limit: 500 });
    expect(s.reembedMemory).toHaveBeenCalledWith("user", "alice", { dryRun: false, limit: 500 });
  });

  // ---- P4b: unified search + entity/fact tier ----

  it("search forwards the MemorySearchInput (incl. the RFC BW sources selector) unchanged", async () => {
    const s = stubClient();
    const input: MemorySearchInput = { query: "rate limits", scope: "user", scopeId: "alice", topK: 5, sources: ["facts"] };
    await dataLayerFromClient(s.client).search(input);
    expect(s.memorySearch).toHaveBeenCalledWith(input);
  });

  it("listFacts maps every filter to list_facts snake_case + threads the scopeId override", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).listFacts("user", {
      type: "person",
      class: "evidential",
      documentId: "doc-1",
      includeRetired: true,
      asOf: 1700000000000000000,
      limit: 20,
      scopeId: "bob",
    });
    expect(s.document).toHaveBeenCalledWith(
      {
        op: "list_facts",
        scope: "user",
        type: "person",
        class: "evidential",
        document_id: "doc-1",
        include_retired: true,
        as_of: 1700000000000000000,
        limit: 20,
      },
      { scopeId: "bob" },
    );
  });

  it("listFacts omits include_retired when falsy + sends no browse opts without a scopeId", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).listFacts("agent");
    expect(s.document).toHaveBeenCalledWith({ op: "list_facts", scope: "agent" }, undefined);
  });

  it("getChunk addresses the chunk by id + threads the scopeId override", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).getChunk("user", "chunk-1", { scopeId: "bob" });
    expect(s.document).toHaveBeenCalledWith(
      { op: "get_chunk", scope: "user", id: "chunk-1" },
      { scopeId: "bob" },
    );
  });

  it("getChunk sends no browse opts when no scopeId is given", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).getChunk("agent", "chunk-1");
    expect(s.document).toHaveBeenCalledWith({ op: "get_chunk", scope: "agent", id: "chunk-1" }, undefined);
  });

  it("getEdges is DOCUMENT-scoped — it sends document_id, not a chunk id", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).getEdges("user", "doc-1", { scopeId: "bob" });
    expect(s.document).toHaveBeenCalledWith(
      { op: "get_edges", scope: "user", document_id: "doc-1" },
      { scopeId: "bob" },
    );
  });
});
