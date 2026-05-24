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
  // Inline-detail renderer (v0.9.x Library v2). When set, each row
  // gets a content-toggle chevron at the row-end; clicking it
  // expands the definition body inline under the row. Independent
  // of the parent/child collapse chevron at the row-start.
  renderDefinition?: (row: DefRow) => React.ReactNode;
  // v0.10.4 — per-row mutation callbacks. When wired, each row
  // renders Edit / Retire / Promote buttons in the row header.
  // Edit forks the row's definition into a new version; Retire
  // flips the retired flag on the row; Promote sets the active
  // pointer for the name to this row. Buttons stopPropagation so
  // they don't toggle content / selection on click.
  onEditRow?: (row: DefRow) => void;
  onRetireRow?: (row: DefRow) => void;
  onPromoteRow?: (row: DefRow) => void;
}

export default function LineageTree({
  tree,
  activeDefID,
  selectedDefID,
  onSelect,
  renderDefinition,
  onEditRow,
  onRetireRow,
  onPromoteRow,
}: LineageTreeProps) {
  const [expanded, setExpanded] = useState<Map<string, boolean>>(() => new Map());
  const [detailExpanded, setDetailExpanded] = useState<Map<string, boolean>>(
    () => new Map(),
  );

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

  // Default-COLLAPSED for the content detail toggle — opposite of the
  // parent/child collapse default. Operators opt into content view
  // per-row; the detail body can be long and we don't want every
  // row's body rendered by default.
  const toggleDetail = useCallback((id: string) => {
    setDetailExpanded((prev) => {
      const next = new Map(prev);
      const currentExpanded = next.get(id) === true;
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
          detailExpanded={detailExpanded}
          toggleDetail={toggleDetail}
          activeDefID={activeDefID}
          selectedDefID={selectedDefID}
          onSelect={onSelect}
          renderDefinition={renderDefinition}
          onEditRow={onEditRow}
          onRetireRow={onRetireRow}
          onPromoteRow={onPromoteRow}
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
  detailExpanded,
  toggleDetail,
  activeDefID,
  selectedDefID,
  onSelect,
  renderDefinition,
  onEditRow,
  onRetireRow,
  onPromoteRow,
}: {
  node: LineageNode;
  depth: number;
  expanded: Map<string, boolean>;
  toggleExpanded: (id: string) => void;
  detailExpanded: Map<string, boolean>;
  toggleDetail: (id: string) => void;
  activeDefID?: string;
  selectedDefID?: string;
  onSelect?: (defID: string) => void;
  renderDefinition?: (row: DefRow) => React.ReactNode;
  onEditRow?: (row: DefRow) => void;
  onRetireRow?: (row: DefRow) => void;
  onPromoteRow?: (row: DefRow) => void;
}) {
  const isExpanded = expanded.get(node.row.def_id) !== false;
  const isDetailOpen = detailExpanded.get(node.row.def_id) === true;
  const hasChildren = node.children.length > 0;
  const isActive = node.row.def_id === activeDefID;
  const isSelected = node.row.def_id === selectedDefID;
  const row = node.row;

  // Whole-row click toggles content visibility (when renderDefinition is
  // wired) AND records selection (highlights + auto-expands ancestors).
  // The tree caret for parent/child collapse stays separate — its own
  // click stops propagation so it never accidentally toggles content.
  const handleRowClick = () => {
    onSelect?.(row.def_id);
    if (renderDefinition) toggleDetail(row.def_id);
  };
  const handleRowKey = (e: React.KeyboardEvent<HTMLDivElement>) => {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      handleRowClick();
    }
  };

  return (
    <li className={isSelected ? "lineage-row lineage-row-selected" : "lineage-row"}>
      <div
        className="lineage-row-line"
        style={{ paddingLeft: `${depth * 16}px` }}
        onClick={handleRowClick}
        onKeyDown={handleRowKey}
        role={renderDefinition ? "button" : undefined}
        tabIndex={renderDefinition ? 0 : undefined}
        aria-expanded={renderDefinition ? isDetailOpen : undefined}
      >
        {hasChildren ? (
          <button
            type="button"
            className="lineage-caret"
            onClick={(e) => {
              e.stopPropagation();
              toggleExpanded(row.def_id);
            }}
            aria-label={isExpanded ? "Collapse" : "Expand"}
          >
            {isExpanded ? "▾" : "▸"}
          </button>
        ) : (
          <span className="lineage-caret lineage-caret-empty">·</span>
        )}
        <span className="lineage-row-content">
          {row.version === 0 && row.def_id.startsWith("static:") ? (
            <span className="lineage-version">static</span>
          ) : (
            <span className="lineage-version">v{row.version}</span>
          )}
          {row.version === 0 && row.def_id.startsWith("static:") && (
            <span className="def-chip def-chip-static">static</span>
          )}
          {isActive && <span className="def-chip def-chip-active">active ★</span>}
          {row.retired && <span className="def-chip def-chip-retired">retired</span>}
          {row.bootstrapped_from_static && (
            <span className="def-chip def-chip-bootstrap">bootstrapped</span>
          )}
          <span className="lineage-defid mono">{shortDefID(row.def_id)}</span>
        </span>
        {renderDefinition && (
          <span
            className="lineage-content-indicator"
            aria-hidden="true"
          >
            {isDetailOpen ? "▾" : "▸"} content
          </span>
        )}
        {(onEditRow || onRetireRow || onPromoteRow) && (
          <span className="lineage-row-actions">
            {onEditRow && (
              <button
                type="button"
                className="lineage-row-action"
                onClick={(e) => {
                  e.stopPropagation();
                  onEditRow(row);
                }}
                title={
                  row.def_id.startsWith("static:")
                    ? "First fork bootstraps a v1 from yaml, then attaches this fork as v2."
                    : "Fork a new version from this row"
                }
              >
                {row.def_id.startsWith("static:") ? "Edit (forks from yaml)" : "Edit ✎"}
              </button>
            )}
            {onPromoteRow && !isActive && !row.def_id.startsWith("static:") && !row.retired && (
              <button
                type="button"
                className="lineage-row-action"
                onClick={(e) => {
                  e.stopPropagation();
                  onPromoteRow(row);
                }}
                title="Set this version as the active one"
              >
                Promote ▲
              </button>
            )}
            {onRetireRow && !row.def_id.startsWith("static:") && !row.retired && (
              <button
                type="button"
                className="lineage-row-action lineage-row-action-danger"
                onClick={(e) => {
                  e.stopPropagation();
                  onRetireRow(row);
                }}
                title={
                  isActive
                    ? "Retire the active version (leaves the entry with no active version until you promote another)"
                    : "Retire this inactive version (stays in lineage; agents stop seeing it)"
                }
              >
                Retire ⊘
              </button>
            )}
          </span>
        )}
      </div>
      {isDetailOpen && renderDefinition && (
        <div
          className="lineage-row-detail"
          style={{ paddingLeft: `${(depth + 1) * 16 + 12}px` }}
        >
          <div className="lineage-detail-header">
            <span className="mono">{row.def_id}</span>
            {row.parent_def_id && (
              <span className="lineage-detail-meta">← {row.parent_def_id}</span>
            )}
            {row.created_at && (
              <span className="lineage-detail-meta">
                created {new Date(row.created_at).toLocaleString()}
              </span>
            )}
            {row.content_sha256 && (
              <span
                className="mono lineage-detail-meta"
                title={row.content_sha256}
              >
                {shortenSHA(row.content_sha256)}
              </span>
            )}
          </div>
          {renderDefinition(row)}
        </div>
      )}
      {isExpanded && hasChildren && (
        <ul role="group">
          {node.children.map((child) => (
            <LineageNodeRow
              key={child.row.def_id}
              node={child}
              depth={depth + 1}
              expanded={expanded}
              toggleExpanded={toggleExpanded}
              detailExpanded={detailExpanded}
              toggleDetail={toggleDetail}
              activeDefID={activeDefID}
              selectedDefID={selectedDefID}
              onSelect={onSelect}
              renderDefinition={renderDefinition}
              onEditRow={onEditRow}
              onRetireRow={onRetireRow}
              onPromoteRow={onPromoteRow}
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

function shortenSHA(s: string): string {
  // `sha256:64hex` → compact form for the inline-detail header; hover
  // shows the full value via the title attribute.
  if (s.length <= 18) return s;
  return s.slice(0, 14) + "…";
}
