import type { Agent } from "../api";
import AgentsTree, { type TreeNode } from "./AgentsTree";

// AgentsTreePanel is the left side of the split agents view: a
// toolbar (filter buttons + count + refreshing indicator) on top
// and the scrollable agents tree below.
//
// Pure presentation — the parent (<AgentsView>) owns the agents
// list, filter state, polling, and the bug-fix-on-filter-change
// clear logic. This component just renders.

export type StatusFilter = "all" | "running" | "completed" | "failed" | "cancelled";

export const STATUS_FILTERS: StatusFilter[] = [
  "running", "completed", "failed", "cancelled", "all",
];

export interface AgentsTreePanelProps {
  agents: Agent[];
  tree: TreeNode[];
  filter: StatusFilter;
  loading: boolean;
  err: string | null;
  userId: string;
  onFilterChange: (f: StatusFilter) => void;
  selectedId?: string;
  onSelect?: (agentId: string) => void;
}

export default function AgentsTreePanel({
  agents,
  tree,
  filter,
  loading,
  err,
  userId,
  onFilterChange,
  selectedId,
  onSelect,
}: AgentsTreePanelProps) {
  return (
    <div className="agents-tree-panel">
      <div className="toolbar">
        <div className="filter">
          {STATUS_FILTERS.map((s) => (
            <button
              key={s}
              type="button"
              className={s === filter ? "on" : ""}
              onClick={() => onFilterChange(s)}
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
      <div className="tree-scroll">
        <AgentsTree tree={tree} selectedId={selectedId} onSelect={onSelect} />
      </div>
    </div>
  );
}
