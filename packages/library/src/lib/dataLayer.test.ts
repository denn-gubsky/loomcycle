import { describe, it, expect, vi } from "vitest";
import type { LoomcycleClient } from "@loomcycle/client";
import { dataLayerFromClient } from "./dataLayer";

// A recording stub for the three op-discriminated substrate methods + the three
// library-list methods. Each returns a marker so we can assert routing.
function stubClient() {
  const agentDef = vi.fn().mockResolvedValue({ def_id: "a" });
  const skillDef = vi.fn().mockResolvedValue({ def_id: "s" });
  const mcpServerDef = vi.fn().mockResolvedValue({ def_id: "m" });
  const listLibraryAgents = vi.fn().mockResolvedValue({ entries: ["agents"] });
  const listLibrarySkills = vi.fn().mockResolvedValue({ entries: ["skills"] });
  const listLibraryMcpServers = vi.fn().mockResolvedValue({ entries: ["mcp"] });
  const client = {
    agentDef,
    skillDef,
    mcpServerDef,
    listLibraryAgents,
    listLibrarySkills,
    listLibraryMcpServers,
  } as unknown as LoomcycleClient;
  return { client, agentDef, skillDef, mcpServerDef, listLibraryAgents, listLibrarySkills, listLibraryMcpServers };
}

describe("dataLayerFromClient — @loomcycle/client → wire mapping", () => {
  it("routes each kind to its op-discriminated client method", async () => {
    const s = stubClient();
    const dl = dataLayerFromClient(s.client);
    await dl.listDefVersionsByName("agentdef", "n");
    await dl.listDefVersionsByName("skilldef", "n");
    await dl.listDefVersionsByName("mcpserverdef", "n");
    expect(s.agentDef).toHaveBeenCalledWith({ op: "list", name: "n" });
    expect(s.skillDef).toHaveBeenCalledWith({ op: "list", name: "n" });
    expect(s.mcpServerDef).toHaveBeenCalledWith({ op: "list", name: "n" });
  });

  it("createDef nests the overlay + carries promote (not spread)", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).createDef("agentdef", "x", { model: "m" }, true);
    expect(s.agentDef).toHaveBeenCalledWith({
      op: "create",
      name: "x",
      overlay: { model: "m" },
      promote: true,
    });
  });

  it("forkDef includes parent_def_id only when given", async () => {
    const s = stubClient();
    const dl = dataLayerFromClient(s.client);
    await dl.forkDef("skilldef", "x", { body: "b" }, false, "parent1");
    expect(s.skillDef).toHaveBeenCalledWith({
      op: "fork",
      name: "x",
      overlay: { body: "b" },
      promote: false,
      parent_def_id: "parent1",
    });
    s.skillDef.mockClear();
    await dl.forkDef("skilldef", "x", {}, true);
    expect(s.skillDef).toHaveBeenCalledWith({ op: "fork", name: "x", overlay: {}, promote: true });
    expect(s.skillDef.mock.calls[0][0]).not.toHaveProperty("parent_def_id");
  });

  it("promote / retire send def_id (+ retired:true)", async () => {
    const s = stubClient();
    const dl = dataLayerFromClient(s.client);
    await dl.promoteDef("agentdef", "d1");
    await dl.retireDef("mcpserverdef", "d2");
    expect(s.agentDef).toHaveBeenCalledWith({ op: "promote", def_id: "d1" });
    expect(s.mcpServerDef).toHaveBeenCalledWith({ op: "retire", def_id: "d2", retired: true });
  });

  it("rediscover is mcpserverdef-only", async () => {
    const s = stubClient();
    await dataLayerFromClient(s.client).rediscoverMcpServerDef("srv");
    expect(s.mcpServerDef).toHaveBeenCalledWith({ op: "rediscover", name: "srv" });
  });

  it("list* route to the matching listLibrary* method", async () => {
    const s = stubClient();
    const dl = dataLayerFromClient(s.client);
    expect(await dl.listAgents()).toEqual({ entries: ["agents"] });
    expect(await dl.listSkills()).toEqual({ entries: ["skills"] });
    expect(await dl.listMcpServers()).toEqual({ entries: ["mcp"] });
  });

  it("throws loudly on an unsupported kind rather than silently no-op'ing", () => {
    const s = stubClient();
    // webhookdef is a valid SubstrateKind but not a Library-managed one. The
    // dispatch guard throws synchronously (a programming error, not a runtime
    // path — the Library only ever passes agentdef/skilldef/mcpserverdef).
    expect(() =>
      dataLayerFromClient(s.client).createDef("webhookdef" as never, "x", {}, true),
    ).toThrow(/not supported/);
  });
});
