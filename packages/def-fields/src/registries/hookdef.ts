import { AGENT_HOOK_EVENTS } from "../lib/hooks";
import type { DefRegistry } from "../types";

// The HookDef parameter registry: one hook, stored once and named wherever it
// is used. Every field of the stored definition is here; the drift test checks
// that against the runtime's own definition.
export const hookDefRegistry: DefRegistry = {
  kind: "hookdef",
  groups: [
    { name: "What it answers", hint: "The event the hook runs on. Wherever it is attached, it must be attached under this event." },
    { name: "Body", hint: "What runs: a code-js function in-process, or a webhook the runtime calls." },
    { name: "Failure", hint: "What happens when the hook itself fails or takes too long." },
  ],
  fields: [
    { key: "description", label: "Description", group: "What it answers", type: "textarea",
      hint: "Shown to whoever this hook stops, including a tenant that did not write it — say what it checks and why. The body is never shown.",
      unsetMeans: "no description" },
    { key: "event", label: "Event", group: "What it answers", type: "enum", options: AGENT_HOOK_EVENTS,
      hint: "pre / post / post_failure run around tool calls; the others on the run's lifecycle. Required." },
    { key: "match", label: "Match", group: "What it answers", type: "object",
      hint: "Narrows a tool-event hook to some tools, within whatever it is attached to. Tool events only.",
      unsetMeans: "every tool it is attached to",
      fields: [
        { key: "tools", label: "Tools", group: "What it answers", type: "string-list", placeholder: "WebFetch",
          hint: "Exact tool names, or a prefix ending in * (mcp__search__*)." },
      ] },
    { key: "body", label: "Body", group: "Body", type: "object",
      hint: "Required. A code-js body defines function hook(ev) and returns a decision; an http body posts the event to a URL and reads the decision from the reply.",
      fields: [
        { key: "kind", label: "Kind", group: "Body", type: "enum", options: ["code-js", "http"],
          hint: "code-js runs in-process (needs code hooks enabled on the server); http calls the URL below." },
        { key: "code", label: "Code", group: "Body", type: "textarea", placeholder: "function hook(ev) { return {}; }",
          hint: "code-js only. Must define hook(ev); it may call Interruption to ask an operator." },
        { key: "url", label: "URL", group: "Body", type: "text", placeholder: "https://hooks.example/gate",
          hint: "http only. The runtime posts the event here on every call it gates." },
        { key: "headers", label: "Headers", group: "Body", type: "kv",
          hint: "http only. Sent with each call. Put secrets here as $cred:<name>, resolved for the run — never in the URL." },
      ] },
    { key: "fail_mode", label: "Fail mode", group: "Failure", type: "enum", options: ["open", "closed"],
      hint: "open lets the action through when the hook fails; closed blocks it (on agent_stop, closed holds the answer for review).",
      unsetMeans: "open" },
    { key: "timeout_ms", label: "Timeout (ms)", group: "Failure", type: "int", min: 0,
      hint: "How long one call may take before it counts as failed.",
      unsetMeans: "the runtime default" },
  ],
};
