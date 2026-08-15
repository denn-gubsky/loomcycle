import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import {
  documentGetChunk,
  judgeFact,
  listFacts,
  type BrowseScope,
  type FactRow,
} from "../api";
import { factActions, factVerdict } from "../lib/factVerdict";

// The facts a scope holds, with the evidence each was drawn from and whatever verdict it
// carries — and the two controls that let a person overrule the judge.
//
// THE SAFETY VALVE IS THE POINT. Verified writes shipped with "a wrong verdict is always
// recoverable by re-judging" as its argument for withholding rather than deleting, and
// that recovery existed only as an API call. A judge that wrongly refuses a true fact had
// no operator-reachable fix.
//
// THE SPAN IS SHOWN, NOT HIDDEN BEHIND A DISCLOSURE. It is the evidence; a surface that
// tucks it away invites judging a claim on how plausible it reads, which is the failure
// the whole line exists to prevent.
export default function FactsPanel({ browse }: { browse?: BrowseScope }) {
  const [facts, setFacts] = useState<FactRow[] | null>(null);
  const [includeRefuted, setIncludeRefuted] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  // Which row is asking for a reason, and in which direction. A verdict without a stated
  // ground is refused by the server, and it should be: an operator reading "withheld"
  // months from now needs to know why.
  const [arming, setArming] = useState<{ id: string; good: boolean } | null>(null);
  const [reason, setReason] = useState("");
  // Bodies are fetched per opened row. The listing carries a claim truncated at 80
  // characters, which is the whole sentence for most facts and visibly clipped when not.
  const [expanded, setExpanded] = useState<Record<string, string>>({});

  const load = useCallback(async () => {
    setBusy(true);
    try {
      const r = await listFacts("user", { includeRefuted, limit: 50 }, browse);
      setFacts(r.facts ?? []);
      setErr(null);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
      setFacts(null);
    } finally {
      setBusy(false);
    }
  }, [browse, includeRefuted]);

  useEffect(() => {
    void load();
  }, [load]);

  const apply = async (id: string, good: boolean) => {
    if (!reason.trim()) return;
    setBusy(true);
    try {
      // No marker in the reason: the server stamps who judged from the call's own
      // context, so an off-run call like this one records "operator" without asking.
      await judgeFact(id, good ? "supported" : "unsupported", reason, "user", browse);
      setArming(null);
      setReason("");
      await load();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
      setBusy(false);
    }
  };

  const expand = async (f: FactRow) => {
    if (expanded[f.id] !== undefined) {
      setExpanded((prev) => {
        const next = { ...prev };
        delete next[f.id];
        return next;
      });
      return;
    }
    try {
      const c = await documentGetChunk(f.id, "user", browse);
      setExpanded((prev) => ({ ...prev, [f.id]: (c as { body?: string }).body ?? f.title }));
    } catch {
      // A body that will not load is not worth an error banner over the whole panel —
      // the row still shows its claim and its evidence.
      setExpanded((prev) => ({ ...prev, [f.id]: f.title }));
    }
  };

  return (
    <div className="facts-panel">
      <div className="settings-row">
        <label className="facts-toggle">
          <input
            type="checkbox"
            checked={includeRefuted}
            onChange={(e) => setIncludeRefuted(e.target.checked)}
          />
          show refused facts
        </label>
        <button type="button" onClick={() => void load()} disabled={busy}>
          {busy ? "reading…" : "Refresh"}
        </button>
      </div>

      {err && <div className="settings-error">{err}</div>}

      {facts && facts.length === 0 && (
        <div className="settings-muted">
          {includeRefuted ? "No facts stored." : "No facts currently returned."}
        </div>
      )}

      {facts?.map((f) => {
        const v = factVerdict(f.entity);
        const a = factActions(v);
        const armed = arming?.id === f.id;
        return (
          <div key={f.id} className="fact-row" data-state={v.state}>
            <div className="fact-claim">
              <button type="button" className="fact-expand" onClick={() => void expand(f)}>
                {expanded[f.id] !== undefined ? "−" : "+"}
              </button>
              <span>{expanded[f.id] ?? f.title}</span>
              <span className={"fact-badge fact-badge-" + v.state}>{v.state}</span>
            </div>

            {/* The evidence, always visible. */}
            {f.entity?.source_quote ? (
              <blockquote className="fact-span">{f.entity.source_quote}</blockquote>
            ) : (
              <div className="fact-span fact-span-missing">
                no source span — this fact cannot be verified by anyone
              </div>
            )}

            {v.reason && (
              <div className="settings-muted fact-reason">
                {v.byOperator ? "an operator" : v.judgedBy || "an earlier verdict"} said:{" "}
                {v.reason}
              </div>
            )}

            <div className="settings-row-actions">
              {a.canMarkWrong && (
                <button type="button" className="ghost-btn" disabled={busy}
                        onClick={() => { setArming({ id: f.id, good: false }); setReason(""); }}>
                  mark wrong
                </button>
              )}
              {a.canMarkGood && (
                <button type="button" disabled={busy}
                        onClick={() => { setArming({ id: f.id, good: true }); setReason(""); }}>
                  {v.state === "withheld" ? "restore" : "mark good"}
                </button>
              )}
              <Link className="settings-action-link" to={`/documents/${encodeURIComponent(f.document_id)}?scope=user`}>
                open →
              </Link>
            </div>

            {armed && (
              <div className="settings-row fact-reason-form">
                <input
                  autoFocus
                  value={reason}
                  placeholder={arming.good ? "why is this right?" : "why is this wrong?"}
                  onChange={(e) => setReason(e.target.value)}
                />
                <button type="button" disabled={busy || !reason.trim()}
                        onClick={() => void apply(f.id, arming.good)}>
                  save verdict
                </button>
                <button type="button" className="ghost-btn" onClick={() => setArming(null)}>
                  cancel
                </button>
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}
