import type { Agent } from "../api";
import type { BreadcrumbAncestor } from "../components/Breadcrumbs";

// parentRunHref is the run detail header's "parent run" target: the run
// terminal attached to that exact run. An agent id is reused by every run of
// that agent, so the link names the run: /run?attach= replays one run by its
// run id, finished or live.
export function parentRunHref(parentRunId: string): string {
  return `/run?attach=${encodeURIComponent(parentRunId)}`;
}

// RunSelection is what the runs view selects: one run when the row carries a
// run id, else the agent (a row written before runs carried one). An agent id
// alone cannot name a run — every run of the agent shares it, every walk of
// one team shares "team:<name>" — so selecting by it highlights all of them
// and opens whichever the server resolves the agent id to.
export interface RunSelection {
  runId?: string;
  agentId: string;
}

// rowKey is a row's identity in the runs view — the same key its React node
// uses, and the one expand state, the highlight and the ancestor walk use.
export function rowKey(a: Pick<Agent, "run_id" | "agent_id">): string {
  return a.run_id || a.agent_id;
}

export function selectionOf(a: Pick<Agent, "run_id" | "agent_id">): RunSelection {
  return a.run_id ? { runId: a.run_id, agentId: a.agent_id } : { agentId: a.agent_id };
}

export function selectionKey(s: RunSelection): string {
  return s.runId || s.agentId;
}

// runRowHref is a row's link in the runs view: ?run= when it has a run id,
// ?agent= (the older form, still accepted) when it does not.
export function runRowHref(s: RunSelection): string {
  return s.runId
    ? `/agents?run=${encodeURIComponent(s.runId)}`
    : `/agents?agent=${encodeURIComponent(s.agentId)}`;
}

// selectedRowKey maps the view's URL selection to a row key. ?run= names the
// row outright. ?agent= (older links and bookmarks) names the agent; the row it
// highlights is that agent's newest listed run. The server resolves an agent
// id to its most recently started run, so when that run is listed this is the
// row the detail pane shows.
export function selectedRowKey(
  agents: Agent[],
  runId: string | undefined,
  agentId: string | undefined,
): string | undefined {
  if (runId) return runId;
  if (!agentId) return undefined;
  const newest = newestRunOf(agents, agentId);
  return newest ? rowKey(newest) : agentId;
}

function newestRunOf(agents: Agent[], agentId: string): Agent | undefined {
  let newest: Agent | undefined;
  for (const a of agents) {
    if (a.agent_id !== agentId) continue;
    if (!newest || a.started_at.localeCompare(newest.started_at) > 0) newest = a;
  }
  return newest;
}

// breadcrumbAncestors is the chain from the root run down to (not including)
// the selected row. A run's parent is the run its parent_run_id names; a row
// with no parent_run_id falls back to parent_agent_id, as buildTree does. A
// parent outside the listed rows ends the chain with a dim stub.
export function breadcrumbAncestors(agents: Agent[], selectedKey: string | undefined): BreadcrumbAncestor[] {
  if (!selectedKey) return [];
  const byKey = new Map(agents.map((a) => [rowKey(a), a]));
  const byAgentId = new Map(agents.map((a) => [a.agent_id, a]));
  const chain: BreadcrumbAncestor[] = [];
  let cur = byKey.get(selectedKey);
  // parent_agent_id can name the row's own agent id, so a lookup by it can
  // come back to a row already on the chain — stop there rather than loop.
  const seen = new Set<Agent>(cur ? [cur] : []);
  while (cur) {
    const parentId = cur.parent_run_id || cur.parent_agent_id;
    if (!parentId) break;
    const p = cur.parent_run_id ? byKey.get(cur.parent_run_id) : byAgentId.get(parentId);
    if (p && seen.has(p)) break;
    if (!p) {
      chain.unshift({
        agent_id: cur.parent_agent_id || parentId,
        run_id: cur.parent_run_id,
        inResultSet: false,
      });
      break;
    }
    chain.unshift({
      agent_id: p.agent_id,
      run_id: p.run_id || undefined,
      agent: p.agent,
      status: p.status,
      inResultSet: true,
    });
    seen.add(p);
    cur = p;
  }
  return chain;
}
