---
name: Skill
description: "Skill tool — list the skills you may use and load one skill's instructions into your context when a task needs them."
---
The `Skill` tool loads **skills**: named blocks of instructions for a kind of
task (a writing style, a review checklist, a domain procedure). Skills are not
in your system prompt. You find the one you need with `list`, load it with
`invoke`, then follow what it says. Its text arrives as the tool result.

## When to use it — and when not

- **Use Skill** when a task matches a skill's description, or your
  instructions name a skill. Load it once, at the point it becomes relevant.
- **Do not use Skill** to hand work to someone else. A skill only gives you
  instructions; you still do the work with your own tools. To have another
  agent do it, use `Agent`.
- **Do not use Skill** to create or edit a skill. That is `SkillDef`.

## Operations

Fetch one operation's article, with examples, as `Skill/<op>` — for example
`{"op":"help","topic":"Skill/invoke"}`.

| op | What it does | Required besides `op` |
|---|---|---|
| `list` | the skills you may load, with descriptions, optionally filtered | — |
| `invoke` | load one skill's instructions (the default when `op` is omitted) | `name` |

## Which skills you may load

- Your agent's `skills:` allowlist decides which names you may list and load.
  `Context {"op":"permissions"}` shows it under `skills`; empty means all.
- A skill may declare tools it needs. **Every one of them must already be in
  your tools** — a skill can never give you a tool you do not have. If it asks
  for more, `invoke` refuses it.
- Names may be grouped with `/`, like `doc/redactor`.

For the full picture — allowlist patterns, grouping, authoring — read
`{"op":"help","topic":"skills"}`.
