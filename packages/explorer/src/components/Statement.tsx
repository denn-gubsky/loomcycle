import { useState } from "react";
import type { StatementFields } from "../types";
import {
  clipQuote,
  evidencePreview,
  methodLabel,
  statusLabel,
  validityLabel,
  verdictHint,
} from "../lib/statement";

// StatementRuling is how a reference row hands an operator's ruling on a proposed
// statement to its host: accept (an optional reason) or reject (a reason). It
// resolves once the ruling is stored — the host refetches the edges — and rejects
// with the runtime's refusal, which the row shows inline.
export type StatementRuling = (id: string, accept: boolean, reason?: string) => Promise<void>;

// StatementMeta is what a reference row shows of the RFC FN envelope beyond the
// link itself: a status badge for a statement that does not simply hold, how it
// was made when that is unusual, whether it has been judged (the reason is the
// tooltip), when it is valid, and the first evidence quotes. Rendered inside the
// row's flex container; the quote list wraps onto its own line. Quotes are plain
// text nodes — never Markdown or HTML.
export function StatementMeta({ statement }: { statement: StatementFields }) {
  const status = statusLabel(statement);
  const method = methodLabel(statement);
  const verdict = verdictHint(statement);
  const validity = validityLabel(statement);
  const { rows, more } = evidencePreview(statement);
  return (
    <>
      {status && <span className={`doc-stmt-status ${status}`}>{status}</span>}
      {method && (
        <span className="doc-stmt-method" title="how this relation was made">
          {method}
        </span>
      )}
      {verdict && (
        <span className="doc-stmt-verdict" title={verdict}>
          judged
        </span>
      )}
      {validity && <span className="doc-stmt-valid">{validity}</span>}
      {rows.length > 0 && (
        <ul className="doc-stmt-evidence">
          {rows.map((ev) => (
            <li key={ev.id} title={ev.quote}>
              “{clipQuote(ev.quote)}”
              {ev.stale && <span className="doc-stmt-stale"> (source changed)</span>}
            </li>
          ))}
          {more > 0 && <li className="doc-stmt-more">+{more} more</li>}
        </ul>
      )}
    </>
  );
}

// StatementReview is the Accept / Reject control on a proposed statement.
// Reject opens an inline reason field (a rejection must say why); a refusal from
// the runtime is shown next to the buttons.
export function StatementReview({ id, onRule }: { id: string; onRule: StatementRuling }) {
  const [rejecting, setRejecting] = useState(false);
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const rule = async (accept: boolean) => {
    setBusy(true);
    setErr(null);
    try {
      await onRule(id, accept, accept ? undefined : reason.trim());
      setRejecting(false);
      setReason("");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <span className="doc-stmt-review">
      {rejecting ? (
        <>
          <input
            type="text"
            aria-label="Reason for rejecting"
            placeholder="why reject?"
            value={reason}
            disabled={busy}
            onChange={(e) => setReason(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && reason.trim()) void rule(false);
              if (e.key === "Escape") setRejecting(false);
            }}
            autoFocus
          />
          <button
            type="button"
            className="danger"
            disabled={busy || !reason.trim()}
            onClick={() => void rule(false)}
          >
            Reject
          </button>
          <button type="button" disabled={busy} onClick={() => setRejecting(false)}>
            Cancel
          </button>
        </>
      ) : (
        <>
          <button type="button" disabled={busy} onClick={() => void rule(true)}>
            Accept
          </button>
          <button type="button" disabled={busy} onClick={() => setRejecting(true)}>
            Reject…
          </button>
        </>
      )}
      {err && (
        <span className="doc-stmt-err" role="alert">
          {err}
        </span>
      )}
    </span>
  );
}
