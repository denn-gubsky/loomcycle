import { useEffect, useState } from "react";
import type { ChunkDetail, DocEdge, FactEntity, MemoryScope } from "../types";
import { useMemoryData } from "../lib/dataLayer";

// ChunkDetailPanel — the fact/chunk inspector shared by the FactViewer and the
// SearchPanel (a document hit opens the same detail). Given a (scope, chunkId)
// it fetches the chunk body + entity block (get_chunk) AND the edges of its
// document (get_edges), then renders — minimalistically, per RFC BV — a LIST of
// the supersession chain + relations, NOT a graph canvas (that is loomboard's
// job, on this same package).
//
// The two time axes are surfaced separately and explicitly, because conflating
// them is the classic bi-temporal bug: WORLD time (valid_at → invalid_at, when
// the fact was true) is a different question from BELIEF time (created_at →
// expired_at, when the store believed it). `retired` keys on expired_at.
export interface ChunkDetailPanelProps {
  scope: MemoryScope;
  /** Browse-by-subject override; forwarded to get_chunk + get_edges. */
  scopeId?: string;
  /** The chunk (fact) to inspect. */
  chunkId: string;
  /** Navigate the detail to another chunk (a supersession/relation target). */
  onNavigate?: (chunkId: string) => void;
}

export default function ChunkDetailPanel({ scope, scopeId, chunkId, onNavigate }: ChunkDetailPanelProps) {
  const data = useMemoryData();
  const [chunk, setChunk] = useState<ChunkDetail | null>(null);
  const [edges, setEdges] = useState<DocEdge[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setErr(null);
    (async () => {
      try {
        const c = await data.getChunk(scope, chunkId, scopeId ? { scopeId } : undefined);
        if (cancelled) return;
        setChunk(c);
        // get_edges is document-scoped — pass the fact's document_id, then the
        // render filters to the edges that actually touch this chunk. A missing
        // document_id (defensive) means no chain to show, not an error.
        if (c.document_id) {
          const e = await data.getEdges(scope, c.document_id, scopeId ? { scopeId } : undefined);
          if (cancelled) return;
          setEdges(e.edges ?? []);
        } else {
          setEdges([]);
        }
      } catch (e) {
        if (!cancelled) setErr(e instanceof Error ? e.message : String(e));
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [data, scope, scopeId, chunkId]);

  if (err) return <div className="err memory-err">{err}</div>;
  if (!chunk) return <div className="empty">{loading ? "loading…" : "no chunk"}</div>;

  const ent = chunk.entity;
  // Supersession chain, split by direction. The backend records a supersede as
  // an edge (from_id = the REPLACEMENT, to_id = the RETIRED fact), so:
  //   from_id === this  → this fact REPLACED to_id  (what it supersedes)
  //   to_id   === this  → from_id REPLACED this     (what superseded it)
  const supersedes = edges
    .filter((e) => e.kind === "supersedes" && e.from_id === chunkId)
    .map((e) => ({ id: e.to_id, title: e.to_title, type: e.to_type }));
  const supersededBy = edges
    .filter((e) => e.kind === "supersedes" && e.to_id === chunkId)
    .map((e) => ({ id: e.from_id, title: e.from_title, type: e.from_type }));
  // Any non-supersedes edge touching this chunk → a plain relation. Pick the
  // OTHER endpoint relative to this chunk.
  const related = edges
    .filter((e) => e.kind !== "supersedes" && (e.from_id === chunkId || e.to_id === chunkId))
    .map((e) =>
      e.from_id === chunkId
        ? { id: e.to_id, title: e.to_title, type: e.to_type, kind: e.kind }
        : { id: e.from_id, title: e.from_title, type: e.from_type, kind: e.kind },
    );

  return (
    <div className="fact-detail">
      <div className="fact-detail-head">
        <h4>{chunk.title || <span className="meta">(untitled)</span>}</h4>
        <div className="fact-badges">
          {chunk.type && <span className="fact-type-badge">{chunk.type}</span>}
          {chunk.status && <span className="fact-status-badge">{chunk.status}</span>}
          {ent && <ClassBadge entity={ent} />}
          {ent?.retired && <span className="fact-retired-badge">retired</span>}
        </div>
      </div>

      {ent && (
        <>
          {/* Two time axes, kept visually distinct so world time and belief time
              are never read as the same thing. */}
          <div className="fact-axes">
            <div className="fact-axis">
              <span className="fact-axis-label">world time</span>
              <span className="fact-axis-range">
                {fmtNanos(ent.valid_at)} <span className="fact-axis-arrow">→</span>{" "}
                {ent.invalid_at !== undefined ? fmtNanos(ent.invalid_at) : <em>still true</em>}
              </span>
            </div>
            <div className="fact-axis">
              <span className="fact-axis-label">belief time</span>
              <span className="fact-axis-range">
                {fmtNanos(ent.created_at)} <span className="fact-axis-arrow">→</span>{" "}
                {ent.expired_at !== undefined ? fmtNanos(ent.expired_at) : <em>still believed</em>}
              </span>
            </div>
          </div>

          <dl className="fact-provenance">
            {fmtConfidence(ent.confidence) && (
              <>
                <dt>confidence</dt>
                <dd>{fmtConfidence(ent.confidence)}</dd>
              </>
            )}
            {ent.origin && (
              <>
                <dt>origin</dt>
                <dd><code>{ent.origin}</code></dd>
              </>
            )}
            {ent.natural_key && (
              <>
                <dt>natural key</dt>
                <dd><code>{ent.natural_key}</code></dd>
              </>
            )}
            {ent.run_id && (
              <>
                <dt>run</dt>
                <dd><code>{ent.run_id}</code></dd>
              </>
            )}
            {ent.session_id && (
              <>
                <dt>session</dt>
                <dd><code>{ent.session_id}</code></dd>
              </>
            )}
          </dl>
        </>
      )}

      {(supersededBy.length > 0 || supersedes.length > 0) && (
        <div className="fact-chain">
          <div className="fact-chain-title">supersession chain</div>
          {supersededBy.length > 0 && (
            <EdgeList label="superseded by" refs={supersededBy} onNavigate={onNavigate} />
          )}
          {supersedes.length > 0 && (
            <EdgeList label="supersedes" refs={supersedes} onNavigate={onNavigate} />
          )}
        </div>
      )}

      {related.length > 0 && (
        <div className="fact-chain">
          <div className="fact-chain-title">related</div>
          <EdgeList refs={related} onNavigate={onNavigate} withKind />
        </div>
      )}

      <div className="fact-body-label">body</div>
      <pre className="fact-body">{chunk.body || ""}</pre>
    </div>
  );
}

// ClassBadge — "evidential" facts are pinned SOURCES (retention-exempt); mark
// them distinctly from machine-"derived" ones.
function ClassBadge({ entity }: { entity: FactEntity }) {
  if (!entity.class) return null;
  const isSource = entity.class === "evidential";
  return (
    <span
      className={"fact-class-badge" + (isSource ? " fact-class-source" : "")}
      title={isSource ? "evidential — a pinned source, retention-exempt" : "derived — machine-distilled"}
    >
      {isSource ? "source" : entity.class}
    </span>
  );
}

interface EdgeRef {
  id: string;
  title?: string;
  type?: string;
  kind?: string;
}

// EdgeList renders a clickable list of edge targets. Clicking navigates the
// detail to that chunk id (a retired target may not be in the current list, so
// navigation re-points the panel rather than selecting a row).
function EdgeList({
  label,
  refs,
  onNavigate,
  withKind,
}: {
  label?: string;
  refs: EdgeRef[];
  onNavigate?: (chunkId: string) => void;
  withKind?: boolean;
}) {
  return (
    <div className="fact-edge-group">
      {label && <span className="fact-edge-label">{label}</span>}
      <ul className="fact-edge-list">
        {refs.map((r) => (
          <li key={(r.kind ?? "") + r.id}>
            <button
              type="button"
              className="fact-edge-btn"
              onClick={() => onNavigate?.(r.id)}
              disabled={!onNavigate}
              title={r.id}
            >
              {withKind && r.kind && <span className="fact-edge-kind">{r.kind}</span>}
              <span className="fact-edge-target">{r.title || r.id}</span>
              {r.type && <span className="fact-edge-type">{r.type}</span>}
            </button>
          </li>
        ))}
      </ul>
    </div>
  );
}

// fmtNanos formats a unix-NANOSECOND timestamp as a human date. Nanos → ms is a
// safe double divide (ms since epoch is well under 2^53). Undefined → em dash.
function fmtNanos(n?: number): string {
  if (n === undefined) return "—";
  return new Date(n / 1e6).toLocaleString();
}

// fmtConfidence renders a 0..1 confidence as a percentage, or null when absent.
function fmtConfidence(c?: number): string | null {
  if (c === undefined) return null;
  return `${Math.round(c * 100)}%`;
}
