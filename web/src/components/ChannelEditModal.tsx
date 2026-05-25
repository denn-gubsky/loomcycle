import { useState } from "react";
import {
  type ChannelDescriptor,
  type ChannelCreateRequest,
  type ChannelUpdateRequest,
  createChannel,
  updateChannel,
} from "../api";

// ChannelEditModal — v0.11.5 runtime-substrate channel CRUD.
//
// Mode "create": all fields editable, POSTs /v1/_channels and the
// returned descriptor is handed to onSaved.
// Mode "edit": name + scope are locked (substrate primary key + the
// cursor namespace can't change); the four mutable fields (description,
// default_ttl, max_messages, semantic) PATCH /v1/_channels/{name}.
//
// yaml-declared channels never reach this modal — they're filtered by
// the caller via channel.source !== "yaml". Server-side check is
// authoritative (returns 409 channel_yaml_immutable).

export type ChannelEditMode = "create" | "edit";

export interface ChannelEditModalProps {
  mode: ChannelEditMode;
  existing?: ChannelDescriptor;
  onClose: () => void;
  onSaved: (desc: ChannelDescriptor) => void;
}

export default function ChannelEditModal({
  mode,
  existing,
  onClose,
  onSaved,
}: ChannelEditModalProps) {
  const [name, setName] = useState(existing?.name ?? "");
  const [description, setDescription] = useState(existing?.description ?? "");
  const [scope, setScope] = useState(existing?.scope ?? "global");
  const [semantic, setSemantic] = useState(existing?.semantic ?? "queue");
  const [defaultTTL, setDefaultTTL] = useState<string>(
    existing?.default_ttl !== undefined ? String(existing.default_ttl) : "0",
  );
  const [maxMessages, setMaxMessages] = useState<string>(
    existing?.max_messages !== undefined ? String(existing.max_messages) : "0",
  );
  const [submitting, setSubmitting] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const submit = async () => {
    setSubmitting(true);
    setErr(null);
    try {
      const ttl = parseIntOr0(defaultTTL);
      const max = parseIntOr0(maxMessages);
      let saved: ChannelDescriptor;
      if (mode === "create") {
        const req: ChannelCreateRequest = {
          name: name.trim(),
          description,
          scope,
          semantic,
          default_ttl: ttl,
          max_messages: max,
        };
        saved = await createChannel(req);
      } else {
        if (!existing) throw new Error("edit mode requires an existing channel");
        const patch: ChannelUpdateRequest = {
          description,
          default_ttl: ttl,
          max_messages: max,
          semantic,
        };
        saved = await updateChannel(existing.name, patch);
      }
      onSaved(saved);
    } catch (e) {
      setErr(explainChannelRefusal(e instanceof Error ? e.message : String(e)));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal library-modal" onClick={(e) => e.stopPropagation()}>
        <h3>{mode === "create" ? "Create channel" : `Edit channel · ${existing?.name}`}</h3>

        <div className="library-modal-fields">
          <label className="library-modal-field">
            <span>Name</span>
            <input
              type="text"
              value={name}
              onChange={(e) => setName(e.target.value)}
              disabled={mode === "edit"}
              placeholder="briefing-ready"
              autoFocus={mode === "create"}
            />
          </label>

          <label className="library-modal-field">
            <span>Description</span>
            <input
              type="text"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="Researcher signals editor that a new briefing is ready"
            />
          </label>

          <div className="library-modal-field-row">
            <label className="library-modal-field">
              <span>Scope</span>
              <select
                value={scope}
                onChange={(e) => setScope(e.target.value)}
                disabled={mode === "edit"}
              >
                <option value="global">global</option>
                <option value="agent">agent</option>
                <option value="user">user</option>
              </select>
            </label>

            <label className="library-modal-field">
              <span>Semantic</span>
              <select
                value={semantic}
                onChange={(e) => setSemantic(e.target.value)}
              >
                <option value="queue">queue</option>
                <option value="topic">topic</option>
              </select>
            </label>
          </div>

          <div className="library-modal-field-row">
            <label className="library-modal-field">
              <span>Default TTL (seconds; 0 = no TTL)</span>
              <input
                type="number"
                min="0"
                value={defaultTTL}
                onChange={(e) => setDefaultTTL(e.target.value)}
              />
            </label>

            <label className="library-modal-field">
              <span>Max messages (0 = unbounded)</span>
              <input
                type="number"
                min="0"
                value={maxMessages}
                onChange={(e) => setMaxMessages(e.target.value)}
              />
            </label>
          </div>
        </div>

        {err && <div className="error-banner">{err}</div>}

        <div className="modal-buttons">
          <button type="button" onClick={onClose} disabled={submitting}>
            Cancel
          </button>
          <button
            type="button"
            className="primary"
            onClick={submit}
            disabled={submitting || (mode === "create" && !name.trim())}
          >
            {submitting ? "Saving…" : mode === "create" ? "Create" : "Save"}
          </button>
        </div>
      </div>
    </div>
  );
}

function parseIntOr0(s: string): number {
  const n = parseInt(s, 10);
  return Number.isFinite(n) && n >= 0 ? n : 0;
}

// explainChannelRefusal turns the raw fetch-error message into a
// friendlier human sentence by pattern-matching the JSON error code
// the server wraps refusals in.
function explainChannelRefusal(msg: string): string {
  if (msg.includes("channel_yaml_immutable")) {
    return "This channel is declared in the loomcycle yaml — edit the yaml + restart instead of changing it from the UI.";
  }
  if (msg.includes("channel_name_in_use")) {
    return "A channel with that name already exists.";
  }
  if (msg.includes("channel_not_found")) {
    return "Channel not found in the runtime substrate.";
  }
  if (msg.includes("invalid_request") || msg.includes("invalid_body")) {
    // Strip the {…} envelope prefix to surface the human text.
    const match = msg.match(/"error":"([^"]+)"/);
    if (match) return match[1] ?? msg;
  }
  return msg;
}
