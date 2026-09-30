import { createElement, isValidElement, type ReactElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import type { Agent } from "../api";
import AgentsTree, { AgentsTreeNode, buildTree, collectAncestorIds, type TreeNode } from "./AgentsTree";

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

// Two walks of one team: every walk's row carries agent_id "team:triage",
// each has its own run id and its own child.
const twoWalks = () =>
  buildTree([
    run({ agent_id: "team:triage", run_id: "r_new", started_at: "2026-09-29T00:02:00Z" }),
    run({ agent_id: "team:triage", run_id: "r_old", started_at: "2026-09-29T00:01:00Z" }),
    run({ agent_id: "a_cn", run_id: "r_cn", parent_agent_id: "team:triage", parent_run_id: "r_new" }),
    run({ agent_id: "a_co", run_id: "r_co", parent_agent_id: "team:triage", parent_run_id: "r_old" }),
  ]);

// findByClass returns the first host element in a rendered-but-not-mounted
// element tree whose className includes cls. The tree is not mounted (there
// is no DOM in these tests), so a handler is reached by calling it directly.
function findByClass(el: unknown, cls: string): ReactElement<Record<string, unknown>> | undefined {
  if (Array.isArray(el)) {
    for (const c of el) {
      const hit = findByClass(c, cls);
      if (hit) return hit;
    }
    return undefined;
  }
  if (!isValidElement<Record<string, unknown>>(el)) return undefined;
  const className = el.props.className;
  if (typeof className === "string" && className.split(" ").includes(cls)) return el;
  return findByClass(el.props.children, cls);
}

const nodeProps = (node: TreeNode, over: Partial<Parameters<typeof AgentsTreeNode>[0]> = {}) => ({
  node,
  depth: 0,
  expandedMap: new Map<string, boolean>(),
  setExpanded: () => {},
  onSelect: () => {},
  ...over,
});

describe("AgentsTree row state", () => {
  it("highlights only the selected run when runs share an agent id", () => {
    const html = renderToStaticMarkup(
      createElement(AgentsTree, { tree: twoWalks(), selectedId: "r_old", onSelect: () => {} }),
    );
    // Rows render newest first: r_new, r_cn, r_old, r_co.
    const selected = [...html.matchAll(/<li class="([^"]*)"/g)].map((m) => m[1].split(" ").includes("selected"));
    expect(selected).toEqual([false, false, true, false]);
  });

  it("collapses one run without collapsing another run of the same agent", () => {
    const [newer, older] = twoWalks();
    const expandedMap = new Map([["r_old", false]]);
    const olderHtml = renderToStaticMarkup(createElement(AgentsTreeNode, nodeProps(older, { expandedMap })));
    const newerHtml = renderToStaticMarkup(createElement(AgentsTreeNode, nodeProps(newer, { expandedMap })));
    expect(olderHtml).not.toContain('class="children"');
    expect(newerHtml).toContain('class="children"');
  });

  it("records a caret toggle under the run it was clicked on", () => {
    const older = twoWalks()[1];
    const setExpanded = vi.fn();
    const caret = findByClass(AgentsTreeNode(nodeProps(older, { setExpanded })), "tree-caret");
    (caret?.props.onClick as (e: { stopPropagation: () => void }) => void)({ stopPropagation: () => {} });
    expect(setExpanded).toHaveBeenCalledWith("r_old", false);
  });

  it("selecting the older of two runs of one agent selects that run", () => {
    const older = twoWalks()[1];
    const onSelect = vi.fn();
    const link = findByClass(AgentsTreeNode(nodeProps(older, { onSelect })), "agent-link");
    (link?.props.onClick as () => void)();
    expect(onSelect).toHaveBeenCalledWith({ runId: "r_old", agentId: "team:triage" });
  });

  it("selects by agent id a row that carries no run id", () => {
    const [legacy] = buildTree([run({ agent_id: "a_legacy", run_id: "" })]);
    const onSelect = vi.fn();
    const link = findByClass(AgentsTreeNode(nodeProps(legacy, { onSelect })), "agent-link");
    (link?.props.onClick as () => void)();
    expect(onSelect).toHaveBeenCalledWith({ agentId: "a_legacy" });
  });

  it("names the selected run's own parent run as its ancestor", () => {
    // The child of the OLDER walk expands r_old only — not every run
    // that shares the walk's agent id.
    expect(collectAncestorIds(twoWalks(), "r_co")).toEqual(["r_old"]);
    expect(collectAncestorIds(twoWalks(), "r_cn")).toEqual(["r_new"]);
  });
});
