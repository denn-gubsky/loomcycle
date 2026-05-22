---
name: loomcycle
description: What loomcycle is, what tools you have, how agents and runs fit together.
---
You are running inside **loomcycle** — an agentic runtime that owns the
LLM tool-use loop end-to-end. The runtime gives you a curated set of
built-in tools plus zero or more operator-supplied MCP tools, persists
your conversation transcripts, and lets you spawn sub-agents and
coordinate with other agents via durable primitives.

## The shape of one run

A **run** is one POST /v1/runs invocation — model → tool_use →
tool_result → model, repeated until the model says `end_turn`. Each
run lives inside a **session** (the conversation thread); a session
can have many runs (each continuing the prior turn).

You can find out who you are right now with `Context.self`:

```
{"op":"self"}
→ {"agent_name", "agent_id", "user_id", "user_tier", "agent_def_id"}
```

## The built-in primitives

Loomcycle ships nine higher-level built-in tools beyond the basic
file/network group (`Read` / `Write` / `Edit` / `Grep` / `Glob` /
`NotebookEdit` / `HTTP` / `WebFetch` / `WebSearch` / `Bash`):

- **Grep** (v0.8.24) — content search with RE2 regex over the sandbox
  root. Three output modes: `files_with_matches` (default), `content`
  (with `-A`/`-B`/`-C` context lines), and `count`. Skips binary files
  automatically; respects `case_insensitive`, `multiline`, `glob`
  filename filter, and `head_limit`. Pure-Go — no `ripgrep` dependency.
- **Glob** (v0.8.24) — file pattern matcher with `**` for recursive
  segment match. Returns paths sorted by mtime DESC (newest first),
  capped at 100 results. Useful for "find all `.tsx` files I touched
  recently."
- **NotebookEdit** (v0.8.24) — surgical Jupyter `.ipynb` cell mutation:
  `replace` (by `cell_id`), `insert` (after `cell_id`, or at index 0 when
  empty), `delete`. Writes atomically; preserves all other cells +
  notebook metadata verbatim.
- **Memory** — persistent key/value scoped to `agent` or `user`.
  v0.9.0 adds **semantic search**: pass `embed: true` + `embed_text`
  on `set` to make a row searchable, then `op: search` to retrieve
  by cosine similarity. See `help(topic="vector-memory")` for the
  full surface + failure modes.
- **Channel** — durable inter-agent message bus with cursor-based
  at-least-once delivery; `_system/*` channels carry runtime signals
  (heartbeats, alarms, pause/resume state).
- **Agent** — spawn a named sub-agent in a fresh session.
- **Skill** — load operator-curated prompt fragments. DB-active
  `SkillDef` rows override the static SKILL.md body when present.
- **AgentDef** — fork / promote / retire agent definitions at runtime.
- **SkillDef** (v0.8.22) — fork / promote / retire SKILL bodies at
  runtime. Mirror of AgentDef for skills.
- **Evaluation** — score (run, def) pairs for self-improvement loops.
- **Interruption** — human-in-the-loop primitive: ask / notify / cancel
  (v0.8.16).
- **Context** — what you're reading right now: introspection over the
  rest of the surface.

Cross-cutting topics that explain how these compose:

- `help(topic="scopes")` — agent vs user scope across Memory + Channel
- `help(topic="subagents")` — when to spawn vs publish to a channel
- `help(topic="experimentation")` — AgentDef + Evaluation fork-and-score
- `help(topic="skills-evolution")` — SkillDef substrate for runtime
  skill body evolution (v0.8.22)
- `help(topic="vector-memory")` — semantic search on Memory: when to
  use it, the `embed: true` / `embed_text` / `search` shape, and the
  four failure modes (v0.9.0)
- `help(topic="system-channels")` — `_system/*` prefix and the admin endpoint
- `help(topic="interruption")` — human-in-the-loop ask / notify / cancel
- `help(topic="pause-resume-snapshot")` — runtime quiesce + portable
  JSON snapshot (operator-driven; agents don't call these directly)
- `help(topic="n8n-integration")` — three patterns for composing
  loomcycle with n8n's workflow builder (bidirectional MCP +
  planned community node)
- `help(topic="content-signatures")` — SHA-256 content_sha256 on
  AgentDef + SkillDef rows; the bundle-vs-deployed comparison
  workflow for Docker-bundled operators
- `help(topic="dynamic-mcp")` — operator-admin-only MCPServerDef
  substrate for registering HTTP / Streamable-HTTP MCP servers at
  runtime without a yaml edit; the n8n self-registration pattern

## Your transcript records what YOU received

v0.9.x: every run persists two transcript events BEFORE the loop's
first model call:

- `user_input` — the caller's `segments` from `POST /v1/runs` /
  `POST /v1/sessions/{id}/messages` (or from `Agent.spawn` on a
  sub-run). Surfaced in the Web UI as the **"input · user"** card.
- `system_prompt` — the resolved system prompt: the
  AgentDef-derived template + AgentDef overlay (when forked) +
  SkillDef bodies (when DB-active rows override the static
  SKILL.md). Payload carries provenance — `agent_def_id` (when
  pinned) + `skill_def_ids` (the active SkillDef row id per
  resolved skill). Surfaced as the **"input · system"** card.

Operators inspecting a run see exactly what instructions you saw
at THAT moment, not just what the AgentDef template says NOW (the
template may have been forked + promoted between runs). If an
agent's behaviour drifts after a SkillDef promote, comparing two
runs' `system_prompt` events shows the diff directly.

## Discovering what you have

Three Context ops are your map:

```
{"op":"tools"}        → catalog of YOUR allowed_tools (post-filter)
{"op":"doc","name":X} → input schema + side_effect_class for tool X
{"op":"permissions"}  → policy bundles that gate your behaviour
```

Side-effect classes (`pure` / `state` / `network` / `filesystem` /
`privileged` / `unknown`) let you reason about a tool's posture before
calling it.

## Why introspection is here

Self-evolving agents (you, especially if you're running against a
forked AgentDef) need to know what's available without having every
runtime convention re-injected into your system prompt. `Context.help`
and `Context.doc` are the canonical references — pull what you need
when you need it.
