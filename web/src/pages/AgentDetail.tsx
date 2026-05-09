import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { Agent, TranscriptEvent, cancelAgent, getAgent, getTranscript } from "../api";

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
        // Schedule next fetch only when status is still running.
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
        {renderedEvents.map((ev, i) => (
          <EventCard key={i} ev={ev} />
        ))}
        <div ref={tailRef} />
      </div>
    </div>
  );
}

// visible filters out events that are noise on the operator dashboard
// (started — purely a marker; usage — folded into the header).
function visible(ev: TranscriptEvent): boolean {
  return ev.type !== "started" && ev.type !== "usage" && ev.type !== "session" && ev.type !== "agent";
}

function EventCard({ ev }: { ev: TranscriptEvent }) {
  switch (ev.type) {
    case "text":
      return (
        <div className="ev ev-text">
          <span className="kind">text</span>
          <pre>{ev.text}</pre>
        </div>
      );
    case "thinking":
      return (
        <div className="ev ev-thinking">
          <span className="kind">thinking</span>
          <pre>{ev.text}</pre>
        </div>
      );
    case "tool_call":
      return (
        <div className="ev ev-tool-call">
          <span className="kind">tool_call</span>
          <div className="tool">
            <strong>{ev.tool_use?.name}</strong>
            <pre>{ev.tool_use ? JSON.stringify(ev.tool_use.input, null, 2) : ""}</pre>
          </div>
        </div>
      );
    case "tool_result":
      return (
        <div className={`ev ev-tool-result ${ev.is_error ? "err" : ""}`}>
          <span className="kind">tool_result{ev.is_error ? " (error)" : ""}</span>
          <pre>{ev.text}</pre>
        </div>
      );
    case "error":
      return (
        <div className="ev ev-error">
          <span className="kind">error</span>
          <pre>{ev.error}</pre>
        </div>
      );
    case "retry":
      return (
        <div className="ev ev-retry">
          <span className="kind">retry</span>
          <pre>rate-limited; sleeping…</pre>
        </div>
      );
    case "done":
      return (
        <div className="ev ev-done">
          <span className="kind">done</span>
          <pre>stop_reason: {ev.stop_reason ?? "?"}</pre>
        </div>
      );
    default:
      return (
        <div className="ev ev-unknown">
          <span className="kind">{ev.type}</span>
          <pre>{JSON.stringify(ev, null, 2)}</pre>
        </div>
      );
  }
}
