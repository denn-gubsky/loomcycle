import { Link } from "react-router-dom";
import type { Agent } from "../api";

// BreadcrumbAncestor is one node in the ancestor chain from root
// down to (but not including) the currently-selected agent.
//
// `inResultSet=true` means the parent's row is in the current
// agents list — we have its name + status, can render a link. When
// false, the parent is out of the result set (e.g. status filter
// hid completed parents); we only have its id, so we render a
// dim slug for context without making it clickable.
export interface BreadcrumbAncestor {
  agent_id: string;
  agent?: string;
  status?: string;
  inResultSet: boolean;
}

export interface BreadcrumbsProps {
  ancestors: BreadcrumbAncestor[];
  selected: Agent | null;
  // When set, clicking a linkable ancestor calls onSelect instead
  // of navigating. The split-view layout (commit 5) uses this to
  // re-target the right pane without a route change. When omitted,
  // we fall back to <Link to=/agents/:id/> for the standalone
  // detail page.
  onSelect?: (agentId: string) => void;
}

export default function Breadcrumbs({ ancestors, selected, onSelect }: BreadcrumbsProps) {
  return (
    <nav className="crumbs-bar" aria-label="agent hierarchy">
      <Link to="/" className="crumb-link crumb-home">
        ← runs
      </Link>
      {ancestors.length > 0 && <span className="crumb-sep">›</span>}
      {ancestors.map((a, i) => (
        <span key={a.agent_id} className="crumb">
          {a.inResultSet ? (
            onSelect ? (
              <button
                type="button"
                className="crumb-link"
                onClick={() => onSelect(a.agent_id)}
              >
                {a.agent || a.agent_id.slice(0, 8)}
              </button>
            ) : (
              <Link to={`/agents/${a.agent_id}`} className="crumb-link">
                {a.agent || a.agent_id.slice(0, 8)}
              </Link>
            )
          ) : (
            <span className="crumb-orphan" title={a.agent_id}>
              {a.agent_id.slice(0, 8)}…
            </span>
          )}
          {(i < ancestors.length - 1 || selected) && <span className="crumb-sep">›</span>}
        </span>
      ))}
      {selected && (
        <span className="crumb crumb-current">
          {selected.agent || selected.agent_id.slice(0, 8)}
        </span>
      )}
    </nav>
  );
}
