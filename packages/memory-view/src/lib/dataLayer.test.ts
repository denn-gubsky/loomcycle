import { describe, it, expect, vi } from "vitest";
import type { LoomcycleClient } from "@loomcycle/client";
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
  const client = {
    listMemoryScopes,
    listMemoryScopeIDs,
    listMemoryEntries,
    getMemoryEntry,
    setMemoryEntry,
    deleteMemoryEntry,
    memoryEmbedStats,
    reembedMemory,
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
});
