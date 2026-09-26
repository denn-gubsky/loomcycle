import type { DefValue } from "@loomcycle/def-fields";

// The overlay plumbing of the HookDef editor, kept apart from the component so
// it can be tested without a DOM.

// The server-set keys a row's definition may carry beside the overlay.
const SERVER_KEYS = new Set(["name", "def_id", "version", "parent_def_id", "created_at", "retired", "content_sha256"]);

/** sourceHookOverlay lifts a HookDef row's definition into the editable overlay. */
export function sourceHookOverlay(def: unknown): DefValue {
  const out: DefValue = {};
  if (!def || typeof def !== "object") return out;
  for (const [k, v] of Object.entries(def as Record<string, unknown>)) {
    if (SERVER_KEYS.has(k) || v === undefined || v === null) continue;
    out[k] = v;
  }
  return out;
}

/** forkOverlay turns the edited definition into a fork overlay. A fork merges
 *  its overlay over the parent field by field, so a field the operator REMOVED
 *  has to be sent as null — left out, the parent's value would come back. */
export function forkOverlay(source: DefValue, edited: DefValue): Record<string, unknown> {
  const out: Record<string, unknown> = { ...edited };
  for (const k of Object.keys(source)) {
    if (!(k in edited)) out[k] = null;
  }
  return out;
}
