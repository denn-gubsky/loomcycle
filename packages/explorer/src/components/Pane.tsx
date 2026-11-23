import type { ReactNode } from "react";

// The collapsible panes' shared parts (RFC GZ). The Path tree, the chunk
// outline and the side panel all collapse the same way: a labelled button on
// the pane's edge, a double-click on the divider, a keyboard shortcut, and a
// remembered state. A collapsed pane leaves a thin rail with its icon and a
// count that opens it again.

export type PaneIconKind =
  | "tree"
  | "outline"
  | "annotation"
  | "references"
  | "classification"
  | "history"
  | "search"
  | "collapse-left"
  | "collapse-right"
  | "close";

// PaneIcon draws the rail and header icons, stroke-only in currentColor like
// the tree icons, at one size.
export function PaneIcon({ kind }: { kind: PaneIconKind }) {
  return (
    <svg
      className="pane-svg"
      width="16"
      height="16"
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.4"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
    >
      {PANE_PATHS[kind]}
    </svg>
  );
}

const PANE_PATHS: Record<PaneIconKind, ReactNode> = {
  tree: <path d="M1.75 4.25v8a1 1 0 0 0 1 1h10.5a1 1 0 0 0 1-1v-6.5a1 1 0 0 0-1-1H8L6.5 3.25H2.75a1 1 0 0 0-1 1z" />,
  outline: <path d="M2.5 3.5h.01M5 3.5h8.5M4.5 8h.01M7 8h6.5M4.5 12.5h.01M7 12.5h6.5" />,
  annotation: (
    <>
      <path d="M2.75 2.75h10.5v8h-6l-3 2.5v-2.5h-1.5z" />
      <path d="M5.5 5.75h5M5.5 8h3" />
    </>
  ),
  references: (
    <>
      <path d="M6.75 9.25a2.75 2.75 0 0 0 3.9 0l2-2a2.75 2.75 0 0 0-3.9-3.9l-.5.5" />
      <path d="M9.25 6.75a2.75 2.75 0 0 0-3.9 0l-2 2a2.75 2.75 0 0 0 3.9 3.9l.5-.5" />
    </>
  ),
  classification: (
    <>
      <path d="M8.5 1.75h5v5l-6.75 6.75-5-5z" />
      <circle cx="11" cy="4.25" r="0.75" />
    </>
  ),
  history: (
    <>
      <circle cx="8" cy="8" r="5.75" />
      <path d="M8 5v3.25l2 1.25" />
    </>
  ),
  search: (
    <>
      <circle cx="7" cy="7" r="4.25" />
      <path d="M10.25 10.25l3.5 3.5" />
    </>
  ),
  "collapse-left": <path d="M10 3.5L5.5 8l4.5 4.5" />,
  "collapse-right": <path d="M6 3.5L10.5 8 6 12.5" />,
  close: <path d="M4 4l8 8M12 4l-8 8" />,
};

// CollapseButton hides a pane. `shortcut` is the key that does the same, in
// the aria-keyshortcuts form.
export function CollapseButton({
  label,
  side,
  onClick,
  shortcut,
}: {
  label: string;
  side: "left" | "right";
  onClick: () => void;
  shortcut?: string;
}) {
  return (
    <button
      type="button"
      className="pane-collapse"
      onClick={onClick}
      aria-label={`Collapse the ${label}`}
      title={`Collapse the ${label}${shortcut ? ` (${shortcut})` : ""}`}
      aria-keyshortcuts={shortcut}
    >
      <PaneIcon kind={side === "left" ? "collapse-left" : "collapse-right"} />
    </button>
  );
}

// PaneRail is a collapsed pane: one button with the pane's icon and count.
// A click opens it again.
export function PaneRail({
  label,
  icon,
  count,
  countLabel,
  onExpand,
  shortcut,
  side,
}: {
  label: string;
  icon: PaneIconKind;
  // count shown under the icon; undefined shows none.
  count?: number;
  // countLabel names what is counted, for the button's label ("chunks").
  countLabel?: string;
  onExpand: () => void;
  shortcut?: string;
  side: "left" | "right";
}) {
  const counted = count !== undefined && countLabel ? `, ${count} ${countLabel}` : "";
  return (
    <div className={`pane-rail pane-rail-${side}`}>
      <button
        type="button"
        className="pane-rail-button"
        onClick={onExpand}
        aria-label={`Show the ${label}${counted}`}
        title={`Show the ${label}${shortcut ? ` (${shortcut})` : ""}`}
        aria-keyshortcuts={shortcut}
        aria-expanded={false}
      >
        <PaneIcon kind={icon} />
        {count !== undefined && <span className="pane-rail-count">{count}</span>}
      </button>
    </div>
  );
}
