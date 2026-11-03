import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";

// StackSplitter is Splitter turned on its side: two children stacked, with a
// draggable horizontal handle between them. The BOTTOM pane's height is owned
// in state and (with `storageKey`) remembered per browser. The top pane takes
// the rest, so when the column grows it is the top pane that grows.
//
// Used in the document viewer between a chunk's text and its links: a chunk
// with many references used to push its own text down to a sliver, since the
// links took their full height and only the text scrolled.

export interface StackSplitterProps {
  children: [ReactNode, ReactNode];
  /** Initial bottom-pane height in px, until the reader drags or a remembered
   *  value is restored. */
  defaultBottomHeight?: number;
  /** Neither pane is dragged smaller than these. */
  minTopHeight?: number;
  minBottomHeight?: number;
  /** localStorage key; omit for a split that resets on every mount. */
  storageKey?: string;
  className?: string;
}

const HANDLE_HEIGHT = 6;

/** clampBottom keeps a dragged bottom height inside a container of `total` px:
 *  at least minBottom, and never so tall that the top pane drops below minTop. */
export function clampBottom(want: number, total: number, minTop: number, minBottom: number): number {
  const max = Math.max(minBottom, total - minTop - HANDLE_HEIGHT);
  return Math.round(Math.max(minBottom, Math.min(want, max)));
}

export default function StackSplitter({
  children,
  defaultBottomHeight = 240,
  minTopHeight = 96,
  minBottomHeight = 48,
  storageKey,
  className,
}: StackSplitterProps) {
  const [bottom, setBottom] = useState<number>(() => {
    if (storageKey) {
      try {
        const n = parseInt(localStorage.getItem(storageKey) ?? "", 10);
        if (Number.isFinite(n) && n >= minBottomHeight) return n;
      } catch {
        // storage blocked: the default stands for this page
      }
    }
    return defaultBottomHeight;
  });
  const containerRef = useRef<HTMLDivElement | null>(null);
  const [dragging, setDragging] = useState(false);

  useEffect(() => {
    if (!storageKey || dragging) return;
    try {
      localStorage.setItem(storageKey, String(bottom));
    } catch {
      // storage blocked: the split lasts for this page only
    }
  }, [storageKey, bottom, dragging]);

  const onPointerDown = useCallback((e: React.PointerEvent) => {
    e.preventDefault();
    setDragging(true);
    (e.target as HTMLElement).setPointerCapture(e.pointerId);
  }, []);

  const onPointerMove = useCallback(
    (e: React.PointerEvent) => {
      if (!dragging || !containerRef.current) return;
      const rect = containerRef.current.getBoundingClientRect();
      setBottom(clampBottom(rect.bottom - e.clientY, rect.height, minTopHeight, minBottomHeight));
    },
    [dragging, minTopHeight, minBottomHeight],
  );

  const onPointerUp = useCallback((e: React.PointerEvent) => {
    setDragging(false);
    try {
      (e.target as HTMLElement).releasePointerCapture(e.pointerId);
    } catch {
      // capture already lost; nothing to release
    }
  }, []);

  return (
    <div
      ref={containerRef}
      className={`stack-splitter ${className ?? ""} ${dragging ? "dragging" : ""}`}
      style={{
        // CSS clamps a remembered height again against the container as it is
        // now (a since-shrunk window, or the viewer embedded in a smaller pane).
        gridTemplateRows: `minmax(${minTopHeight}px, 1fr) ${HANDLE_HEIGHT}px min(${bottom}px, calc(100% - ${minTopHeight + HANDLE_HEIGHT}px))`,
      }}
    >
      <div className="stack-splitter-pane stack-splitter-top">{children[0]}</div>
      <div
        className="stack-splitter-handle"
        role="separator"
        aria-orientation="horizontal"
        aria-valuenow={bottom}
        title="Drag to share the space between the text and its links"
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        onPointerCancel={onPointerUp}
      />
      <div className="stack-splitter-pane stack-splitter-bottom">{children[1]}</div>
    </div>
  );
}
