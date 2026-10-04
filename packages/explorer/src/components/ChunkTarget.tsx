// ChunkTarget renders the far end of a link — a reference, a backlink, a related
// chunk, an unlinked mention — as something the reader can follow.
//
// A chunk in THIS document selects in place. A chunk in ANOTHER document opens
// that document at the chunk; it keeps the ↗ mark and the "another document"
// hint, because that click replaces what is on screen and the reader should know
// before making it. Cross-document targets used to be an inert label: the viewer
// showed one document and had nowhere to go, so the links the edge list
// advertised could not be followed at all.
//
// With no onOpenDocument (a host that cannot leave its document), a
// cross-document target stays a label rather than a button that does nothing.
export interface ChunkTargetProps {
  // The far chunk, its title, and the document it lives in.
  id: string;
  title?: string;
  targetDocumentId?: string;
  // The document on screen. A target with no document id of its own is treated
  // as belonging to it (an endpoint the runtime could not enrich).
  documentId: string;
  onSelectChunk: (id: string) => void;
  onOpenDocument?: (documentId: string, chunkId: string, title?: string) => void;
}

export default function ChunkTarget({
  id,
  title,
  targetDocumentId,
  documentId,
  onSelectChunk,
  onOpenDocument,
}: ChunkTargetProps) {
  const label = title || id.slice(0, 8);
  if (!targetDocumentId || targetDocumentId === documentId) {
    return (
      <button type="button" className="doc-ref-target" onClick={() => onSelectChunk(id)}>
        {label}
      </button>
    );
  }
  if (!onOpenDocument) {
    return (
      <span className="doc-ref-target external" title="In another document">
        {label} ↗
      </span>
    );
  }
  return (
    <button
      type="button"
      className="doc-ref-target external"
      title="In another document — opens it at this chunk"
      onClick={() => onOpenDocument(targetDocumentId, id, title)}
    >
      {label} ↗
    </button>
  );
}
