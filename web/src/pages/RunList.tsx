import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { Agent, listAgents } from "../api";
import { useUserId } from "../components/Layout";

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

  // Build the parent → children tree. Top-level entries are agents
  // whose parent_agent_id is null OR whose parent isn't in the
  // current result set (e.g. parent completed and was filtered out
  // by the status query).
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
      <ul className="tree">
        {tree.map((node) => (
          <RunNode key={node.agent.agent_id} node={node} depth={0} />
        ))}
      </ul>
    </div>
  );
}

interface TreeNode {
  agent: Agent;
  children: TreeNode[];
}

function buildTree(agents: Agent[]): TreeNode[] {
  const byId = new Map<string, TreeNode>();
  agents.forEach((a) => byId.set(a.agent_id, { agent: a, children: [] }));
  const roots: TreeNode[] = [];
  agents.forEach((a) => {
    const node = byId.get(a.agent_id)!;
    if (a.parent_agent_id && byId.has(a.parent_agent_id)) {
      byId.get(a.parent_agent_id)!.children.push(node);
    } else {
      roots.push(node);
    }
  });
  // Sort by started_at desc at every level so newest run is first.
  const sortRecursive = (nodes: TreeNode[]) => {
    nodes.sort((a, b) => b.agent.started_at.localeCompare(a.agent.started_at));
    nodes.forEach((n) => sortRecursive(n.children));
  };
  sortRecursive(roots);
  return roots;
}

function RunNode({ node, depth }: { node: TreeNode; depth: number }) {
  const a = node.agent;
  return (
    <li className={`node depth-${depth} status-${a.status}`}>
      <div className="row">
        <span className={`pill ${a.status}`}>{a.status}</span>
        <Link to={`/agents/${a.agent_id}`} className="agent-link">
          <strong>{a.agent || "(unknown agent)"}</strong>
          <code className="agent-id">{a.agent_id.slice(0, 12)}…</code>
        </Link>
        <span className="model">{a.usage?.model || "—"}</span>
        <span className="time">{relativeTime(a.started_at)}</span>
        {a.error && <span className="error-flag" title={a.error}>error</span>}
      </div>
      {node.children.length > 0 && (
        <ul className="children">
          {node.children.map((c) => (
            <RunNode key={c.agent.agent_id} node={c} depth={depth + 1} />
          ))}
        </ul>
      )}
    </li>
  );
}

function relativeTime(iso: string): string {
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return iso;
  const ms = Date.now() - t;
  if (ms < 60_000) return `${Math.floor(ms / 1000)}s ago`;
  if (ms < 3_600_000) return `${Math.floor(ms / 60_000)}m ago`;
  if (ms < 86_400_000) return `${Math.floor(ms / 3_600_000)}h ago`;
  return new Date(iso).toLocaleString();
}
