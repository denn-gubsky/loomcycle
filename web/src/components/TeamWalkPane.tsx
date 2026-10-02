import { useCallback, useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import {
  type Agent,
  type InterruptRow,
  type RunBreakpoints,
  cancelRun,
  getRun,
  getRunBreakpoints,
  listRunInterrupts,
  listWalkRuns,
  renderTeamDiagram,
  resolveInterrupt,
  reviewRun,
  setRunBreakpoints,
  streamWalkRunStates,
} from "../api";
import { useTheme } from "../hooks/useTheme";
import { useMermaidSvg } from "../hooks/useMermaidSvg";
import { runRowHref } from "../lib/runLineage";
import {
  type WalkRunRow,
  type WalkView,
  boardDocuments,
  countdownLabel,
  currentState,
  durationLabel,
  emptyWalk,
  foldWalk,
  holdSecondsLeft,
  httpStatusOf,
  isTerminal,
  parseBreakpointList,
  parseBreakpointPause,
  parseReviewTTL,
  teamNameOf,
  visitGroups,
  watchWalk,
} from "../lib/walkView";

// TeamWalkPane is the runs page's detail pane for a team walk (a run filed
// under agent_id "team:<name>"). It shows the walk's header, the team diagram
// with the current state outlined, every member run grouped by state visit,
// and — while the walk is live — its review holds, breakpoint pauses,
// breakpoint arming and cancel. Members link to their own run, which opens
// the ordinary AgentDetailPane.
//
// The container below owns the fetching; TeamWalkView renders from props so
// it can be tested without a server.

const POLL_MS = 5_000;

export type BreakpointsState =
  | { kind: "loading" }
  | { kind: "ready"; data: RunBreakpoints }
  | { kind: "unavailable"; reason: string };

export type GraphState =
  | { kind: "loading" }
  // A runs:read-only principal cannot read TeamDefs (403): no graph at all.
  | { kind: "hidden" }
  | { kind: "error"; message: string }
  | { kind: "ready"; source: string; svg: string; renderErr: string };

export interface WalkActions {
  review: (memberRunId: string, decision: "approve" | "reject", feedback?: string) => Promise<void>;
  resolvePause: (interruptId: string, answer: string) => Promise<void>;
  saveBreakpoints: (breakpoints: string[], reviewTTLSeconds: number) => Promise<void>;
  cancelWalk: () => Promise<void>;
}

export default function TeamWalkPane({ runId }: { runId: string }) {
  const { theme } = useTheme();
  const [walk, setWalk] = useState<Agent | null>(null);
  const [view, setView] = useState<WalkView>(() => emptyWalk(runId));
  const [err, setErr] = useState<string | null>(null);
  const [interrupts, setInterrupts] = useState<InterruptRow[]>([]);
  const [breakpoints, setBreakpoints] = useState<BreakpointsState>({ kind: "loading" });
  const [diagram, setDiagram] = useState<
    { kind: "loading" } | { kind: "hidden" } | { kind: "error"; message: string } | { kind: "ready"; source: string }
  >({ kind: "loading" });
  const [now, setNow] = useState(() => Date.now());
  // Bumped after an action so the pending pauses and arming are re-read now
  // rather than on the next poll.
  const [refresh, setRefresh] = useState(0);

  // A new selection starts from nothing: the previous walk's rows must not
  // linger under the new header.
  useEffect(() => {
    setWalk(null);
    setView(emptyWalk(runId));
    setErr(null);
    setInterrupts([]);
    setBreakpoints({ kind: "loading" });
    setDiagram({ kind: "loading" });
  }, [runId]);

  useEffect(
    () =>
      watchWalk(
        {
          listPage: (cursor, signal) => listWalkRuns(runId, cursor, signal),
          stream: (userId, onEvent, signal) => streamWalkRunStates(userId, runId, onEvent, signal),
          readWalk: () => getRun(runId),
        },
        runId,
        {
          onRows: (rows) => setView((v) => (v.walkRunId === runId ? foldWalk(v, rows) : v)),
          onWalk: (a) => {
            setWalk(a);
            setErr(null);
          },
          onError: (e) => setErr(e instanceof Error ? e.message : String(e)),
        },
        { pollMs: POLL_MS },
      ),
    [runId],
  );

  const live = walk?.status === "running";

  // Pending pauses are questions asked on the walk's own run.
  useEffect(() => {
    if (!live) {
      setInterrupts([]);
      return;
    }
    let cancelled = false;
    const read = () =>
      listRunInterrupts(runId, "pending")
        .then((r) => {
          if (!cancelled) setInterrupts(r.interrupts ?? []);
        })
        .catch(() => {
          // Not fatal: the timeline and graph still work without them.
          if (!cancelled) setInterrupts([]);
        });
    void read();
    const t = window.setInterval(read, POLL_MS);
    return () => {
      cancelled = true;
      window.clearInterval(t);
    };
  }, [runId, live, refresh]);

  // The arming lives in memory on the walk's replica, live walks only.
  useEffect(() => {
    if (!live) return;
    let cancelled = false;
    getRunBreakpoints(runId)
      .then((data) => {
        if (!cancelled) setBreakpoints({ kind: "ready", data });
      })
      .catch((e) => {
        if (!cancelled) setBreakpoints({ kind: "unavailable", reason: breakpointsUnavailableReason(e) });
      });
    return () => {
      cancelled = true;
    };
  }, [runId, live, refresh]);

  const teamName = walk ? teamNameOf(walk.agent_id) : "";
  const highlight = useMemo(() => currentState(view, interrupts), [view, interrupts]);

  // Once refused, the principal cannot read TeamDefs: stop asking on every
  // state change. A new selection resets it.
  const graphRefused = diagram.kind === "hidden";
  useEffect(() => {
    if (!teamName || graphRefused) return;
    let cancelled = false;
    renderTeamDiagram(teamName, highlight)
      .then((d) => {
        if (!cancelled) setDiagram({ kind: "ready", source: d.diagram });
      })
      .catch((e) => {
        if (cancelled) return;
        setDiagram(httpStatusOf(e) === 403 ? { kind: "hidden" } : { kind: "error", message: e instanceof Error ? e.message : String(e) });
      });
    return () => {
      cancelled = true;
    };
  }, [teamName, highlight, graphRefused]);

  const { svg, renderErr } = useMermaidSvg(diagram.kind === "ready" ? diagram.source : undefined, theme, "walk-mmd");
  const graph: GraphState = diagram.kind === "ready" ? { kind: "ready", source: diagram.source, svg, renderErr } : diagram;

  // The review countdowns tick only while some hold has a deadline.
  const anyDeadline = live && [...view.members.values()].some((m) => m.awaited === "review" && m.holdExpiresAt);
  useEffect(() => {
    if (!anyDeadline) return;
    const t = window.setInterval(() => setNow(Date.now()), 1_000);
    return () => window.clearInterval(t);
  }, [anyDeadline]);

  const bump = useCallback(() => setRefresh((n) => n + 1), []);
  const actions: WalkActions = useMemo(
    () => ({
      review: async (memberRunId, decision, feedback) => {
        await reviewRun(memberRunId, decision, feedback);
        bump();
      },
      resolvePause: async (interruptId, answer) => {
        await resolveInterrupt(runId, interruptId, answer);
        bump();
      },
      saveBreakpoints: async (bps, ttl) => {
        const data = await setRunBreakpoints(runId, { breakpoints: bps, review_ttl_seconds: ttl });
        setBreakpoints({ kind: "ready", data });
      },
      cancelWalk: async () => {
        await cancelRun(runId, "cancelled from UI");
        bump();
      },
    }),
    [runId, bump],
  );

  return (
    <TeamWalkView
      runId={runId}
      walk={walk}
      view={view}
      err={err}
      interrupts={interrupts}
      breakpoints={breakpoints}
      graph={graph}
      highlight={highlight}
      now={now}
      actions={actions}
    />
  );
}

function breakpointsUnavailableReason(e: unknown): string {
  switch (httpStatusOf(e)) {
    case 404:
      return "Breakpoints can be changed only on the server running this walk; it is not live on this one.";
    case 403:
      return "Your token cannot read this walk's breakpoints.";
    case 503:
      return "Team breakpoints are not enabled on this server.";
    default:
      return e instanceof Error ? e.message : String(e);
  }
}

export interface TeamWalkViewProps {
  runId: string;
  walk: Agent | null;
  view: WalkView;
  err?: string | null;
  interrupts: InterruptRow[];
  breakpoints: BreakpointsState;
  graph: GraphState;
  highlight?: string;
  now: number;
  actions: WalkActions;
}

export function TeamWalkView(p: TeamWalkViewProps) {
  const { walk, view } = p;
  const live = walk?.status === "running";
  const groups = useMemo(() => visitGroups(view), [view]);
  const docs = useMemo(() => boardDocuments(view), [view]);
  return (
    <div className="agent-detail team-walk">
      {p.err && <div className="err">{p.err}</div>}
      {walk ? (
        <WalkHeader walk={walk} live={live} onCancel={p.actions.cancelWalk} docs={docs} />
      ) : (
        <div className="empty">loading…</div>
      )}
      {live && <PausePanel interrupts={p.interrupts} onResolve={p.actions.resolvePause} />}
      <WalkGraph graph={p.graph} highlight={p.highlight} />
      {live && <BreakpointsEditor state={p.breakpoints} onSave={p.actions.saveBreakpoints} />}
      <VisitTimeline groups={groups} live={live} now={p.now} onReview={p.actions.review} />
    </div>
  );
}

function WalkHeader({
  walk,
  live,
  onCancel,
  docs,
}: {
  walk: Agent;
  live: boolean;
  onCancel: () => Promise<void>;
  docs: { documentId: string; scope: string }[];
}) {
  const finalText = walk.result?.final_text;
  return (
    <div className="agent-header">
      <div className="line1">
        <span className={`pill ${walk.status}`}>{walk.status}</span>
        {walk.live && <span className="live-run-busy-dot" title="Live on this server" />}
        <strong>team {teamNameOf(walk.agent_id)}</strong>
        <code className="agent-id">{walk.run_id}</code>
        {live && <ConfirmButton label="cancel walk" confirmLabel="Confirm cancel" keepLabel="Keep running" onConfirm={onCancel} />}
      </div>
      <div className="line2">
        <span>started: {formatInstant(walk.started_at)}</span>
        {walk.completed_at && <span>finished: {formatInstant(walk.completed_at)}</span>}
        <span>{durationLabel(walk.started_at, walk.completed_at ?? undefined) || (live ? "running" : "")}</span>
        <span>user: {walk.user_id || "—"}</span>
        {walk.parent_run_id && (
          <span>
            parent run:{" "}
            <Link to={runRowHref({ runId: walk.parent_run_id, agentId: walk.parent_agent_id ?? "" })}>
              <code>{walk.parent_run_id.slice(0, 12)}…</code>
            </Link>
          </span>
        )}
        {docs.map((d) => (
          <span key={d.documentId}>
            board:{" "}
            <Link to={`/documents/${encodeURIComponent(d.documentId)}?scope=${encodeURIComponent(d.scope)}`}>
              <code>{d.documentId.slice(0, 12)}…</code>
            </Link>
          </span>
        ))}
      </div>
      {walk.error && <div className="agent-err">error: {walk.error}</div>}
      {finalText && (
        <details className="team-walk-result" open>
          <summary>result</summary>
          <pre>{finalText}</pre>
        </details>
      )}
    </div>
  );
}

function WalkGraph({ graph, highlight }: { graph: GraphState; highlight?: string }) {
  const [showSource, setShowSource] = useState(false);
  if (graph.kind === "hidden") return null;
  return (
    <section className="team-walk-section team-walk-graph">
      <h3>
        Graph{highlight ? (
          <>
            {" "}
            <span className="team-walk-muted">— current state</span> <code>{highlight}</code>
          </>
        ) : null}
      </h3>
      <p className="team-walk-muted">
        Drawn from the team&apos;s current definition. The version this walk ran is not recorded, so a team changed since
        it started can show states the walk did not have.
      </p>
      {graph.kind === "loading" && <p className="team-walk-muted">loading diagram…</p>}
      {graph.kind === "error" && <div className="err">Diagram unavailable: {graph.message}</div>}
      {graph.kind === "ready" && (
        <>
          {graph.renderErr && <div className="err">Diagram render failed: {graph.renderErr}</div>}
          {graph.svg && (
            // Safe as innerHTML: server-sanitized source rendered with mermaid's
            // strict security level (see useMermaidSvg).
            <div className="team-walk-svg" dangerouslySetInnerHTML={{ __html: graph.svg }} />
          )}
          <button type="button" onClick={() => setShowSource((s) => !s)}>
            {showSource ? "hide source" : "view source"}
          </button>
          {(showSource || (!graph.svg && !graph.renderErr)) && <pre className="team-walk-source">{graph.source}</pre>}
        </>
      )}
    </section>
  );
}

function VisitTimeline({
  groups,
  live,
  now,
  onReview,
}: {
  groups: ReturnType<typeof visitGroups>;
  live: boolean;
  now: number;
  onReview: WalkActions["review"];
}) {
  return (
    <section className="team-walk-section">
      <h3>Visits</h3>
      {groups.length === 0 ? (
        <p className="team-walk-muted">No member runs yet.</p>
      ) : (
        groups.map((g) => (
          <div key={g.key} className="team-walk-visit">
            <div className="team-walk-visit-head">
              <span className="team-walk-visit-n">#{g.visit || "?"}</span> <code>{g.state || "(no state)"}</code>{" "}
              <span className="team-walk-muted">
                {g.members.length} run{g.members.length === 1 ? "" : "s"}
              </span>
            </div>
            <ul className="team-walk-members">
              {g.members.map((m) => (
                <MemberRow key={m.runId} m={m} live={live} now={now} onReview={onReview} />
              ))}
            </ul>
          </div>
        ))
      )}
    </section>
  );
}

function MemberRow({
  m,
  live,
  now,
  onReview,
}: {
  m: WalkRunRow;
  live: boolean;
  now: number;
  onReview: WalkActions["review"];
}) {
  const held = !isTerminal(m.status) && m.awaited === "review";
  const left = held ? holdSecondsLeft(m.holdExpiresAt, now) : undefined;
  const ended = m.completedAt ?? (isTerminal(m.status) ? m.ts : undefined);
  const dur = durationLabel(m.startedAt, ended ?? (live ? now : undefined));
  return (
    <li className="team-walk-member">
      <div className="team-walk-member-line">
        {m.waveId && <span className="team-walk-muted">[{m.waveIndex ?? 0}]</span>}
        <span className={`pill ${m.status}`}>{m.status}</span>
        <Link to={runRowHref({ runId: m.runId, agentId: m.agentId })}>
          <strong>{m.agent || m.agentId}</strong>
        </Link>
        {m.awaited && !isTerminal(m.status) && (
          <span className={`await-chip ${m.awaited === "review" ? "await-chip-interrupted" : "await-chip-channel"}`}>
            {m.awaited === "review" ? "awaiting review" : `awaiting ${m.awaited}`}
            {m.awaitedOn && m.awaited !== "review" ? <code>{m.awaitedOn}</code> : null}
          </span>
        )}
        {left !== undefined && (
          <span className="team-walk-countdown" title={`An unruled hold ends rejected at ${m.holdExpiresAt}`}>
            {countdownLabel(left)} left
          </span>
        )}
        <span className="team-walk-muted">
          tokens {m.inputTokens ?? "—"} in / {m.outputTokens ?? "—"} out
        </span>
        {dur && <span className="team-walk-muted">{dur}</span>}
      </div>
      {m.error && <div className="agent-err">error: {m.error}</div>}
      {live && held && <ReviewControls runId={m.runId} onReview={onReview} />}
    </li>
  );
}

function ReviewControls({ runId, onReview }: { runId: string; onReview: WalkActions["review"] }) {
  const [rejecting, setRejecting] = useState(false);
  const [feedback, setFeedback] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const run = async (decision: "approve" | "reject") => {
    setBusy(true);
    setErr(null);
    try {
      await onReview(runId, decision, decision === "reject" ? feedback.trim() || undefined : undefined);
      setRejecting(false);
      setFeedback("");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="team-walk-review">
      {rejecting ? (
        <>
          <textarea
            value={feedback}
            onChange={(e) => setFeedback(e.target.value)}
            placeholder="Feedback: the run revises its answer with it. Leave empty to end the run rejected."
            rows={3}
            disabled={busy}
          />
          <div className="team-walk-buttons">
            <button type="button" className="cancel-btn" disabled={busy} onClick={() => void run("reject")}>
              {busy ? "…" : feedback.trim() ? "Confirm reject with feedback" : "Confirm reject"}
            </button>
            <button type="button" disabled={busy} onClick={() => setRejecting(false)}>
              Keep holding
            </button>
          </div>
        </>
      ) : (
        <div className="team-walk-buttons">
          <button type="button" className="resume-btn" disabled={busy} onClick={() => void run("approve")}>
            {busy ? "…" : "Approve"}
          </button>
          <button type="button" className="cancel-btn" disabled={busy} onClick={() => setRejecting(true)}>
            Reject…
          </button>
        </div>
      )}
      {err && <div className="err">{err}</div>}
    </div>
  );
}

function PausePanel({
  interrupts,
  onResolve,
}: {
  interrupts: InterruptRow[];
  onResolve: WalkActions["resolvePause"];
}) {
  if (interrupts.length === 0) return null;
  return (
    <section className="team-walk-section team-walk-pauses">
      <h3>Waiting on you</h3>
      {interrupts.map((ir) => {
        const pause = parseBreakpointPause(ir.question);
        return (
          <div key={ir.interrupt_id} className="team-walk-pause">
            {pause ? (
              <p>
                Paused before dispatching <code>{pause.state}</code>: {pause.pending} of {pause.waveSize} runs pending.
              </p>
            ) : null}
            <pre className="team-walk-question">{ir.question}</pre>
            {pause ? (
              <PauseControls interruptId={ir.interrupt_id} pending={pause.pending} onResolve={onResolve} />
            ) : (
              <p className="team-walk-muted">
                Answer it in the <Link to="/interrupts">interrupts inbox</Link>.
              </p>
            )}
          </div>
        );
      })}
    </section>
  );
}

function PauseControls({
  interruptId,
  pending,
  onResolve,
}: {
  interruptId: string;
  pending: number;
  onResolve: WalkActions["resolvePause"];
}) {
  const [n, setN] = useState("1");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const answer = async (a: string) => {
    setBusy(true);
    setErr(null);
    try {
      await onResolve(interruptId, a);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  const count = Number(n);
  const countOk = Number.isInteger(count) && count >= 1;
  return (
    <div className="team-walk-buttons">
      <button type="button" className="resume-btn" disabled={busy} onClick={() => void answer("continue")}>
        Continue (dispatch {pending})
      </button>
      <input
        type="number"
        min={1}
        max={Math.max(1, pending)}
        value={n}
        onChange={(e) => setN(e.target.value)}
        disabled={busy}
        aria-label="runs to release"
        className="team-walk-n"
      />
      <button type="button" disabled={busy || !countOk} onClick={() => void answer(`release:${count}`)}>
        Release {countOk ? count : "n"}
      </button>
      <ConfirmButton label="Abort walk" confirmLabel="Confirm abort" keepLabel="Keep paused" onConfirm={() => answer("abort")} disabled={busy} />
      {err && <div className="err">{err}</div>}
    </div>
  );
}

function BreakpointsEditor({
  state,
  onSave,
}: {
  state: BreakpointsState;
  onSave: WalkActions["saveBreakpoints"];
}) {
  const armed = state.kind === "ready" ? (state.data.armed ?? []).join(", ") : "";
  const ttl = state.kind === "ready" ? String(state.data.review_ttl_seconds) : "0";
  const [text, setText] = useState(armed);
  const [ttlText, setTTLText] = useState(ttl);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  // Re-seed the fields from what the server reports (first load, after a save).
  useEffect(() => {
    setText(armed);
    setTTLText(ttl);
  }, [armed, ttl]);

  if (state.kind === "loading") return null;
  if (state.kind === "unavailable") {
    return (
      <section className="team-walk-section">
        <h3>Breakpoints</h3>
        <p className="team-walk-muted">{state.reason}</p>
      </section>
    );
  }
  const save = async () => {
    const parsed = parseReviewTTL(ttlText);
    if (!parsed.ok) {
      setErr(parsed.error);
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      await onSave(parseBreakpointList(text), parsed.seconds);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  const dirty = text !== armed || ttlText !== ttl;
  return (
    <section className="team-walk-section">
      <h3>Breakpoints</h3>
      <p className="team-walk-muted">
        The whole armed set, comma separated. <code>&lt;state&gt;</code> pauses before dispatching that state;{" "}
        <code>&lt;state&gt;:review</code> holds each of its runs for your verdict. Removing a review releases the runs it
        holds, as approved.
      </p>
      <div className="team-walk-bp">
        <input
          type="text"
          value={text}
          onChange={(e) => setText(e.target.value)}
          placeholder="none armed"
          disabled={busy}
          aria-label="armed breakpoints"
        />
        <label>
          review deadline (s, 0 = none){" "}
          <input
            type="text"
            inputMode="numeric"
            value={ttlText}
            onChange={(e) => setTTLText(e.target.value)}
            disabled={busy}
            className="team-walk-n"
          />
        </label>
        <button type="button" disabled={busy || !dirty} onClick={() => void save()}>
          {busy ? "Saving…" : "Save"}
        </button>
      </div>
      {err && <div className="err">{err}</div>}
    </section>
  );
}

// ConfirmButton is the two-step confirm the run detail panes use (see
// DraftPanel's discard): the first click asks, the second acts.
function ConfirmButton({
  label,
  confirmLabel,
  keepLabel,
  onConfirm,
  disabled,
}: {
  label: string;
  confirmLabel: string;
  keepLabel: string;
  onConfirm: () => Promise<void>;
  disabled?: boolean;
}) {
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  if (!asking) {
    return (
      <button type="button" className="cancel-btn" disabled={disabled} onClick={() => setAsking(true)}>
        {label}
      </button>
    );
  }
  return (
    <span className="team-walk-confirm">
      <button
        type="button"
        className="cancel-btn"
        disabled={busy || disabled}
        onClick={async () => {
          setBusy(true);
          setErr(null);
          try {
            await onConfirm();
            setAsking(false);
          } catch (e) {
            setErr(e instanceof Error ? e.message : String(e));
          } finally {
            setBusy(false);
          }
        }}
      >
        {busy ? "…" : confirmLabel}
      </button>
      <button type="button" disabled={busy} onClick={() => setAsking(false)}>
        {keepLabel}
      </button>
      {err && <span className="err">{err}</span>}
    </span>
  );
}

function formatInstant(s: string | null | undefined): string {
  if (!s) return "—";
  const t = Date.parse(s);
  return Number.isNaN(t) ? s : new Date(t).toLocaleString();
}
