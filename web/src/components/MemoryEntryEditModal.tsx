import { useState } from "react";
import {
  type MemoryEntry,
  type MemoryEntrySetRequest,
  setMemoryEntry,
} from "../api";

// MemoryEntryEditModal — v0.11.5 simple set/edit form for one
// (scope, scope_id, key) memory row. Idempotent PUT: existing rows
// are overwritten in place; new ones are created.
//
// Value is edited as JSON text so structured values + plain strings
// are both expressible. The validation pass parses + re-stringifies
// before submit so malformed JSON surfaces locally.
//
// embed (boolean) is exposed when the operator wired memory.embedder
// yaml; otherwise the server-side path returns embedded:false with a
// warning that the modal surfaces in the response banner.
//
// scope + scope_id default from the current selection in MemoryView
// — they're locked in edit mode (the URL is the row identity) but
// editable in create mode so operators can choose where to write.

export type MemoryEntryEditMode = "create" | "edit";

export interface MemoryEntryEditModalProps {
  mode: MemoryEntryEditMode;
  scope: string;
  scopeID: string;
  existing?: MemoryEntry;
  onClose: () => void;
  onSaved: () => void;
}

export default function MemoryEntryEditModal({
  mode,
  scope: initialScope,
  scopeID: initialScopeID,
  existing,
  onClose,
  onSaved,
}: MemoryEntryEditModalProps) {
  const [scope, setScope] = useState(initialScope || "user");
  const [scopeID, setScopeID] = useState(initialScopeID);
  const [key, setKey] = useState(existing?.key ?? "");
  const [valueText, setValueText] = useState(() => {
    if (existing) return safePretty(existing.value);
    return "";
  });
  const [embed, setEmbed] = useState(false);
  const [ttlSeconds, setTtlSeconds] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [warning, setWarning] = useState<string | null>(null);

  const submit = async () => {
    setSubmitting(true);
    setErr(null);
    setWarning(null);
    let parsed: unknown;
    try {
      parsed = JSON.parse(valueText);
    } catch (e) {
      setErr(`Value must be valid JSON. ${e instanceof Error ? e.message : ""}`);
      setSubmitting(false);
      return;
    }
    const body: MemoryEntrySetRequest = { value: parsed, embed };
    const ttl = parseInt(ttlSeconds, 10);
    if (Number.isFinite(ttl) && ttl > 0) body.ttl_seconds = ttl;
    try {
      const resp = await setMemoryEntry(scope, scopeID, key, body);
      if (embed && resp.embed_warning) {
        setWarning(`Saved, but embedding failed: ${resp.embed_warning}`);
        // Don't auto-close on a warning — operator should see it.
        setSubmitting(false);
        return;
      }
      onSaved();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal library-modal" onClick={(e) => e.stopPropagation()}>
        <h3>
          {mode === "create" ? "Set memory entry" : `Edit ${scope}/${scopeID}/${existing?.key}`}
        </h3>

        <div className="library-modal-fields">
          <div className="library-modal-field-row">
            <label className="library-modal-field">
              <span>Scope</span>
              <select
                value={scope}
                onChange={(e) => setScope(e.target.value)}
                disabled={mode === "edit"}
              >
                <option value="agent">agent</option>
                <option value="user">user</option>
              </select>
            </label>

            <label className="library-modal-field">
              <span>Scope ID</span>
              <input
                type="text"
                value={scopeID}
                onChange={(e) => setScopeID(e.target.value)}
                disabled={mode === "edit"}
                placeholder={scope === "user" ? "alice" : "researcher-agent"}
              />
            </label>
          </div>

          <label className="library-modal-field">
            <span>Key</span>
            <input
              type="text"
              value={key}
              onChange={(e) => setKey(e.target.value)}
              disabled={mode === "edit"}
              placeholder="company-policy"
            />
          </label>

          <label className="library-modal-field">
            <span>Value (JSON — strings must be quoted)</span>
            <textarea
              rows={8}
              value={valueText}
              onChange={(e) => setValueText(e.target.value)}
              placeholder={`"All agents must respect rate limits."  or  {"format": "json"}`}
              spellCheck={false}
            />
          </label>

          <div className="library-modal-field-row">
            <label className="library-modal-field memory-modal-checkbox-field">
              <span>Embed value (uses configured embedder; may incur cost)</span>
              <input
                type="checkbox"
                checked={embed}
                onChange={(e) => setEmbed(e.target.checked)}
              />
            </label>

            <label className="library-modal-field">
              <span>TTL (seconds; blank = no expiry)</span>
              <input
                type="number"
                min="0"
                value={ttlSeconds}
                onChange={(e) => setTtlSeconds(e.target.value)}
                placeholder="3600"
              />
            </label>
          </div>
        </div>

        {err && <div className="error-banner">{err}</div>}
        {warning && <div className="warning-banner">{warning}</div>}

        <div className="modal-buttons">
          <button type="button" onClick={onClose} disabled={submitting}>
            Cancel
          </button>
          <button
            type="button"
            className="primary"
            onClick={submit}
            disabled={
              submitting ||
              !scope ||
              !scopeID.trim() ||
              !key.trim() ||
              !valueText.trim()
            }
          >
            {submitting ? "Saving…" : "Save"}
          </button>
        </div>
      </div>
    </div>
  );
}

function safePretty(v: unknown): string {
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}
