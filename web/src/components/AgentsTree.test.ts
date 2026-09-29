import { describe, expect, it } from "vitest";
import type { Agent } from "../api";
import { buildTree, type TreeNode } from "./AgentsTree";

const run = (over: Partial<Agent> & Pick<Agent, "agent_id" | "run_id">): Agent => ({
  session_id: "s",
  agent: "a",
  parent_agent_id: null,
  user_id: "u",
  status: "completed",
  started_at: "2026-09-29T00:00:00Z",
  completed_at: null,
  stop_reason: null,
  error: null,
  usage: {},
  last_heartbeat_at: null,
  live: false,
  ...over,
});

const shape = (nodes: TreeNode[]): unknown =>
  nodes.map((n) => ({ run: n.agent.run_id, children: shape(n.children) }));

describe("buildTree", () => {
  it("hangs a child under the run its parent_run_id names, not another run of the same agent", () => {
    // Two runs of one parent agent share its agent id; the child was spawned
    // by the OLDER one. parent_agent_id alone cannot tell them apart.
    const tree = buildTree([
      run({ agent_id: "a_lead", run_id: "r_new", started_at: "2026-09-29T00:02:00Z" }),
      run({ agent_id: "a_lead", run_id: "r_old", started_at: "2026-09-29T00:01:00Z" }),
      run({ agent_id: "a_child", run_id: "r_child", parent_agent_id: "a_lead", parent_run_id: "r_old" }),
    ]);
    expect(shape(tree)).toEqual([
      { run: "r_new", children: [] },
      { run: "r_old", children: [{ run: "r_child", children: [] }] },
    ]);
  });

  it("falls back to parent_agent_id for a row that names no parent run", () => {
    const tree = buildTree([
      run({ agent_id: "a_lead", run_id: "r_lead" }),
      run({ agent_id: "a_child", run_id: "r_child", parent_agent_id: "a_lead" }),
    ]);
    expect(shape(tree)).toEqual([{ run: "r_lead", children: [{ run: "r_child", children: [] }] }]);
  });

  it("keeps a run whose named parent run is not in the result set at the top level", () => {
    // Another run of the parent agent is present, but it is not the parent.
    const tree = buildTree([
      run({ agent_id: "a_lead", run_id: "r_other", started_at: "2026-09-29T00:02:00Z" }),
      run({ agent_id: "a_child", run_id: "r_child", parent_agent_id: "a_lead", parent_run_id: "r_filtered_out" }),
    ]);
    expect(shape(tree)).toEqual([
      { run: "r_other", children: [] },
      { run: "r_child", children: [] },
    ]);
  });

  it("gives every row its own node when runs share an agent id", () => {
    const tree = buildTree([
      run({ agent_id: "a_lead", run_id: "r_1", started_at: "2026-09-29T00:02:00Z" }),
      run({ agent_id: "a_lead", run_id: "r_2", started_at: "2026-09-29T00:01:00Z" }),
    ]);
    expect(shape(tree)).toEqual([
      { run: "r_1", children: [] },
      { run: "r_2", children: [] },
    ]);
  });
});
