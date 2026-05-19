import { useCallback, useState } from "react";
import { Link } from "react-router-dom";
import type { Agent } from "../api";

// AgentsTree renders the parent → children agent hierarchy as a
// nested <ul>. Each node link navigates to /agents/:id; a caret
// button to the left of the status pill toggles whether the
// children subtree is rendered.
//
// Expand state lives in a single Map<agent_id, boolean> owned by
// the tree (not per-node) — that way a render-time effect can
// expand ancestors of a selected node centrally, and a future
// "collapse all" / "expand all" toolbar gets a single setter to
// flip. Default-expanded: missing key OR true is "expanded";
// only an explicit `false` collapses. This preserves the v0.8.19
// behaviour of "everything visible on first render".

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
  const [expandedMap, setExpandedMap] = useState<Map<string, boolean>>(() => new Map());
  // Clone the Map on every write so React picks up the change via
  // reference inequality — Maps are reference-compared like all
  // objects, but their internals aren't.
  const setExpanded = useCallback((id: string, expanded: boolean) => {
    setExpandedMap((prev) => {
      const next = new Map(prev);
      next.set(id, expanded);
      return next;
    });
  }, []);
  return (
    <ul className="tree">
      {tree.map((node) => (
        <AgentsTreeNode
          key={node.agent.agent_id}
          node={node}
          depth={0}
          expandedMap={expandedMap}
          setExpanded={setExpanded}
        />
      ))}
    </ul>
  );
}

interface NodeProps {
  node: TreeNode;
  depth: number;
  expandedMap: Map<string, boolean>;
  setExpanded: (id: string, expanded: boolean) => void;
}

function AgentsTreeNode({ node, depth, expandedMap, setExpanded }: NodeProps) {
  const a = node.agent;
  const hasChildren = node.children.length > 0;
  // Default-expanded: only an explicit `false` collapses.
  const expanded = expandedMap.get(a.agent_id) !== false;
  return (
    <li className={`node depth-${depth} status-${a.status}`}>
      <div className="row">
        <button
          type="button"
          className="tree-caret"
          aria-label={expanded ? "collapse" : "expand"}
          disabled={!hasChildren}
          onClick={(e) => {
            // Don't bubble to the row — the row click navigates via
            // the <Link> on the agent name; the caret is the only
            // expand/collapse affordance.
            e.stopPropagation();
            if (hasChildren) setExpanded(a.agent_id, !expanded);
          }}
        >
          {hasChildren ? (expanded ? "▼" : "▶") : "·"}
        </button>
        <span className={`pill ${a.status}`}>{a.status}</span>
        <Link to={`/agents/${a.agent_id}`} className="agent-link">
          <strong>{a.agent || "(unknown agent)"}</strong>
          <code className="agent-id">{a.agent_id.slice(0, 12)}…</code>
        </Link>
        <span className="model">{a.usage?.model || "—"}</span>
        <span className="time">{relativeTime(a.started_at)}</span>
        {a.error && <span className="error-flag" title={a.error}>error</span>}
      </div>
      {hasChildren && expanded && (
        <ul className="children">
          {node.children.map((c) => (
            <AgentsTreeNode
              key={c.agent.agent_id}
              node={c}
              depth={depth + 1}
              expandedMap={expandedMap}
              setExpanded={setExpanded}
            />
          ))}
        </ul>
      )}
    </li>
  );
}
