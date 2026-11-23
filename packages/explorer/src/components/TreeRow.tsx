import type { CSSProperties, KeyboardEvent, ReactNode } from "react";

// The one row design both explorer trees share (RFC GZ): the Path tree and the
// document's chunk outline. A row is a single 28px line on a fixed grid —
// indent × depth | caret | icon | name | marks — and the whole row selects;
// the caret is the only separate target and never selects.
//
// The classes are the explorer's own (.etree-*), not the agent-run tree's
// .tree / .node .row, so styling one can never restyle the other.

export type TreeIconKind =
  | "folder"
  | "document"
  | "volume"
  | "memory"
  | "annotation"
  | "image"
  | "diagram"
  | "link";

// TreeIcon draws the row icons as inline SVG at one fixed size, so every name
// starts at the same offset (the emoji they replace varied in width per font).
// Stroke-only shapes in currentColor, so they follow the theme.
export function TreeIcon({ kind }: { kind: TreeIconKind }) {
  return (
    <svg
      className={`etree-svg etree-svg-${kind}`}
      width="16"
      height="16"
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.3"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
    >
      {ICON_PATHS[kind]}
    </svg>
  );
}

const ICON_PATHS: Record<TreeIconKind, ReactNode> = {
  folder: <path d="M1.75 4.25v8a1 1 0 0 0 1 1h10.5a1 1 0 0 0 1-1v-6.5a1 1 0 0 0-1-1H8L6.5 3.25H2.75a1 1 0 0 0-1 1z" />,
  document: (
    <>
      <path d="M9.25 1.75h-5a1 1 0 0 0-1 1v10.5a1 1 0 0 0 1 1h7.5a1 1 0 0 0 1-1v-8.25z" />
      <path d="M9.25 1.75v3.5h3.5M5.5 8.5h5M5.5 11h3.5" />
    </>
  ),
  volume: (
    <>
      <rect x="1.75" y="4.25" width="12.5" height="7.5" rx="1" />
      <path d="M4.5 8h.01M7 8h4.5" />
    </>
  ),
  memory: (
    <>
      <ellipse cx="8" cy="3.75" rx="5.25" ry="2" />
      <path d="M2.75 3.75v8.5c0 1.1 2.35 2 5.25 2s5.25-.9 5.25-2v-8.5M2.75 8c0 1.1 2.35 2 5.25 2s5.25-.9 5.25-2" />
    </>
  ),
  annotation: (
    <>
      <path d="M9.25 1.75h-5a1 1 0 0 0-1 1v10.5a1 1 0 0 0 1 1h7.5a1 1 0 0 0 1-1v-8.25z" />
      <path d="M9.25 1.75v3.5h3.5M5.5 8.5l1.5 1.5 3-3" />
    </>
  ),
  image: (
    <>
      <rect x="1.75" y="2.75" width="12.5" height="10.5" rx="1" />
      <circle cx="5.5" cy="6.25" r="1.25" />
      <path d="M14.25 10.5l-3.5-3.5-7.5 6.25" />
    </>
  ),
  diagram: (
    <>
      <rect x="1.75" y="1.75" width="4.5" height="3.5" rx="0.5" />
      <rect x="9.75" y="10.75" width="4.5" height="3.5" rx="0.5" />
      <path d="M4 5.25v4.5a1 1 0 0 0 1 1h4.75" />
    </>
  ),
  link: (
    <>
      <path d="M6.75 9.25a2.75 2.75 0 0 0 3.9 0l2-2a2.75 2.75 0 0 0-3.9-3.9l-.5.5" />
      <path d="M9.25 6.75a2.75 2.75 0 0 0-3.9 0l-2 2a2.75 2.75 0 0 0 3.9 3.9l.5-.5" />
    </>
  ),
};

// shortType is a type's compact mark: its first four letters, lowercase. The
// full type is in the mark's title and accessible name.
export function shortType(type: string): string {
  return type.trim().slice(0, 4).toLowerCase();
}

// TreeMarks are a row's compact type tag and status dot at its right edge.
// Each carries the full label as its title and accessible name.
export function TreeMarks({
  type,
  status,
  statusColor,
  extra,
}: {
  type?: string;
  status?: string;
  statusColor?: string;
  extra?: ReactNode;
}) {
  if (!type && !status && !extra) return null;
  return (
    <span className="etree-marks">
      {extra}
      {type && (
        <abbr className="etree-mark etree-mark-type" title={`type: ${type}`} aria-label={`type ${type}`}>
          {shortType(type)}
        </abbr>
      )}
      {status && (
        <span
          className="etree-mark etree-mark-status"
          role="img"
          title={`status: ${status}`}
          aria-label={`status ${status}`}
          style={statusColor ? ({ "--etree-dot": statusColor } as CSSProperties) : undefined}
        />
      )}
    </span>
  );
}

export interface TreeItemProps {
  // depth is 0 for a top-level row; the indent is depth × one step.
  depth: number;
  // expandable: the row has (or may have) children, so its caret is live.
  expandable: boolean;
  expanded: boolean;
  onToggle: () => void;
  selected: boolean;
  onSelect: () => void;
  // tabbable: this row is the tree's tab stop (the selected row, and the first
  // row so a tree with no visible selection can still be entered).
  tabbable: boolean;
  icon?: ReactNode;
  name: ReactNode;
  title?: string;
  marks?: ReactNode;
  // style carries the row's tint (rowTint / chunkRowTint) — a custom property
  // only, never a background.
  style?: CSSProperties;
  className?: string;
  // children: the child rows, drawn in a role="group" list only when expanded.
  children?: ReactNode;
}

// TreeItem is one treeitem: the row and, when expanded, its children.
export function TreeItem({
  depth,
  expandable,
  expanded,
  onToggle,
  selected,
  onSelect,
  tabbable,
  icon,
  name,
  title,
  marks,
  style,
  className,
  children,
}: TreeItemProps) {
  const isOpen = expandable && expanded;
  return (
    <li
      className={`etree-node${className ? ` ${className}` : ""}${selected ? " selected" : ""}`}
      role="treeitem"
      aria-level={depth + 1}
      aria-selected={selected}
      aria-expanded={expandable ? isOpen : undefined}
      tabIndex={tabbable ? 0 : -1}
    >
      <div
        className="etree-row"
        style={{ ...style, "--etree-depth": depth } as CSSProperties}
        title={title}
        onClick={onSelect}
      >
        <button
          type="button"
          className="etree-caret"
          tabIndex={-1}
          aria-label={isOpen ? "collapse" : "expand"}
          aria-expanded={expandable ? isOpen : undefined}
          disabled={!expandable}
          onClick={(e) => {
            // The caret folds; it never selects the row.
            e.stopPropagation();
            if (expandable) onToggle();
          }}
        >
          {expandable && (
            <svg width="10" height="10" viewBox="0 0 10 10" aria-hidden="true" focusable="false">
              <path d="M3.5 2l3 3-3 3" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" strokeLinejoin="round" />
            </svg>
          )}
        </button>
        <span className="etree-icon">{icon}</span>
        <span className="etree-name">{name}</span>
        {marks}
      </div>
      {isOpen && children !== undefined && (
        <ul className="etree-children" role="group">
          {children}
        </ul>
      )}
    </li>
  );
}

// treeKeyDown is the tree's keyboard handler, attached to the role="tree"
// list. It works on the rendered rows (a collapsed row's children are not in
// the DOM), and acts through the row's own click targets, so both trees get
// the same keys without passing their state in: up / down move, right opens or
// steps into the first child, left closes or steps to the parent, Home / End
// jump, Enter / Space select, and a letter jumps to the next row whose name
// starts with it.
export function treeKeyDown(e: KeyboardEvent<HTMLElement>) {
  // Only a focused row navigates: a key on a control inside the tree (a "load
  // more" or "retry" button) keeps its own meaning.
  const item = e.target as HTMLElement;
  if (item.getAttribute("role") !== "treeitem" || !e.currentTarget.contains(item)) return;
  const items = Array.from(e.currentTarget.querySelectorAll<HTMLElement>('[role="treeitem"]'));
  const at = items.indexOf(item);
  const row = item.querySelector<HTMLElement>(":scope > .etree-row");
  const caret = row?.querySelector<HTMLButtonElement>(":scope > .etree-caret");
  const expanded = item.getAttribute("aria-expanded");
  const move = (to: HTMLElement | undefined | null) => {
    if (!to) return;
    items.forEach((el) => (el.tabIndex = el === to ? 0 : -1));
    to.focus();
  };
  switch (e.key) {
    case "ArrowDown":
      move(items[at + 1]);
      break;
    case "ArrowUp":
      move(items[at - 1]);
      break;
    case "Home":
      move(items[0]);
      break;
    case "End":
      move(items[items.length - 1]);
      break;
    case "ArrowRight":
      if (expanded === "false") caret?.click();
      else if (expanded === "true") move(item.querySelector<HTMLElement>(':scope > [role="group"] > [role="treeitem"]'));
      else return;
      break;
    case "ArrowLeft":
      if (expanded === "true") caret?.click();
      else move(item.parentElement?.closest<HTMLElement>('[role="treeitem"]'));
      break;
    case "Enter":
    case " ":
      row?.click();
      break;
    default: {
      if (e.key.length !== 1 || e.ctrlKey || e.metaKey || e.altKey) return;
      const ch = e.key.toLowerCase();
      const order = [...items.slice(at + 1), ...items.slice(0, at + 1)];
      move(
        order.find((el) =>
          el.querySelector(":scope > .etree-row > .etree-name")?.textContent?.trim().toLowerCase().startsWith(ch),
        ),
      );
    }
  }
  e.preventDefault();
  e.stopPropagation();
}
