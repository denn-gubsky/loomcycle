import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import type { Agent } from "../api";

// AgentsTree renders the parent → children agent hierarchy as a
// nested <ul>. A caret button on each row toggles whether the
// children subtree is rendered. The row's agent-name affordance
// either navigates to /agents/:id (standalone) or fires onSelect
// (split-view; commit 5).
//
// Expand state lives in a single Map<agent_id, boolean> owned by
// the tree (not per-node) so a render-time effect can expand
// ancestors of a selected node centrally, and a future
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
  // Optional selection highlight + click callback. When onSelect
  // is set, clicking the row fires the callback (split-view).
  // When omitted, the row navigates via <Link to=/agents/:id/>
  // (standalone tree usage; legacy /agents/:agentId route).
  selectedId?: string;
  onSelect?: (agentId: string) => void;
}

export default function AgentsTree({ tree, selectedId, onSelect }: AgentsTreeProps) {
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

  // Auto-expand the ancestors of the currently-selected node so a
  // deep-link reload doesn't leave the selection buried under a
  // collapsed parent. Walks parent_agent_id back to root using the
  // tree itself as the lookup (each node is reachable via DFS).
  useEffect(() => {
    if (!selectedId) return;
    const parentIds = collectAncestorIds(tree, selectedId);
    if (parentIds.length === 0) return;
    setExpandedMap((prev) => {
      let mutated = false;
      const next = new Map(prev);
      for (const id of parentIds) {
        if (next.get(id) === false) {
          next.set(id, true);
          mutated = true;
        }
      }
      return mutated ? next : prev;
    });
  }, [tree, selectedId]);

  return (
    <ul className="tree">
      {tree.map((node) => (
        <AgentsTreeNode
          key={node.agent.agent_id}
          node={node}
          depth={0}
          expandedMap={expandedMap}
          setExpanded={setExpanded}
          selectedId={selectedId}
          onSelect={onSelect}
        />
      ))}
    </ul>
  );
}

// collectAncestorIds walks the tree to find the node with id ===
// selectedId and returns every parent_agent_id on the path from
// root down to (but not including) that node. Empty if not found.
function collectAncestorIds(roots: TreeNode[], selectedId: string): string[] {
  const path: string[] = [];
  const walk = (nodes: TreeNode[], parents: string[]): boolean => {
    for (const n of nodes) {
      if (n.agent.agent_id === selectedId) {
        path.push(...parents);
        return true;
      }
      if (walk(n.children, [...parents, n.agent.agent_id])) return true;
    }
    return false;
  };
  walk(roots, []);
  return path;
}

interface NodeProps {
  node: TreeNode;
  depth: number;
  expandedMap: Map<string, boolean>;
  setExpanded: (id: string, expanded: boolean) => void;
  selectedId?: string;
  onSelect?: (agentId: string) => void;
}

function AgentsTreeNode({ node, depth, expandedMap, setExpanded, selectedId, onSelect }: NodeProps) {
  const a = node.agent;
  const hasChildren = node.children.length > 0;
  // Default-expanded: only an explicit `false` collapses.
  const expanded = expandedMap.get(a.agent_id) !== false;
  const isSelected = selectedId === a.agent_id;
  return (
    <li className={`node depth-${depth} status-${a.status} ${awaitClass(a.awaited_state, a.status)} ${isSelected ? "selected" : ""}`}>
      <div className="row">
        <button
          type="button"
          className="tree-caret"
          aria-label={expanded ? "collapse" : "expand"}
          disabled={!hasChildren}
          onClick={(e) => {
            // Don't bubble to the row — the row click navigates
            // or selects via the affordance below; the caret is
            // the only expand/collapse control.
            e.stopPropagation();
            if (hasChildren) setExpanded(a.agent_id, !expanded);
          }}
        >
          {hasChildren ? (expanded ? "▼" : "▶") : "·"}
        </button>
        <span className="row-badges">
          <span className={`pill ${a.status}`}>{a.status}</span>
          {a.interactive && (
            <span className="pill-interactive" title="Interactive session — parks for operator steering; re-attachable in the run terminal">
              interactive
            </span>
          )}
          {a.resident && (
            <span className="pill-resident" title="Resident sub-agent — a persistent interactive child driven via Agent open/send/close">
              resident{a.resident_state ? ` · ${a.resident_state}` : ""}
            </span>
          )}
        </span>
        {onSelect ? (
          <button
            type="button"
            className="agent-link"
            onClick={() => onSelect(a.agent_id)}
          >
            <strong>{a.agent || "(unknown agent)"}</strong>
            <code className="agent-id">{a.agent_id.slice(0, 12)}…</code>
          </button>
        ) : (
          <Link to={`/agents/${a.agent_id}`} className="agent-link">
            <strong>{a.agent || "(unknown agent)"}</strong>
            <code className="agent-id">{a.agent_id.slice(0, 12)}…</code>
          </Link>
        )}
        <span className="model">{a.usage?.model || "—"}</span>
        {a.replica_id && (
          <span
            className="replica"
            style={{ background: replicaHue(a.replica_id) }}
            title={`Running on ${a.replica_id}`}
          >
            {a.replica_id}
          </span>
        )}
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
              selectedId={selectedId}
              onSelect={onSelect}
            />
          ))}
        </ul>
      )}
    </li>
  );
}

// awaitClass maps a row to one of three tint classes. The server
// only emits awaited_state for the channel / interrupted cases;
// every other running row falls through to await-running (so the
// whole running-status set is visually distinguishable in the
// tree). Terminal rows (completed / failed / cancelled) return no
// extra class — they keep the existing status-* styling.
function awaitClass(state: Agent["awaited_state"] | undefined, status: Agent["status"]): string {
  if (state === "channel") return "await-channel";
  if (state === "interrupted") return "await-interrupted";
  if (status === "running") return "await-running";
  return "";
}

// replicaHue maps a replica_id to a stable HSL background so different
// replicas get visually distinct chips without an ever-growing color
// table. djb2 hash → hue; fixed low-saturation pastel keeps it
// readable on both light and dark themes alongside the existing
// .pill / .model chips.
function replicaHue(replicaID: string): string {
  let hash = 5381;
  for (let i = 0; i < replicaID.length; i++) {
    hash = ((hash << 5) + hash + replicaID.charCodeAt(i)) | 0;
  }
  const hue = Math.abs(hash) % 360;
  return `hsl(${hue}, 55%, 82%)`;
}
