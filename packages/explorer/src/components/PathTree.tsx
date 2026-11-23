import type { CSSProperties, ReactNode } from "react";
import type { DirListing, DirMap } from "../lib/lazyPathTree";
import { ANNOTATION_ENTRY } from "../lib/annotation";
import {
  docColor,
  effectiveScheme,
  rowTint,
  statusDotColor,
  type DocSummary,
} from "../lib/colorScheme";
import { TreeIcon, TreeItem, TreeMarks, treeKeyDown, type TreeIconKind } from "./TreeRow";

// PathTree renders the RFC AL dirent tree from per-directory listings (see
// lib/lazyPathTree): a directory's children exist only once it has been opened
// and listed. The expand set, the loading and the paging are owned by the
// caller; this component only draws them — a caret per directory, a status row
// while a directory loads / is empty / failed, and a "load more" row while a
// directory has pages left.

export interface PathNode {
  name: string;
  fullPath: string;
  kind: string; // directory | document | volume_mount | memory_entry
  resourceRef?: unknown;
  // hasChildren: a non-directory (a document) that also has names beneath it,
  // so it opens like a directory while its name still opens the document.
  hasChildren?: boolean;
  // listing is this node's children's load state; undefined = never listed
  // (and always undefined for a node that cannot be opened).
  listing?: DirListing;
  children: PathNode[];
}

// buildLazyTree turns the listings into a hierarchy rooted at `dir` ("" =
// root). A one-level listing already includes implicit directories (the runtime
// synthesizes them from deeper descendants), so nothing is synthesized here.
export function buildLazyTree(dirs: DirMap, dir = ""): PathNode[] {
  const listing = dirs.get(dir);
  if (!listing) return [];
  const nodes = listing.entries.map((e): PathNode => {
    // The prefix check keeps a malformed entry (a child spelled like its own
    // directory) from recursing forever.
    const hasChildren = e.kind !== "directory" && e.has_children === true;
    const opens = (e.kind === "directory" || hasChildren) && e.full_path.startsWith(`${dir}/`);
    return {
      name: e.name,
      fullPath: e.full_path,
      kind: e.kind,
      resourceRef: e.resource_ref,
      ...(hasChildren ? { hasChildren } : {}),
      listing: opens ? dirs.get(e.full_path) : undefined,
      children: opens ? buildLazyTree(dirs, e.full_path) : [],
    };
  });
  nodes.sort((a, b) => {
    const ad = a.kind === "directory";
    const bd = b.kind === "directory";
    if (ad !== bd) return ad ? -1 : 1; // directories first
    return a.name.localeCompare(b.name);
  });
  return nodes;
}

// The icon of each entry kind; an unknown kind draws as a document.
const KIND_ICON: Record<string, TreeIconKind> = {
  directory: "folder",
  document: "document",
  volume_mount: "volume",
  memory_entry: "memory",
};

// isAnnotationEntry: the reserved `_annotation` document of a folder.
export function isAnnotationEntry(node: Pick<PathNode, "kind" | "name">): boolean {
  return node.kind === "document" && node.name === ANNOTATION_ENTRY;
}

// nodeBrief is a row's brief (shown in its tooltip): a document's own, and for
// a folder the brief of its `_annotation` document — known only once the
// folder has been listed, which is when its children's summaries arrive. No
// extra call is made for it.
export function nodeBrief(node: PathNode, summaries?: Map<string, DocSummary>): string | undefined {
  if (node.kind === "directory") {
    const ann = node.children.find(isAnnotationEntry);
    return ann ? documentSummaryOf(ann, summaries)?.brief || undefined : undefined;
  }
  if (isAnnotationEntry(node)) return undefined; // its brief is the folder's
  return documentSummaryOf(node, summaries)?.brief || undefined;
}

export interface PathTreeProps {
  tree: PathNode[];
  // root is the root directory's listing (its status/paging rows).
  root?: DirListing;
  expanded: ReadonlySet<string>;
  onToggle: (dir: string) => void;
  // onLoadMore fetches a directory's next page ("" = root); onRetry re-tries a
  // directory whose listing failed.
  onLoadMore: (dir: string) => void;
  onRetry: (dir: string) => void;
  selectedPath?: string;
  onSelect: (node: PathNode) => void;
  // RFC BN: per-document display metadata keyed by document_id, used to color a
  // document row + badge its type/status. Unset → neutral rows (prior behavior).
  summaries?: Map<string, DocSummary>;
}

export default function PathTree(props: PathTreeProps) {
  const { tree, root } = props;
  if (!root || (!root.loaded && root.loading)) {
    return (
      <div className="empty">
        <p>Loading…</p>
      </div>
    );
  }
  if (root.loaded && tree.length === 0 && !root.nextCursor) {
    return (
      <div className="empty">
        <p>This tree is empty. Create a folder or document to begin.</p>
      </div>
    );
  }
  const top = visibleChildren(tree);
  return (
    <ul className="etree path-tree" role="tree" aria-label="Path tree" onKeyDown={treeKeyDown}>
      {top.map((n, i) => (
        <PathTreeNode key={n.fullPath} node={n} depth={0} first={i === 0} {...props} />
      ))}
      <ListingRows dir="" listing={root} {...props} />
    </ul>
  );
}

// ListingRows draws the trailing rows of a directory's children: its status
// (loading / empty / failed) and, while pages remain, a "load more" button.
// Entries are never dropped silently — a clipped directory always ends in one.
function ListingRows({
  dir,
  listing,
  depth,
  onLoadMore,
  onRetry,
}: { dir: string; listing?: DirListing; depth?: number } & PathTreeProps) {
  const rows: ReactNode[] = [];
  // Indented as the directory's children are (the root's rows at depth 0).
  const indent = { "--etree-depth": depth === undefined ? 0 : depth + 1 } as CSSProperties;
  // A folder holding only its `_annotation` draws no child row, so it reads empty.
  const shown = listing ? listing.entries.filter((e) => !isAnnotationEntry(e)).length : 0;
  if (!listing || (listing.loading && listing.entries.length === 0)) {
    rows.push(
      <li key="status" className="path-status" role="none" style={indent}>
        <span className="path-implicit">loading…</span>
      </li>,
    );
  } else if (listing.error) {
    rows.push(
      <li key="status" className="path-status" role="none" style={indent}>
        {/* The root's failure is already the banner above the tree. */}
        {dir !== "" && <span className="path-err">failed to load: {listing.error}</span>}{" "}
        <button type="button" className="path-more" onClick={() => onRetry(dir)}>
          retry
        </button>
      </li>,
    );
  } else if (listing.loaded && shown === 0 && !listing.nextCursor && dir !== "") {
    rows.push(
      <li key="status" className="path-status" role="none" style={indent}>
        <span className="path-implicit">empty</span>
      </li>,
    );
  }
  if (listing?.nextCursor && !listing.error) {
    rows.push(
      <li key="more" className="path-status" role="none" style={indent}>
        <button
          type="button"
          className="path-more"
          disabled={listing.loading}
          onClick={() => onLoadMore(dir)}
        >
          {listing.loading
            ? "loading…"
            : `Load more (${listing.entries.length} shown)`}
        </button>
      </li>,
    );
  }
  return <>{rows}</>;
}

// documentSummaryOf returns the summary for a `document` node (by its dirent's
// document_id), or undefined for a non-document node / missing summary.
function documentSummaryOf(node: PathNode, summaries?: Map<string, DocSummary>): DocSummary | undefined {
  if (node.kind !== "document" || !summaries) return undefined;
  const ref = node.resourceRef as { document_id?: string } | undefined;
  return ref?.document_id ? summaries.get(ref.document_id) : undefined;
}

// visibleChildren drops a folder's `_annotation` entry from the rows drawn: its
// brief is the folder's (nodeBrief reads it from the built tree, so the entry
// stays in the tree and only its row is left out).
function visibleChildren(nodes: PathNode[]): PathNode[] {
  return nodes.filter((n) => !isAnnotationEntry(n));
}

function PathTreeNode(props: { node: PathNode; depth: number; first?: boolean } & PathTreeProps) {
  const { node, depth, first, expanded, onToggle, selectedPath, onSelect, summaries } = props;
  // A directory opens, and so does a document with names beneath it (RFC GC):
  // its caret lists them while the row still opens the document. Children are
  // unknown until listed, so the caret is live even before then.
  const isDir = node.kind === "directory" || node.hasChildren === true;
  const isOpen = isDir && expanded.has(node.fullPath);
  const isSelected = selectedPath === node.fullPath;
  const summary = documentSummaryOf(node, summaries);
  const brief = nodeBrief(node, summaries);
  const scheme = summary ? effectiveScheme(summary.color_scheme) : undefined;
  const coloured = !!summary?.color_enabled;
  return (
    <TreeItem
      className={`path-node kind-${node.kind}`}
      depth={depth}
      expandable={isDir}
      expanded={isOpen}
      onToggle={() => onToggle(node.fullPath)}
      selected={isSelected}
      onSelect={() => onSelect(node)}
      tabbable={isSelected || first === true}
      icon={<TreeIcon kind={isAnnotationEntry(node) ? "annotation" : (KIND_ICON[node.kind] ?? "document")} />}
      name={node.name}
      title={brief ? `${node.fullPath}\n${brief}` : node.fullPath}
      marks={
        <TreeMarks
          type={summary?.type}
          status={summary?.status}
          statusColor={statusDotColor(summary?.status, coloured ? scheme : undefined)}
        />
      }
      style={coloured && scheme ? rowTint(docColor(summary?.type, summary?.status, scheme)) : undefined}
    >
      {isOpen ? (
        <>
          {visibleChildren(node.children).map((c) => (
            <PathTreeNode key={c.fullPath} {...props} node={c} depth={depth + 1} first={false} />
          ))}
          <ListingRows {...props} dir={node.fullPath} listing={node.listing} />
        </>
      ) : undefined}
    </TreeItem>
  );
}
