import { useCallback, useEffect, useState } from "react";
import type { PathEntry } from "../types";
import {
  docColor,
  effectiveScheme,
  tintStyle,
  type DocSummary,
} from "../lib/colorScheme";

// PathTree renders the RFC AL dirent tree. The Path tool's `ls recursive`
// returns a FLAT list of dirents; buildPathTree reconstructs the hierarchy,
// synthesizing intermediate `directory` nodes for path segments that have no
// explicit dirent row (implicit dirs, S3-style). A single expand Map owned by
// the tree, default-expanded, click-to-select.

export interface PathNode {
  name: string;
  fullPath: string;
  kind: string; // directory | document | volume_mount | memory_entry
  resourceRef?: unknown;
  explicit: boolean; // false = synthesized intermediate dir (no dirent row)
  children: PathNode[];
}

// buildPathTree turns a flat dirent list into a hierarchy. Each entry's
// full_path is split into segments; ancestors with no explicit row are created
// as implicit `directory` nodes (an explicit dirent later upgrades them).
export function buildPathTree(entries: PathEntry[]): PathNode[] {
  const root: PathNode = {
    name: "",
    fullPath: "",
    kind: "directory",
    explicit: true,
    children: [],
  };
  const byPath = new Map<string, PathNode>([["", root]]);
  const ensure = (fullPath: string): PathNode => {
    const hit = byPath.get(fullPath);
    if (hit) return hit;
    const idx = fullPath.lastIndexOf("/");
    const parentPath = idx <= 0 ? "" : fullPath.slice(0, idx);
    const name = fullPath.slice(idx + 1);
    const parent = ensure(parentPath);
    const node: PathNode = {
      name,
      fullPath,
      kind: "directory",
      explicit: false,
      children: [],
    };
    parent.children.push(node);
    byPath.set(fullPath, node);
    return node;
  };
  for (const e of entries) {
    const node = ensure(e.full_path);
    node.kind = e.kind;
    node.explicit = true;
    node.resourceRef = e.resource_ref;
  }
  const sortRec = (nodes: PathNode[]) => {
    nodes.sort((a, b) => {
      const ad = a.kind === "directory";
      const bd = b.kind === "directory";
      if (ad !== bd) return ad ? -1 : 1; // directories first
      return a.name.localeCompare(b.name);
    });
    nodes.forEach((n) => sortRec(n.children));
  };
  sortRec(root.children);
  return root.children;
}

// parentPathOf returns the canonical parent of a node path ("" = root).
export function parentPathOf(fullPath: string): string {
  const idx = fullPath.lastIndexOf("/");
  return idx <= 0 ? "" : fullPath.slice(0, idx);
}

// collectDocumentIds walks a subtree and returns the document_id of every
// `document` node under it (used to cascade-delete Documents before rm'ing a
// branch, so a branch delete leaves no orphaned Document content).
export function collectDocumentIds(node: PathNode): string[] {
  const ids: string[] = [];
  const walk = (n: PathNode) => {
    if (n.kind === "document") {
      const ref = n.resourceRef as { document_id?: string } | undefined;
      if (ref?.document_id) ids.push(ref.document_id);
    }
    n.children.forEach(walk);
  };
  walk(node);
  return ids;
}

const KIND_ICON: Record<string, string> = {
  directory: "📁",
  document: "📄",
  volume_mount: "💾",
  memory_entry: "🧠",
};

export interface PathTreeProps {
  tree: PathNode[];
  selectedPath?: string;
  onSelect: (node: PathNode) => void;
  // RFC BN: per-document display metadata keyed by document_id, used to color a
  // document row + badge its type/status. Unset → neutral rows (prior behavior).
  summaries?: Map<string, DocSummary>;
}

export default function PathTree({ tree, selectedPath, onSelect, summaries }: PathTreeProps) {
  const [expanded, setExpanded] = useState<Map<string, boolean>>(() => new Map());
  const toggle = useCallback((p: string) => {
    setExpanded((prev) => {
      const next = new Map(prev);
      next.set(p, prev.get(p) === false ? true : false);
      return next;
    });
  }, []);

  // Auto-expand the ancestors of the selection so a post-refresh / deep
  // selection isn't buried under a collapsed parent.
  useEffect(() => {
    if (!selectedPath) return;
    setExpanded((prev) => {
      let mutated = false;
      const next = new Map(prev);
      let p = parentPathOf(selectedPath);
      while (p !== "") {
        if (next.get(p) === false) {
          next.set(p, true);
          mutated = true;
        }
        p = parentPathOf(p);
      }
      return mutated ? next : prev;
    });
  }, [selectedPath]);

  if (tree.length === 0) {
    return (
      <div className="empty">
        <p>This tree is empty. Create a folder or document to begin.</p>
      </div>
    );
  }
  return (
    <ul className="tree path-tree">
      {tree.map((n) => (
        <PathTreeNode
          key={n.fullPath}
          node={n}
          expanded={expanded}
          toggle={toggle}
          selectedPath={selectedPath}
          onSelect={onSelect}
          summaries={summaries}
        />
      ))}
    </ul>
  );
}

interface NodeProps {
  node: PathNode;
  expanded: Map<string, boolean>;
  toggle: (p: string) => void;
  selectedPath?: string;
  onSelect: (node: PathNode) => void;
  summaries?: Map<string, DocSummary>;
}

// documentSummaryOf returns the summary for a `document` node (by its dirent's
// document_id), or undefined for a non-document node / missing summary.
function documentSummaryOf(node: PathNode, summaries?: Map<string, DocSummary>): DocSummary | undefined {
  if (node.kind !== "document" || !summaries) return undefined;
  const ref = node.resourceRef as { document_id?: string } | undefined;
  return ref?.document_id ? summaries.get(ref.document_id) : undefined;
}

function PathTreeNode({ node, expanded, toggle, selectedPath, onSelect, summaries }: NodeProps) {
  const hasChildren = node.children.length > 0;
  const isOpen = expanded.get(node.fullPath) !== false; // default-expanded
  const isSelected = selectedPath === node.fullPath;
  const summary = documentSummaryOf(node, summaries);
  const rowStyle =
    summary && summary.color_enabled
      ? tintStyle(docColor(summary.type, summary.status, effectiveScheme(summary.color_scheme)))
      : undefined;
  return (
    <li className={`node path-node kind-${node.kind} ${isSelected ? "selected" : ""}`}>
      <div className="row path-row" style={rowStyle}>
        <button
          type="button"
          className="tree-caret"
          aria-label={isOpen ? "collapse" : "expand"}
          disabled={!hasChildren}
          onClick={(e) => {
            e.stopPropagation();
            if (hasChildren) toggle(node.fullPath);
          }}
        >
          {hasChildren ? (isOpen ? "▼" : "▶") : "·"}
        </button>
        <button
          type="button"
          className="path-link"
          onClick={() => onSelect(node)}
          title={node.fullPath}
        >
          <span className="path-kind-icon" aria-hidden>
            {KIND_ICON[node.kind] ?? "•"}
          </span>
          <span className="path-name">{node.name}</span>
          {summary?.type && <span className="chunk-badge">{summary.type}</span>}
          {summary?.status && <span className="chunk-badge chunk-status">{summary.status}</span>}
          {!node.explicit && (
            <span className="path-implicit" title="Implicit directory (no stored entry)">
              implicit
            </span>
          )}
        </button>
      </div>
      {hasChildren && isOpen && (
        <ul className="children path-children">
          {node.children.map((c) => (
            <PathTreeNode
              key={c.fullPath}
              node={c}
              expanded={expanded}
              toggle={toggle}
              selectedPath={selectedPath}
              onSelect={onSelect}
              summaries={summaries}
            />
          ))}
        </ul>
      )}
    </li>
  );
}
