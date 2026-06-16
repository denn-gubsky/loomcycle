import { useCallback, useEffect, useState } from "react";
import { listAgents, type Agent } from "../api";

const POLL_MS = 4000;

// useInteractiveSessions polls the operator's RUNNING interactive sessions
// (listAgents(userId, "running") filtered to interactive runs). Lifted out of
// InteractiveSessionsPanel so the run page's left column has ONE poll shared by
// both states: the expanded panel (the list) and the collapsed strip (just the
// count). Returns the last good list; a transient poll failure is swallowed.
export function useInteractiveSessions(userId: string): Agent[] {
  const [sessions, setSessions] = useState<Agent[]>([]);

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

  return sessions;
}
