import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { InterruptRow, listUserInterrupts, resolveInterrupt } from "../api";
import { useUserId } from "../components/Layout";

// Auto-refresh interval. Polls /v1/users/{user_id}/interrupts because
// there's no global SSE feed of pending-interrupt state today; once
// the v0.8.x channel-subscribe-via-SSE lands the page would subscribe
// to _system/interrupts/pending instead.
const REFRESH_MS = 3_000;

// InterruptInbox is the user-scoped "what do I need to answer?" view.
// Shows pending interrupts for the currently-selected user_id; clicking
// a row opens the Answer modal which posts to the v0.8.16 resolve
// endpoint.
export default function InterruptInbox() {
  const userId = useUserId();
  const [rows, setRows] = useState<InterruptRow[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [active, setActive] = useState<InterruptRow | null>(null);

  useEffect(() => {
    if (!userId) {
      setRows([]);
      return;
    }
    let cancelled = false;
    const fetchOnce = async () => {
      try {
        setLoading(true);
        const resp = await listUserInterrupts(userId, "pending");
        if (!cancelled) {
          setRows(resp.interrupts ?? []);
          setErr(null);
        }
      } catch (e) {
        if (!cancelled) setErr(e instanceof Error ? e.message : String(e));
      } finally {
        if (!cancelled) setLoading(false);
      }
    };
    fetchOnce();
    const t = setInterval(fetchOnce, REFRESH_MS);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, [userId]);

  if (!userId) {
    return (
      <div className="empty">
        <p>
          Enter a <code>user_id</code> in the top bar to see pending
          interrupts.
        </p>
      </div>
    );
  }

  return (
    <div className="interrupt-inbox">
      <div className="toolbar">
        <h2>Pending interrupts</h2>
        <div className="meta">
          {loading && <span className="spin">refreshing…</span>}
          <span>
            {rows.length} pending for {userId}
          </span>
        </div>
      </div>
      {err && <div className="err">{err}</div>}
      {rows.length === 0 && !loading && (
        <div className="empty">
          <p>
            No pending interrupts for <code>{userId}</code>.
          </p>
        </div>
      )}
      <ul className="interrupt-list">
        {rows.map((r) => (
          <li key={r.interrupt_id} className={`interrupt-row priority-${r.priority}`}>
            <div className="interrupt-meta">
              <span className={`pill ${r.priority}`}>{r.priority}</span>
              <Link to={`/agents/${r.agent_id ?? r.run_id}`} className="agent-link">
                {r.agent_name || r.agent_id || r.run_id.slice(0, 12)}
              </Link>
              <span className="time">{relativeTime(r.created_at)}</span>
              {r.expires_at && (
                <span className="expires" title={`expires ${r.expires_at}`}>
                  expires in {relativeUntil(r.expires_at)}
                </span>
              )}
            </div>
            <div className="interrupt-question">{r.question}</div>
            {r.context_data && (
              <div className="interrupt-context">{r.context_data}</div>
            )}
            <div className="interrupt-actions">
              <button type="button" onClick={() => setActive(r)}>
                Answer
              </button>
            </div>
          </li>
        ))}
      </ul>
      {active && (
        <AnswerModal
          row={active}
          onClose={() => setActive(null)}
          onResolved={() => {
            setActive(null);
            // The polling refresh picks up the change within
            // REFRESH_MS; nothing else to do here.
          }}
        />
      )}
    </div>
  );
}

interface AnswerModalProps {
  row: InterruptRow;
  onClose: () => void;
  onResolved: () => void;
}

function AnswerModal({ row, onClose, onResolved }: AnswerModalProps) {
  const [answer, setAnswer] = useState<string>("");
  const [submitting, setSubmitting] = useState(false);
  const [submitErr, setSubmitErr] = useState<string | null>(null);
  const hasOptions = row.options && row.options.length > 0;

  const submit = async () => {
    if (!answer && !hasOptions) {
      setSubmitErr("Answer is required for free-text interrupts.");
      return;
    }
    try {
      setSubmitting(true);
      setSubmitErr(null);
      await resolveInterrupt(row.run_id, row.interrupt_id, answer);
      onResolved();
    } catch (e) {
      setSubmitErr(e instanceof Error ? e.message : String(e));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <h3>{row.question}</h3>
        {row.context_data && <p className="modal-context">{row.context_data}</p>}
        {hasOptions ? (
          <div className="modal-options">
            {row.options!.map((opt) => (
              <button
                key={opt}
                type="button"
                className={answer === opt ? "option selected" : "option"}
                onClick={() => setAnswer(opt)}
                disabled={submitting}
              >
                {opt}
              </button>
            ))}
          </div>
        ) : (
          <textarea
            className="modal-textarea"
            value={answer}
            onChange={(e) => setAnswer(e.target.value)}
            placeholder="Type your answer…"
            disabled={submitting}
            autoFocus
            rows={4}
          />
        )}
        {submitErr && <div className="modal-err">{submitErr}</div>}
        <div className="modal-buttons">
          <button type="button" onClick={onClose} disabled={submitting}>
            Cancel
          </button>
          <button
            type="button"
            className="primary"
            onClick={submit}
            disabled={submitting || (!answer && !hasOptions)}
          >
            {submitting ? "Submitting…" : "Submit answer"}
          </button>
        </div>
      </div>
    </div>
  );
}

// relativeTime / relativeUntil — same shape as in RunList.tsx, kept
// inline rather than extracted so this page stays self-contained for
// review.
function relativeTime(iso: string): string {
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return iso;
  const ms = Date.now() - t;
  if (ms < 60_000) return `${Math.floor(ms / 1000)}s ago`;
  if (ms < 3_600_000) return `${Math.floor(ms / 60_000)}m ago`;
  if (ms < 86_400_000) return `${Math.floor(ms / 3_600_000)}h ago`;
  return new Date(iso).toLocaleString();
}

function relativeUntil(iso: string): string {
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return "—";
  const ms = t - Date.now();
  if (ms <= 0) return "now";
  if (ms < 60_000) return `${Math.floor(ms / 1000)}s`;
  if (ms < 3_600_000) return `${Math.floor(ms / 60_000)}m`;
  if (ms < 86_400_000) return `${Math.floor(ms / 3_600_000)}h`;
  return `${Math.floor(ms / 86_400_000)}d`;
}
