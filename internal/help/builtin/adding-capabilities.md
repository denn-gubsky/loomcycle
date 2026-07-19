---
name: adding-capabilities
description: "How to gain a capability you don't have — you can't widen your own tools; load a skill within your ceiling, delegate to a sub-agent that holds the tool, or ask the operator to widen you."
aliases: [tool-ceiling, widening-tools]
---
Your tools are **fixed for this run**. You can only call the tools in
your own tool set, and you **cannot widen it yourself** — not with a
skill, not by authoring or forking an agent. This is a security
boundary: if an agent could grant itself new tools, a single injected
instruction would become privilege escalation. So when you need a
capability you don't have, use one of the paths below — do not try to
work around a missing tool.

## Decision tree

**1. You need a skill (instructions), and it only uses tools you
already have.** This is free. Use the `Skill` tool: `op=list` to
discover what's available, `op=invoke` to load a skill's body as a
`tool_result` at the moment it's relevant. If you also hold `SkillDef`
and your `skills:` allowlist permits the name, you can author one —
but the skill's declared `tools:` must be a **subset of yours**.
Loading a skill that needs a tool you lack is refused. See
`Context op=help topic=skills`.

**2. You need a TOOL you don't have.** You can't give it to yourself.
Two real paths:

- **Delegate to a sub-agent that has it.** If you hold the `Agent`
  tool, spawn a purpose-built sub-agent that holds the tool and let it
  do the work — a sub-agent may hold tools you don't (it was vetted by
  the operator, not minted by you). This is the sanctioned way to
  compose a capability beyond your ceiling: e.g. spawn a code/sandbox
  agent to compile and run code, or an agent that holds a specific MCP
  tool to make that call. Prefer this whenever the missing capability
  is a self-contained task. See `Context op=help topic=subagents`.
- **Ask the human operator to widen you.** Adding a tool to your own
  set is an operator action (they derive an agent that carries the
  extra tool). You can't do it; if the task genuinely needs it and
  delegation doesn't fit, surface the need — use the `Interruption`
  tool to ask — rather than silently failing or faking the result.

**3. Report honestly when you're blocked.** If you lack a tool, can't
delegate (no `Agent` tool or no suitable sub-agent), and can't ask,
say so plainly. Never pretend a tool ran or fabricate its output.

## If you author agents (meta-agents)

If you hold `AgentDef` (and a non-empty `agent_def_scopes`), you can
create and fork agents — but **you still can't escalate**:

- `create` — a new agent's `tools:` can never exceed **your own**
  tools. You cannot mint a child more capable than yourself.
- `fork` — may only **narrow** an existing agent's tools; the lineage
  root is a permanent ceiling. A fork can never add a tool the root
  lacks.

So to give any agent a tool that no existing agent (in your reach)
already holds, a **human operator** must introduce it. What you *can*
do is compose and specialise within the tools already available to
you.

## Granting a whole MCP server

When a `tools:` list should include every tool of one MCP server, a
single prefix-glob entry covers it: `mcp__<server>__*` (e.g.
`mcp__slack__*`). It matches all of that server's advertised tools, so
tools later added to the server are covered automatically. (This is
still subject to the ceiling rules above — it's a convenience for
listing, not a way to widen past what you may already grant.)

## See also

- `Context op=help topic=skills` — the on-demand skill model.
- `Context op=help topic=subagents` — spawning vs channel handoff.
- `Context op=help topic=scopes` — what your grants are scoped to.
- `Context op=self` — inspect your own tools, model, and sandbox right now.
