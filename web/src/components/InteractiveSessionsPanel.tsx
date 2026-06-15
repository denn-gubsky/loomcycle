import { useCallback, useEffect, useState } from "react";
import { listAgents, type Agent } from "../api";

// Persisted collapse state so the operator's choice survives navigation.
const COLLAPSE_KEY = "loomcycle.run.interactive-sessions.collapsed";
const POLL_MS = 4000;

// InteractiveSessionsPanel lists the operator's RUNNING interactive sessions in
// the run page's left column, so they can switch between sessions or re-open one
// they left — without detouring through the runs page "resume in terminal" link.
//
// Source: listAgents(userId, "running") filtered to interactive runs (the
// runs.interactive flag, surfaced on the run-list row). Clicking a row
// re-attaches it in the run terminal (onOpen → useRunStream.attach, which
// replays from seq 0 then live-tails). Collapsible; collapsed it shows just the
// "N interactive sessions" header.
export default function InteractiveSessionsPanel({
  userId,
  currentRunId,
  onOpen,
}: {
  userId: string;
  currentRunId?: string;
  onOpen: (runId: string) => void;
}) {
  const [sessions, setSessions] = useState<Agent[]>([]);
  const [collapsed, setCollapsed] = useState<boolean>(
    () => localStorage.getItem(COLLAPSE_KEY) === "1",
  );
  useEffect(() => {
    localStorage.setItem(COLLAPSE_KEY, collapsed ? "1" : "0");
  }, [collapsed]);

  const refresh = useCallback(() => {
    if (!userId) {
      setSessions([]);
      return;
    }
    listAgents(userId, "running")
      .then((r) => setSessions((r.agents ?? []).filter((a) => a.interactive)))
      .catch(() => {
        /* transient poll failure — keep the last good list */
      });
  }, [userId]);

  useEffect(() => {
    refresh();
    const id = window.setInterval(refresh, POLL_MS);
    return () => window.clearInterval(id);
  }, [refresh]);

  // No user context → nothing to list (admins without a picked user, etc.).
  if (!userId) return null;

  const count = sessions.length;
  return (
    <div
      className={
        "interactive-sessions" + (collapsed ? " interactive-sessions-collapsed" : "")
      }
    >
      <button
        type="button"
        className="interactive-sessions-head"
        onClick={() => setCollapsed((c) => !c)}
        aria-expanded={!collapsed}
        title={collapsed ? "Show interactive sessions" : "Hide interactive sessions"}
      >
        <span className="interactive-sessions-caret" aria-hidden="true">
          {collapsed ? "▸" : "▾"}
        </span>
        <span className="interactive-sessions-title">
          {count} interactive session{count === 1 ? "" : "s"}
        </span>
      </button>
      {!collapsed && (
        <div className="interactive-sessions-list">
          {count === 0 ? (
            <div className="interactive-sessions-empty">No interactive sessions.</div>
          ) : (
            sessions.map((s) => {
              const isCurrent = !!currentRunId && s.run_id === currentRunId;
              return (
                <button
                  key={s.run_id}
                  type="button"
                  className={
                    "interactive-session-row" +
                    (isCurrent ? " interactive-session-current" : "")
                  }
                  onClick={() => onOpen(s.run_id)}
                  disabled={isCurrent}
                  title={
                    isCurrent ? "Currently open" : `Open ${s.agent} in the terminal`
                  }
                >
                  <span className="interactive-session-agent">
                    {s.agent || "(unknown agent)"}
                  </span>
                  <code className="interactive-session-runid">
                    {s.run_id.slice(0, 12)}…
                  </code>
                  {isCurrent && <span className="pill-interactive">open</span>}
                </button>
              );
            })
          )}
        </div>
      )}
    </div>
  );
}
