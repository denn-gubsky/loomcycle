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

// isTeamWalkAgentId reports whether an agent id is a team walk's. A walk's run
// is filed under "team:<name>", shared by every walk of that team, and the
// agent-id routes (/v1/agents/{id}, its cancel and channels) reject the colon.
// Such a run is read and cancelled by its run id.
export function isTeamWalkAgentId(agentId: string | null | undefined): boolean {
  return Boolean(agentId?.startsWith("team:"));
}

// paneRunId is the run the detail pane reads for the view's URL selection.
// ?run= names it. A ?agent= team walk id cannot be read by agent id, so it
// resolves to the run id of the newest listed walk (the row selectedRowKey
// highlights); undefined when none is listed, and the pane then asks for a
// specific walk. Other ?agent= ids stay agent ids, which the server resolves.
export function paneRunId(
  agents: Agent[],
  runId: string | undefined,
  agentId: string | undefined,
): string | undefined {
  if (runId) return runId;
  if (!agentId || !isTeamWalkAgentId(agentId)) return undefined;
  return newestRunOf(agents, agentId)?.run_id || undefined;
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

// RunTeam is the team a run belongs to, read from its spec: `team_scope` is on
// every run in a walk's spawn tree (and a continuation of one), and
// `agent_version.team_def_id` marks a run of one of the team's OWN agents —
// declared in its definition, run as "<team>/<name>", and not runnable outside
// a walk of that team.
export interface RunTeam {
  team: string;
  ownAgent: boolean;
}

/** runTeamOf reads the team a run belongs to, or undefined for a run outside
 *  any team (a walk's own run included: it carries `team`, not `team_scope`). */
export function runTeamOf(a: Pick<Agent, "spec">): RunTeam | undefined {
  const sc = a.spec?.team_scope;
  if (!sc || typeof sc !== "object") return undefined;
  const team = (sc as { team?: unknown }).team;
  if (typeof team !== "string" || team === "") return undefined;
  const av = a.spec?.agent_version;
  const tdid = av && typeof av === "object" ? (av as { team_def_id?: unknown }).team_def_id : undefined;
  return { team, ownAgent: typeof tdid === "string" && tdid !== "" };
}

// AwaitedChildren is a running run's wait for its background (poll-mode)
// children: it ended its turn and takes its next one when every child it
// started has ended. runIds are the children the server names in awaited_on,
// a bounded list; more counts the ones it left out (", +N more", or an id cut
// short to fit, which is no use as a link).
export interface AwaitedChildren {
  runIds: string[];
  more: number;
}

/** awaitedChildrenOf reads a run's wait for its background children, or
 *  undefined for a run that is not waiting on them. */
export function awaitedChildrenOf(a: Pick<Agent, "status" | "awaited_state" | "awaited_on">): AwaitedChildren | undefined {
  if (a.status !== "running" || a.awaited_state !== "children") return undefined;
  const out: AwaitedChildren = { runIds: [], more: 0 };
  for (const raw of (a.awaited_on ?? "").split(",")) {
    const part = raw.trim();
    if (part === "") continue;
    const m = /^\+(\d+) more$/.exec(part);
    if (m) {
      out.more += Number(m[1]);
    } else if (part.endsWith("…")) {
      out.more += 1;
    } else {
      out.runIds.push(part);
    }
  }
  return out;
}

// RunClock is a code agent's time budget as its run read reports it: the
// budget its definition or run set (absent when the operator's default
// applies), and — once the run has paused at least once — the active time it
// had used against that budget, the time it had spent waiting (which never
// counts against it) and how long it had lived, as of that pause. The live
// figures are not on the run read: a run reports them to itself through
// Context op=self.
export interface RunClock {
  budgetSeconds?: number;
  atLastPause?: { activeMs: number; waitedMs: number; wallMs: number };
}

/** runClockOf reads a code agent's run clock from its spec, or undefined for a
 *  run that keeps none (every model-driven run). */
export function runClockOf(a: Pick<Agent, "spec" | "usage">): RunClock | undefined {
  const spec = a.spec ?? {};
  const rc = spec.run_clock;
  const recorded = rc !== null && typeof rc === "object";
  if (!recorded && a.usage?.provider !== "code-js") return undefined;
  const out: RunClock = {};
  const budget = spec.run_timeout_seconds;
  if (typeof budget === "number" && budget > 0) out.budgetSeconds = budget;
  if (recorded) {
    const r = rc as { active_ms?: unknown; waited_ms?: unknown; wall_ms?: unknown };
    const ms = (v: unknown) => (typeof v === "number" && v >= 0 ? v : 0);
    out.atLastPause = { activeMs: ms(r.active_ms), waitedMs: ms(r.waited_ms), wallMs: ms(r.wall_ms) };
  }
  return out;
}

/** shortDuration renders a span of milliseconds the way the run header does. */
export function shortDuration(ms: number): string {
  if (ms < 1000) return `${Math.round(ms)}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`;
  if (ms < 3_600_000) return `${Math.floor(ms / 60_000)}m ${Math.round((ms % 60_000) / 1000)}s`;
  return `${Math.floor(ms / 3_600_000)}h ${Math.round((ms % 3_600_000) / 60_000)}m`;
}
