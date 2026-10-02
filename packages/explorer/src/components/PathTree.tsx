import type { ReactNode } from "react";
import type { DirListing, DirMap } from "../lib/lazyPathTree";
import {
  docColor,
  effectiveScheme,
  tintStyle,
  type DocSummary,
} from "../lib/colorScheme";

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
  // listing is this directory's load state; undefined = never listed (and
  // always undefined for a non-directory).
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
    const isDir = e.kind === "directory" && e.full_path.startsWith(`${dir}/`);
    return {
      name: e.name,
      fullPath: e.full_path,
      kind: e.kind,
      resourceRef: e.resource_ref,
      listing: isDir ? dirs.get(e.full_path) : undefined,
      children: isDir ? buildLazyTree(dirs, e.full_path) : [],
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

const KIND_ICON: Record<string, string> = {
  directory: "📁",
  document: "📄",
  volume_mount: "💾",
  memory_entry: "🧠",
};

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
  return (
    <ul className="tree path-tree">
      {tree.map((n) => (
        <PathTreeNode key={n.fullPath} node={n} {...props} />
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
  onLoadMore,
  onRetry,
}: { dir: string; listing?: DirListing } & PathTreeProps) {
  const rows: ReactNode[] = [];
  if (!listing || (listing.loading && listing.entries.length === 0)) {
    rows.push(
      <li key="status" className="node path-status">
        <span className="path-implicit">loading…</span>
      </li>,
    );
  } else if (listing.error) {
    rows.push(
      <li key="status" className="node path-status">
        {/* The root's failure is already the banner above the tree. */}
        {dir !== "" && <span className="path-err">failed to load: {listing.error}</span>}{" "}
        <button type="button" className="path-more" onClick={() => onRetry(dir)}>
          retry
        </button>
      </li>,
    );
  } else if (listing.loaded && listing.entries.length === 0 && dir !== "") {
    rows.push(
      <li key="status" className="node path-status">
        <span className="path-implicit">empty</span>
      </li>,
    );
  }
  if (listing?.nextCursor && !listing.error) {
    rows.push(
      <li key="more" className="node path-status">
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

function PathTreeNode(props: { node: PathNode } & PathTreeProps) {
  const { node, expanded, onToggle, selectedPath, onSelect, summaries } = props;
  // Only a directory can be opened: its children are unknown until it is
  // listed, so its caret is live even before then.
  const isDir = node.kind === "directory";
  const isOpen = isDir && expanded.has(node.fullPath);
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
          aria-expanded={isDir ? isOpen : undefined}
          disabled={!isDir}
          onClick={(e) => {
            e.stopPropagation();
            if (isDir) onToggle(node.fullPath);
          }}
        >
          {isDir ? (isOpen ? "▼" : "▶") : "·"}
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
        </button>
      </div>
      {isOpen && (
        <ul className="children path-children">
          {node.children.map((c) => (
            <PathTreeNode key={c.fullPath} {...props} node={c} />
          ))}
          <ListingRows {...props} dir={node.fullPath} listing={node.listing} />
        </ul>
      )}
    </li>
  );
}
