// DocUnavailable stands in for a document that could not be read: deleted, in a
// scope other than the one being browsed, or not the reader's to see. It is
// reached most easily by following a reference — an edge outlives the document
// it points at — and it has to say so in place, because the alternative is an
// empty pane that looks like a viewer bug. The host's trail stays above it, so
// the way back is still there.
export interface DocUnavailableProps {
  documentId: string;
  scope: string;
  // The runtime's own reason, when it gave one.
  detail?: string;
}

export default function DocUnavailable({ documentId, scope, detail }: DocUnavailableProps) {
  return (
    <div className="doc-unavailable" role="alert">
      <p className="doc-unavailable-lead">This document can’t be opened.</p>
      <p>
        It may have been deleted, live in a scope other than <code>{scope}</code>, or not be
        yours to read.
      </p>
      <p className="doc-unavailable-detail">
        <code>{documentId}</code>
        {detail && <> — {detail}</>}
      </p>
    </div>
  );
}
