import { useEffect, useMemo, useState } from "react";
import type { AcceptClassificationAnswer, BrowseScope, ClassifyAnswer, DocScope } from "../types";
import { useExplorerData } from "../lib/dataLayer";
import {
  acceptSummary,
  canApply,
  classDisposition,
  classifyErrorText,
  formatShare,
  pendingClassification,
  resultLabel,
  summarizeStep,
} from "../lib/classify";

// Classification is the RFC FO classification surface of the document viewer,
// rendered between the toolbar and the chunk split:
//   - the Classify panel — opened from the toolbar, it first runs classify with
//     write=report (nothing written) and shows the answer; Apply then runs it
//     with write=propose and reloads the document.
//   - a Pending block whenever the root chunk's classification record waits for
//     an operator, with Accept (accept_classification).
// Every answer string (quotes, class and concept names, warnings) is rendered as
// a plain text node — never Markdown or HTML. Refusals show inline.
//
// The host keys this component by document, so its state never outlives one.
export interface ClassificationProps {
  documentId: string;
  scope: DocScope;
  browse?: BrowseScope;
  // The root chunk's fields (undefined until loaded) — the pending record's source.
  rootFields: unknown;
  // Non-zero while the panel is open; each change to a non-zero value (the
  // toolbar's Classify toggle) runs a fresh report. 0 = closed.
  requestKey: number;
  onClose: () => void;
  // Reloads the document after a write (Apply / Accept).
  onChanged: () => void;
}

export default function Classification({
  documentId,
  scope,
  browse,
  rootFields,
  requestKey,
  onClose,
  onChanged,
}: ClassificationProps) {
  const data = useExplorerData();
  const [answer, setAnswer] = useState<ClassifyAnswer | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [accepting, setAccepting] = useState(false);
  const [acceptErr, setAcceptErr] = useState<string | null>(null);
  const [accepted, setAccepted] = useState<AcceptClassificationAnswer | null>(null);

  const pending = useMemo(
    () => (data.documentAcceptClassification ? pendingClassification(rootFields) : undefined),
    [data, rootFields],
  );

  // Each opening runs a report — nothing is written until Apply.
  useEffect(() => {
    if (requestKey === 0 || !data.documentClassify) return;
    let cancelled = false;
    setBusy(true);
    setErr(null);
    setAnswer(null);
    data
      .documentClassify(documentId, "report", scope, browse)
      .then((a) => !cancelled && setAnswer(a))
      .catch((e) => !cancelled && setErr(classifyErrorText(e)))
      .finally(() => !cancelled && setBusy(false));
    return () => {
      cancelled = true;
    };
  }, [data, documentId, scope, browse, requestKey]);

  const apply = async () => {
    if (!data.documentClassify) return;
    setBusy(true);
    setErr(null);
    try {
      setAnswer(await data.documentClassify(documentId, "propose", scope, browse));
      onChanged();
    } catch (e) {
      setErr(classifyErrorText(e));
    } finally {
      setBusy(false);
    }
  };

  const accept = async () => {
    if (!data.documentAcceptClassification) return;
    setAccepting(true);
    setAcceptErr(null);
    try {
      setAccepted(await data.documentAcceptClassification(documentId, scope, browse));
      onChanged();
    } catch (e) {
      setAcceptErr(classifyErrorText(e));
    } finally {
      setAccepting(false);
    }
  };

  const open = requestKey > 0;
  if (!open && !pending && !accepted && !acceptErr) return null;
  const acceptedSummary = accepted ? acceptSummary(accepted) : undefined;

  return (
    <div className="doc-classify">
      {pending && (
        <div className="doc-classify-pending">
          <span className="doc-classify-label">Pending classification</span>
          {pending.proposedType && (
            <span>
              class <strong>{pending.proposedType}</strong>
            </span>
          )}
          {pending.proposedTags.length > 0 && (
            <span>
              tags{" "}
              {pending.proposedTags.map((t) => (
                <span className="chunk-tag" key={t}>
                  {t}
                </span>
              ))}
            </span>
          )}
          {pending.statementCount > 0 && (
            <span>
              {pending.statementCount} proposed statement{pending.statementCount === 1 ? "" : "s"}
            </span>
          )}
          <button type="button" disabled={accepting} onClick={() => void accept()}>
            {accepting ? "Accepting…" : "Accept"}
          </button>
        </div>
      )}
      {acceptErr && (
        <div className="doc-classify-err" role="alert">
          {acceptErr}
          <button type="button" className="doc-classify-dismiss" onClick={() => setAcceptErr(null)} aria-label="Dismiss">
            ×
          </button>
        </div>
      )}
      {acceptedSummary && (
        <div className="doc-classify-notice" role="status">
          <span>{acceptedSummary.applied}</span>
          {acceptedSummary.notices.map((n, i) => (
            <span className="doc-classify-warn" key={i}>
              {n}
            </span>
          ))}
          <button type="button" className="doc-classify-dismiss" onClick={() => setAccepted(null)} aria-label="Dismiss">
            ×
          </button>
        </div>
      )}
      {open && (
        <div className="doc-classify-panel">
          <div className="doc-classify-head">
            <h4>Classify</h4>
            {answer && <span className={`doc-classify-result ${answer.result}`}>{resultLabel(answer)}</span>}
            {answer?.write === "report" && <span className="doc-classify-hint">report only — nothing written</span>}
            <button type="button" className="doc-classify-dismiss" onClick={onClose} aria-label="Close">
              ×
            </button>
          </div>
          {!answer && !err && <p className="doc-classify-hint">Classifying…</p>}
          {err && (
            <p className="doc-classify-err" role="alert">
              {err}
            </p>
          )}
          {answer && <ClassifyAnswerView answer={answer} />}
          {answer && canApply(answer) && (
            <div className="doc-classify-actions">
              <button
                type="button"
                disabled={busy}
                onClick={() => void apply()}
                title="Apply a unique rule match to the document; file everything else for review"
              >
                {busy ? "Applying…" : "Apply"}
              </button>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

export function ClassifyAnswerView({ answer }: { answer: ClassifyAnswer }) {
  const disposition = classDisposition(answer);
  const fields = Object.entries(answer.fields ?? {});
  const issuers = answer.issued_by ?? [];
  const concepts = answer.concepts ?? [];
  const steps = (answer.steps ?? []).map(summarizeStep);
  const written = answer.statements?.length ?? 0;
  return (
    <>
      <dl className="doc-classify-rows">
        {answer.class && (
          <>
            <dt>class</dt>
            <dd>
              <strong>{answer.class}</strong>
              {disposition && <span className="doc-classify-hint"> ({disposition})</span>}
            </dd>
          </>
        )}
        {answer.work_key && (
          <>
            <dt>work key</dt>
            <dd className="doc-classify-mono">{answer.work_key}</dd>
          </>
        )}
        {fields.length > 0 && (
          <>
            <dt>fields</dt>
            <dd className="doc-classify-mono">
              {fields.map(([k, v]) => `${k}=${typeof v === "string" ? v : JSON.stringify(v)}`).join(" · ")}
            </dd>
          </>
        )}
        {issuers.length > 0 && (
          <>
            <dt>issued by</dt>
            <dd>
              {issuers.map((iss, i) => (
                <span className="doc-classify-item" key={`${iss.instance}-${i}`} title={iss.quote}>
                  {iss.instance}
                  {iss.quote && <span className="doc-classify-hint"> “{iss.quote}”</span>}
                </span>
              ))}
            </dd>
          </>
        )}
        {concepts.length > 0 && (
          <>
            <dt>concepts</dt>
            <dd>
              {concepts.map((c) => (
                <span className="doc-classify-item" key={c.tag} title={c.tag}>
                  {c.concept}
                  <span className="doc-classify-hint"> {formatShare(c.share)}</span>
                </span>
              ))}
            </dd>
          </>
        )}
        {written > 0 && (
          <>
            <dt>statements</dt>
            <dd>
              {written} written
              {answer.statements!.some((s) => s.status === "proposed") && (
                <span className="doc-classify-hint"> (proposed ones wait for review)</span>
              )}
            </dd>
          </>
        )}
      </dl>
      {steps.length > 0 && (
        <ol className="doc-classify-steps">
          {steps.map((s, i) => (
            <li key={`${s.label}-${i}`}>
              <span className="doc-classify-step">{s.label}</span>
              {s.method && <span className="doc-classify-hint"> [{s.method}]</span>}
              {s.chosen ? <> → <strong>{s.chosen}</strong></> : <span className="doc-classify-hint"> no choice</span>}
              {s.neighbours != null && <span className="doc-classify-hint"> · {s.neighbours} neighbours</span>}
              {s.reason && <span className="doc-classify-hint"> — {s.reason}</span>}
              {s.candidates.length > 0 && (
                <details className="doc-classify-candidates">
                  <summary>
                    {s.candidates.length} candidate{s.candidates.length === 1 ? "" : "s"}
                  </summary>
                  <ul>
                    {s.candidates.map((c, j) => (
                      <li key={`${c.class}-${j}`}>
                        <strong>{c.class}</strong>
                        {c.detail && <span className="doc-classify-hint"> {c.detail}</span>}
                      </li>
                    ))}
                  </ul>
                </details>
              )}
            </li>
          ))}
        </ol>
      )}
      {answer.warnings && answer.warnings.length > 0 && (
        <ul className="doc-classify-warnings">
          {answer.warnings.map((w, i) => (
            <li className="doc-classify-warn" key={i}>
              {w}
            </li>
          ))}
        </ul>
      )}
    </>
  );
}
