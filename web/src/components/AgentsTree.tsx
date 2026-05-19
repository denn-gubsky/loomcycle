import { Link } from "react-router-dom";
import type { Agent } from "../api";

// AgentsTree renders the parent → children agent hierarchy as a
// nested <ul>. Each node link navigates to /agents/:id. Stateless;
// the parent owns the agents list and feeds in the built tree.
//
// Extracted from RunList.tsx in commit 2 of the v0.8.20 Web UI
// refactor — pure refactor with byte-identical render.

export interface TreeNode {
  agent: Agent;
  children: TreeNode[];
}

// buildTree groups agents into parent → children. Top-level entries
// are agents whose parent_agent_id is null OR whose parent isn't in
// the current result set (e.g. parent was filtered out by the
// status query).
export function buildTree(agents: Agent[]): TreeNode[] {
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

export function relativeTime(iso: string): string {
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return iso;
  const ms = Date.now() - t;
  if (ms < 60_000) return `${Math.floor(ms / 1000)}s ago`;
  if (ms < 3_600_000) return `${Math.floor(ms / 60_000)}m ago`;
  if (ms < 86_400_000) return `${Math.floor(ms / 3_600_000)}h ago`;
  return new Date(iso).toLocaleString();
}

export interface AgentsTreeProps {
  tree: TreeNode[];
}

export default function AgentsTree({ tree }: AgentsTreeProps) {
  return (
    <ul className="tree">
      {tree.map((node) => (
        <AgentsTreeNode key={node.agent.agent_id} node={node} depth={0} />
      ))}
    </ul>
  );
}

function AgentsTreeNode({ node, depth }: { node: TreeNode; depth: number }) {
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
            <AgentsTreeNode key={c.agent.agent_id} node={c} depth={depth + 1} />
          ))}
        </ul>
      )}
    </li>
  );
}
