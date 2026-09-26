// The hook-entry model the hooks editor reads and writes. It mirrors the shape
// the runtime stores (internal/hooks/entry.go): wherever a hook is attached —
// an agent's hooks, a tool's own hooks, a team state or the walk — the value is
// a map from event to an ordered list of entries, and an entry is either the
// NAME of a HookDef ("gate", or "gate@3" pinned) or an inline webhook object.

/** An inline webhook: the compatible home for what used to be registered. */
export interface InlineWebhook {
  name: string;
  url: string;
  fail_mode?: "open" | "closed";
  timeout_ms?: number;
  /** Sent with each call; a value may name a credential as $cred:<name>. */
  headers?: Record<string, string>;
}

/** A HookDef name (optionally @version) or an inline webhook. */
export type HookEntry = string | InlineWebhook;

/** event -> the entries attached under it, in the order they run. */
export type EventHooks = Record<string, HookEntry[]>;

/** tool name -> that tool's own pre / post / post_failure hooks. */
export type ToolHooks = Record<string, EventHooks>;

/** The events a tool's own hooks answer. */
export const TOOL_HOOK_EVENTS = ["pre", "post", "post_failure"] as const;

/** Every event an agent-level hooks map may carry: the tool events (applied to
 *  every tool) and the run's lifecycle. */
export const AGENT_HOOK_EVENTS = [
  ...TOOL_HOOK_EVENTS,
  "agent_start",
  "agent_stop",
  "subagent_start",
  "subagent_stop",
  "pre_compact",
  "post_compact",
  "run_end",
] as const;

/** What an event means, for the editor's labels. */
export const HOOK_EVENT_HINTS: Record<string, string> = {
  pre: "before a tool call — may deny it or rewrite its input",
  post: "after a tool call — may rewrite its result or add context",
  post_failure: "after a tool call that failed",
  agent_start: "once, before the first model call — may deny the run or add context",
  agent_stop: "each time the model finishes an answer — may send it back or hold it for review",
  subagent_start: "before a sub-agent starts — may deny it",
  subagent_stop: "after a sub-agent ends — may deny or annotate its result",
  pre_compact: "before the context is compacted — may decline it",
  post_compact: "after a compaction (observe only)",
  run_end: "when the run ends, whatever its outcome (observe only)",
};

export const isInline = (e: HookEntry): e is InlineWebhook => typeof e === "object" && e !== null;

/** entryLabel names an entry the way hook decisions do. */
export function entryLabel(e: HookEntry): string {
  return isInline(e) ? e.name || "(unnamed webhook)" : e;
}

function asEntry(v: unknown): HookEntry | null {
  if (typeof v === "string") return v;
  if (v && typeof v === "object" && !Array.isArray(v)) {
    const o = v as Record<string, unknown>;
    const out: InlineWebhook = {
      name: typeof o.name === "string" ? o.name : "",
      url: typeof o.url === "string" ? o.url : "",
    };
    if (o.fail_mode === "open" || o.fail_mode === "closed") out.fail_mode = o.fail_mode;
    if (typeof o.timeout_ms === "number") out.timeout_ms = o.timeout_ms;
    if (o.headers && typeof o.headers === "object" && !Array.isArray(o.headers)) {
      const h: Record<string, string> = {};
      for (const [k, x] of Object.entries(o.headers as Record<string, unknown>)) {
        if (typeof x === "string") h[k] = x;
      }
      if (Object.keys(h).length > 0) out.headers = h;
    }
    return out;
  }
  return null;
}

/** asEventHooks reads an overlay value as an event map, dropping anything the
 *  runtime could not have stored. */
export function asEventHooks(v: unknown): EventHooks {
  if (!v || typeof v !== "object" || Array.isArray(v)) return {};
  const out: EventHooks = {};
  for (const [event, list] of Object.entries(v as Record<string, unknown>)) {
    if (!Array.isArray(list)) continue;
    out[event] = list.map(asEntry).filter((e): e is HookEntry => e !== null);
  }
  return out;
}

/** asToolHooks reads an overlay value as a tool -> event map. */
export function asToolHooks(v: unknown): ToolHooks {
  if (!v || typeof v !== "object" || Array.isArray(v)) return {};
  const out: ToolHooks = {};
  for (const [tool, ev] of Object.entries(v as Record<string, unknown>)) {
    out[tool] = asEventHooks(ev);
  }
  return out;
}

/** pruneEventHooks drops events with no entries, and returns undefined for an
 *  empty map, so clearing the last hook leaves the key unset (inherit) rather
 *  than writing an empty object into the definition. */
export function pruneEventHooks(h: EventHooks): EventHooks | undefined {
  const out: EventHooks = {};
  for (const [event, list] of Object.entries(h)) {
    if (list.length > 0) out[event] = list;
  }
  return Object.keys(out).length > 0 ? out : undefined;
}

/** pruneToolHooks drops tools with no hooks left. Unlike events, a tool row
 *  the operator just added is kept while it is being filled in — the editor
 *  holds it in its own state — so this runs only on what is written out. */
export function pruneToolHooks(t: ToolHooks): ToolHooks | undefined {
  const out: ToolHooks = {};
  for (const [tool, ev] of Object.entries(t)) {
    const p = pruneEventHooks(ev);
    if (p && tool.trim() !== "") out[tool] = p;
  }
  return Object.keys(out).length > 0 ? out : undefined;
}

/** nameProblem mirrors the runtime's HookDef name rule: `/`-joined segments of
 *  letters, digits, _ and -. An inline webhook's name follows it too — it names
 *  the hook in decisions and in the host-widen permit. */
export function nameProblem(name: string): string | null {
  if (name === "") return "a name is required";
  if (name.length > 128) return "a name is at most 128 characters";
  for (const seg of name.split("/")) {
    if (seg === "") return "a name has no empty segment (no leading, trailing or double /)";
    if (!/^[A-Za-z0-9_-]+$/.test(seg)) return "a name is letters, digits, _ and -, with / between segments";
  }
  return null;
}

// The headers a webhook call sets itself; the runtime refuses them.
const RESERVED_HEADERS = new Set(["host", "content-type", "content-length", "accept", "transfer-encoding", "connection"]);

/** headersProblem mirrors the runtime's header rule. */
export function headersProblem(h: Record<string, string> | undefined): string | null {
  for (const [k, v] of Object.entries(h ?? {})) {
    if (k === "") return "a header needs a name";
    if (!/^[A-Za-z0-9_-]+$/.test(k)) return `header ${k}: a name is letters, digits, - and _`;
    if (RESERVED_HEADERS.has(k.toLowerCase())) return `header ${k} is set by the call itself`;
    if (/[\r\n]/.test(v)) return `header ${k}: a value cannot contain a line break`;
  }
  return null;
}

/** entryProblem reports what the runtime would refuse about an entry, or null.
 *  Checked here so an operator sees it beside the entry, not as a save error. */
export function entryProblem(e: HookEntry): string | null {
  if (!isInline(e)) {
    let name = e;
    const at = e.lastIndexOf("@");
    if (at >= 0) {
      if (!/^[1-9][0-9]*$/.test(e.slice(at + 1))) return "the version after @ must be a positive number";
      name = e.slice(0, at);
    }
    return nameProblem(name);
  }
  const n = nameProblem(e.name);
  if (n) return `webhook: ${n}`;
  if (!e.url.startsWith("http://") && !e.url.startsWith("https://")) return "the url must start with http:// or https://";
  if (e.timeout_ms !== undefined && e.timeout_ms < 0) return "timeout_ms cannot be negative";
  return headersProblem(e.headers);
}
