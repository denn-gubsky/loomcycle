import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";
import { Agent, EventPayload, TranscriptEvent, cancelAgent, getAgent, getTranscript } from "../api";

// Auto-refresh cadence for live runs. Static runs (completed /
// failed / cancelled) skip polling.
const REFRESH_MS = 1_500;

export default function AgentDetail() {
  const { agentId } = useParams<{ agentId: string }>();
  const [agent, setAgent] = useState<Agent | null>(null);
  const [events, setEvents] = useState<TranscriptEvent[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [cancelInFlight, setCancelInFlight] = useState(false);
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

  const renderedEvents = useMemo(() => events.filter(visible), [events]);

  if (!agentId) {
    return <div className="empty">Missing agent id in URL.</div>;
  }
  return (
    <div className="agent-detail">
      <nav className="crumbs">
        <Link to="/">← runs</Link>
      </nav>
      {err && <div className="err">{err}</div>}
      {agent ? (
        <div className="agent-header">
          <div className="line1">
            <span className={`pill ${agent.status}`}>{agent.status}</span>
            <strong>{agent.agent_type}</strong>
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
          </div>
          <div className="line2">
            <span>{agent.model || "—"}</span>
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
              tokens: {agent.input_tokens ?? "?"} in / {agent.output_tokens ?? "?"} out
              {agent.cache_read_tokens ? `, ${agent.cache_read_tokens} cache-read` : ""}
            </span>
            {agent.duration_ms != null && <span>{(agent.duration_ms / 1000).toFixed(1)}s</span>}
          </div>
          {agent.error && <div className="agent-err">error: {agent.error}</div>}
        </div>
      ) : (
        <div className="empty">loading…</div>
      )}
      <div className="events">
        {renderedEvents.map((ev) => (
          <EventCard key={ev.seq} row={ev} />
        ))}
        <div ref={tailRef} />
      </div>
    </div>
  );
}

// visible filters out events that are noise on the operator dashboard
// (started — purely a marker; usage — folded into the header;
// session/agent — side-channel ID announcements; user_input — the
// initial prompt, separately rendered in v0.8).
function visible(ev: TranscriptEvent): boolean {
  const t = ev.event?.type ?? ev.type;
  return t !== "started" && t !== "usage" && t !== "session" && t !== "agent" && t !== "user_input";
}

// EventCard renders one transcript row as a collapsed panel by
// default — first-line summary + a "▶" affordance — that expands on
// click to show the full text / inputs / outputs / tool params.
// Operators scrolling a long transcript see the shape of the run
// without a wall of text; clicking dives into a specific event.
function EventCard({ row }: { row: TranscriptEvent }) {
  const [open, setOpen] = useState(false);
  const ev = row.event ?? ({ type: row.type } as EventPayload);
  const kind = ev.type ?? row.type;

  const summary = summaryFor(ev);
  const detail = detailFor(ev);

  const toggle = () => setOpen((v) => !v);

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
        <span className="kind">{labelFor(ev)}</span>
        {!open && <span className="summary">{summary}</span>}
        {row.ts_ns > 0 && <span className="ts">{formatTime(row.ts_ns)}</span>}
      </div>
      {open && <div className="ev-detail">{detail}</div>}
    </div>
  );
}

// labelFor is the small uppercase tag on the left side of each card.
// Tool calls embed the tool name to reduce the click-to-discover
// distance for "what tool was this".
function labelFor(ev: EventPayload): string {
  switch (ev.type) {
    case "tool_call":
      return `tool_call · ${ev.tool_use?.name ?? "?"}`;
    case "tool_result":
      return ev.is_error ? "tool_result · error" : "tool_result";
    case "done":
      return `done · ${ev.stop_reason ?? "?"}`;
    case "retry":
      return `retry · ${ev.retry?.reason ?? ""}`.trim();
    default:
      return ev.type ?? "?";
  }
}

// summaryFor is the one-line preview shown when the card is
// collapsed. Keeps the transcript scannable; full content opens on
// click.
function summaryFor(ev: EventPayload): string {
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
    default:
      return "";
  }
}

// detailFor is the full content shown when the card is expanded.
// Returns React nodes so each event type can format its payload how
// it wants (raw text, pretty-printed JSON, etc).
function detailFor(ev: EventPayload): ReactNode {
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
