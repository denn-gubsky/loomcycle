import { useState } from "react";
import { type VolumeDefRow, type VolumeMode, createVolume } from "../api";

// VolumeEditModal — RFC AH Phase 4 create form for a dynamic VolumeDef.
//
// Create-only (a VolumeDef is FLAT — no edit/fork/promote/retire lifecycle).
// A volume is created by NAME + MODE only: the runtime DERIVES the on-disk
// path inside the operator-blessed dynamic_root (<root>/<tenant>/<name>), so
// there is no path field — the caller never supplies a host path. To repoint a
// name, delete/purge then create.
//
// Name charset mirrors the server's volumeNameRe (^[a-z0-9][a-z0-9_-]{0,63}$):
// the form validates it client-side for a friendly message, but the server is
// authoritative (a refusal surfaces in the error banner).

const NAME_RE = /^[a-z0-9][a-z0-9_-]{0,63}$/;

export interface VolumeEditModalProps {
  /** Names already taken (static + dynamic) so create can warn before the
   *  server refuses a collision. */
  existingNames: string[];
  onClose: () => void;
  onSaved: (row: VolumeDefRow) => void;
}

export default function VolumeEditModal({
  existingNames,
  onClose,
  onSaved,
}: VolumeEditModalProps) {
  const [name, setName] = useState("");
  const [mode, setMode] = useState<VolumeMode>("rw");
  const [submitting, setSubmitting] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const trimmed = name.trim();
  const nameValid = NAME_RE.test(trimmed);
  const nameTaken = existingNames.includes(trimmed);
  const canSubmit = nameValid && !nameTaken && !submitting;

  const submit = async () => {
    setSubmitting(true);
    setErr(null);
    try {
      const row = await createVolume(trimmed, mode);
      onSaved(row);
    } catch (e) {
      setErr(explainRefusal(e instanceof Error ? e.message : String(e)));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal library-modal" onClick={(e) => e.stopPropagation()}>
        <h3>Create volume</h3>

        <div className="library-modal-fields">
          <label className="library-modal-field">
            <span>Name</span>
            <input
              type="text"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="repo-a"
              autoFocus
            />
          </label>
          {trimmed !== "" && !nameValid && (
            <div className="library-modal-field-hint">
              Lowercase letters, digits, <code>_</code> and <code>-</code> only
              (1–64 chars, no leading dot or slash).
            </div>
          )}
          {nameTaken && (
            <div className="library-modal-field-hint">
              A volume named <code>{trimmed}</code> already exists — pick another
              name (static yaml names and existing dynamic volumes are reserved).
            </div>
          )}

          <label className="library-modal-field">
            <span>Mode</span>
            <select value={mode} onChange={(e) => setMode(e.target.value as VolumeMode)}>
              <option value="rw">rw — read + write (Write/Edit/Bash allowed)</option>
              <option value="ro">ro — read-only (Bash refused; Write/Edit refused)</option>
            </select>
          </label>

          <div className="library-modal-field-hint">
            The path is derived by the runtime inside the operator-blessed
            dynamic root — you choose only the name + mode.
          </div>
        </div>

        {err && <div className="error-banner">{err}</div>}

        <div className="modal-buttons">
          <button type="button" onClick={onClose} disabled={submitting}>
            Cancel
          </button>
          <button type="button" className="primary" onClick={submit} disabled={!canSubmit}>
            {submitting ? "Creating…" : "Create"}
          </button>
        </div>
      </div>
    </div>
  );
}

// explainRefusal lifts the human-readable text out of the substrate's
// {"code":"tool_refused","error":"…"} 422 envelope that jsonFetch surfaces in
// the thrown Error message; falls back to the raw message.
function explainRefusal(msg: string): string {
  if (msg.includes("no dynamic volume root configured")) {
    return "No dynamic volume root is configured — an operator must mark a static volume `dynamic_root: true` in the loomcycle yaml before dynamic volumes can be created.";
  }
  const match = msg.match(/"error":"([^"]+)"/);
  if (match && match[1]) return match[1];
  return msg;
}
