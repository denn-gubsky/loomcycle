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
  const backfillEmbeddings = vi
    .fn()
    .mockResolvedValue({ scope: "user", scope_id: "bob", dry_run: true, candidates: 0 });
  const purgeStaleEmbeddings = vi
    .fn()
    .mockResolvedValue({ scope: "user", scope_id: "bob", dry_run: true, scanned: 0, stale: 0, purged: 0, truncated: false });
  // P4b — the unified search + Document (list_facts / get_chunk / get_edges) legs.
  const memorySearch = vi
    .fn()
    .mockResolvedValue({ scope: "user", scope_id: "alice", entries: [], query_embedding_dim: 0, truncated: false });
  const document = vi.fn().mockResolvedValue({ facts: [], count: 0, truncated: false });
  // Scope-id suggestion sources.
  const whoami = vi
    .fn()
    .mockResolvedValue({ subject: "me", tenant_id: "acme", scopes: [], is_admin: true, legacy: false });
  const listUsers = vi.fn().mockResolvedValue({ users: [{ user_id: "alice" }, { user_id: "bob" }] });
  const listLibraryAgents = vi.fn().mockResolvedValue({
    entries: [
      { name: "static-agent", in_static: true },
      { name: "dynamic-active", in_static: false, active_def_id: "d2" },
      { name: "dynamic-retired", in_static: false },
    ],
  });
  const client = {
    listMemoryScopes,
    listMemoryScopeIDs,
    listMemoryEntries,
    getMemoryEntry,
    setMemoryEntry,
    deleteMemoryEntry,
    memoryEmbedStats,
    reembedMemory,
    backfillEmbeddings,
    purgeStaleEmbeddings,
    memorySearch,
    document,
    whoami,
    listUsers,
    listLibraryAgents,
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
    backfillEmbeddings,
    purgeStaleEmbeddings,
    memorySearch,
    document,
    whoami,
    listUsers,
    listLibraryAgents,
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
    // The focus object is forwarded on every browse method now; with no tenant
    // set its field is undefined, which the client drops, so the request is
    // byte-identical to before it existed.
    expect(s.listMemoryScopeIDs).toHaveBeenCalledWith("user", { tenant: undefined });
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
      tenant: undefined,
    });
  });

  it("listEntries forwards an omitted opts as the focus alone (client applies defaults)", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).listEntries("user", "alice");
    expect(s.listMemoryEntries).toHaveBeenCalledWith("user", "alice", { tenant: undefined });
  });

  it("binds a tenant focus onto every browse method", async () => {
    // The focus is a property of the CONSOLE SESSION — which tenant's workspace
    // the operator is looking at — so it binds when the layer is built rather
    // than travelling through twenty call sites that never vary it.
    const s = stubClient();
    const dl = dataLayerFromClient(s.client, { tenant: "acme" });
    await dl.listScopeIDs("user");
    await dl.listEntries("user", "alice");
    await dl.getEntry("user", "alice", "tone");
    await dl.setEntry("user", "alice", "tone", { value: "warm" });
    await dl.deleteEntry("user", "alice", "tone");
    expect(s.listMemoryScopeIDs).toHaveBeenCalledWith("user", { tenant: "acme" });
    expect(s.listMemoryEntries).toHaveBeenCalledWith("user", "alice", { tenant: "acme" });
    expect(s.getMemoryEntry).toHaveBeenCalledWith("user", "alice", "tone", { tenant: "acme" });
    expect(s.setMemoryEntry).toHaveBeenCalledWith("user", "alice", "tone", {
      value: "warm",
      tenant: "acme",
    });
    expect(s.deleteMemoryEntry).toHaveBeenCalledWith("user", "alice", "tone", { tenant: "acme" });
  });

  it("never sends a focus on listScopes", async () => {
    // That route answers "what KINDS of scope exist" — a constant set, not any
    // tenant's data. Sending a tenant would imply the answer varies by tenant.
    const s = stubClient();
    await dataLayerFromClient(s.client, { tenant: "acme" }).listScopes();
    expect(s.listMemoryScopes).toHaveBeenCalledWith();
  });

  it("treats a blank focus as no focus", async () => {
    // The topbar switcher is empty until an admin types a tenant, and empty there
    // means "my own", not a tenant named "". Normalising here also stops the
    // layer being rebuilt on every keystroke through useResolvedDataLayer's memo.
    const s = stubClient();
    await dataLayerFromClient(s.client, { tenant: "   " }).listScopeIDs("user");
    expect(s.listMemoryScopeIDs).toHaveBeenCalledWith("user", { tenant: undefined });
  });

  it("getEntry passes scope + scope_id + key", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).getEntry("agent", "researcher", "note");
    expect(s.getMemoryEntry).toHaveBeenCalledWith("agent", "researcher", "note", {
      tenant: undefined,
    });
  });

  it("setEntry passes the identifier tuple + the SetMemoryEntryOptions", async () => {
    const s = stubClient();
    const input = { value: { a: 1 }, embed: true, ttl_seconds: 3600 };
    await dataLayerFromClient(s.client).setEntry("user", "alice", "company-policy", input);
    expect(s.setMemoryEntry).toHaveBeenCalledWith("user", "alice", "company-policy", {
      ...input,
      tenant: undefined,
    });
  });

  it("deleteEntry passes scope + scope_id + key", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).deleteEntry("user", "alice", "stale");
    expect(s.deleteMemoryEntry).toHaveBeenCalledWith("user", "alice", "stale", {
      tenant: undefined,
    });
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

  it("listFacts sends include_refuted ONLY when asked, alongside include_retired", async () => {
    // The two are independent axes — retired means a later fact corrected this one,
    // refused means a verdict rejected it — so one must never imply the other on the
    // wire. An earlier edit here dropped include_retired entirely while adding its
    // neighbour, which this pins.
    const s = stubClient();
    await dataLayerFromClient(s.client).listFacts("user", {
      includeRetired: true,
      includeRefuted: true,
    });
    expect(s.document).toHaveBeenCalledWith(
      { op: "list_facts", scope: "user", claims_only: true, include_retired: true, include_refuted: true },
      undefined,
    );

    const q = stubClient();
    await dataLayerFromClient(q.client).listFacts("user", { includeRefuted: true });
    expect(q.document).toHaveBeenCalledWith(
      { op: "list_facts", scope: "user", claims_only: true, include_refuted: true },
      undefined,
    );
  });

  it("backfillEmbeddings and purgeStaleEmbeddings pass the dry-run flag through in BOTH directions", async () => {
    // Asserted both ways per op, deliberately. Checking only the `true` case cannot
    // detect a layer that hardcodes true, and checking only `false` cannot detect one
    // that hardcodes false — and the two failures are not equally bad: a commit that
    // silently became a plan does nothing, while a PLAN that silently became a commit
    // changes data with no preview. Neither is caught by a one-directional assertion.
    for (const dryRun of [true, false]) {
      const s = stubClient();
      await dataLayerFromClient(s.client).backfillEmbeddings("user", "bob", { dryRun });
      expect(s.backfillEmbeddings).toHaveBeenCalledWith("user", "bob", { dryRun });

      const q = stubClient();
      await dataLayerFromClient(q.client).purgeStaleEmbeddings("user", "bob", { dryRun });
      expect(q.purgeStaleEmbeddings).toHaveBeenCalledWith("user", "bob", { dryRun });
    }
  });

  it("remember sends the statement ONCE — the server writes the span from it", async () => {
    // A client that also sent source_quote could point a claim at text that does not
    // contain it. The self-citation is the server's guarantee, not a client convention.
    const s = stubClient();
    await dataLayerFromClient(s.client).remember("user", "The user deploys on Fridays.", {
      scopeId: "bob",
    });
    expect(s.document).toHaveBeenCalledWith(
      { op: "remember", scope: "user", text: "The user deploys on Fridays." },
      { scopeId: "bob" },
    );
  });

  it("remember forwards a type/subject only when given", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).remember("user", "Ada runs platform.", {
      type: "person",
      subject: "Ada",
    });
    expect(s.document).toHaveBeenCalledWith(
      { op: "remember", scope: "user", text: "Ada runs platform.", type: "person", subject: "Ada" },
      undefined,
    );
  });

  it("judgeFact sends the verdict WORD and a reason, and nothing else", async () => {
    // The server maps the word to a confidence, stamps the time, and stamps who judged
    // from the call's own context. A client that sent any of those would be claiming
    // authority it does not have.
    const s = stubClient();
    await dataLayerFromClient(s.client).judgeFact("user", "c1", "unsupported", "no city named", {
      scopeId: "bob",
    });
    expect(s.document).toHaveBeenCalledWith(
      { op: "judge_fact", scope: "user", id: "c1", verdict: "unsupported", reason: "no city named" },
      { scopeId: "bob" },
    );
  });

  it("verificationStats asks for the scope's coverage", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).verificationStats("user", { scopeId: "bob" });
    expect(s.document).toHaveBeenCalledWith(
      { op: "verification_stats", scope: "user" },
      { scopeId: "bob" },
    );
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
        // Sent on every listing — this surface reads facts to a person, and an entity
        // identity node is a name with no body. See the data layer for why.
        claims_only: true,
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
    expect(s.document).toHaveBeenCalledWith(
      { op: "list_facts", scope: "agent", claims_only: true },
      undefined,
    );
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

  // ---- scope-id suggestions (RFC BW UI) ----
  it("whoami maps the client's tenant_id → tenant (and passes subject through)", async () => {
    const s = stubClient();
    const who = await dataLayerFromClient(s.client).whoami!();
    expect(who).toEqual({ subject: "me", tenant: "acme" });
  });

  it("listUserIds maps users[].user_id", async () => {
    const s = stubClient();
    const ids = await dataLayerFromClient(s.client).listUserIds!();
    expect(ids).toEqual(["alice", "bob"]);
  });

  it("listAgentNames returns ONLY active agents (in_static || active_def_id), by name", async () => {
    const s = stubClient();
    const names = await dataLayerFromClient(s.client).listAgentNames!();
    // static-agent (in_static) + dynamic-active (has active_def_id) survive;
    // dynamic-retired (neither) is dropped.
    expect(names).toEqual(["static-agent", "dynamic-active"]);
  });
});
