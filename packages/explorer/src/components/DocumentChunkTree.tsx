import { useCallback, useContext, useEffect, useState } from "react";
import type { ChunkRow } from "../types";
import { chunkRowTint, effectiveScheme, statusDotColor, type ColorScheme } from "../lib/colorScheme";
import { TreeIcon, TreeItem, TreeMarks, treeKeyDown, type TreeIconKind } from "./TreeRow";
import { CardScopeContext, chunkCard } from "./AnnotationCard";

// DocumentChunkTree renders a document's chunk hierarchy (RFC AK), built from
// the flat query_chunks list by parent_id + position. A single expand Map owned
// by the tree, default-expanded, click-to-select.

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

// typeIcon returns the icon for a media chunk type (RFC BO) so the tree
// distinguishes image / diagram chunks at a glance; undefined for a plain text
// chunk, whose icon cell stays empty (the cell keeps its width, so titles align).
function typeIcon(type: string | undefined): TreeIconKind | undefined {
  if (type === "image") return "image";
  if (type === "mermaid") return "diagram";
  return undefined;
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
  // RFC BN: when colorEnabled, chunk rows are tinted by status via `scheme`
  // (falls back to the default palette). Unset → neutral rows (prior behavior).
  colorEnabled?: boolean;
  scheme?: ColorScheme;
  // The document's own colour (docColor of its type + status). Its row at the
  // top of the tree wears it, and a chunk with no status colour of its own a
  // lighter wash of it (chunkRowTint). Unset → status colours only.
  documentColor?: string;
  // RFC BN P4: chunk ids that have at least one cross-reference edge — those
  // rows get a link mark. Unset → no marks.
  linkedIds?: Set<string>;
  // The document's own type and status (RFC GZ): a row marks its type or status
  // only where it differs from the document's, so a uniform document's outline
  // carries no marks at all. Unset → every row marks what it has.
  documentType?: string;
  documentStatus?: string;
}

export default function DocumentChunkTree({
  tree,
  selectedId,
  onSelect,
  colorEnabled,
  scheme,
  documentColor,
  linkedIds,
  documentType,
  documentStatus,
}: DocumentChunkTreeProps) {
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

  // Resolve the palette once (null → no tinting) and thread it down.
  const tintScheme = colorEnabled ? effectiveScheme(scheme) : null;

  if (tree.length === 0) {
    return (
      <div className="empty">
        <p>No chunks.</p>
      </div>
    );
  }
  // One tab stop: the selected row while it is drawn (every chunk above it
  // open), else the first row.
  const selectedDrawn =
    !!selectedId &&
    (tree.some((n) => n.row.id === selectedId) ||
      (() => {
        const parents = ancestorIds(tree, selectedId);
        return parents.length > 0 && parents.every((id) => expanded.get(id) !== false);
      })());
  const tabStop = selectedDrawn ? selectedId : tree[0].row.id;
  return (
    <ul className="etree chunk-tree" role="tree" aria-label="Chunk outline" onKeyDown={treeKeyDown}>
      {tree.map((n) => (
        <ChunkTreeNode
          key={n.row.id}
          node={n}
          depth={0}
          tabStop={tabStop}
          expanded={expanded}
          toggle={toggle}
          selectedId={selectedId}
          onSelect={onSelect}
          tintScheme={tintScheme}
          documentColor={documentColor}
          isRoot
          linkedIds={linkedIds}
          documentType={documentType}
          documentStatus={documentStatus}
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
  tintScheme: ColorScheme | null;
  documentColor?: string;
  // A top-level node: the document's root chunk, i.e. the document itself.
  isRoot?: boolean;
  linkedIds?: Set<string>;
  documentType?: string;
  documentStatus?: string;
  depth: number;
  // The id of the tree's one tab stop.
  tabStop?: string;
}

function ChunkTreeNode({
  node,
  expanded,
  toggle,
  selectedId,
  onSelect,
  tintScheme,
  documentColor,
  isRoot = false,
  linkedIds,
  documentType,
  documentStatus,
  depth,
  tabStop,
}: NodeProps) {
  const hasChildren = node.children.length > 0;
  const isOpen = expanded.get(node.row.id) !== false; // default-expanded
  const isSelected = selectedId === node.row.id;
  const icon = isRoot ? "document" : typeIcon(node.row.type);
  const type = node.row.type !== documentType ? node.row.type : undefined;
  const status = node.row.status !== documentStatus ? node.row.status : undefined;
  const cardScope = useContext(CardScopeContext);
  return (
    <TreeItem
      className="chunk-node"
      depth={depth}
      expandable={hasChildren}
      expanded={isOpen}
      onToggle={() => toggle(node.row.id)}
      selected={isSelected}
      onSelect={() => onSelect(node)}
      tabbable={node.row.id === tabStop}
      icon={icon ? <TreeIcon kind={icon} /> : undefined}
      name={node.row.title || "(untitled)"}
      card={chunkCard(node.row, cardScope)}
      marks={
        <TreeMarks
          bare
          type={type}
          status={status}
          statusColor={statusDotColor(status, tintScheme ?? undefined)}
          extra={
            linkedIds?.has(node.row.id) ? (
              <span className="etree-mark etree-mark-link" role="img" aria-label="linked">
                <TreeIcon kind="link" />
              </span>
            ) : undefined
          }
        />
      }
      style={chunkRowTint({ status: node.row.status, isRoot, documentColor, scheme: tintScheme })}
    >
      {hasChildren
        ? node.children.map((c) => (
            <ChunkTreeNode
              key={c.row.id}
              node={c}
              expanded={expanded}
              toggle={toggle}
              selectedId={selectedId}
              onSelect={onSelect}
              tintScheme={tintScheme}
              documentColor={documentColor}
              linkedIds={linkedIds}
              documentType={documentType}
              documentStatus={documentStatus}
              depth={depth + 1}
              tabStop={tabStop}
            />
          ))
        : undefined}
    </TreeItem>
  );
}
