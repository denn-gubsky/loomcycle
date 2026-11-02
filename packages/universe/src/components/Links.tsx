import { useMemo, useState } from "react";
import type { ModelEdge, UniverseModel } from "../lib/model";
import { kindHex } from "../lib/colors";
import { toMs } from "../lib/time";

// Links.tsx: the selection's links as a list (what the lines on the map are),
// with a ruling on each proposed one, and the dialog that writes a new link
// between two stars. Both only call the data layer; the runtime decides who may
// write and its refusal is shown as is.

const MAX_ROWS = 200;

/** A chunk's label: its heading, else its document's title (a root). */
function starTitle(m: UniverseModel, c: number, fallbackId: string, fallbackDoc: string): string {
  if (c < 0) return fallbackDoc ? `${fallbackId} (in a document not loaded)` : fallbackId;
  const di = m.chunk.doc[c];
  if (m.doc.root[di] === c) return m.doc.title[di] || m.chunk.id[c];
  return `${m.chunk.title[c] || m.chunk.id[c]} — ${m.doc.title[di]}`;
}

export interface LinkRow {
  edge: ModelEdge;
  /** The far end, from the selection's side. */
  other: number;
  otherTitle: string;
  outgoing: boolean;
  expired: boolean;
}

/**
 * linkRows lists the edges touching the selected chunks, from the selection's
 * side: an edge whose source is selected reads outgoing, else incoming. Grouped
 * by kind, proposed first within a kind (they are the ones asking for a ruling).
 */
export function linkRows(
  m: UniverseModel,
  edges: readonly ModelEdge[],
  selectedChunks: ReadonlySet<number>,
  nowMs: number,
): Map<string, LinkRow[]> {
  const groups = new Map<string, LinkRow[]>();
  for (const e of edges) {
    const fromSel = selectedChunks.has(e.from);
    const toSel = selectedChunks.has(e.to);
    if (!fromSel && !toSel) continue;
    const outgoing = fromSel;
    const other = outgoing ? e.to : e.from;
    const row: LinkRow = {
      edge: e,
      other,
      otherTitle: outgoing ? starTitle(m, e.to, e.toId, e.toDocId) : starTitle(m, e.from, e.fromId, e.fromDocId),
      outgoing,
      expired: e.invalidAt !== null && toMs(e.invalidAt) <= nowMs,
    };
    const g = groups.get(e.kind);
    if (g) g.push(row);
    else groups.set(e.kind, [row]);
  }
  for (const g of groups.values()) {
    g.sort((a, b) => Number(b.edge.status === "proposed") - Number(a.edge.status === "proposed"));
  }
  return groups;
}

export function LinksPanel({
  model: m,
  edges,
  selectedChunks,
  canReview,
  onJump,
  onReview,
}: {
  model: UniverseModel;
  edges: readonly ModelEdge[];
  selectedChunks: ReadonlySet<number>;
  canReview: boolean;
  onJump: (chunk: number) => void;
  onReview: (edge: ModelEdge, verdict: "accept" | "reject", reason?: string) => Promise<void>;
}) {
  const [open, setOpen] = useState(true);
  const [rejecting, setRejecting] = useState<string | null>(null);
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const groups = useMemo(() => linkRows(m, edges, selectedChunks, Date.now()), [m, edges, selectedChunks]);
  let total = 0;
  for (const g of groups.values()) total += g.length;
  if (total === 0) return null;

  const rule = async (edge: ModelEdge, verdict: "accept" | "reject", why?: string) => {
    setBusy(edge.id);
    setError(null);
    try {
      await onReview(edge, verdict, why);
      setRejecting(null);
      setReason("");
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  };

  let shown = 0;
  return (
    <div className="lu-links">
      <button type="button" className="lu-links__head" onClick={() => setOpen((o) => !o)} aria-expanded={open}>
        {open ? "▾" : "▸"} Links <span>{total}</span>
      </button>
      {open && (
        <div className="lu-links__body">
          {error && <div className="lu-links__error">{error}</div>}
          {[...groups.entries()].map(([kind, rows]) => (
            <div key={kind} className="lu-links__group">
              <div className="lu-links__kind">
                <i style={{ background: kindHex(kind) }} />
                {kind}
              </div>
              {rows.map((r) => {
                if (shown++ >= MAX_ROWS) return null;
                const e = r.edge;
                const proposed = e.status === "proposed";
                return (
                  <div key={e.id} className={r.expired ? "lu-link lu-link--expired" : "lu-link"}>
                    <button
                      type="button"
                      className="lu-link__target"
                      disabled={r.other < 0}
                      onClick={() => onJump(r.other)}
                      title={r.other < 0 ? "Its document is not on the map" : "Go to this star"}
                    >
                      {r.outgoing ? "→" : "←"} {r.otherTitle}
                    </button>
                    <span className="lu-link__meta">
                      <span className={`lu-badge lu-badge--${r.expired ? "expired" : e.status}`}>
                        {r.expired ? "expired" : e.status}
                      </span>
                      {e.method && <span>{e.method}</span>}
                      {e.confidence !== null && <span>{Math.round(e.confidence * 100)}%</span>}
                    </span>
                    {proposed && canReview && rejecting !== e.id && (
                      <span className="lu-link__rule">
                        <button type="button" disabled={busy === e.id} onClick={() => rule(e, "accept")} title="Accept">
                          ✓
                        </button>
                        <button
                          type="button"
                          disabled={busy === e.id}
                          onClick={() => {
                            setRejecting(e.id);
                            setReason("");
                          }}
                          title="Reject (asks why)"
                        >
                          ✕
                        </button>
                      </span>
                    )}
                    {rejecting === e.id && (
                      <form
                        className="lu-link__reason"
                        onSubmit={(ev) => {
                          ev.preventDefault();
                          if (reason.trim()) void rule(e, "reject", reason.trim());
                        }}
                      >
                        <input
                          autoFocus
                          value={reason}
                          onChange={(ev) => setReason(ev.target.value)}
                          placeholder="Why is it wrong?"
                          aria-label="Reason for rejecting"
                        />
                        <button type="submit" disabled={!reason.trim() || busy === e.id}>
                          reject
                        </button>
                        <button type="button" onClick={() => setRejecting(null)}>
                          cancel
                        </button>
                      </form>
                    )}
                  </div>
                );
              })}
            </div>
          ))}
          {total > MAX_ROWS && <div className="lu-links__more">{total - MAX_ROWS} more not listed</div>}
        </div>
      )}
    </div>
  );
}

/** Kinds offered for a new link: those already on the map, `references`
 *  excluded (the runtime writes it only from [[name]] links in bodies). */
export function linkKindChoices(kinds: readonly string[]): string[] {
  const out = kinds.filter((k) => k && k !== "references");
  if (!out.includes("related")) out.unshift("related");
  return out;
}

export function LinkDialog({
  model: m,
  from,
  to,
  onCancel,
  onSubmit,
}: {
  model: UniverseModel;
  from: number;
  to: number;
  onCancel: () => void;
  onSubmit: (kind: string, mode: "propose" | "assert") => Promise<void>;
}) {
  const choices = useMemo(() => linkKindChoices(m.kinds), [m.kinds]);
  const [kind, setKind] = useState(choices[0] ?? "related");
  const [mode, setMode] = useState<"propose" | "assert">("propose");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const title = (c: number) => starTitle(m, c, m.chunk.id[c] ?? "", "");

  const submit = async () => {
    const k = kind.trim();
    if (!k) return;
    setBusy(true);
    setError(null);
    try {
      await onSubmit(k, mode);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setBusy(false);
    }
  };

  return (
    <form
      className="lu-dialog"
      role="dialog"
      aria-label="New link"
      onSubmit={(e) => {
        e.preventDefault();
        void submit();
      }}
      onKeyDown={(e) => {
        if (e.key === "Escape") onCancel();
      }}
    >
      <div className="lu-dialog__ends">
        <span>{title(from)}</span>
        <span className="lu-dialog__arrow">→</span>
        <span>{title(to)}</span>
      </div>
      <label>
        kind
        <input
          autoFocus
          list="lu-link-kinds"
          value={kind}
          onChange={(e) => setKind(e.target.value)}
          aria-label="Link kind"
        />
        <datalist id="lu-link-kinds">
          {choices.map((k) => (
            <option key={k} value={k} />
          ))}
        </datalist>
      </label>
      <fieldset>
        <label title="Lands as proposed (dashed) and waits for a ruling">
          <input type="radio" name="lu-link-mode" checked={mode === "propose"} onChange={() => setMode("propose")} />
          propose for review
        </label>
        <label title="Sets the link at once">
          <input type="radio" name="lu-link-mode" checked={mode === "assert"} onChange={() => setMode("assert")} />
          assert now
        </label>
      </fieldset>
      {error && <div className="lu-dialog__error">{error}</div>}
      <div className="lu-dialog__actions">
        <button type="submit" disabled={busy || !kind.trim()}>
          {mode === "propose" ? "propose" : "link"}
        </button>
        <button type="button" onClick={onCancel}>
          cancel
        </button>
      </div>
    </form>
  );
}
