import { useEffect, useMemo, useState } from "react";
import { Agent, listAgents } from "../api";
import { useUserId } from "../components/Layout";
import AgentsTree, { buildTree } from "../components/AgentsTree";

type StatusFilter = "all" | "running" | "completed" | "failed" | "cancelled";

// Auto-refresh interval for the running-agents list. Polls
// /v1/users/{user_id}/agents because there's no global SSE feed of
// state transitions today; once that lands (v0.8 candidate) we'd
// drop polling and subscribe.
const REFRESH_MS = 3_000;

export default function RunList() {
  const userId = useUserId();
  const [agents, setAgents] = useState<Agent[]>([]);
  const [filter, setFilter] = useState<StatusFilter>("running");
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (!userId) {
      setAgents([]);
      return;
    }
    // Clear stale data synchronously on filter (or user) change so the
    // previous filter's results don't linger during the ~50–300ms
    // network round-trip — and don't persist indefinitely if the new
    // fetch errors (the catch below only sets `err`; without this
    // clear, the prior agents would stay visible under the new
    // filter's label until the next successful poll).
    setAgents([]);
    setErr(null);
    let cancelled = false;
    const fetchOnce = async () => {
      try {
        setLoading(true);
        const status = filter === "all" ? undefined : filter;
        const resp = await listAgents(userId, status);
        if (!cancelled) {
          setAgents(resp.agents ?? []);
          setErr(null);
        }
      } catch (e) {
        if (!cancelled) setErr(e instanceof Error ? e.message : String(e));
      } finally {
        if (!cancelled) setLoading(false);
      }
    };
    fetchOnce();
    const t = setInterval(fetchOnce, REFRESH_MS);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, [userId, filter]);

  const tree = useMemo(() => buildTree(agents), [agents]);

  if (!userId) {
    return (
      <div className="empty">
        <p>Enter a <code>user_id</code> in the top bar to see runs.</p>
      </div>
    );
  }

  return (
    <div className="run-list">
      <div className="toolbar">
        <div className="filter">
          {(["running", "completed", "failed", "cancelled", "all"] as StatusFilter[]).map((s) => (
            <button
              key={s}
              className={s === filter ? "on" : ""}
              onClick={() => setFilter(s)}
            >
              {s}
            </button>
          ))}
        </div>
        <div className="meta">
          {loading && <span className="spin">refreshing…</span>}
          <span>{agents.length} run{agents.length === 1 ? "" : "s"}</span>
        </div>
      </div>
      {err && <div className="err">{err}</div>}
      {tree.length === 0 && !loading && (
        <div className="empty">
          <p>No <code>{filter}</code> runs for <code>{userId}</code>.</p>
        </div>
      )}
      <AgentsTree tree={tree} />
    </div>
  );
}
