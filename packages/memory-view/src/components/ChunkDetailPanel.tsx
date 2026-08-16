import { useEffect, useState } from "react";
import type { ChunkDetail, DocEdge, FactEntity, MemoryScope } from "../types";
import { useMemoryData } from "../lib/dataLayer";
import { factActions, factVerdict } from "../lib/factVerdict";

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
  /** Called after a verdict is recorded, so a list showing this fact can refresh —
   *  withholding one removes it from the default view. */
  onVerdictRecorded?: () => void;
}

export default function ChunkDetailPanel({
  scope,
  scopeId,
  chunkId,
  onNavigate,
  onVerdictRecorded,
}: ChunkDetailPanelProps) {
  const data = useMemoryData();
  const [chunk, setChunk] = useState<ChunkDetail | null>(null);
  const [edges, setEdges] = useState<DocEdge[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  // A verdict needs a stated ground — the server refuses one without a reason, and it
  // should: a withheld fact whose reason nobody recorded is indistinguishable from a bug.
  const [arming, setArming] = useState<null | { good: boolean }>(null);
  const [reason, setReason] = useState("");
  const [saving, setSaving] = useState(false);

  const judge = async (good: boolean) => {
    if (!reason.trim()) return;
    setSaving(true);
    try {
      await data.judgeFact(
        scope,
        chunkId,
        good ? "supported" : "unsupported",
        reason.trim(),
        scopeId ? { scopeId } : undefined,
      );
      setArming(null);
      setReason("");
      // Re-read rather than patching local state: the server owns the confidence the
      // word maps to and stamps who judged, so anything reconstructed here would be a
      // guess at what it decided.
      const c = await data.getChunk(scope, chunkId, scopeId ? { scopeId } : undefined);
      setChunk(c);
      onVerdictRecorded?.();
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  };

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

      <VerdictBlock
        entity={chunk.entity}
        arming={arming}
        reason={reason}
        saving={saving}
        onArm={(good) => {
          setArming({ good });
          setReason("");
        }}
        onReason={setReason}
        onCancel={() => setArming(null)}
        onSave={judge}
      />

      <div className="fact-body-label">body</div>
      <pre className="fact-body">{chunk.body || ""}</pre>
    </div>
  );
}

// VerdictBlock — the evidence a fact was drawn from, whatever verdict it carries, and
// the two controls that let a person overrule the judge (RFC CC).
//
// THE SPAN IS SHOWN, NOT TUCKED AWAY. It is the thing the claim is checked against; a
// panel that hides it invites judging a claim on how plausible it reads, which is the
// failure the whole verified-writes line exists to prevent. A fact with no span says so
// in place of the quote — that fact can never be verified by anyone, which is a
// different state from merely unjudged.
function VerdictBlock({
  entity,
  arming,
  reason,
  saving,
  onArm,
  onReason,
  onCancel,
  onSave,
}: {
  entity?: FactEntity;
  arming: null | { good: boolean };
  reason: string;
  saving: boolean;
  onArm: (good: boolean) => void;
  onReason: (s: string) => void;
  onCancel: () => void;
  onSave: (good: boolean) => void;
}) {
  if (!entity) return null;
  const v = factVerdict(entity);
  const a = factActions(v);
  return (
    <div className="fact-verdict" data-state={v.state}>
      <div className="fact-verdict-head">
        <span className="fact-body-label">evidence</span>
        <span className={"fact-verdict-badge fact-verdict-" + v.state}>{v.state}</span>
      </div>
      {entity.source_quote ? (
        <blockquote className="fact-span">{entity.source_quote}</blockquote>
      ) : (
        <div className="fact-span fact-span-missing">
          no source span — this fact cannot be verified by anyone
        </div>
      )}
      {v.reason && (
        <div className="fact-verdict-reason">
          {v.byOperator ? "an operator" : v.judgedBy || "an earlier verdict"} said: {v.reason}
        </div>
      )}
      <div className="fact-verdict-actions">
        {a.canMarkWrong && (
          <button type="button" disabled={saving} onClick={() => onArm(false)}>
            mark wrong
          </button>
        )}
        {a.canMarkGood && (
          <button type="button" disabled={saving} onClick={() => onArm(true)}>
            {v.state === "withheld" ? "restore" : "mark good"}
          </button>
        )}
      </div>
      {arming && (
        <div className="fact-verdict-form">
          <input
            autoFocus
            value={reason}
            placeholder={arming.good ? "why is this right?" : "why is this wrong?"}
            onChange={(e) => onReason(e.target.value)}
          />
          <button type="button" disabled={saving || !reason.trim()} onClick={() => onSave(arming.good)}>
            save verdict
          </button>
          <button type="button" onClick={onCancel}>
            cancel
          </button>
        </div>
      )}
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
