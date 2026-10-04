import type { Agent, InterruptRow, RunStateEvent, WalkRunsPage } from "../api";
import { isTeamWalkAgentId, rowKey } from "./runLineage";

// The team-walk view's pure half: what a walk looks like, folded from one
// listing (GET /v1/runs?walk_id=) and one filtered stream
// (GET /v1/users/{user_id}/agents/stream?walk_id=).
//
// A walk is a run filed under agent_id "team:<name>". Every run it starts
// carries parent_context.walk_id (= the walk's run id), the state that started
// it and that state's visit number. The merge rules are ported from loomboard's
// workflow canvas (packages/workflow/src/lib/runs.ts foldWalk, and the app's
// walkWatch / walkRows): a newer row wins, a terminal row is never replaced by
// a non-terminal one. Ported rather than depended on: the canvas package
// brings xyflow with it.

/** The team a walk's agent id ("team:<name>") names. */
export function teamNameOf(agentId: string): string {
  return isTeamWalkAgentId(agentId) ? agentId.slice("team:".length) : agentId;
}

/** The team a walk ran, as its run recorded it when it started. `defId` and
 *  `version` are absent on a walk that started before walks recorded them: only
 *  the name is known then, and it resolves to whatever is active NOW. */
export interface WalkTeam {
  name: string;
  defId?: string;
  version?: number;
}

/** walkTeamOf reads the team out of a walk's run: the `team` record in its
 *  spec when there is one, else just the name its agent id carries. */
export function walkTeamOf(walk: Pick<Agent, "agent_id" | "spec">): WalkTeam {
  const name = teamNameOf(walk.agent_id);
  const rec = walk.spec?.team;
  if (!rec || typeof rec !== "object") return { name };
  const { def_id, version } = rec as { def_id?: unknown; version?: unknown };
  if (typeof def_id !== "string" || def_id === "") return { name };
  return { name, defId: def_id, version: typeof version === "number" && version > 0 ? version : undefined };
}

/** graphCaption says which definition the walk's graph is drawn from: the
 *  version the walk recorded, or — for a walk that recorded none — whatever
 *  is active now. */
export function graphCaption(team: WalkTeam | undefined): string {
  if (!team) return "";
  if (team.defId) {
    return team.version
      ? `Drawn from version ${team.version} of the team's definition, the version this walk ran.`
      : "Drawn from the version of the team's definition this walk ran.";
  }
  return "Drawn from the team's current definition. This walk started before walks recorded the version they ran, so a team changed since it started can show states the walk did not have.";
}

/** One run as the walk view needs it. */
export interface WalkRunRow {
  runId: string;
  agentId: string;
  agent: string;
  /** running | completed | failed | cancelled | rejected | configured */
  status: string;
  /** RFC3339 instant of this row's state; the newer row wins. */
  ts: string;
  /** Members only: the state that started this run, and which visit of it. */
  state?: string;
  stateVisit?: number;
  /** Starter members only. */
  waveId?: string;
  waveIndex?: number;
  /** What a running run is blocked on. */
  awaited?: "channel" | "interrupted" | "review" | "input";
  awaitedOn?: string;
  /** When an unruled review hold ends as rejected. Only the stream carries it. */
  holdExpiresAt?: string;
  error?: string;
  stopReason?: string;
  // Facts only the listing carries. A stream frame omits them, so the fold
  // keeps the last listed value rather than blanking the column.
  startedAt?: string;
  completedAt?: string;
  inputTokens?: number;
  outputTokens?: number;
  boardDocumentId?: string;
  boardScope?: string;
}

export interface WalkView {
  walkRunId: string;
  /** The walk's own run, once seen. */
  walk?: WalkRunRow;
  /** Every member run, by run id. */
  members: ReadonlyMap<string, WalkRunRow>;
}

export function emptyWalk(walkRunId: string): WalkView {
  return { walkRunId, members: new Map() };
}

/** Terminal statuses. A held run is still `running` with awaited=review. */
const TERMINAL = new Set(["completed", "failed", "cancelled", "rejected"]);

export function isTerminal(status: string): boolean {
  return TERMINAL.has(status);
}

function awaitedOf(s: string | undefined): WalkRunRow["awaited"] {
  return s === "channel" || s === "interrupted" || s === "review" || s === "input" ? s : undefined;
}

/** A listing row (listWalkRuns / getRun). Its instant is the latest it knows:
 *  completion if it ended, otherwise its last heartbeat or its start. */
export function rowFromAgent(a: Agent): WalkRunRow {
  const pc = a.parent_context;
  return {
    runId: a.run_id,
    agentId: a.agent_id,
    agent: a.agent,
    status: a.status,
    ts: a.completed_at ?? a.last_heartbeat_at ?? a.started_at,
    state: pc?.state || undefined,
    stateVisit: pc?.state_visit,
    waveId: pc?.wave_id || undefined,
    waveIndex: pc?.wave_id ? pc.wave_index : undefined,
    awaited: awaitedOf(a.awaited_state),
    awaitedOn: a.awaited_on || undefined,
    error: a.error ?? undefined,
    stopReason: a.stop_reason ?? undefined,
    startedAt: a.started_at,
    completedAt: a.completed_at ?? undefined,
    inputTokens: a.usage?.input_tokens,
    outputTokens: a.usage?.output_tokens,
    boardDocumentId: pc?.board_document_id || undefined,
    boardScope: pc?.board_scope || undefined,
  };
}

/** A run-state transition from the stream. */
export function rowFromEvent(e: RunStateEvent): WalkRunRow {
  const pc = e.parent_context;
  return {
    runId: e.run_id,
    agentId: e.agent_id,
    agent: e.agent,
    status: e.status,
    ts: e.ts,
    state: pc?.state || undefined,
    stateVisit: pc?.state_visit,
    waveId: pc?.wave_id || undefined,
    waveIndex: pc?.wave_id ? pc.wave_index : undefined,
    awaited: awaitedOf(e.awaited_state),
    awaitedOn: e.awaited_on || undefined,
    holdExpiresAt: e.hold_expires_at || undefined,
    error: e.error || undefined,
    stopReason: e.stop_reason || undefined,
    // A terminal transition's instant is when the run ended.
    completedAt: isTerminal(e.status) ? e.ts : undefined,
    boardDocumentId: pc?.board_document_id || undefined,
    boardScope: pc?.board_scope || undefined,
  };
}

/** True when an event belongs to this walk: its own run, or one it started.
 *  Checked client-side as well as by the server's filter, so a server that
 *  ignored walk_id cannot flood the view with every run the user has. */
export function belongsToWalk(e: RunStateEvent, walkRunId: string): boolean {
  return e.run_id === walkRunId || e.parent_context?.walk_id === walkRunId;
}

// Instants are compared as times, not strings. The listing writes the server's
// local offset with microseconds ("…12:37:14.827396+03:00"), the stream UTC
// whole seconds ("…09:37:48Z"), so as strings a listing row east of UTC always
// looks newer and would roll every stream transition back. An unparseable
// instant falls back to the string order loomboard uses.
function notOlder(next: string, prev: string): boolean {
  const a = Date.parse(next);
  const b = Date.parse(prev);
  if (Number.isNaN(a) || Number.isNaN(b)) return next >= prev;
  return a >= b;
}

/** Fold rows into the view. A row replaces the one it has only when it is NOT
 *  older: the stream can deliver a transition before the listing page that
 *  predates it, and the page must not roll the run back. Equal instants take
 *  the newcomer, and a terminal row is never replaced by a non-terminal one,
 *  because a run does not come back from completed. */
export function foldWalk(view: WalkView, rows: readonly WalkRunRow[]): WalkView {
  let walk = view.walk;
  let members: Map<string, WalkRunRow> | undefined;
  const newer = (prev: WalkRunRow | undefined, next: WalkRunRow) => {
    if (!prev) return true;
    if (isTerminal(prev.status) && !isTerminal(next.status)) return false;
    return notOlder(next.ts, prev.ts);
  };
  for (const r of rows) {
    if (r.runId === view.walkRunId) {
      if (newer(walk, r)) walk = walk ? carryOver(r, walk) : r;
      continue;
    }
    const cur = (members ?? view.members).get(r.runId);
    if (!newer(cur, r)) continue;
    members ??= new Map(view.members);
    members.set(r.runId, cur ? carryOver(r, cur) : r);
  }
  if (walk === view.walk && !members) return view;
  return { ...view, walk, members: members ?? view.members };
}

/** The newer row is authoritative for everything that CHANGES — status, and
 *  above all `awaited`: a hold that has cleared is reported by the field's
 *  ABSENCE, so merging "defined fields only" would keep a stale hold forever.
 *  Carried over when a row omits them: the run's place in the graph, which
 *  never changes, and the listing-only facts a stream frame does not carry.
 *
 *  holdExpiresAt is carried only while the run is STILL held for review: a
 *  listing row never has it, so a heartbeat page landing mid-hold would
 *  otherwise drop the countdown. Once the hold clears, so does the deadline. */
function carryOver(next: WalkRunRow, prev: WalkRunRow): WalkRunRow {
  return {
    ...next,
    state: next.state ?? prev.state,
    stateVisit: next.stateVisit ?? prev.stateVisit,
    waveId: next.waveId ?? prev.waveId,
    waveIndex: next.waveIndex ?? prev.waveIndex,
    startedAt: next.startedAt ?? prev.startedAt,
    completedAt: next.completedAt ?? prev.completedAt,
    inputTokens: next.inputTokens ?? prev.inputTokens,
    outputTokens: next.outputTokens ?? prev.outputTokens,
    boardDocumentId: next.boardDocumentId ?? prev.boardDocumentId,
    boardScope: next.boardScope ?? prev.boardScope,
    holdExpiresAt:
      next.holdExpiresAt ??
      (next.awaited === "review" && prev.awaited === "review" ? prev.holdExpiresAt : undefined),
  };
}

/** One visit of one state: the members that visit started, in wave order. */
export interface VisitGroup {
  key: string;
  visit: number;
  state: string;
  members: WalkRunRow[];
}

/** Members grouped by state visit, then state, each group in wave order.
 *  A member with no recorded visit sorts as visit 0, ahead of the counted
 *  visits, so it stays visible rather than being dropped. */
export function visitGroups(view: WalkView): VisitGroup[] {
  const byKey = new Map<string, VisitGroup>();
  for (const r of view.members.values()) {
    const visit = r.stateVisit ?? 0;
    const state = r.state ?? "";
    const key = `${visit}\u0000${state}`;
    let g = byKey.get(key);
    if (!g) {
      g = { key, visit, state, members: [] };
      byKey.set(key, g);
    }
    g.members.push(r);
  }
  const groups = [...byKey.values()];
  groups.sort((a, b) => a.visit - b.visit || (a.state < b.state ? -1 : a.state > b.state ? 1 : 0));
  for (const g of groups) {
    g.members.sort(
      (a, b) =>
        (a.waveIndex ?? 0) - (b.waveIndex ?? 0) ||
        cmp(a.startedAt ?? a.ts, b.startedAt ?? b.ts) ||
        cmp(a.runId, b.runId),
    );
  }
  return groups;
}

function cmp(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

/** A pending breakpoint pause, read off the question the walk asks. The walk
 *  raises it as a plain Interruption question on its own run; the text is the
 *  only place its state is recorded. */
export interface BreakpointPause {
  state: string;
  wave: string;
  waveSize: number;
  pending: number;
}

// Matches the walk's own wording (formatBreakpoint in the TeamDef tool):
//   Team "<name>": state "<state>" paused BEFORE dispatching wave <wave> (<n> runs in the wave, <m> pending).
// Names are Go %q-quoted.
const PAUSE_RE =
  /^Team "(?:[^"\\]|\\.)*": state "((?:[^"\\]|\\.)*)" paused BEFORE dispatching wave (\S+) \((\d+) runs in the wave, (\d+) pending\)/;

export function parseBreakpointPause(question: string | undefined): BreakpointPause | undefined {
  if (!question) return undefined;
  const m = PAUSE_RE.exec(question);
  if (!m) return undefined;
  return { state: unquote(m[1]), wave: m[2], waveSize: Number(m[3]), pending: Number(m[4]) };
}

// Go's %q escapes are JSON-compatible for every printable state id; one that
// is not (a \x escape) falls back to the raw text, which still names it.
function unquote(raw: string): string {
  try {
    return JSON.parse(`"${raw}"`) as string;
  } catch {
    return raw;
  }
}

/** The state to highlight on the diagram.
 *
 *  A pending breakpoint pause wins: the walk is stopped in that state before
 *  dispatching it, so no member of it runs yet and the members still running
 *  (if any) belong to a visit the walk has moved past. Otherwise it is the
 *  state of the highest visit among members still running or waiting. A walk
 *  with neither has no current state. */
export function currentState(view: WalkView, pending: readonly InterruptRow[]): string | undefined {
  let pause: { state: string; at: string } | undefined;
  for (const ir of pending) {
    if (ir.status && ir.status !== "pending") continue;
    const p = parseBreakpointPause(ir.question);
    if (p && (!pause || ir.created_at > pause.at)) pause = { state: p.state, at: ir.created_at };
  }
  if (pause) return pause.state;
  let best: WalkRunRow | undefined;
  for (const r of view.members.values()) {
    if (isTerminal(r.status) || !r.state) continue;
    if (!best || (r.stateVisit ?? 0) > (best.stateVisit ?? 0)) best = r;
  }
  return best?.state;
}

/** Every page of a walk's listing, following next_cursor to the end. Stops,
 *  rather than looping, if the server ever hands back a cursor it already
 *  gave. */
export async function fetchAllWalkPages(
  fetchPage: (cursor: string | undefined) => Promise<WalkRunsPage>,
): Promise<Agent[]> {
  const out: Agent[] = [];
  const seen = new Set<string>();
  let cursor: string | undefined;
  for (;;) {
    const page = await fetchPage(cursor);
    out.push(...(page.agents ?? []));
    const next = page.next_cursor;
    if (!next || seen.has(next)) return out;
    seen.add(next);
    cursor = next;
  }
}

export interface WalkWatchDeps {
  listPage: (cursor: string | undefined, signal: AbortSignal) => Promise<WalkRunsPage>;
  stream: (userId: string, onEvent: (e: RunStateEvent) => void, signal: AbortSignal) => Promise<void>;
  readWalk: (signal: AbortSignal) => Promise<Agent>;
}

export interface WalkWatchHandlers {
  onRows: (rows: WalkRunRow[]) => void;
  /** The walk's own run, each time it is read. */
  onWalk: (walk: Agent) => void;
  onError?: (e: unknown) => void;
}

export interface WatchTiming {
  reconnectMs?: number;
  /** How often the walk's OWN run is read while it is live. */
  pollMs?: number;
}

const RECONNECT_MS = 2000;
const POLL_MS = 5000;

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) return resolve();
    const t = setTimeout(resolve, ms);
    signal.addEventListener(
      "abort",
      () => {
        clearTimeout(t);
        resolve();
      },
      { once: true },
    );
  });
}

/** Follow one walk: hydrate every page, then stream its members' transitions.
 *
 *  The server caps the stream at about 30 minutes, so on a clean end (or an
 *  error) it reconnects and HYDRATES AGAIN, which picks up anything missed in
 *  the gap; foldWalk is order-tolerant, so a page that lands after a newer
 *  frame cannot roll a run back.
 *
 *  The walk's OWN run is polled every pollMs, because the filtered stream
 *  matches parent_context.walk_id, which members carry and the walk's run does
 *  not. It is also read at once when a member ends, since a walk ends moments
 *  after its last member. With no user id to stream under, it falls back to
 *  re-listing every pollMs.
 *
 *  It stops on its own once the walk's run is terminal and one final listing
 *  has been folded. Returns the stop function. */
export function watchWalk(
  deps: WalkWatchDeps,
  walkRunId: string,
  h: WalkWatchHandlers,
  timing: WatchTiming = {},
): () => void {
  const reconnectMs = timing.reconnectMs ?? RECONNECT_MS;
  const pollMs = timing.pollMs ?? POLL_MS;
  const ac = new AbortController();
  const { signal } = ac;

  // Returns true when the listing shows the walk ended.
  const hydrate = async (): Promise<boolean> => {
    const agents = await fetchAllWalkPages((c) => deps.listPage(c, signal));
    if (signal.aborted) return false;
    const rows = agents.map(rowFromAgent);
    if (rows.length) h.onRows(rows);
    return rows.some((r) => r.runId === walkRunId && isTerminal(r.status));
  };

  // The walk has ended: fold one last listing, so every member's final state
  // lands even if its own frame was lost, and stop everything.
  let finishing = false;
  const finish = async () => {
    if (finishing) return;
    finishing = true;
    try {
      await hydrate();
    } catch (e) {
      if (!signal.aborted) h.onError?.(e);
    }
    ac.abort();
  };

  const readWalk = async (): Promise<Agent | undefined> => {
    try {
      const a = await deps.readWalk(signal);
      if (signal.aborted) return undefined;
      h.onWalk(a);
      h.onRows([rowFromAgent(a)]);
      if (isTerminal(a.status)) await finish();
      return a;
    } catch (e) {
      if (!signal.aborted) h.onError?.(e);
      return undefined;
    }
  };

  void (async () => {
    const first = await readWalk();
    if (signal.aborted) return;

    void (async () => {
      while (!signal.aborted) {
        await sleep(pollMs, signal);
        if (!signal.aborted) await readWalk();
      }
    })();

    const userId = first?.user_id;
    while (!signal.aborted) {
      try {
        if (await hydrate()) return void (await finish());
        if (userId) {
          await deps.stream(
            userId,
            (e) => {
              if (signal.aborted || !belongsToWalk(e, walkRunId)) return;
              h.onRows([rowFromEvent(e)]);
              if (e.run_id === walkRunId && isTerminal(e.status)) void finish();
              else if (isTerminal(e.status)) void readWalk();
            },
            signal,
          );
        }
        // Clean end (the server's stream cap) → reconnect and re-hydrate.
      } catch (e) {
        if (signal.aborted) return;
        h.onError?.(e);
      }
      await sleep(userId ? reconnectMs : pollMs, signal);
    }
  })();

  return () => ac.abort();
}

/** Seconds left on a review hold, floored at 0; undefined with no deadline. */
export function holdSecondsLeft(expiresAt: string | undefined, nowMs: number): number | undefined {
  if (!expiresAt) return undefined;
  const t = Date.parse(expiresAt);
  if (Number.isNaN(t)) return undefined;
  return Math.max(0, Math.floor((t - nowMs) / 1000));
}

export function countdownLabel(seconds: number): string {
  if (seconds <= 0) return "expiring";
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = seconds % 60;
  const ss = String(s).padStart(2, "0");
  if (h > 0) return `${h}h ${String(m).padStart(2, "0")}m`;
  return m > 0 ? `${m}m ${ss}s` : `${s}s`;
}

export function durationLabel(startedAt: string | undefined, endedAt: string | number | undefined): string {
  if (!startedAt || endedAt === undefined) return "";
  const a = Date.parse(startedAt);
  const b = typeof endedAt === "number" ? endedAt : Date.parse(endedAt);
  if (!Number.isFinite(a) || !Number.isFinite(b) || b < a) return "";
  const ms = b - a;
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`;
  return `${Math.floor(ms / 60_000)}m ${Math.round((ms % 60_000) / 1000)}s`;
}

/** The HTTP status an api.ts call failed with ("403 Forbidden: …"), if any. */
export function httpStatusOf(e: unknown): number | undefined {
  const m = /^(\d{3}) /.exec(e instanceof Error ? e.message : String(e));
  return m ? Number(m[1]) : undefined;
}

/** The breakpoint editor's text → the whole set to PUT: comma, space or
 *  newline separated, trimmed, de-duplicated, in the order written. The
 *  server validates each entry. */
export function parseBreakpointList(text: string): string[] {
  const out: string[] = [];
  for (const raw of text.split(/[\s,]+/)) {
    const s = raw.trim();
    if (s && !out.includes(s)) out.push(s);
  }
  return out;
}

/** The review-deadline field → whole seconds (0 = no deadline). */
export function parseReviewTTL(text: string): { ok: true; seconds: number } | { ok: false; error: string } {
  const t = text.trim();
  if (t === "") return { ok: true, seconds: 0 };
  if (!/^\d+$/.test(t)) return { ok: false, error: "review deadline must be a whole number of seconds (0 = none)" };
  return { ok: true, seconds: Number(t) };
}

/** The distinct board documents the walk's members write to. */
export function boardDocuments(view: WalkView): { documentId: string; scope: string }[] {
  const out: { documentId: string; scope: string }[] = [];
  for (const r of view.members.values()) {
    if (!r.boardDocumentId || out.some((d) => d.documentId === r.boardDocumentId)) continue;
    out.push({ documentId: r.boardDocumentId, scope: r.boardScope || "user" });
  }
  return out;
}

/** Which detail pane the runs view opens for its selection.
 *
 *  - "walk": the selected row is a team walk; runId is the walk's run id.
 *  - "agent": an ordinary run (or an agent-only selection with no run id).
 *  - "unknown": a ?run= the listing does not hold (a finished walk under the
 *    default "running" filter, a deep link) — the caller reads the run to tell. */
export type RunPaneChoice = { kind: "walk"; runId: string } | { kind: "agent" } | { kind: "unknown"; runId: string };

export function runPaneFor(agents: readonly Agent[], selectedKey: string | undefined, runId: string | undefined): RunPaneChoice {
  const row = selectedKey ? agents.find((a) => rowKey(a) === selectedKey) : undefined;
  if (row) {
    return isTeamWalkAgentId(row.agent_id) && row.run_id ? { kind: "walk", runId: row.run_id } : { kind: "agent" };
  }
  return runId ? { kind: "unknown", runId } : { kind: "agent" };
}
