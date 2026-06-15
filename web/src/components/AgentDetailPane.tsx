import { useEffect, useMemo, useRef, useState, type MouseEvent as ReactMouseEvent, type ReactNode } from "react";
import { Link } from "react-router-dom";
import {
  Agent,
  EventPayload,
  SystemPromptPayload,
  TranscriptEvent,
  UserInputPayload,
  cancelAgent,
  getAgent,
  getTranscript,
} from "../api";
import Breadcrumbs, { type BreadcrumbAncestor } from "./Breadcrumbs";
import TerminalTranscript from "./TerminalTranscript";
import ViewToggle, { useViewMode } from "./ViewToggle";
import {
  AgentTabStrip,
  ChannelsTab,
  InterruptsTab,
  MemoryTab,
  useAgentTab,
} from "./AgentDetailTabs";

// AgentDetailPane renders one agent's status header + the
// scrollable event-card stream. Takes agentId as a prop so it can
// live inside the new split-view layout (commit 5) OR be wrapped by
// a standalone page that reads useParams (current AgentDetail.tsx
// shell).
//
// Extracted from pages/AgentDetail.tsx in commit 2 of the v0.8.20
// Web UI refactor — pure refactor with byte-identical render.

// Auto-refresh cadence for live runs. Static runs (completed /
// failed / cancelled) skip polling.
const REFRESH_MS = 1_500;

export interface AgentDetailPaneProps {
  agentId: string;
  // Optional ancestors chain — used by the split-view parent
  // (commit 5+) to feed the full hierarchy without per-pane
  // re-fetching. When omitted, the pane derives a single-level
  // ancestor (the parent_agent_id of the loaded agent, if any)
  // from a one-shot fetch — enough for the standalone
  // /agents/:agentId deep-link route.
  ancestors?: BreadcrumbAncestor[];
  // Optional select callback — used by the split-view parent to
  // re-target the right pane on breadcrumb click. When omitted,
  // breadcrumbs fall back to <Link to=/agents/:id/>.
  onSelect?: (agentId: string) => void;
}

export default function AgentDetailPane({ agentId, ancestors, onSelect }: AgentDetailPaneProps) {
  const [agent, setAgent] = useState<Agent | null>(null);
  const [events, setEvents] = useState<TranscriptEvent[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [cancelInFlight, setCancelInFlight] = useState(false);
  const [parentAgent, setParentAgent] = useState<Agent | null>(null);
  const tailRef = useRef<HTMLDivElement | null>(null);

  // Initial fetch + auto-refresh while running.
  useEffect(() => {
    if (!agentId) return;
    let cancelled = false;
    let timer: number | undefined;

    const fetchOnce = async () => {
      try {
        const a = await getAgent(agentId);
        if (cancelled) return;
        setAgent(a);
        if (a.session_id) {
          const t = await getTranscript(a.session_id);
          if (!cancelled) setEvents(t.events ?? []);
        }
        setErr(null);
      } catch (e) {
        if (!cancelled) setErr(e instanceof Error ? e.message : String(e));
      } finally {
        if (!cancelled) {
          if (agent?.status === "running" || agent === null) {
            timer = window.setTimeout(fetchOnce, REFRESH_MS);
          }
        }
      }
    };
    fetchOnce();
    return () => {
      cancelled = true;
      if (timer !== undefined) window.clearTimeout(timer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [agentId, agent?.status]);

  // Auto-scroll to the bottom on new events while running.
  useEffect(() => {
    if (agent?.status === "running" && tailRef.current) {
      tailRef.current.scrollIntoView({ behavior: "smooth", block: "end" });
    }
  }, [events.length, agent?.status]);

  // Fetch the immediate parent for the breadcrumb when no
  // ancestors prop is supplied. One-level only — for full
  // multi-level ancestry the split-view parent passes `ancestors`
  // and skips this branch.
  useEffect(() => {
    if (ancestors !== undefined) return;
    if (!agent?.parent_agent_id) {
      setParentAgent(null);
      return;
    }
    let cancelled = false;
    getAgent(agent.parent_agent_id)
      .then((p) => { if (!cancelled) setParentAgent(p); })
      .catch(() => { if (!cancelled) setParentAgent(null); });
    return () => { cancelled = true; };
  }, [ancestors, agent?.parent_agent_id]);

  // Resolve which ancestors to render. Either the prop the parent
  // supplied (split-view), or the one-shot parentAgent fetch
  // above (standalone deep link).
  const resolvedAncestors: BreadcrumbAncestor[] = useMemo(() => {
    if (ancestors !== undefined) return ancestors;
    if (!agent?.parent_agent_id) return [];
    if (parentAgent) {
      return [{
        agent_id: parentAgent.agent_id,
        agent: parentAgent.agent,
        status: parentAgent.status,
        inResultSet: true,
      }];
    }
    // Parent id known but row not yet loaded — render the slug
    // dimmed so the user still sees the hierarchy depth.
    return [{ agent_id: agent.parent_agent_id, inResultSet: false }];
  }, [ancestors, agent?.parent_agent_id, parentAgent]);

  const renderedEvents = useMemo(() => coalesceText(events.filter(visible)), [events]);
  const awaited = useMemo(() => deriveAwaitedState(events), [events]);
  const [viewMode, setViewMode] = useViewMode();
  // v0.9.x — sub-tab strip above the transcript. Default "transcript"
  // shows the existing event-card stream + view toggle.
  const [activeTab, setActiveTab] = useAgentTab();

  return (
    <div className="agent-detail">
      <Breadcrumbs ancestors={resolvedAncestors} selected={agent} onSelect={onSelect} />
      {err && <div className="err">{err}</div>}
      {agent ? (
        <div className="agent-header">
          <div className="line1">
            <span className={`pill ${agent.status}`}>{agent.status}</span>
            {agent.interactive && (
              <span className="pill-interactive" title="Interactive session — parks for operator steering; re-attachable in the run terminal">
                interactive
              </span>
            )}
            <strong>{agent.agent || "(unknown agent)"}</strong>
            <code className="agent-id">{agent.agent_id}</code>
            {agent.status === "running" && (
              <button
                className="cancel-btn"
                disabled={cancelInFlight}
                onClick={async () => {
                  setCancelInFlight(true);
                  try {
                    await cancelAgent(agent.agent_id, "cancelled from UI");
                  } catch (e) {
                    setErr(e instanceof Error ? e.message : String(e));
                  } finally {
                    setCancelInFlight(false);
                  }
                }}
              >
                {cancelInFlight ? "cancelling…" : "cancel"}
              </button>
            )}
            {agent.status === "running" && agent.run_id && (
              // Re-attach to a live (notably interactive/parked) run in the
              // terminal — the run kept executing after you navigated away.
              <Link className="resume-btn" to={`/run?attach=${encodeURIComponent(agent.run_id)}`}>
                resume in terminal
              </Link>
            )}
          </div>
          {agent.status === "running" && (
            <div className="line-await">
              <AwaitChip state={awaited} />
            </div>
          )}
          <div className="line2">
            <span>{agent.usage?.model || "—"}</span>
            <span>user: {agent.user_id || "—"}</span>
            {agent.parent_agent_id && (
              <span>
                parent:{" "}
                <Link to={`/agents/${agent.parent_agent_id}`}>
                  <code>{agent.parent_agent_id.slice(0, 12)}…</code>
                </Link>
              </span>
            )}
            <span>
              tokens: {agent.usage?.input_tokens ?? "?"} in / {agent.usage?.output_tokens ?? "?"} out
              {agent.usage?.cache_read_tokens ? `, ${agent.usage.cache_read_tokens} cache-read` : ""}
            </span>
            {agent.completed_at && (
              <span>{durationLabel(agent.started_at, agent.completed_at)}</span>
            )}
          </div>
          {agent.error && <div className="agent-err">error: {agent.error}</div>}
        </div>
      ) : (
        <div className="empty">loading…</div>
      )}
      <AgentTabStrip tab={activeTab} onChange={setActiveTab} />
      {activeTab === "transcript" && (
        <>
          <ViewToggle mode={viewMode} onChange={setViewMode} />
          {viewMode === "panels" ? (
            <div className="events">
              {renderedEvents.map((ev) => (
                <EventCard key={ev.seq} row={ev} />
              ))}
              <div ref={tailRef} />
            </div>
          ) : (
            <TerminalTranscript events={events} />
          )}
        </>
      )}
      {activeTab === "memory" && <MemoryTab agentName={agent?.agent ?? ""} />}
      {activeTab === "interrupts" && <InterruptsTab runID={agent?.run_id ?? ""} />}
      {activeTab === "channels" && <ChannelsTab agentName={agent?.agent ?? ""} />}
    </div>
  );
}

// visible filters out events that are noise on the operator dashboard
// (started — purely a marker; usage — folded into the header;
// session/agent — side-channel ID announcements; user_input — the
// initial prompt, separately rendered in v0.8).
function visible(ev: TranscriptEvent): boolean {
  const t = ev.event?.type ?? ev.type;
  // v0.9.x: user_input + system_prompt are explicitly visible now —
  // they're the "what the agent received" cards that anchor the
  // transcript. Before v0.9.x user_input was filtered because there
  // was no renderer for it; the new switch branches in detailFor /
  // summaryFor / labelFor handle both.
  return t !== "started" && t !== "usage" && t !== "session" && t !== "agent";
}

// AwaitedState is what the agent is currently blocked on (or
// "running" if it's actively making progress / waiting on a
// provider response, which we can't distinguish from real CPU
// work from outside the loop).
type AwaitedState =
  | { kind: "running" }
  | { kind: "channel"; channel: string }
  | { kind: "interrupted"; op: string };

// deriveAwaitedState walks the transcript and identifies the last
// tool_call that hasn't received a matching tool_result. If that
// open call is a Channel.subscribe or an Interruption.ask, the
// agent is blocked on a long-poll; otherwise it's "running".
//
// Why look at unresolved tool_calls (not a server-side flag): the
// loomcycle loop doesn't currently surface "I'm inside a tool that
// is blocking on i/o" anywhere — the only signal is the absence of
// a tool_result for the most recent tool_call. The events stream
// already carries that signal; computing it client-side keeps the
// wire shape stable and avoids a server roundtrip. Note this is
// best-effort: if the operator filters events somehow, the
// derivation degrades to "running".
function deriveAwaitedState(events: TranscriptEvent[]): AwaitedState {
  const settledIDs = new Set<string>();
  for (const row of events) {
    const ev = row.event ?? ({ type: row.type } as EventPayload);
    if (ev.type === "tool_result" && ev.tool_use_id) {
      settledIDs.add(ev.tool_use_id);
    }
  }
  // Walk from newest to oldest looking for an unresolved tool_call.
  for (let i = events.length - 1; i >= 0; i--) {
    const row = events[i];
    const ev = row.event ?? ({ type: row.type } as EventPayload);
    if (ev.type !== "tool_call" || !ev.tool_use) continue;
    if (settledIDs.has(ev.tool_use.id)) continue;
    const input = (ev.tool_use.input ?? {}) as Record<string, unknown>;
    if (ev.tool_use.name === "Channel" && input.op === "subscribe") {
      const channel = typeof input.channel === "string" ? input.channel : "?";
      return { kind: "channel", channel };
    }
    if (ev.tool_use.name === "Interruption") {
      const op = typeof input.op === "string" ? input.op : "";
      // v0.8.16 emits kind="question" for every blocking ask, but
      // call sites may set kind explicitly in future versions —
      // prefer it when present, fall back to a human-friendly
      // mapping of op.
      const kind = typeof input.kind === "string" ? input.kind : op === "ask" ? "question" : op;
      return { kind: "interrupted", op: kind || "?" };
    }
    // First unresolved tool_call is neither Channel nor Interruption
    // — agent is doing normal tool work. Stop walking; no blocking
    // state.
    return { kind: "running" };
  }
  return { kind: "running" };
}

// AwaitChip renders one of three pills next to a running agent's
// status. Colour comes from CSS class — see styles.css .await-chip.*.
function AwaitChip({ state }: { state: AwaitedState }) {
  if (state.kind === "channel") {
    return (
      <span className="await-chip await-chip-channel">
        channel <code>{state.channel}</code>
      </span>
    );
  }
  if (state.kind === "interrupted") {
    return (
      <span className="await-chip await-chip-interrupted">
        interrupted: <code>{state.op}</code>
      </span>
    );
  }
  return <span className="await-chip await-chip-running">running</span>;
}

// coalesceText folds runs of consecutive `text` (and `thinking`) events
// into a single merged event for rendering. Providers that stream one
// token per delta (DeepSeek, Ollama Cloud) used to produce hundreds of
// single-token cards per run — illegible in the operator dashboard.
// The backend now coalesces at the driver level (PR addressing
// r_935214273f141fb9 on 2026-05-17), but the UI keeps its own pass for
// two reasons: (1) historical transcripts persisted pre-fix still
// contain per-token rows that we want to render compactly, (2) the
// driver's 64-byte threshold still produces ~phrase-sized chunks
// which look better as one paragraph than as many cards.
//
// The merged event reuses the FIRST input event's seq (React key) and
// ts_ns (timestamp label). text is concatenated in order, preserving
// whitespace exactly as the provider wrote it. Only consecutive
// same-type events merge; a tool_call between two text events is a
// hard boundary.
function coalesceText(events: TranscriptEvent[]): TranscriptEvent[] {
  const out: TranscriptEvent[] = [];
  for (const ev of events) {
    const last = out[out.length - 1];
    const kind = ev.event?.type ?? ev.type;
    const lastKind = last ? (last.event?.type ?? last.type) : "";
    if (last && (kind === "text" || kind === "thinking") && kind === lastKind) {
      // Append text to the existing merged event. Clone rather than
      // mutate so React's reference equality picks up the change.
      out[out.length - 1] = {
        ...last,
        event: { ...last.event, text: (last.event?.text ?? "") + (ev.event?.text ?? "") },
      };
    } else {
      out.push(ev);
    }
  }
  return out;
}

// EventCard renders one transcript row as a collapsed panel by
// default — first-line summary + a "▶" affordance — that expands on
// click to show the full text / inputs / outputs / tool params.
// Operators scrolling a long transcript see the shape of the run
// without a wall of text; clicking dives into a specific event.
function EventCard({ row }: { row: TranscriptEvent }) {
  const [open, setOpen] = useState(false);
  // copyState shows feedback after click: idle → copied → idle.
  // "error" fires when the clipboard API is blocked (insecure
  // origin, no document focus, etc.) so the operator sees that
  // copy didn't happen instead of silently swallowing.
  const [copyState, setCopyState] = useState<"idle" | "copied" | "error">("idle");
  const ev = row.event ?? ({ type: row.type } as EventPayload);
  const kind = ev.type ?? row.type;

  const summary = summaryFor(ev, row);
  const detail = detailFor(ev, row);

  const toggle = () => setOpen((v) => !v);

  const copyPayload = async (e: ReactMouseEvent) => {
    e.stopPropagation(); // don't collapse the card on copy click
    const text = textPayloadFor(ev, row);
    try {
      await navigator.clipboard.writeText(text);
      setCopyState("copied");
      window.setTimeout(() => setCopyState("idle"), 1200);
    } catch {
      setCopyState("error");
      window.setTimeout(() => setCopyState("idle"), 1500);
    }
  };

  return (
    <div
      className={`ev ev-${kind} ${ev.is_error ? "err" : ""} ${open ? "open" : ""}`}
      onClick={toggle}
      role="button"
      tabIndex={0}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          toggle();
        }
      }}
    >
      <div className="ev-header">
        <span className="caret">{open ? "▼" : "▶"}</span>
        <span className="kind">{labelFor(ev, row)}</span>
        {!open && <span className="summary">{summary}</span>}
        {row.ts_ns > 0 && <span className="ts">{formatTime(row.ts_ns)}</span>}
      </div>
      {open && (
        <div className="ev-detail">
          <button
            type="button"
            className={`copy-btn copy-state-${copyState}`}
            onClick={copyPayload}
            aria-label="Copy event content to clipboard"
            title={
              copyState === "copied"
                ? "copied"
                : copyState === "error"
                  ? "copy failed (clipboard blocked)"
                  : "copy to clipboard"
            }
          >
            {copyState === "copied" ? "✓ copied" : copyState === "error" ? "✗" : "⧉ copy"}
          </button>
          {detail}
        </div>
      )}
    </div>
  );
}

// textPayloadFor produces the plain-text representation copied to
// the clipboard when the operator hits the copy button on an
// expanded EventCard. Each branch mirrors detailFor's React render
// but in flat text — keeps tool input/result/usage/etc. captured
// without leaking React's escape-sequence artifacts.
function textPayloadFor(ev: EventPayload, row?: TranscriptEvent): string {
  switch (ev.type) {
    case "text":
    case "thinking":
      return ev.text ?? "";
    case "tool_call": {
      const t = ev.tool_use;
      const head = `tool: ${t?.name ?? "?"}${t?.id ? `  id: ${t.id}` : ""}`;
      const input = `input:\n${prettyJSON(t?.input)}`;
      return `${head}\n${input}`;
    }
    case "tool_result":
      return ev.text ?? "";
    case "error":
      return ev.error ?? "";
    case "retry":
      return JSON.stringify(ev.retry ?? {}, null, 2);
    case "done": {
      const lines = [`stop_reason: ${ev.stop_reason ?? "?"}`];
      if (ev.reasoning) lines.push("", "reasoning:", ev.reasoning);
      if (ev.usage) lines.push("", "usage:", JSON.stringify(ev.usage, null, 2));
      return lines.join("\n");
    }
    case "user_input": {
      // Filter out role="system" segments — the agent's system prompt
      // is already surfaced by the dedicated system_prompt card; showing
      // it again here makes the operator scroll past it to find what
      // was actually said to the agent. Sub-agent spawns + run-creation
      // paths prepend a system segment for provider wire-shape reasons
      // (see server.go runSubAgent), but the operator dashboard wants
      // the "user said" view.
      const segs = ((row?.payload as UserInputPayload[] | undefined) ?? []).filter(
        (s) => s.role === "user",
      );
      return JSON.stringify(segs, null, 2);
    }
    case "system_prompt": {
      const p = row?.payload as SystemPromptPayload | undefined;
      if (!p) return "";
      const lines = [p.system_prompt ?? ""];
      if (p.agent_def_id) lines.push("", `agent_def_id: ${p.agent_def_id}`);
      if (p.skill_def_ids) {
        lines.push("", `skill_def_ids: ${JSON.stringify(p.skill_def_ids, null, 2)}`);
      }
      return lines.join("\n");
    }
    default:
      return JSON.stringify(ev, null, 2);
  }
}

// labelFor is the small uppercase tag on the left side of each card.
// Tool calls embed the tool name to reduce the click-to-discover
// distance for "what tool was this".
function labelFor(ev: EventPayload, _row?: TranscriptEvent): string {
  switch (ev.type) {
    case "tool_call":
      return `tool_call · ${ev.tool_use?.name ?? "?"}`;
    case "tool_result":
      return ev.is_error ? "tool_result · error" : "tool_result";
    case "done":
      return `done · ${ev.stop_reason ?? "?"}`;
    case "retry":
      return `retry · ${ev.retry?.reason ?? ""}`.trim();
    case "user_input":
      return "input · user";
    case "system_prompt":
      return "input · system";
    default:
      return ev.type ?? "?";
  }
}

// summaryFor is the one-line preview shown when the card is
// collapsed. Keeps the transcript scannable; full content opens on
// click.
function summaryFor(ev: EventPayload, row?: TranscriptEvent): string {
  switch (ev.type) {
    case "text":
    case "thinking":
      return firstLine(ev.text ?? "", 200);
    case "tool_call": {
      const args = ev.tool_use?.input;
      if (!args) return "(no args)";
      const json = typeof args === "string" ? args : JSON.stringify(args);
      return firstLine(json, 200);
    }
    case "tool_result":
      return firstLine(ev.text ?? "", 200);
    case "error":
      return firstLine(ev.error ?? "", 200);
    case "retry": {
      const r = ev.retry ?? {};
      return `${r.provider ?? "?"} attempt ${r.attempt ?? "?"} · wait ${r.wait_ms ?? "?"}ms`;
    }
    case "done":
      return ev.usage
        ? `${ev.usage.input_tokens ?? 0} in / ${ev.usage.output_tokens ?? 0} out${
            ev.usage.model ? ` · ${ev.usage.model}` : ""
          }`
        : "";
    case "user_input": {
      const segs = (row?.payload as UserInputPayload[] | undefined) ?? [];
      // Surface the first user-role content text (the prompt the
      // caller sent). Skip system-role segments — those duplicate the
      // AgentDef prompt that the separate system_prompt card surfaces.
      const userSeg = segs.find((s) => s.role === "user");
      const text = userSeg?.content?.[0]?.text ?? "";
      return firstLine(text, 200);
    }
    case "system_prompt": {
      const p = row?.payload as SystemPromptPayload | undefined;
      return firstLine(p?.system_prompt ?? "", 200);
    }
    default:
      return "";
  }
}

// detailFor is the full content shown when the card is expanded.
// Returns React nodes so each event type can format its payload how
// it wants (raw text, pretty-printed JSON, etc).
function detailFor(ev: EventPayload, row?: TranscriptEvent): ReactNode {
  switch (ev.type) {
    case "text":
    case "thinking":
      return <pre className="full-text">{ev.text ?? ""}</pre>;
    case "tool_call":
      return (
        <div className="tool-detail">
          <div className="tool-meta">
            <span>name:</span> <code>{ev.tool_use?.name ?? "?"}</code>
            {ev.tool_use?.id && (
              <>
                {" · "}
                <span>id:</span> <code>{ev.tool_use.id}</code>
              </>
            )}
          </div>
          <div className="tool-params">
            <span>input:</span>
            <pre className="full-text">{prettyJSON(ev.tool_use?.input)}</pre>
          </div>
        </div>
      );
    case "tool_result":
      return <pre className="full-text">{ev.text ?? ""}</pre>;
    case "error":
      return <pre className="full-text">{ev.error ?? ""}</pre>;
    case "retry":
      return (
        <pre className="full-text">
          {JSON.stringify(ev.retry ?? {}, null, 2)}
        </pre>
      );
    case "done":
      return (
        <div>
          <div>stop_reason: {ev.stop_reason ?? "?"}</div>
          {ev.reasoning && (
            <div>
              <strong>reasoning trace:</strong>
              <pre className="full-text">{ev.reasoning}</pre>
            </div>
          )}
          {ev.usage && (
            <pre className="full-text">{JSON.stringify(ev.usage, null, 2)}</pre>
          )}
        </div>
      );
    case "user_input": {
      const segs = ((row?.payload as UserInputPayload[] | undefined) ?? []).filter(
        (s) => s.role === "user",
      );
      return (
        <div className="user-input-detail">
          {segs.map((seg, i) => (
            <div key={i} className="user-input-segment">
              {seg.content?.map((c, j) => (
                <pre key={j} className="full-text">{c.text ?? JSON.stringify(c)}</pre>
              ))}
            </div>
          ))}
        </div>
      );
    }
    case "system_prompt": {
      const p = row?.payload as SystemPromptPayload | undefined;
      if (!p) {
        return <pre className="full-text">(no payload)</pre>;
      }
      return (
        <div className="system-prompt-detail">
          <pre className="full-text">{p.system_prompt ?? ""}</pre>
          {(p.agent_def_id || p.skill_def_ids) && (
            <div className="system-prompt-provenance">
              {p.agent_def_id && (
                <div>
                  <span>agent_def_id:</span> <code>{p.agent_def_id}</code>
                </div>
              )}
              {p.skill_def_ids && Object.keys(p.skill_def_ids).length > 0 && (
                <div>
                  <span>skill_def_ids:</span>
                  <pre className="full-text">{JSON.stringify(p.skill_def_ids, null, 2)}</pre>
                </div>
              )}
            </div>
          )}
        </div>
      );
    }
    default:
      return <pre className="full-text">{JSON.stringify(ev, null, 2)}</pre>;
  }
}

function firstLine(s: string, max: number): string {
  if (!s) return "";
  const nl = s.indexOf("\n");
  let line = nl >= 0 ? s.slice(0, nl) : s;
  if (line.length > max) line = line.slice(0, max) + "…";
  return line;
}

function prettyJSON(v: unknown): string {
  if (v == null) return "";
  if (typeof v === "string") return v;
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}

function formatTime(ns: number): string {
  if (!ns) return "";
  const ms = Math.floor(ns / 1_000_000);
  return new Date(ms).toLocaleTimeString();
}

function durationLabel(startedAt: string, completedAt: string): string {
  const a = new Date(startedAt).getTime();
  const b = new Date(completedAt).getTime();
  if (!Number.isFinite(a) || !Number.isFinite(b)) return "";
  const ms = b - a;
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`;
  return `${Math.floor(ms / 60_000)}m ${Math.round((ms % 60_000) / 1000)}s`;
}
