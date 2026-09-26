import { describe, it, expect, vi } from "vitest";
import type { LoomcycleClient } from "@loomcycle/client";
import { dataLayerFromClient, dataLayerFromConnection, hookEntriesFromNames } from "./dataLayer";
import { forkOverlay, sourceHookOverlay } from "./hookDefOverlay";

describe("HookDefs through the data layer", () => {
  it("routes the hookdef kind to the client's hookDef method", async () => {
    const hookDef = vi.fn().mockResolvedValue({ def_id: "h" });
    const dl = dataLayerFromClient({ hookDef } as unknown as LoomcycleClient);
    await dl.createDef("hookdef", "gate", { event: "pre" }, true);
    expect(hookDef).toHaveBeenCalledWith({ op: "create", name: "gate", overlay: { event: "pre" }, promote: true });
  });

  // The client cannot list HookDefs, so a client-only data layer offers no list
  // — and the Library leaves the tab out rather than showing it empty.
  it("only a connection-built data layer can list them", () => {
    expect(dataLayerFromClient({} as LoomcycleClient).listHooks).toBeUndefined();
    const dl = dataLayerFromConnection({ baseUrl: "" }, {} as LoomcycleClient);
    expect(dl.listHooks).toBeTypeOf("function");
  });

  it("lists them from the names endpoint with the connection's token and fetch", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ names: [{ name: "gate", version_count: 2, active_def_id: "hdf_1", live_version_count: 2 }] })),
    );
    const dl = dataLayerFromConnection({ baseUrl: "https://lc.example", token: "t", fetch: fetchMock }, {} as LoomcycleClient);
    const r = await dl.listHooks!();
    expect(fetchMock.mock.calls[0]![0]).toBe("https://lc.example/v1/_hookdef/names");
    expect((fetchMock.mock.calls[0]![1] as RequestInit).headers).toMatchObject({ Authorization: "Bearer t" });
    expect(r.entries).toEqual([
      expect.objectContaining({ name: "gate", source: "dynamic-only", in_substrate: true, version_count: 2, active_def_id: "hdf_1" }),
    ]);
  });

  it("a failed list is an error, not an empty tab", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response("no", { status: 403 }));
    const dl = dataLayerFromConnection({ baseUrl: "", fetch: fetchMock }, {} as LoomcycleClient);
    await expect(dl.listHooks!()).rejects.toThrow(/403/);
  });

  // An admin sees every tenant's names: one name, listed once.
  it("a name held by two tenants is one entry with both counted", () => {
    const e = hookEntriesFromNames([
      { name: "gate", version_count: 2, live_version_count: 1 },
      { name: "gate", version_count: 3, live_version_count: 3 },
      { name: "log", version_count: 1 },
    ]);
    expect(e.map((x) => [x.name, x.version_count, x.live_version_count])).toEqual([["gate", 5, 4], ["log", 1, undefined]]);
    expect(hookEntriesFromNames(null)).toEqual([]);
  });
});

describe("a HookDef fork's overlay", () => {
  it("drops the server-set keys when opening a row", () => {
    expect(sourceHookOverlay({ def_id: "x", version: 2, event: "pre", body: { kind: "http", url: "u" } }))
      .toEqual({ event: "pre", body: { kind: "http", url: "u" } });
  });

  // A fork merges field by field, so a field the operator removed must go out
  // as null — otherwise the parent's value silently comes back.
  it("sends a removed field as null", () => {
    const source = { event: "pre", match: { tools: ["WebFetch"] }, fail_mode: "closed" };
    expect(forkOverlay(source, { event: "pre", fail_mode: "open" })).toEqual({ event: "pre", fail_mode: "open", match: null });
  });
});

describe("the agent editor's hook check", async () => {
  const { hooksProblem } = await import("../components/LibraryEditModal");
  it("names the first refusable entry and where it is", () => {
    expect(hooksProblem({})).toBeNull();
    expect(hooksProblem({ hooks: { agent_stop: ["cite@3"] }, tool_hooks: { WebFetch: { pre: [{ name: "gate", url: "https://h" }] } } })).toBeNull();
    expect(hooksProblem({ hooks: { run_end: ["bad@0"] } })).toMatch(/^hooks · run_end: .*positive/);
    expect(hooksProblem({ tool_hooks: { WebFetch: { pre: [{ name: "gate", url: "ftp://h" }] } } })).toMatch(/^tool hooks · WebFetch pre: .*http/);
  });
});
