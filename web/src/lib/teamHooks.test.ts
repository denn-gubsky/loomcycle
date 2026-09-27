import { describe, expect, it } from "vitest";
import { WALK, hookTargets, keepWalkHookRemoval, readTeamHooks, writeTeamHooks } from "./teamHooks";

const graph = {
  entry: "review",
  hooks: { run_end: ["log"] },
  states: [
    { state: "review", handler: { kind: "agent", agent: "reviewer", tool_hooks: { WebFetch: { pre: ["gate"] } } } },
    { state: "fan", handler: { kind: "parallel", agents: ["a", "b"], consolidator: "merge" } },
    { state: "vars", handler: { kind: "vars", set: {} } },
    { state: "done", handler: { kind: "terminal" } },
  ],
  transitions: [],
};

describe("a team's hook targets", () => {
  // Hooks on a state that starts no run would name gates that never fire, and
  // the runtime refuses them — so those states are not offered.
  it("are the walk and the states that start runs", () => {
    expect(hookTargets(graph).map((t) => t.id)).toEqual([WALK, "review", "fan"]);
    expect(hookTargets(null).map((t) => t.id)).toEqual([WALK]);
  });
});

describe("reading and writing a team's hooks", () => {
  it("reads the walk's and a state's", () => {
    expect(readTeamHooks(graph, WALK)).toEqual({ hooks: { run_end: ["log"] } });
    expect(readTeamHooks(graph, "review").tool_hooks).toEqual({ WebFetch: { pre: ["gate"] } });
    expect(readTeamHooks(graph, "nope")).toEqual({ hooks: undefined, tool_hooks: undefined });
  });

  it("writes one key at one target and leaves the rest, without touching the input", () => {
    const next = writeTeamHooks(graph, "fan", "hooks", { agent_stop: ["cite"] }) as typeof graph;
    expect(next.states[1]!.handler).toMatchObject({ kind: "parallel", hooks: { agent_stop: ["cite"] } });
    expect(next.states[0]).toBe(graph.states[0]);
    expect(next.hooks).toEqual({ run_end: ["log"] });
    expect((graph.states[1]!.handler as Record<string, unknown>).hooks).toBeUndefined();
  });

  it("undefined removes the key", () => {
    const next = writeTeamHooks(graph, WALK, "hooks", undefined) as Record<string, unknown>;
    expect("hooks" in next).toBe(false);
    const st = (writeTeamHooks(graph, "review", "tool_hooks", undefined) as typeof graph).states[0]!.handler;
    expect("tool_hooks" in st).toBe(false);
  });

  it("a walk takes no tool hooks", () => {
    expect(writeTeamHooks(graph, WALK, "tool_hooks", { X: { pre: ["g"] } })).toBe(graph);
  });
});

// A fork keeps the parent's walk hooks unless the graph clears them with {}.
describe("a team fork's walk hooks", () => {
  it("are sent as a clear when the operator removed them", () => {
    const edited = writeTeamHooks(graph, WALK, "hooks", undefined);
    expect(keepWalkHookRemoval(graph, edited)).toMatchObject({ hooks: {} });
    expect(keepWalkHookRemoval(graph, { ...graph, hooks: null })).toMatchObject({ hooks: {} });
  });

  it("are left alone when kept, edited, or never there", () => {
    expect(keepWalkHookRemoval(graph, graph)).toBe(graph);
    const edited = writeTeamHooks(graph, WALK, "hooks", { run_end: ["other"] });
    expect(keepWalkHookRemoval(graph, edited)).toBe(edited);
    const bare = { entry: "review", states: [] };
    expect(keepWalkHookRemoval(bare, bare)).toBe(bare);
    expect(keepWalkHookRemoval(undefined, bare)).toBe(bare);
  });
});
