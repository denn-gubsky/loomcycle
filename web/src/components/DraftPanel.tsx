import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { deleteConfiguredRun, updateConfiguredRun, type Agent } from "../api";
import { draftPromptText, draftSettings } from "../lib/draft";

// DraftPanel is the detail pane for a CONFIGURED run — created, not started.
// Nothing has run: it holds no slot and no budget. The operator can edit the
// prompt, see what else it will run with, start it (in the run terminal) or
// discard it.
//
// Only the plain-text prompt shape (one user segment, one trusted-text block —
// what the run form creates) is editable here. A richer draft is shown but not
// offered a text box: rewriting it through one would silently flatten it.
export default function DraftPanel({
  agent,
  onDraftChange,
  onDiscarded,
}: {
  agent: Agent;
  onDraftChange: (draft: Record<string, unknown>) => void;
  onDiscarded: () => void;
}) {
  const stored = draftPromptText(agent.draft);
  const [prompt, setPrompt] = useState(stored ?? "");
  const [saving, setSaving] = useState(false);
  const [confirmDiscard, setConfirmDiscard] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  // A different draft (or a saved edit coming back) resets the editor.
  useEffect(() => {
    setPrompt(stored ?? "");
    setConfirmDiscard(false);
    setErr(null);
  }, [agent.run_id, stored]);

  const dirty = stored !== null && prompt !== stored;
  const settings = draftSettings(agent.draft);
  const startHref = `/run?start_draft=${encodeURIComponent(agent.run_id)}&draft_agent=${encodeURIComponent(agent.agent_id)}`;

  const save = async () => {
    if (!prompt.trim()) {
      setErr("The prompt cannot be empty.");
      return;
    }
    setSaving(true);
    setErr(null);
    try {
      const updated = await updateConfiguredRun(agent.run_id, { prompt });
      onDraftChange(updated.draft);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  };

  const discard = async () => {
    setBusy(true);
    setErr(null);
    try {
      await deleteConfiguredRun(agent.run_id);
      onDiscarded();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
      setConfirmDiscard(false);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="draft-panel">
      <div className="draft-panel-head">
        <strong>Draft — not started</strong>
        <span className="draft-panel-hint">
          Nothing has run yet: it holds no slot and no budget until you start it.
        </span>
      </div>

      {stored !== null ? (
        <div className="library-form-row">
          <label htmlFor="draft-prompt">prompt</label>
          <textarea
            id="draft-prompt"
            className="library-prompt-textarea"
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            disabled={saving || busy}
            rows={6}
          />
        </div>
      ) : (
        <div className="draft-panel-hint">
          This draft's prompt is not plain text (several segments, images or untrusted blocks), so it is not
          editable here. Edit it through the API.
        </div>
      )}

      {Object.keys(settings).length > 0 && (
        <details className="draft-panel-settings">
          <summary>runs with</summary>
          <pre className="mono">{JSON.stringify(settings, null, 2)}</pre>
        </details>
      )}

      {err && <div className="modal-err">{err}</div>}

      <div className="run-form-actions">
        {stored !== null && (
          <button type="button" disabled={!dirty || saving || busy} onClick={save}>
            {saving ? "Saving…" : "Save prompt"}
          </button>
        )}
        {dirty ? (
          <button type="button" className="primary" disabled title="Save or undo your edit first">
            Start
          </button>
        ) : (
          <Link className="resume-btn" to={startHref}>
            Start
          </Link>
        )}
        {confirmDiscard ? (
          <>
            <button type="button" className="cancel-btn" disabled={busy} onClick={discard}>
              {busy ? "Discarding…" : "Confirm discard"}
            </button>
            <button type="button" disabled={busy} onClick={() => setConfirmDiscard(false)}>
              Keep it
            </button>
          </>
        ) : (
          <button type="button" className="cancel-btn" disabled={saving} onClick={() => setConfirmDiscard(true)}>
            Discard
          </button>
        )}
      </div>
    </div>
  );
}
