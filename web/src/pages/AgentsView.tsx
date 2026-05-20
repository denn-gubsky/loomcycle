import { useCallback, useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { Agent, listAgents } from "../api";
import { useUserId } from "../components/Layout";
import AgentsTreePanel, { type StatusFilter } from "../components/AgentsTreePanel";
import AgentDetailPane from "../components/AgentDetailPane";
import type { BreadcrumbAncestor } from "../components/Breadcrumbs";
import { buildTree } from "../components/AgentsTree";
import Splitter from "../components/Splitter";

// AgentsView is the v0.8.20 split-view replacement for the
// sequential RunList → AgentDetail navigation. Two side-by-side
// panels:
//
//   LEFT  — agents tree + filter toolbar (was the whole RunList)
//   RIGHT — selected agent's transcript + status (was AgentDetail)
//
// Selection lives in the URL search param `?agent=` so reloads and
// deep-links work. The tree's onSelect callback rewrites that
// param via useSearchParams — no React Router navigate (avoids
// history-stack spam during quick sibling clicks).

// Poll cadence matches the previous RunList behaviour. Will become
// SSE/WebSocket-driven when the v0.8.x global-events feed lands.
const REFRESH_MS = 3_000;

export default function AgentsView() {
  const userId = useUserId();
  const [agents, setAgents] = useState<Agent[]>([]);
  const [filter, setFilter] = useState<StatusFilter>("running");
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [searchParams, setSearchParams] = useSearchParams();
  const selectedId = searchParams.get("agent") ?? undefined;

  // Polling + filter-race fix lifted from RunList. Clearing the
  // agents list synchronously on filter change is the bug fix from
  // commit 1; it lives here now that state moved up.
  useEffect(() => {
    if (!userId) {
      setAgents([]);
      return;
    }
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

  // Walk parent_agent_id back to root using the byId lookup over
  // the current agents list. If an ancestor isn't in the current
  // filter result set, insert a dim slug stub and stop (we can't
  // walk further without the row).
  const ancestors: BreadcrumbAncestor[] = useMemo(() => {
    if (!selectedId) return [];
    const byId = new Map(agents.map((a) => [a.agent_id, a]));
    const chain: BreadcrumbAncestor[] = [];
    let cur = byId.get(selectedId);
    while (cur?.parent_agent_id) {
      const p = byId.get(cur.parent_agent_id);
      if (!p) {
        chain.unshift({
          agent_id: cur.parent_agent_id,
          inResultSet: false,
        });
        break;
      }
      chain.unshift({
        agent_id: p.agent_id,
        agent: p.agent,
        status: p.status,
        inResultSet: true,
      });
      cur = p;
    }
    return chain;
  }, [agents, selectedId]);

  const setSelected = useCallback(
    (agentId: string) => {
      const next = new URLSearchParams(searchParams);
      next.set("agent", agentId);
      // Replace history so back-button doesn't accumulate one
      // entry per sibling click.
      setSearchParams(next, { replace: true });
    },
    [searchParams, setSearchParams],
  );

  const onFilterChange = useCallback((f: StatusFilter) => {
    setFilter(f);
  }, []);

  if (!userId) {
    return (
      <div className="empty">
        <p>Enter a <code>user_id</code> in the top bar to see runs.</p>
      </div>
    );
  }

  return (
    <Splitter
      className="agents-view"
      defaultLeftWidth={420}
      minLeftWidth={260}
      minRightWidth={320}
      storageKey="loomcycle.split.agents"
    >
      <div className="left">
        <AgentsTreePanel
          agents={agents}
          tree={tree}
          filter={filter}
          loading={loading}
          err={err}
          userId={userId}
          onFilterChange={onFilterChange}
          selectedId={selectedId}
          onSelect={setSelected}
        />
      </div>
      <div className="right">
        {selectedId ? (
          <AgentDetailPane
            agentId={selectedId}
            ancestors={ancestors}
            onSelect={setSelected}
          />
        ) : (
          <div className="empty">
            <p>Select an agent on the left to view its transcript.</p>
          </div>
        )}
      </div>
    </Splitter>
  );
}
