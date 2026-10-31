import { useRef, useState } from "react";
import type { BrowseScope, DocEdge } from "../types";
import { useExplorerData } from "../lib/dataLayer";
import type { DocRulingAnswer } from "../lib/dataLayer";
import { ruleDocumentProposals, rulingNote } from "../lib/bulkReview";
import { chunkCount, undoRulings, type RulingAction, type RulingOutcome, type RulingScope } from "../lib/ruling";
import ChunkTarget from "./ChunkTarget";
import { StatementMeta } from "./Statement";

// ProposalReview is the Connections panel's bulk review of one document's
// proposals (RFC FS P1): pick rows, or take every proposal filed from the
// document, and accept or reject them in one ruling. Every ruling counts
// first (a dry run) and asks before it rules; a rejection needs a reason; the
// result offers an undo when the data layer can revert.
export interface ProposalReviewProps {
  documentId: string;
  proposals: DocEdge[];
  // The document's edge list stopped at its page cap, so proposals past it are
  // not listed here (the "all" actions still reach them).
  incomplete?: boolean;
  scope: RulingScope;
  browse?: BrowseScope;
  // Called after a ruling or an undo changed statements: the host refetches.
  onChanged: () => void;
  onSelectChunk: (id: string) => void;
  onOpenDocument?: (documentId: string, chunkId: string, title?: string) => void;
}

// SAMPLE is how many of the dry run's sample the confirm lists.
const SAMPLE = 3;

type Step =
  | { kind: "idle" }
  | { kind: "reason"; action: RulingAction; all: boolean }
  | { kind: "counting"; action: RulingAction; all: boolean }
  | { kind: "confirm"; action: RulingAction; all: boolean; dry: DocRulingAnswer; changed: boolean }
  | { kind: "ruling"; action: RulingAction; ruled?: number; total?: number };

interface Result {
  text: string;
  rulingIds: string[];
  undone?: boolean;
}

export default function ProposalReview({
  documentId,
  proposals,
  incomplete,
  scope,
  browse,
  onChanged,
  onSelectChunk,
  onOpenDocument,
}: ProposalReviewProps) {
  const data = useExplorerData();
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [step, setStep] = useState<Step>({ kind: "idle" });
  const [reason, setReason] = useState("");
  const [err, setErr] = useState<string | null>(null);
  const [result, setResult] = useState<Result | null>(null);
  const confirmRef = useRef<((ok: boolean) => void) | null>(null);

  // Only rows still listed count as picked: a reload drops the ruled ones.
  const listed = new Set(proposals.map((p) => p.id!));
  const picked = [...selected].filter((id) => listed.has(id));
  const busy = step.kind !== "idle" && step.kind !== "reason";

  const begin = (action: RulingAction, all: boolean) => {
    setErr(null);
    setResult(null);
    if (action === "reject") setStep({ kind: "reason", action, all });
    else void rule(action, all, "");
  };

  const rule = async (action: RulingAction, all: boolean, why: string) => {
    setStep({ kind: "counting", action, all });
    let out: RulingOutcome;
    try {
      out = await ruleDocumentProposals(
        data,
        { documentId, action, ids: all ? undefined : picked, reason: why.trim() || undefined, scope, browse },
        {
          confirm: (dry, reasonFor) =>
            new Promise<boolean>((resolve) => {
              confirmRef.current = resolve;
              setStep({ kind: "confirm", action, all, dry, changed: reasonFor !== "first" });
            }),
          onProgress: (p) => setStep({ kind: "ruling", action, ruled: p.ruled, total: p.total }),
        },
      );
    } catch (e) {
      out = { rulingIds: [], ruled: 0, failed: 0, skipped: 0, total: 0, cancelled: false, failures: [], error: e };
    }
    confirmRef.current = null;
    setStep({ kind: "idle" });
    setReason("");
    if (out.rulingIds.length > 0) {
      setSelected(new Set());
      setResult({ text: rulingNote(action, out), rulingIds: out.rulingIds });
      onChanged();
    } else if (out.error === undefined && !out.cancelled) {
      setResult({ text: "Nothing to rule: no proposals match.", rulingIds: [] });
    }
    if (out.error !== undefined) setErr(out.error instanceof Error ? out.error.message : String(out.error));
  };

  const answer = (ok: boolean) => {
    if (ok && step.kind === "confirm") setStep({ kind: "ruling", action: step.action });
    confirmRef.current?.(ok);
  };

  const undo = async () => {
    if (!result || !data.documentRevertRuling) return;
    const r = result;
    const u = await undoRulings((id) => data.documentRevertRuling!(id, scope, browse), r.rulingIds);
    const parts = [`${u.reverted} came back to review`];
    if (u.changed > 0) parts.push(`${u.changed} changed since and left as ruled`);
    if (u.errors.length > 0) parts.push(`not undone: ${u.errors.join("; ")}`);
    setResult({ ...r, undone: true, text: parts.join("; ") + "." });
    onChanged();
  };

  const toggle = (id: string) =>
    setSelected((cur) => {
      const next = new Set(cur);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  const allPicked = proposals.length > 0 && picked.length === proposals.length;
  // Nothing listed and nothing past the list: no "all" to rule.
  const noneToRule = proposals.length === 0 && !incomplete;

  return (
    <div className="doc-conn-group doc-bulk">
      <h5>Proposals from this document ({proposals.length})</h5>
      <div className="doc-bulk-bar">
        <label>
          <input
            type="checkbox"
            aria-label="select every proposal shown"
            checked={allPicked}
            disabled={busy || proposals.length === 0}
            onChange={() => setSelected(allPicked ? new Set() : new Set(listed))}
          />{" "}
          all shown
        </label>
        <button type="button" disabled={busy || picked.length === 0} onClick={() => begin("accept", false)}>
          Accept selected ({picked.length})
        </button>
        <button type="button" className="danger" disabled={busy || picked.length === 0} onClick={() => begin("reject", false)}>
          Reject selected…
        </button>
        <button type="button" disabled={busy || noneToRule} onClick={() => begin("accept", true)}>
          Accept all from this document
        </button>
        <button type="button" className="danger" disabled={busy || noneToRule} onClick={() => begin("reject", true)}>
          Reject all…
        </button>
      </div>

      {step.kind === "reason" && (
        <form
          className="doc-bulk-step"
          onSubmit={(e) => {
            e.preventDefault();
            if (reason.trim()) void rule(step.action, step.all, reason);
          }}
        >
          <input
            type="text"
            aria-label="Reason for rejecting"
            placeholder="why reject?"
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            autoFocus
          />
          <button type="submit" className="danger" disabled={!reason.trim()}>
            Count
          </button>
          <button type="button" onClick={() => setStep({ kind: "idle" })}>
            Cancel
          </button>
        </form>
      )}
      {step.kind === "counting" && <p className="doc-conn-empty">counting…</p>}
      {step.kind === "confirm" && <BulkConfirm step={step} onAnswer={answer} />}
      {step.kind === "ruling" && (
        <p className="doc-conn-empty">
          {step.total ? `ruled ${step.ruled ?? 0} of ${step.total}…` : "ruling…"}
        </p>
      )}
      {err && (
        <p className="doc-stmt-err" role="alert">
          {err}
        </p>
      )}
      {result && (
        <p className="doc-bulk-result" role="status">
          {result.text}
          {result.rulingIds.length > 0 && !result.undone && data.documentRevertRuling && (
            <button type="button" onClick={() => void undo()}>
              Undo
            </button>
          )}
        </p>
      )}

      {proposals.length === 0 ? (
        <p className="doc-conn-empty">none</p>
      ) : (
        <ul className="doc-conn-list">
          {proposals.map((e) => (
            <li className="doc-conn-row" key={e.id}>
              <input
                type="checkbox"
                aria-label={`select ${e.from_title ?? e.from_id} ${e.kind} ${e.to_title ?? e.to_id}`}
                checked={selected.has(e.id!)}
                disabled={busy}
                onChange={() => toggle(e.id!)}
              />
              <ChunkTarget
                id={e.from_id}
                title={e.from_title}
                targetDocumentId={e.from_document_id}
                documentId={documentId}
                onSelectChunk={onSelectChunk}
                onOpenDocument={onOpenDocument}
              />
              <span className="doc-ref-kind">{e.kind}</span>
              <ChunkTarget
                id={e.to_id}
                title={e.to_title}
                targetDocumentId={e.to_document_id}
                documentId={documentId}
                onSelectChunk={onSelectChunk}
                onOpenDocument={onOpenDocument}
              />
              <StatementMeta statement={e} />
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// BulkConfirm is the dry run before a document's ruling: how many, a sample,
// what will be skipped.
export function BulkConfirm({
  step,
  onAnswer,
}: {
  step: { action: RulingAction; all: boolean; dry: DocRulingAnswer; changed: boolean };
  onAnswer: (ok: boolean) => void;
}) {
  const { dry } = step;
  const verb = step.action === "accept" ? "Accept" : "Reject";
  const chunks = chunkCount(dry.matched);
  return (
    <div className="doc-bulk-step doc-bulk-confirm">
      {step.changed && <p className="doc-bulk-warn">The proposals changed since the last count; counted again.</p>}
      <p>
        {verb} {dry.matched} statement{dry.matched === 1 ? "" : "s"}
        {step.all ? " filed from this document" : ""}
        {chunks > 1 ? `, in ${chunks} rulings of up to 1,000` : ""}?
        {dry.skipped > 0 && ` ${dry.skipped} of them are not yours to rule and will be skipped.`}
      </p>
      {(dry.sample ?? []).length > 0 && (
        <ul className="doc-bulk-sample">
          {(dry.sample ?? []).slice(0, SAMPLE).map((s) => (
            <li key={s.id}>
              {s.from_title || s.from_id} <span className="doc-ref-kind">{s.kind}</span> {s.to_title || s.to_id}
            </li>
          ))}
        </ul>
      )}
      <button type="button" className={step.action === "reject" ? "danger" : undefined} onClick={() => onAnswer(true)}>
        {verb} {dry.matched}
      </button>
      <button type="button" onClick={() => onAnswer(false)}>
        Cancel
      </button>
    </div>
  );
}
