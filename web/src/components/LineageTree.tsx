import { useCallback, useEffect, useState } from "react";
import type { DefRow } from "../api";

// LineageTree renders parent → child substrate versions as a nested
// <ul>. Mirror of AgentsTree.tsx but generalised over the substrate
// DefRow shape (def_id / parent_def_id / version / retired /
// bootstrapped_from_static). One component backs the Agent / Skill /
// MCP Server sub-tabs in the Library view.
//
// Expand state is per-component (Map<def_id, boolean>); default-true
// for v1 — operators inspecting lineage usually want the full chain
// visible. Selected row is highlighted; clicking fires onSelect.

export interface LineageNode {
  row: DefRow;
  children: LineageNode[];
}

// buildLineageTree groups rows into parent → child nodes via
// parent_def_id. Roots are rows whose parent_def_id is empty OR
// missing from the input set (defensive against partial pages).
// Returned roots are sorted by version ASC so v1 sits at the top.
export function buildLineageTree(rows: DefRow[]): LineageNode[] {
  const byID = new Map<string, LineageNode>();
  rows.forEach((r) => byID.set(r.def_id, { row: r, children: [] }));
  const roots: LineageNode[] = [];
  rows.forEach((r) => {
    const node = byID.get(r.def_id)!;
    if (r.parent_def_id && byID.has(r.parent_def_id)) {
      byID.get(r.parent_def_id)!.children.push(node);
    } else {
      roots.push(node);
    }
  });
  const sortByVersionAsc = (nodes: LineageNode[]) => {
    nodes.sort((a, b) => a.row.version - b.row.version);
    nodes.forEach((n) => sortByVersionAsc(n.children));
  };
  sortByVersionAsc(roots);
  return roots;
}

export interface LineageTreeProps {
  tree: LineageNode[];
  // Highlight + click callback. activeDefID — the row stored in the
  // *_def_active overlay; gets the "active ★" chip. selectedDefID —
  // the currently-clicked row (independent of active).
  activeDefID?: string;
  selectedDefID?: string;
  onSelect?: (defID: string) => void;
}

export default function LineageTree({
  tree,
  activeDefID,
  selectedDefID,
  onSelect,
}: LineageTreeProps) {
  const [expanded, setExpanded] = useState<Map<string, boolean>>(() => new Map());

  // Auto-expand ancestors of the selected node so a deep-link / fresh
  // click never leaves the selection buried under a collapsed parent.
  useEffect(() => {
    if (!selectedDefID) return;
    const ancestors = collectAncestors(tree, selectedDefID);
    if (ancestors.length === 0) return;
    setExpanded((prev) => {
      let mutated = false;
      const next = new Map(prev);
      for (const id of ancestors) {
        if (next.get(id) === false) {
          next.set(id, true);
          mutated = true;
        }
      }
      return mutated ? next : prev;
    });
  }, [tree, selectedDefID]);

  const toggleExpanded = useCallback((id: string) => {
    setExpanded((prev) => {
      const next = new Map(prev);
      // Default-expanded: missing key OR true is expanded; only an
      // explicit false collapses. So clicking flips the local state.
      const currentExpanded = next.get(id) !== false;
      next.set(id, !currentExpanded);
      return next;
    });
  }, []);

  return (
    <ul className="lineage-tree" role="tree">
      {tree.map((node) => (
        <LineageNodeRow
          key={node.row.def_id}
          node={node}
          depth={0}
          expanded={expanded}
          toggleExpanded={toggleExpanded}
          activeDefID={activeDefID}
          selectedDefID={selectedDefID}
          onSelect={onSelect}
        />
      ))}
    </ul>
  );
}

function LineageNodeRow({
  node,
  depth,
  expanded,
  toggleExpanded,
  activeDefID,
  selectedDefID,
  onSelect,
}: {
  node: LineageNode;
  depth: number;
  expanded: Map<string, boolean>;
  toggleExpanded: (id: string) => void;
  activeDefID?: string;
  selectedDefID?: string;
  onSelect?: (defID: string) => void;
}) {
  const isExpanded = expanded.get(node.row.def_id) !== false;
  const hasChildren = node.children.length > 0;
  const isActive = node.row.def_id === activeDefID;
  const isSelected = node.row.def_id === selectedDefID;
  const row = node.row;

  return (
    <li className={isSelected ? "lineage-row lineage-row-selected" : "lineage-row"}>
      <div className="lineage-row-line" style={{ paddingLeft: `${depth * 16}px` }}>
        {hasChildren ? (
          <button
            type="button"
            className="lineage-caret"
            onClick={() => toggleExpanded(row.def_id)}
            aria-label={isExpanded ? "Collapse" : "Expand"}
          >
            {isExpanded ? "▾" : "▸"}
          </button>
        ) : (
          <span className="lineage-caret lineage-caret-empty">·</span>
        )}
        <button
          type="button"
          className="lineage-row-button"
          onClick={() => onSelect?.(row.def_id)}
        >
          <span className="lineage-version">v{row.version}</span>
          {isActive && <span className="def-chip def-chip-active">active ★</span>}
          {row.retired && <span className="def-chip def-chip-retired">retired</span>}
          {row.bootstrapped_from_static && (
            <span className="def-chip def-chip-bootstrap">bootstrapped</span>
          )}
          <span className="lineage-defid mono">{shortDefID(row.def_id)}</span>
        </button>
      </div>
      {isExpanded && hasChildren && (
        <ul role="group">
          {node.children.map((child) => (
            <LineageNodeRow
              key={child.row.def_id}
              node={child}
              depth={depth + 1}
              expanded={expanded}
              toggleExpanded={toggleExpanded}
              activeDefID={activeDefID}
              selectedDefID={selectedDefID}
              onSelect={onSelect}
            />
          ))}
        </ul>
      )}
    </li>
  );
}

function collectAncestors(tree: LineageNode[], targetID: string): string[] {
  const ancestors: string[] = [];
  function walk(nodes: LineageNode[], path: string[]): boolean {
    for (const node of nodes) {
      if (node.row.def_id === targetID) {
        ancestors.push(...path);
        return true;
      }
      if (walk(node.children, [...path, node.row.def_id])) return true;
    }
    return false;
  }
  walk(tree, []);
  return ancestors;
}

function shortDefID(defID: string): string {
  // def_<24hex> → def_xxxxxxxx (8 chars after prefix). Substrate UI
  // doesn't need the full 24; full id available via tooltip in caller.
  if (defID.length <= 12) return defID;
  return defID.slice(0, 12) + "…";
}
