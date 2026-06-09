import { useState } from "react";
import { scheduleDefCreate } from "../api";

interface Props {
  existingNames: string[];
  onClose: () => void;
  onCreated: () => void;
}

// ScheduleCreateForm authors a brand-new schedule from scratch (no
// template). Sibling of ScheduleForkForm — fork inherits a template's
// agent/prompt and only overlays identity, whereas create supplies the
// full minimum: name + agent + cron, plus an optional prompt and the
// same identity/credentials fields. Submits via scheduleDefCreate
// (op:"create", promote on by default). The cron-XOR-user_tier_schedules
// and agent-required invariants are enforced server-side.
export default function ScheduleCreateForm({ existingNames, onClose, onCreated }: Props) {
  const [name, setName] = useState("");
  const [agent, setAgent] = useState("");
  const [cron, setCron] = useState("");
  const [prompt, setPrompt] = useState("");
  const [userID, setUserID] = useState("");
  const [userTier, setUserTier] = useState("");
  const [timezone, setTimezone] = useState("");
  const [credentialsJSON, setCredentialsJSON] = useState('{"":""}');
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!name.trim()) {
      setErr("name is required.");
      return;
    }
    if (existingNames.includes(name.trim())) {
      setErr(`A schedule named "${name.trim()}" already exists. Open it and use Fork instead.`);
      return;
    }
    if (!agent.trim()) {
      setErr("agent is required.");
      return;
    }
    if (!cron.trim()) {
      setErr("schedule (cron) is required.");
      return;
    }

    let credentials: Record<string, string> = {};
    if (credentialsJSON.trim()) {
      try {
        const parsed = JSON.parse(credentialsJSON);
        if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
          throw new Error("credentials must be a JSON object");
        }
        for (const [k, v] of Object.entries(parsed)) {
          if (k && typeof v === "string") credentials[k] = v;
        }
      } catch (e) {
        setErr(`credentials JSON: ${e instanceof Error ? e.message : String(e)}`);
        return;
      }
    }

    const overlay: Record<string, unknown> = {
      agent: agent.trim(),
      schedule: cron.trim(),
    };
    if (prompt.trim()) {
      // Operator-authored → trusted-text (matches the run path's segment
      // shape; the scheduler replays this verbatim as the run's input).
      overlay.prompt = [
        { role: "user", content: [{ type: "trusted-text", text: prompt }] },
      ];
    }
    if (userID.trim()) overlay.user_id = userID.trim();
    if (userTier.trim()) overlay.user_tier = userTier.trim();
    if (timezone.trim()) overlay.timezone = timezone.trim();
    if (Object.keys(credentials).length > 0) overlay.user_credentials = credentials;

    setBusy(true);
    setErr(null);
    try {
      await scheduleDefCreate({ name: name.trim(), overlay, promote: true });
      onCreated();
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
          <h3>New schedule</h3>
          <button onClick={onClose} className="modal-close">
            ×
          </button>
        </div>
        <form onSubmit={handleSubmit} className="modal-body">
          <label className="modal-field">
            <span>name</span>
            <input
              type="text"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="nightly-digest"
              autoFocus
            />
          </label>
          <label className="modal-field">
            <span>agent</span>
            <input
              type="text"
              value={agent}
              onChange={(e) => setAgent(e.target.value)}
              placeholder="agent name to run"
            />
          </label>
          <label className="modal-field">
            <span>schedule (cron)</span>
            <input
              type="text"
              value={cron}
              onChange={(e) => setCron(e.target.value)}
              placeholder='e.g. "0 6 * * *"'
            />
          </label>
          <label className="modal-field">
            <span>prompt (optional)</span>
            <textarea
              value={prompt}
              onChange={(e) => setPrompt(e.target.value)}
              rows={4}
              placeholder="What the agent should do each run…"
              spellCheck={false}
            />
          </label>
          <label className="modal-field">
            <span>user_id (optional)</span>
            <input
              type="text"
              value={userID}
              onChange={(e) => setUserID(e.target.value)}
              placeholder="alice@example.com"
            />
          </label>
          <label className="modal-field">
            <span>user_tier (optional)</span>
            <input
              type="text"
              value={userTier}
              onChange={(e) => setUserTier(e.target.value)}
              placeholder="high | middle | low"
            />
          </label>
          <label className="modal-field">
            <span>timezone (optional)</span>
            <input
              type="text"
              value={timezone}
              onChange={(e) => setTimezone(e.target.value)}
              placeholder="UTC (default) | America/New_York"
            />
          </label>
          <label className="modal-field">
            <span>user_credentials (JSON, optional)</span>
            <textarea
              value={credentialsJSON}
              onChange={(e) => setCredentialsJSON(e.target.value)}
              rows={3}
              placeholder='{"jobs": "<bearer>"}'
              className="modal-credentials-textarea"
              spellCheck={false}
            />
          </label>
          <div className="modal-help">
            Credentials are stored in the substrate row (plaintext); the
            operator owns rotation. Leave blank if the agent's MCP tools don't
            need per-run bearers.
          </div>
          {err && <div className="modal-err">{err}</div>}
          <div className="modal-actions">
            <button type="button" onClick={onClose} disabled={busy}>
              Cancel
            </button>
            <button type="submit" disabled={busy}>
              {busy ? "Creating…" : "Create"}
            </button>
          </div>
        </form>
      </div>
    </div>
  );
}
