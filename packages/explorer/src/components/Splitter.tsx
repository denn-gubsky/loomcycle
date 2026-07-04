import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";

// Splitter wraps two children with a draggable vertical handle
// between them. The left pane's width is owned in state and (when
// `storageKey` is set) persisted to localStorage so the operator's
// layout choice survives reloads.
//
// Implementation notes:
//   - CSS Grid with three columns: [left] [handle] [right]. Grid
//     keeps the right pane on a fluid `1fr` regardless of the
//     window width, while the left pane snaps to the dragged size.
//   - Drag uses window-level pointermove + pointerup so the user
//     can drag PAST the handle bounds without losing tracking
//     (common UX pitfall when the listener lives on the handle
//     element). The container ref provides the origin for the
//     fractional clientX calculation.
//   - min/max clamps so the user can't drag a pane into oblivion
//     or off-screen.
//
// Self-contained copy (the loomcycle Web UI + @loomcycle/library carry the same
// component); the explorer package deliberately owns its own copy rather than
// sharing, so it has no cross-package coupling.

export interface SplitterProps {
  children: [ReactNode, ReactNode];
  // Initial left-pane width in px. Used until the user drags or a
  // persisted value is restored from localStorage.
  defaultLeftWidth?: number;
  // Lower bound for the left pane in px. Pane content has its own
  // overflow:auto so it survives squeezing, but a too-narrow pane
  // is unusable.
  minLeftWidth?: number;
  // Upper bound for the left pane in px. Prevents the operator from
  // dragging the right pane to zero by accident.
  minRightWidth?: number;
  // localStorage key for persistence. Omit to make the split
  // ephemeral (resets on every page mount).
  storageKey?: string;
  // Extra class on the outer wrapper (the parent .paths-view rules
  // expect a specific class; this lets the caller keep those
  // selectors intact).
  className?: string;
}

const HANDLE_WIDTH = 6;

export default function Splitter({
  children,
  defaultLeftWidth = 380,
  minLeftWidth = 180,
  minRightWidth = 240,
  storageKey,
  className,
}: SplitterProps) {
  const [leftWidth, setLeftWidth] = useState<number>(() => {
    if (storageKey) {
      const stored = localStorage.getItem(storageKey);
      const n = stored ? parseInt(stored, 10) : NaN;
      if (Number.isFinite(n) && n >= minLeftWidth) return n;
    }
    return defaultLeftWidth;
  });
  const containerRef = useRef<HTMLDivElement | null>(null);
  const [dragging, setDragging] = useState(false);

  // Persist on every settled width. Debouncing isn't needed — drag
  // already produces a single final settle via pointerup.
  useEffect(() => {
    if (!storageKey || dragging) return;
    localStorage.setItem(storageKey, String(Math.round(leftWidth)));
  }, [storageKey, leftWidth, dragging]);

  const onPointerDown = useCallback((e: React.PointerEvent) => {
    e.preventDefault();
    setDragging(true);
    (e.target as HTMLElement).setPointerCapture(e.pointerId);
  }, []);

  const onPointerMove = useCallback(
    (e: React.PointerEvent) => {
      if (!dragging || !containerRef.current) return;
      const rect = containerRef.current.getBoundingClientRect();
      const raw = e.clientX - rect.left;
      const maxAllowed = rect.width - minRightWidth - HANDLE_WIDTH;
      const clamped = Math.max(minLeftWidth, Math.min(raw, maxAllowed));
      setLeftWidth(clamped);
    },
    [dragging, minLeftWidth, minRightWidth],
  );

  const onPointerUp = useCallback((e: React.PointerEvent) => {
    setDragging(false);
    try {
      (e.target as HTMLElement).releasePointerCapture(e.pointerId);
    } catch {
      // Pointer was already released (e.g. capture lost from a
      // browser-internal gesture). Safe to ignore.
    }
  }, []);

  return (
    <div
      ref={containerRef}
      className={`splitter ${className ?? ""} ${dragging ? "dragging" : ""}`}
      style={{
        gridTemplateColumns: `${leftWidth}px ${HANDLE_WIDTH}px 1fr`,
      }}
    >
      <div className="splitter-pane splitter-left">{children[0]}</div>
      <div
        className="splitter-handle"
        role="separator"
        aria-orientation="vertical"
        aria-valuenow={Math.round(leftWidth)}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        onPointerCancel={onPointerUp}
      />
      <div className="splitter-pane splitter-right">{children[1]}</div>
    </div>
  );
}
