import { useState } from "react";
import type { DocEdge } from "../types";
import MermaidDiagram from "./Mermaid";

// CrossReferences renders the RFC BN P4 cross-reference surface for a document:
//   - a References list for the SELECTED chunk (its outgoing + incoming edges),
//     each same-document target clickable to navigate; a cross-document target is
//     labeled (↗) but not navigable from this single-document viewer.
//   - a collapsible whole-document relationship GRAPH (a Mermaid flowchart of the
//     document's edges), capped to the first N with a "show all" expand so a
//     heavily cross-linked document doesn't render an unreadable diagram.
// Fed by one get_edges call (all edges touching the document); renders nothing
// when the document has no edges.
export interface CrossReferencesProps {
  edges: DocEdge[];
  documentId: string;
  selectedId?: string;
  onSelectChunk: (id: string) => void;
}

// GRAPH_CAP is the default number of edges the relationship graph draws before
// requiring an explicit "show all" (keeps a big graph readable + cheap).
const GRAPH_CAP = 24;

export default function CrossReferences({
  edges,
  documentId,
  selectedId,
  onSelectChunk,
}: CrossReferencesProps) {
  const [showGraph, setShowGraph] = useState(false);
  const [showAll, setShowAll] = useState(false);

  if (edges.length === 0) return null;

  const refs = selectedId
    ? edges.filter((e) => e.from_id === selectedId || e.to_id === selectedId)
    : [];
  const graphEdges = showAll ? edges : edges.slice(0, GRAPH_CAP);
  const graphDef = buildGraph(graphEdges);

  return (
    <div className="doc-refs">
      {selectedId && (
        <div className="doc-refs-list-wrap">
          <h4>References</h4>
          {refs.length === 0 ? (
            <p className="doc-refs-empty">This chunk has no links.</p>
          ) : (
            <ul className="doc-refs-list">
              {refs.map((e, i) => (
                <RefRow
                  key={`${e.from_id}-${e.to_id}-${e.kind}-${i}`}
                  edge={e}
                  selectedId={selectedId}
                  documentId={documentId}
                  onSelectChunk={onSelectChunk}
                />
              ))}
            </ul>
          )}
        </div>
      )}

      <div className="doc-graph">
        <button type="button" className="doc-graph-toggle" onClick={() => setShowGraph((s) => !s)}>
          {showGraph ? "▼" : "▶"} relationship graph ({edges.length})
        </button>
        {showGraph && (
          <>
            <MermaidDiagram code={graphDef} />
            {edges.length > GRAPH_CAP && (
              <button type="button" className="doc-graph-more" onClick={() => setShowAll((s) => !s)}>
                {showAll ? `show top ${GRAPH_CAP}` : `show all ${edges.length}`}
              </button>
            )}
          </>
        )}
      </div>
    </div>
  );
}

function RefRow({
  edge,
  selectedId,
  documentId,
  onSelectChunk,
}: {
  edge: DocEdge;
  selectedId: string;
  documentId: string;
  onSelectChunk: (id: string) => void;
}) {
  const outgoing = edge.from_id === selectedId;
  const otherId = outgoing ? edge.to_id : edge.from_id;
  const otherTitle = (outgoing ? edge.to_title : edge.from_title) || otherId.slice(0, 8);
  const otherDoc = outgoing ? edge.to_document_id : edge.from_document_id;
  // An edge whose far endpoint carries no document_id (defensively) is treated as
  // same-document; a different document_id is a cross-document link.
  const sameDoc = !otherDoc || otherDoc === documentId;
  return (
    <li className="doc-ref">
      <span className="doc-ref-kind">{edge.kind}</span>
      <span className="doc-ref-arrow" aria-hidden>
        {outgoing ? "→" : "←"}
      </span>
      {sameDoc ? (
        <button type="button" className="doc-ref-target" onClick={() => onSelectChunk(otherId)}>
          {otherTitle}
        </button>
      ) : (
        <span className="doc-ref-target external" title="In another document">
          {otherTitle} ↗
        </span>
      )}
    </li>
  );
}

// buildGraph renders the document's edges as a Mermaid left-to-right flowchart.
// Node ids are the chunk ids (prefixed + stripped to mermaid-safe chars); labels
// are the endpoint titles, sanitized of mermaid metacharacters and truncated.
// Mermaid dedups nodes by id, so re-declaring a node's label across edges is fine.
function buildGraph(edges: DocEdge[]): string {
  const lines = ["graph LR"];
  for (const e of edges) {
    const from = `${nodeId(e.from_id)}["${label(e.from_title, e.from_id)}"]`;
    const to = `${nodeId(e.to_id)}["${label(e.to_title, e.to_id)}"]`;
    const kind = (e.kind || "link").replace(/[|"]/g, " ");
    lines.push(`  ${from} -->|${kind}| ${to}`);
  }
  return lines.join("\n");
}

function nodeId(id: string): string {
  return "n" + id.replace(/[^a-zA-Z0-9]/g, "");
}

function label(title: string | undefined, id: string): string {
  const base = (title ?? "").replace(/["[\]{}|<>]/g, " ").replace(/\s+/g, " ").trim();
  const text = base || id.slice(0, 6);
  return text.length > 30 ? text.slice(0, 29) + "…" : text;
}
