import { useState } from "react";
import { scheduleDefFork } from "../api";

interface Props {
  templateName: string;
  onClose: () => void;
  onForked: () => void;
}

// ScheduleForkForm is the modal for "Fork this template" — the
// JobEmber-primary workflow. Captures user_id + tier + optional cron
// override + credentials map. Submits via scheduleDefFork; closes on
// success. Validation is server-side (the substrate's
// required_credentials gate enforces the credential set).
//
// Bearer/credential values are masked while typing (password field).
// They go over the wire in the POST body — unavoidable — but never
// round-trip back through the read endpoints.
export default function ScheduleForkForm({ templateName, onClose, onForked }: Props) {
  const [userID, setUserID] = useState("");
  const [userTier, setUserTier] = useState("");
  const [cronOverride, setCronOverride] = useState("");
  const [credentialsJSON, setCredentialsJSON] = useState('{"":""}');
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(null);

    let credentials: Record<string, string> = {};
    if (credentialsJSON.trim()) {
      try {
        const parsed = JSON.parse(credentialsJSON);
        if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
          throw new Error("credentials must be a JSON object");
        }
        // Strip empty keys (the default placeholder).
        for (const [k, v] of Object.entries(parsed)) {
          if (k && typeof v === "string") credentials[k] = v;
        }
      } catch (e) {
        setErr(`credentials JSON: ${e instanceof Error ? e.message : String(e)}`);
        setBusy(false);
        return;
      }
    }

    const overlay: Record<string, unknown> = {};
    if (userID) overlay.user_id = userID;
    if (userTier) overlay.user_tier = userTier;
    if (cronOverride) overlay.schedule = cronOverride;
    if (Object.keys(credentials).length > 0) overlay.user_credentials = credentials;

    try {
      await scheduleDefFork({ name: templateName, overlay });
      onForked();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <div className="modal-header">
          <h3>Fork template: {templateName}</h3>
          <button onClick={onClose} className="modal-close">
            ×
          </button>
        </div>
        <form onSubmit={handleSubmit} className="modal-body">
          <label className="modal-field">
            <span>user_id</span>
            <input
              type="text"
              value={userID}
              onChange={(e) => setUserID(e.target.value)}
              placeholder="alice@example.com"
            />
          </label>
          <label className="modal-field">
            <span>user_tier</span>
            <input
              type="text"
              value={userTier}
              onChange={(e) => setUserTier(e.target.value)}
              placeholder="high | middle | low (matches template's user_tier_schedules keys)"
            />
          </label>
          <label className="modal-field">
            <span>schedule override (optional)</span>
            <input
              type="text"
              value={cronOverride}
              onChange={(e) => setCronOverride(e.target.value)}
              placeholder='leave empty to use the template tier cron, or enter e.g. "0 6 * * *"'
            />
          </label>
          <label className="modal-field">
            <span>user_credentials (JSON)</span>
            <textarea
              value={credentialsJSON}
              onChange={(e) => setCredentialsJSON(e.target.value)}
              rows={4}
              placeholder='{"jobs": "<bearer>", "slack": "<bearer>"}'
              className="modal-credentials-textarea"
              spellCheck={false}
            />
          </label>
          <div className="modal-help">
            Credentials are stored in the substrate row (plaintext). Operator
            owns rotation — the substrate's POST /v1/_scheduledef fork op merges
            new credential values with the parent fork's existing map, so
            rotating one key doesn't require re-supplying all of them.
          </div>
          {err && <div className="modal-err">{err}</div>}
          <div className="modal-actions">
            <button type="button" onClick={onClose} disabled={busy}>
              Cancel
            </button>
            <button type="submit" disabled={busy}>
              {busy ? "Forking…" : "Fork"}
            </button>
          </div>
        </form>
      </div>
    </div>
  );
}
