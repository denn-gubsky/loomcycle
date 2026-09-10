import type { DefRegistry, FieldSpec } from "../types";

// The AgentDef parameter registry — the full operator-settable surface of an
// agent definition, grouped for the folded list and annotated with a hint per
// parameter.
//
// Hints are written for an operator meeting the knob for the first time: what it
// controls and what happens if it is left alone. They deliberately do not cite
// internal RFC letters, which mean nothing at the console.

// Parameters deliberately NOT in the registry, and why. The drift test asserts
// registry ∪ EXCLUDED == the substrate's overlay shape, so an omission has to be
// justified here rather than silently forgotten.
export const AGENTDEF_EXCLUDED: Record<string, string> = {
  system_prompt_base:
    "derived — the pre-skill-bake snapshot of system_prompt, written by the server, never operator input",
  system_prompt_file:
    "load-time only — reads a file off the operator's disk at config load, so a runtime def cannot honour it",
  // These two are operator-settable in yaml but do NOT round-trip the substrate
  // overlay yet (absent from both the write shape and the read adapter), so a
  // control here would silently drop on save. Removed from this list by the
  // backend round-trip fix.
  disable_context: "pending backend round-trip support",
  skill_def_scopes: "pending backend round-trip support",
};

const scopeList = (label: string, key: string, hint: string): FieldSpec => ({
  key, label, group: "Capabilities", type: "string-list", hint,
  placeholder: "scope…", unsetMeans: "no grant — the capability is denied",
});

export const agentDefRegistry: DefRegistry = {
  kind: "agentdef",
  groups: [
    { name: "Identity & prompt" },
    { name: "Routing", hint: "How this agent resolves to a concrete provider and model." },
    { name: "Tools & access" },
    { name: "Limits", hint: "Budgets and caps. Unset means the operator/global default applies." },
    { name: "Sampling", hint: "Decoding parameters. Each driver applies what it supports." },
    { name: "Context & compaction", hint: "How the run's history is retained, distilled and recalled." },
    { name: "Memory" },
    { name: "Capabilities", hint: "Capability gates. An empty grant denies the capability entirely." },
    { name: "Behaviour" },
  ],
  fields: [
    // ---- Identity & prompt ----
    { key: "description", label: "Description", group: "Identity & prompt", type: "text",
      hint: "Human-facing summary of what this agent is for. Shown in the Library.",
      unsetMeans: "inherits the parent def" },
    { key: "system_prompt", label: "System prompt", group: "Identity & prompt", type: "textarea",
      hint: "The agent's instructions, as inline text. Skill bodies are appended to it at prompt assembly.",
      unsetMeans: "inherits the parent def / operator yaml" },
    { key: "code_body", label: "Code (code-js)", group: "Identity & prompt", type: "textarea",
      hint: "Inline code-js orchestrator source. Only for code-js agents; leave empty for an LLM agent.",
      unsetMeans: "not a code-js agent", advanced: true },

    // ---- Routing ----
    { key: "provider", label: "Provider", group: "Routing", type: "text",
      hint: "Pin this agent to one provider instead of letting the tier resolve it.",
      placeholder: "anthropic", unsetMeans: "resolved from the tier" },
    { key: "model", label: "Model", group: "Routing", type: "text",
      hint: "Pin a model id or a configured alias. Prefer an alias so operator overrides still apply.",
      placeholder: "claude-sonnet-5", unsetMeans: "resolved from the tier" },
    { key: "tier", label: "Tier", group: "Routing", type: "text",
      hint: "Which model tier the resolver picks from when no explicit provider+model pin is set.",
      placeholder: "middle", unsetMeans: "the default tier" },
    { key: "effort", label: "Reasoning effort", group: "Routing", type: "enum", options: ["low", "medium", "high"],
      hint: "Reasoning-effort hint passed to models that support it. Errors on a model that does not.",
      unsetMeans: "no hint sent" },
    { key: "providers", label: "Provider priority", group: "Routing", type: "string-list",
      hint: "Per-agent override of the provider fallback order used for tier resolution.",
      unsetMeans: "the library-wide priority", advanced: true },
    { key: "models", label: "Tier candidates", group: "Routing", type: "json",
      hint: "Per-agent override of the tier→candidates map. Replaces the library tiers wholesale, not per key.",
      unsetMeans: "the library tiers", advanced: true },
    { key: "search_providers", label: "Search providers", group: "Routing", type: "string-list",
      hint: "Ordered web-search backends the WebSearch tool tries before giving up.",
      unsetMeans: "the operator default order", advanced: true },

    // ---- Tools & access ----
    { key: "tools", label: "Tools", group: "Tools & access", type: "string-list",
      hint: "Which tools this agent may call. An EMPTY list grants no tools at all; patterns like mcp__server__* are allowed.",
      placeholder: "Read", unsetMeans: "inherits the parent def" },
    { key: "skills", label: "Skills", group: "Tools & access", type: "string-list",
      hint: "Skill names whose bodies are concatenated onto the system prompt. Use -* to drop the auto-added Skill tool.",
      unsetMeans: "inherits the parent def" },
    { key: "volumes", label: "Volumes", group: "Tools & access", type: "string-list",
      hint: "Filesystem volumes this agent's file and exec tools may reach. With none bound, it has no filesystem access.",
      unsetMeans: "no filesystem access" },

    // ---- Limits ----
    { key: "max_tokens", label: "Max output tokens", group: "Limits", type: "int", min: 1,
      hint: "Caps the assistant output of a single iteration. Distinct from the context window.",
      unsetMeans: "the driver default" },
    { key: "max_context_tokens", label: "Context window", group: "Limits", type: "int", min: 1,
      hint: "The INPUT window this agent uses. On local models it sets num_ctx; on cloud models it caps the effective window.",
      unsetMeans: "the provider default" },
    { key: "max_iterations", label: "Max iterations", group: "Limits", type: "int", min: 1,
      hint: "How many provider calls the loop may make before stopping with max_iterations.",
      unsetMeans: "the loop default (16)" },
    { key: "unbounded_iterations", label: "Unbounded iterations", group: "Limits", type: "bool",
      hint: "Lifts the iteration soft-cap for a long-running agent. A hard runaway guard still applies.",
      unsetMeans: "off", advanced: true },
    { key: "max_concurrent_children", label: "Max concurrent children", group: "Limits", type: "int", min: 1,
      hint: "How many sub-agents this agent may run in parallel from one fan-out call.",
      unsetMeans: "the global default" },
    { key: "run_timeout_seconds", label: "Run timeout (s)", group: "Limits", type: "int", min: 1,
      hint: "Wall-clock budget for one code-js run, overriding the global timeout.",
      unsetMeans: "the global timeout", advanced: true },
    { key: "retry_attempts", label: "Retry attempts", group: "Limits", type: "int", min: 0,
      hint: "Same-provider retry budget for this agent, overriding the user tier's.",
      unsetMeans: "the user tier's budget", advanced: true },

    // ---- Sampling ----
    { key: "sampling", label: "Sampling", group: "Sampling", type: "object",
      hint: "Decoding parameters. A value of 0 is a real setting and differs from leaving it unset.",
      unsetMeans: "provider defaults",
      fields: [
        { key: "temperature", label: "Temperature", group: "Sampling", type: "float", min: 0, max: 2, hint: "Randomness. 0 is deterministic; unset lets the provider decide." },
        { key: "top_p", label: "Top-p", group: "Sampling", type: "float", min: 0, max: 1, hint: "Nucleus sampling mass." },
        { key: "top_k", label: "Top-k", group: "Sampling", type: "int", hint: "Sample only from the k most likely tokens." },
        { key: "frequency_penalty", label: "Frequency penalty", group: "Sampling", type: "float", min: -2, max: 2, hint: "Discourages repeating the same tokens." },
        { key: "presence_penalty", label: "Presence penalty", group: "Sampling", type: "float", min: -2, max: 2, hint: "Discourages reusing tokens already present." },
        { key: "seed", label: "Seed", group: "Sampling", type: "int", hint: "Fixed seed for reproducible sampling, where the provider supports it." },
        { key: "stop", label: "Stop sequences", group: "Sampling", type: "string-list", hint: "Generation halts when one of these strings is produced." },
      ] },

    // ---- Context & compaction ----
    { key: "context", label: "Context retention", group: "Context & compaction", type: "object",
      hint: "How the fed history is retained: append, a running recap, or a structured state.",
      unsetMeans: "append (full history)",
      fields: [
        { key: "mode", label: "Mode", group: "Context & compaction", type: "enum", options: ["append", "recap", "stateful", "auto"], hint: "append keeps everything; recap distils older turns; stateful feeds only a state object; auto picks by provider." },
        { key: "keep_last_n", label: "Keep last N", group: "Context & compaction", type: "int", min: 0, hint: "How many recent turns stay verbatim when distilling." },
        { key: "reasoning", label: "Reasoning policy", group: "Context & compaction", type: "enum", options: ["recap", "drop", "keep"], hint: "What happens to evicted reasoning: fold into a recap, drop it, or keep it." },
        { key: "recap_max_chars", label: "Recap max chars", group: "Context & compaction", type: "int", min: 0, hint: "Upper bound on the running recap note." },
        { key: "autorecap_at_pct", label: "Auto-recap at %", group: "Context & compaction", type: "int", min: 50, max: 95, hint: "Distil once the context footprint crosses this share of the window." },
        { key: "recall", label: "Recall", group: "Context & compaction", type: "bool", hint: "Index every evicted span so the agent can fetch a dropped detail back by a free-text Recall. Grants the Recall tool automatically." },
        { key: "harvest_to_memory", label: "Harvest to memory", group: "Context & compaction", type: "bool", hint: "Bank each evicted span for the consolidator, so facts learned mid-run survive into later runs. Needs `user` in memory scopes." },
        { key: "state_schema", label: "State schema", group: "Context & compaction", type: "json", hint: "JSON Schema each state patch is validated against in stateful mode. Optional." },
        { key: "on_invalid_patch", label: "On invalid patch", group: "Context & compaction", type: "enum", options: ["retry", "fail"], hint: "Whether a schema-invalid state patch is retried or ends the run." },
        { key: "max_patch_retries", label: "Max patch retries", group: "Context & compaction", type: "int", min: 0, hint: "How many times an invalid state patch is re-prompted." },
      ] },
    { key: "compaction", label: "Compaction", group: "Context & compaction", type: "object",
      hint: "Summarise older turns once the context fills. Mutually exclusive with recap mode.",
      unsetMeans: "no auto-compaction",
      fields: [
        { key: "enabled", label: "Enabled", group: "Context & compaction", type: "bool", hint: "Turn auto-compaction on for this agent." },
        { key: "target_percentage", label: "Target %", group: "Context & compaction", type: "int", min: 10, max: 50, hint: "How much of the window the summary should occupy." },
        { key: "keep_last_n", label: "Keep last N", group: "Context & compaction", type: "int", min: 0, hint: "Recent turns kept verbatim behind the summary." },
        { key: "keep_first", label: "Keep first turn", group: "Context & compaction", type: "bool", hint: "Pin the opening task turn so the goal is never summarised away." },
        { key: "autocompact_at_pct", label: "Auto-compact at %", group: "Context & compaction", type: "int", min: 50, max: 95, hint: "Compact once the footprint crosses this share of the window." },
        { key: "model", label: "Summary model", group: "Context & compaction", type: "text", hint: "A cheaper same-provider model to write the summary." },
        { key: "memory_flush", label: "Memory flush", group: "Context & compaction", type: "bool", hint: "Bank the discarded span for the consolidator instead of dropping it." },
      ] },

    // ---- Memory ----
    { key: "memory_scopes", label: "Memory scopes", group: "Memory", type: "string-list",
      hint: "Which memory scopes the agent may read and write. With none, the Memory tool refuses every op.",
      placeholder: "user", unsetMeans: "no memory access" },
    { key: "memory_quota_bytes", label: "Memory quota (bytes)", group: "Memory", type: "int", min: 0,
      hint: "Per-scope byte cap for this agent. 0 means unlimited.", unsetMeans: "the global cap" },
    { key: "memory_backend", label: "Memory backend", group: "Memory", type: "text",
      hint: "Route this agent's memory ops through a named backend instead of the default.",
      unsetMeans: "the default in-process backend", advanced: true },
    { key: "sql_scopes", label: "SQL scopes", group: "Memory", type: "string-list",
      hint: "Which per-scope SQL databases the agent may query. Empty denies all SQL.",
      unsetMeans: "no SQL access", advanced: true },
    { key: "sql_quota_bytes", label: "SQL quota (bytes)", group: "Memory", type: "int", min: 0,
      hint: "Per-scope SQL database byte cap for this agent.", unsetMeans: "the global cap", advanced: true },
    { key: "core_blocks", label: "Core blocks", group: "Memory", type: "object-array",
      hint: "Always-resident memory blocks rendered into the system prompt as reference data.",
      unsetMeans: "none attached",
      fields: [
        { key: "label", label: "Label", group: "Memory", type: "text", hint: "Names the block; the backing memory key is core/<label>." },
        { key: "scope", label: "Scope", group: "Memory", type: "enum", options: ["agent", "user", "tenant"], hint: "Where the block's value lives." },
        { key: "limit_bytes", label: "Limit (bytes)", group: "Memory", type: "int", min: 0, hint: "Caps an agent write to this block. 0 means no per-block cap." },
        { key: "read_only", label: "Read only", group: "Memory", type: "bool", hint: "The agent may read the block but never rewrite it." },
      ] },
    { key: "inherit_core_blocks", label: "Inherit core blocks", group: "Memory", type: "bool",
      hint: "Let a sub-agent also receive the parent run's user/tenant blocks. Agent-scope blocks never cross a spawn.",
      unsetMeans: "off", advanced: true },
    { key: "memory_inject_max_tokens", label: "Memory inject cap", group: "Memory", type: "int", min: 0,
      hint: "Caps how much injected memory content may enter the system prompt.",
      unsetMeans: "the global cap", advanced: true },
    { key: "memory_protocol", label: "Memory protocol", group: "Memory", type: "bool",
      hint: "Adds the memory-usage protocol note telling the agent how to keep its own memory.",
      unsetMeans: "off", advanced: true },
    { key: "memory_consolidation", label: "Consolidation ops", group: "Memory", type: "bool",
      hint: "Grants the consolidation control ops (queue drain, cursors, supersede). For maintenance agents.",
      unsetMeans: "denied", advanced: true },
    { key: "memory_index_max_bytes", label: "Memory index cap", group: "Memory", type: "int", min: 0,
      hint: "Soft size the agent is asked to keep its own memory index document under.",
      unsetMeans: "the global default", advanced: true },
    { key: "memory_roots", label: "Memory roots", group: "Memory", type: "text",
      hint: "Controls provisioning of the operator-authored user-root document composed into the prompt.",
      unsetMeans: "the global default", advanced: true },

    // ---- Capabilities ----
    scopeList("Agent defs", "agent_def_scopes", "Lets the agent author or fork other agent definitions."),
    scopeList("Schedule defs", "schedule_def_scopes", "Lets the agent create or change scheduled runs."),
    scopeList("A2A server cards", "a2a_server_card_def_scopes", "Lets the agent publish agent-to-agent server cards."),
    scopeList("A2A agents", "a2a_agent_def_scopes", "Lets the agent register agent-to-agent peers."),
    scopeList("Volume defs", "volume_def_scopes", "Lets the agent define filesystem volumes."),
    scopeList("Evaluation", "evaluation_scopes", "Lets the agent submit evaluation scores against runs."),
    { key: "history_scope", label: "History", group: "Capabilities", type: "string-list",
      hint: "Which past chats the agent may browse, search and annotate. Empty denies the History tool.",
      unsetMeans: "denied" },
    { key: "channels", label: "Channels", group: "Capabilities", type: "object",
      hint: "Which channels the agent may post to and read from.", unsetMeans: "no channel access",
      fields: [
        { key: "publish", label: "Publish to", group: "Capabilities", type: "string-list", hint: "Channels the agent may post messages to." },
        { key: "subscribe", label: "Subscribe to", group: "Capabilities", type: "string-list", hint: "Channels the agent may read from." },
      ] },
    { key: "interruption", label: "Interruption", group: "Capabilities", type: "object",
      hint: "Whether the agent may raise interruptions that pause it for a human answer.",
      unsetMeans: "cannot interrupt",
      fields: [
        { key: "enabled", label: "Enabled", group: "Capabilities", type: "bool", hint: "Allow this agent to raise interruptions." },
        { key: "kinds", label: "Kinds", group: "Capabilities", type: "string-list", hint: "Which interruption kinds it may raise." },
        { key: "max_pending", label: "Max pending", group: "Capabilities", type: "int", min: 0, hint: "How many of its interruptions may await an answer at once." },
      ] },

    // ---- Behaviour ----
    { key: "internal", label: "Internal", group: "Behaviour", type: "bool",
      hint: "Marks the agent as maintenance plumbing: its sessions are kept out of the surfaces people browse.",
      unsetMeans: "a normal, human-facing agent" },
    { key: "inject_tool_guide", label: "Inject tool guide", group: "Behaviour", type: "bool",
      hint: "Appends runtime tool guidance to the prompt. Small local models may copy the framing, so prefer it on capable models.",
      unsetMeans: "off", advanced: true },
  ],
};
