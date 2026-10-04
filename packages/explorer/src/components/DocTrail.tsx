import { useState } from "react";
import { collapseTrail, labelOf, type Trail } from "../lib/docTrail";

// DocTrail is the breadcrumb over a document reached by following references:
// every document on the way here, each one a button back to it — and to the
// chunk that was selected there — plus a Back button for the common single step.
// It renders nothing until a reference has actually been followed (one entry is
// just "the document on screen").
//
// A long trail folds its middle into a "…" that expands on demand; a long title
// is cut with an ellipsis in CSS and carried in full by the tooltip.
export interface DocTrailProps {
  trail: Trail;
  // onJump receives the index of the trail entry to return to.
  onJump: (index: number) => void;
}

export default function DocTrail({ trail, onJump }: DocTrailProps) {
  const [showAll, setShowAll] = useState(false);
  if (trail.length < 2) return null;

  const current = trail.length - 1;
  const previous = trail[current - 1];
  const items = collapseTrail(trail.length, showAll ? trail.length : undefined);

  return (
    <nav className="doc-trail" aria-label="Documents you came through">
      <button
        type="button"
        className="doc-trail-back"
        onClick={() => onJump(current - 1)}
        title={`Back to ${labelOf(previous)}`}
      >
        ← Back
      </button>
      <ol className="doc-trail-list">
        {items.map((item) =>
          item.kind === "gap" ? (
            <li className="doc-trail-item" key="gap">
              <button
                type="button"
                className="doc-trail-gap"
                onClick={() => setShowAll(true)}
                title={`Show ${item.hidden.length} more: ${item.hidden
                  .map((i) => labelOf(trail[i]))
                  .join(" › ")}`}
                aria-label={`Show ${item.hidden.length} hidden documents`}
              >
                …
              </button>
            </li>
          ) : item.index === current ? (
            <li className="doc-trail-item" key={item.index}>
              <span className="doc-trail-current" aria-current="page" title={labelOf(trail[current])}>
                {labelOf(trail[current])}
              </span>
            </li>
          ) : (
            <li className="doc-trail-item" key={item.index}>
              <button
                type="button"
                className="doc-trail-crumb"
                onClick={() => onJump(item.index)}
                title={labelOf(trail[item.index])}
              >
                {labelOf(trail[item.index])}
              </button>
            </li>
          ),
        )}
      </ol>
    </nav>
  );
}
