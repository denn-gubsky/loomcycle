import { useState } from "react";
import {
  scheduleDefAddHook,
  scheduleDefRemoveHook,
  ScheduleHook,
} from "../api";

interface Props {
  // The hooks array from the active def's on_complete list. Each entry
  // is rendered as a row; null/empty renders an empty-state message.
  hooks: Array<Record<string, unknown>>;
  // The substrate def_id whose on_complete list these hooks belong to.
  // When undefined, the section is display-only (yaml-only schedule).
  // When defined, each hook gets a Remove button + an Add Hook CTA at
  // the bottom.
  activeDefID?: string;
  // Called after a successful mutation so the parent re-fetches the
  // substrate row to surface the new active def_id + version.
  onMutated: () => void;
}

// ScheduleHookList renders the on_complete hooks for a schedule. For
// substrate-backed schedules (activeDefID set), each hook gets a
// Remove button and an Add Hook modal is available. For yaml-only
// schedules, the section is display-only with a note pointing
// operators to the yaml.
//
// Each mutation creates a new fork version with full lineage. The
// substrate validates the hook server-side before persisting, so
// malformed hooks refuse with a SubstrateToolRefusedError that the
// component surfaces inline.
export default function ScheduleHookList({ hooks, activeDefID, onMutated }: Props) {
  const [addOpen, setAddOpen] = useState(false);
  const [removingIdx, setRemovingIdx] = useState<number | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const handleRemove = async (idx: number) => {
    if (!activeDefID) return;
    if (!confirm(`Remove hook #${idx} (${hooks[idx]?.kind ?? "?"})? This creates a new fork version with the hook removed.`)) {
      return;
    }
    setRemovingIdx(idx);
    setErr(null);
    try {
      await scheduleDefRemoveHook(activeDefID, idx);
      onMutated();
    } catch (e) {
      setErr(`remove hook: ${e instanceof Error ? e.message : String(e)}`);
    } finally {
      setRemovingIdx(null);
    }
  };

  const handleAdd = async (hook: ScheduleHook) => {
    if (!activeDefID) return;
    setErr(null);
    try {
      await scheduleDefAddHook(activeDefID, hook);
      setAddOpen(false);
      onMutated();
    } catch (e) {
      setErr(`add hook: ${e instanceof Error ? e.message : String(e)}`);
      // Keep the modal open so the operator can fix the input.
    }
  };

  const editable = Boolean(activeDefID);

  return (
    <section className="schedule-detail-block">
      <h4>on_complete hooks</h4>
      {hooks.length === 0 ? (
        <div className="schedule-hooks-empty">No hooks configured.</div>
      ) : (
        <ul className="schedule-hooks">
          {hooks.map((h, i) => (
            <li key={i} className="schedule-hook">
              <span className="schedule-hook-index">#{i}</span>
              <span className="schedule-hook-kind">{h.kind as string}</span>
              <span className="schedule-hook-fields">
                {h.channel ? <code>channel={String(h.channel)}</code> : null}
                {h.server ? (
                  <code>
                    server={String(h.server)}, tool={String(h.tool ?? "")}
                  </code>
                ) : null}
                {h.scope ? (
                  <code>
                    scope={String(h.scope)}, key={String(h.key ?? "")}
                  </code>
                ) : null}
              </span>
              {editable && (
                <button
                  className="schedule-hook-remove"
                  onClick={() => handleRemove(i)}
                  disabled={removingIdx === i}
                  title="Remove this hook (creates a new fork version)"
                >
                  {removingIdx === i ? "Removing…" : "Remove"}
                </button>
              )}
            </li>
          ))}
        </ul>
      )}
      {err && <div className="schedule-detail-err">{err}</div>}
      {editable ? (
        <>
          <button className="schedule-detail-action" onClick={() => setAddOpen(true)}>
            + Add hook
          </button>
          {addOpen && (
            <AddHookForm onCancel={() => setAddOpen(false)} onAdd={handleAdd} />
          )}
        </>
      ) : (
        <div className="schedule-hooks-note">
          This is a yaml-only schedule — hooks are display-only. Edit the
          loomcycle.yaml file to change them, or fork the definition into
          the substrate to enable inline editing.
        </div>
      )}
    </section>
  );
}

interface AddHookFormProps {
  onCancel: () => void;
  onAdd: (hook: ScheduleHook) => void;
}

// AddHookForm is an inline (not modal) form for adding one hook. Three
// kinds with kind-specific fields, validated client-side before the
// POST goes out. The substrate re-validates server-side; this client-
// side check is just a UX nicety to avoid round-trips for obvious
// missing fields.
function AddHookForm({ onCancel, onAdd }: AddHookFormProps) {
  const [kind, setKind] = useState<ScheduleHook["kind"]>("channel.publish");
  const [channel, setChannel] = useState("");
  const [server, setServer] = useState("");
  const [tool, setTool] = useState("");
  const [scope, setScope] = useState<"agent" | "user" | "global">("user");
  const [key, setKey] = useState("");
  const [payloadJSON, setPayloadJSON] = useState("{}");

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();

    let payload: Record<string, unknown> | undefined;
    if (payloadJSON.trim() && payloadJSON.trim() !== "{}") {
      try {
        const parsed = JSON.parse(payloadJSON);
        if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
          throw new Error("payload must be a JSON object");
        }
        payload = parsed;
      } catch (e) {
        alert(`payload JSON: ${e instanceof Error ? e.message : String(e)}`);
        return;
      }
    }

    const hook: ScheduleHook = { kind };
    if (kind === "channel.publish") {
      if (!channel) {
        alert("channel.publish: channel is required");
        return;
      }
      hook.channel = channel;
      if (payload) hook.payload = payload;
    } else if (kind === "mcp.call") {
      if (!server || !tool) {
        alert("mcp.call: server + tool are required");
        return;
      }
      hook.server = server;
      hook.tool = tool;
      if (payload) hook.args = payload;
    } else if (kind === "memory.set") {
      if (!scope || !key) {
        alert("memory.set: scope + key are required");
        return;
      }
      hook.scope = scope;
      hook.key = key;
      if (payload) hook.payload = payload;
    }

    onAdd(hook);
  };

  return (
    <form onSubmit={handleSubmit} className="schedule-hook-form">
      <div className="schedule-hook-form-row">
        <label>
          kind
          <select
            value={kind}
            onChange={(e) => setKind(e.target.value as ScheduleHook["kind"])}
          >
            <option value="channel.publish">channel.publish</option>
            <option value="mcp.call">mcp.call</option>
            <option value="memory.set">memory.set</option>
          </select>
        </label>
      </div>
      {kind === "channel.publish" && (
        <div className="schedule-hook-form-row">
          <label>
            channel
            <input
              type="text"
              value={channel}
              onChange={(e) => setChannel(e.target.value)}
              placeholder="results-alice"
            />
          </label>
        </div>
      )}
      {kind === "mcp.call" && (
        <>
          <div className="schedule-hook-form-row">
            <label>
              server
              <input
                type="text"
                value={server}
                onChange={(e) => setServer(e.target.value)}
                placeholder="telegram"
              />
            </label>
          </div>
          <div className="schedule-hook-form-row">
            <label>
              tool
              <input
                type="text"
                value={tool}
                onChange={(e) => setTool(e.target.value)}
                placeholder="send_message"
              />
            </label>
          </div>
        </>
      )}
      {kind === "memory.set" && (
        <>
          <div className="schedule-hook-form-row">
            <label>
              scope
              <select
                value={scope}
                onChange={(e) => setScope(e.target.value as "agent" | "user" | "global")}
              >
                <option value="agent">agent</option>
                <option value="user">user</option>
                <option value="global">global</option>
              </select>
            </label>
          </div>
          <div className="schedule-hook-form-row">
            <label>
              key
              <input
                type="text"
                value={key}
                onChange={(e) => setKey(e.target.value)}
                placeholder="last_run_summary"
              />
            </label>
          </div>
        </>
      )}
      <div className="schedule-hook-form-row">
        <label>
          {kind === "mcp.call" ? "args" : "payload"} (JSON, optional)
          <textarea
            value={payloadJSON}
            onChange={(e) => setPayloadJSON(e.target.value)}
            rows={3}
            spellCheck={false}
            className="schedule-hook-form-textarea"
          />
        </label>
      </div>
      <div className="schedule-hook-form-actions">
        <button type="button" onClick={onCancel}>
          Cancel
        </button>
        <button type="submit">Add hook</button>
      </div>
    </form>
  );
}
