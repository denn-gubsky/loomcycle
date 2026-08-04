import { useEffect, useState } from "react";
import type {
  Backlink,
  BrowseScope,
  DocScope,
  RelatedChunk,
  UnlinkedMention,
} from "../types";
import { useExplorerData } from "../lib/dataLayer";

// Connections is the RFC BS per-chunk connection surface, a collapsible companion
// to CrossReferences. It shows three lists for the SELECTED chunk, each fetched
// lazily (only once the section is expanded, and refetched when the selection
// changes while open — collapsed keeps the viewer quiet):
//   - Backlinks         — chunks that link TO this one ("what links here").
//   - Related           — semantic neighbours (vector similarity). REFUSES with no
//                          embedder configured → a muted note, not an error.
//   - Unlinked mentions — chunks that name this one but don't link it (candidates).
// A same-document target is clickable (navigates via onSelectChunk); a
// cross-document one is labeled ↗ and not navigable from this single-doc viewer —
// the same rule CrossReferences uses.
export interface ConnectionsProps {
  documentId: string;
  selectedId?: string;
  scope: DocScope;
  browse?: BrowseScope;
  onSelectChunk: (id: string) => void;
}

const RELATED_LIMIT = 8;
const MENTIONS_LIMIT = 20;

// isEmbedderError recognizes the `related` op's refusal when the deployment has no
// embedder/vector backend, so the UI can say "not configured" rather than surface
// a raw error — related is a best-effort enhancement, never load-bearing.
function isEmbedderError(msg: string): boolean {
  return /embedder|embedding|vector|semantic|not configured/i.test(msg);
}

export default function Connections({
  documentId,
  selectedId,
  scope,
  browse,
  onSelectChunk,
}: ConnectionsProps) {
  const data = useExplorerData();
  const [open, setOpen] = useState(false);
  const [loading, setLoading] = useState(false);
  const [loadedId, setLoadedId] = useState<string | undefined>(undefined);
  const [backlinks, setBacklinks] = useState<Backlink[]>([]);
  const [related, setRelated] = useState<RelatedChunk[]>([]);
  const [relatedNote, setRelatedNote] = useState<string | null>(null);
  const [mentions, setMentions] = useState<UnlinkedMention[]>([]);
  const [mentionsTruncated, setMentionsTruncated] = useState(false);

  // Fetch only while expanded AND a chunk is selected; the three reads are
  // independent (allSettled) so a `related` refusal never blanks backlinks/
  // mentions. Cancelled on unmount / re-selection so a stale resolution can't
  // clobber the newer one.
  useEffect(() => {
    if (!open || !selectedId) return;
    const id = selectedId;
    let cancelled = false;
    setLoading(true);
    Promise.allSettled([
      data.documentBacklinks(id, scope, browse),
      data.documentRelated(id, scope, browse, RELATED_LIMIT),
      data.documentUnlinkedMentions(id, scope, browse, MENTIONS_LIMIT),
    ]).then(([b, r, m]) => {
      if (cancelled) return;
      setBacklinks(b.status === "fulfilled" ? b.value.backlinks ?? [] : []);
      if (r.status === "fulfilled") {
        setRelated(r.value.related ?? []);
        setRelatedNote(null);
      } else {
        setRelated([]);
        const msg = r.reason instanceof Error ? r.reason.message : String(r.reason);
        setRelatedNote(isEmbedderError(msg) ? "semantic search not configured" : msg);
      }
      if (m.status === "fulfilled") {
        setMentions(m.value.unlinked_mentions ?? []);
        setMentionsTruncated(!!m.value.truncated);
      } else {
        setMentions([]);
        setMentionsTruncated(false);
      }
      setLoadedId(id);
      setLoading(false);
    });
    return () => {
      cancelled = true;
    };
  }, [data, open, selectedId, scope, browse]);

  const isCrossDoc = (otherDoc?: string) => !!otherDoc && otherDoc !== documentId;
  // Data is current only when it was fetched for THIS selection and no fetch is in
  // flight — otherwise show "loading…" (this also covers the first frame after
  // expanding, before the effect sets loading, so no stale "none" flashes).
  const ready = !!(open && selectedId && !loading && loadedId === selectedId);

  return (
    <div className="doc-conn">
      <button
        type="button"
        className="doc-conn-toggle"
        onClick={() => setOpen((o) => !o)}
      >
        {open ? "▼" : "▶"} Connections
      </button>
      {open && (
        <div className="doc-conn-body">
          {!selectedId ? (
            <p className="doc-conn-empty">Select a chunk.</p>
          ) : !ready ? (
            <p className="doc-conn-empty">loading…</p>
          ) : (
            <>
              <div className="doc-conn-group">
                <h5>Backlinks</h5>
                {backlinks.length === 0 ? (
                  <p className="doc-conn-empty">none</p>
                ) : (
                  <ul className="doc-conn-list">
                    {backlinks.map((e, i) => (
                      <li className="doc-conn-row" key={`${e.from_id}-${e.kind}-${i}`}>
                        <Target
                          id={e.from_id}
                          title={e.from_title}
                          crossDoc={isCrossDoc(e.from_document_id)}
                          onSelect={onSelectChunk}
                        />
                        <span className="doc-ref-kind">{e.kind}</span>
                        {e.auto && (
                          <span className="doc-conn-auto" title="Auto-linked from a [[wikilink]]">
                            auto
                          </span>
                        )}
                      </li>
                    ))}
                  </ul>
                )}
              </div>

              <div className="doc-conn-group">
                <h5>Related</h5>
                {relatedNote ? (
                  <p className="doc-conn-empty">{relatedNote}</p>
                ) : related.length === 0 ? (
                  <p className="doc-conn-empty">none</p>
                ) : (
                  <ul className="doc-conn-list">
                    {related.map((r, i) => (
                      <li className="doc-conn-row" key={`${r.chunk_id}-${i}`}>
                        <Target
                          id={r.chunk_id}
                          title={r.title}
                          crossDoc={isCrossDoc(r.document_id)}
                          onSelect={onSelectChunk}
                        />
                        <span className="doc-conn-score" title="similarity score">
                          {r.score.toFixed(2)}
                        </span>
                      </li>
                    ))}
                  </ul>
                )}
              </div>

              <div className="doc-conn-group">
                <h5>Unlinked mentions</h5>
                {mentions.length === 0 ? (
                  <p className="doc-conn-empty">none</p>
                ) : (
                  <ul className="doc-conn-list">
                    {mentions.map((m, i) => (
                      <li className="doc-conn-row" key={`${m.chunk_id}-${i}`}>
                        <Target
                          id={m.chunk_id}
                          title={m.title}
                          crossDoc={isCrossDoc(m.document_id)}
                          onSelect={onSelectChunk}
                        />
                      </li>
                    ))}
                    {mentionsTruncated && (
                      <li className="doc-conn-more" aria-hidden>
                        (more…)
                      </li>
                    )}
                  </ul>
                )}
              </div>
            </>
          )}
        </div>
      )}
    </div>
  );
}

// Target renders a connection's far endpoint: a same-document chunk is a clickable
// navigation button; a cross-document one is a non-navigable ↗ label (mirrors
// CrossReferences' RefRow). Falls back to a short id when the title is absent.
function Target({
  id,
  title,
  crossDoc,
  onSelect,
}: {
  id: string;
  title?: string;
  crossDoc: boolean;
  onSelect: (id: string) => void;
}) {
  const label = title || id.slice(0, 8);
  if (crossDoc) {
    return (
      <span className="doc-ref-target external" title="In another document">
        {label} ↗
      </span>
    );
  }
  return (
    <button type="button" className="doc-ref-target" onClick={() => onSelect(id)}>
      {label}
    </button>
  );
}
