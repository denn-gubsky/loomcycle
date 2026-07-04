import { useCallback, useEffect, useState } from "react";
import type { ChunkRow } from "../types";

// DocumentChunkTree renders a document's chunk hierarchy (RFC AK), built from
// the flat query_chunks list by parent_id + position. Mirrors PathTree: a single
// expand Map owned by the tree, default-expanded, click-to-select.

export interface ChunkNode {
  row: ChunkRow;
  children: ChunkNode[];
}

// buildChunkTree groups chunks into parent → children (position-ordered). A
// chunk whose parent_id is absent or not in the set is a root (the document's
// root chunk; defensively also any orphan).
export function buildChunkTree(chunks: ChunkRow[]): ChunkNode[] {
  const byId = new Map<string, ChunkNode>();
  chunks.forEach((c) => byId.set(c.id, { row: c, children: [] }));
  const roots: ChunkNode[] = [];
  chunks.forEach((c) => {
    const node = byId.get(c.id)!;
    if (c.parent_id && byId.has(c.parent_id)) byId.get(c.parent_id)!.children.push(node);
    else roots.push(node);
  });
  const sortRec = (nodes: ChunkNode[]) => {
    nodes.sort((a, b) => a.row.position - b.row.position);
    nodes.forEach((n) => sortRec(n.children));
  };
  sortRec(roots);
  return roots;
}

// findChunkNode locates a node by id within a chunk forest.
export function findChunkNode(nodes: ChunkNode[], id: string): ChunkNode | undefined {
  for (const n of nodes) {
    if (n.row.id === id) return n;
    const hit = findChunkNode(n.children, id);
    if (hit) return hit;
  }
  return undefined;
}

export interface DocumentChunkTreeProps {
  tree: ChunkNode[];
  selectedId?: string;
  onSelect: (node: ChunkNode) => void;
}

export default function DocumentChunkTree({ tree, selectedId, onSelect }: DocumentChunkTreeProps) {
  const [expanded, setExpanded] = useState<Map<string, boolean>>(() => new Map());
  const toggle = useCallback((id: string) => {
    setExpanded((prev) => {
      const next = new Map(prev);
      next.set(id, prev.get(id) === false ? true : false);
      return next;
    });
  }, []);

  // Auto-expand ancestors of the selection.
  useEffect(() => {
    if (!selectedId) return;
    const parents = ancestorIds(tree, selectedId);
    if (parents.length === 0) return;
    setExpanded((prev) => {
      let mutated = false;
      const next = new Map(prev);
      for (const id of parents) {
        if (next.get(id) === false) {
          next.set(id, true);
          mutated = true;
        }
      }
      return mutated ? next : prev;
    });
  }, [tree, selectedId]);

  if (tree.length === 0) {
    return (
      <div className="empty">
        <p>No chunks.</p>
      </div>
    );
  }
  return (
    <ul className="tree chunk-tree">
      {tree.map((n) => (
        <ChunkTreeNode
          key={n.row.id}
          node={n}
          expanded={expanded}
          toggle={toggle}
          selectedId={selectedId}
          onSelect={onSelect}
        />
      ))}
    </ul>
  );
}

function ancestorIds(roots: ChunkNode[], id: string): string[] {
  const path: string[] = [];
  const walk = (nodes: ChunkNode[], parents: string[]): boolean => {
    for (const n of nodes) {
      if (n.row.id === id) {
        path.push(...parents);
        return true;
      }
      if (walk(n.children, [...parents, n.row.id])) return true;
    }
    return false;
  };
  walk(roots, []);
  return path;
}

interface NodeProps {
  node: ChunkNode;
  expanded: Map<string, boolean>;
  toggle: (id: string) => void;
  selectedId?: string;
  onSelect: (node: ChunkNode) => void;
}

function ChunkTreeNode({ node, expanded, toggle, selectedId, onSelect }: NodeProps) {
  const hasChildren = node.children.length > 0;
  const isOpen = expanded.get(node.row.id) !== false; // default-expanded
  const isSelected = selectedId === node.row.id;
  return (
    <li className={`node chunk-node ${isSelected ? "selected" : ""}`}>
      <div className="row chunk-row">
        <button
          type="button"
          className="tree-caret"
          aria-label={isOpen ? "collapse" : "expand"}
          disabled={!hasChildren}
          onClick={(e) => {
            e.stopPropagation();
            if (hasChildren) toggle(node.row.id);
          }}
        >
          {hasChildren ? (isOpen ? "▼" : "▶") : "·"}
        </button>
        <button
          type="button"
          className="chunk-link"
          onClick={() => onSelect(node)}
          title={node.row.title}
        >
          <span className="chunk-title">{node.row.title || "(untitled)"}</span>
          {node.row.type && <span className="chunk-badge">{node.row.type}</span>}
          {node.row.status && <span className="chunk-badge chunk-status">{node.row.status}</span>}
        </button>
      </div>
      {hasChildren && isOpen && (
        <ul className="children chunk-children">
          {node.children.map((c) => (
            <ChunkTreeNode
              key={c.row.id}
              node={c}
              expanded={expanded}
              toggle={toggle}
              selectedId={selectedId}
              onSelect={onSelect}
            />
          ))}
        </ul>
      )}
    </li>
  );
}
