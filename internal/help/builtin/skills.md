---
name: skills
description: "The Skill tool — discover and load domain-specific instruction sets on demand, governed by the agent's skills: pattern allowlist."
aliases: [skill]
---
A **skill** is a named, reusable block of instructions (a `SKILL.md`
body) you load into context **only when you need it**. Skills are
**on demand** — they are NOT baked into your system prompt.
You hold the `Skill` tool; you call it to discover what's available
and to pull a skill's body in as a `tool_result` at the moment it's
relevant. This keeps the base prompt small and lets one agent carry a
large library it uses a slice of per run.

If you have the `Skill` tool, you use skills through it. There is no
other step.

## The two operations

```
{"op":"list"}                 discover the skills you may use.
{"op":"list","pattern":"doc/*"}  ...filtered by a /-glob.
{"op":"invoke","name":"doc/redactor"}  load a skill's body.
{"name":"doc/redactor"}       shorthand — op defaults to invoke.
```

- **`list`** returns `[{name, description}]` for every skill visible to
  you (the catalog ∩ your allowlist ∩ the optional `pattern`), sorted.
  Start here when you don't know the exact name.
- **`invoke`** (the default when you pass only `name`) returns the
  skill's markdown body. Read it, then act on it — the body is
  guidance for the current task, not something you echo back.

## What you may access — the `skills:` allowlist

Each agent has a `skills:` field: an **ordered pattern allowlist** that
governs listing, use (invoke), AND authoring (see below) uniformly.
Entries are `/`-globs with an optional sign:

```
doc/*          allow the whole doc/ group (one segment)
marketing/**   allow doc/ and all nested groups (many segments)
seo            allow exactly the skill named "seo"
-doc/secret    deny one name (carve-out)
-*             deny everything (no skills at all)
```

Evaluation: if any **positive** entry is present it's a whitelist —
a name is allowed iff it matches ≥1 positive and no negative. With
only negatives it's a blacklist — allowed unless a negative matches.
**Empty or absent `skills:` = allow all** (and the `Skill` tool is
still auto-added, so on-demand access is the default). `-*` = nothing,
and then the `Skill` tool is not added at all.

`Context op=permissions` shows the effective `skills` allowlist for
the calling agent, so you can see your own limits.

## Grouping with `/`

Skill names may be `/`-grouped — `doc/semantic-chunking`,
`marketing/seo`. A group is just a name prefix; `skills: [doc/*]`
admits the whole `doc/` domain and picks up any new `doc/…` skill
without restating names. Nested `LOOMCYCLE_SKILLS_ROOT` directories
map to grouped names (`SkillsRoot/doc/redactor/SKILL.md` →
`doc/redactor`).

## The tools-subset rule

A skill declares its own `tools:` — the tool ceiling for work done
under it. That set must be a **subset** of your effective `tools`.
A skill can narrow, never widen. If a skill needs a tool you don't
hold, the invoke is refused (surfaced as `is_error`) — request the
tool from the operator, or use a different skill.

## The run-start note

When your `skills:` is a whitelist, a short note is appended to your
prompt at run start naming the permitted patterns and telling you to
call the `Skill` tool. For the allow-all / blacklist cases there's no
note — you still have the `Skill` tool; call `{"op":"list"}` to see
what's there.

## Authoring skills

Creating or revising a skill body at runtime is a separate tool,
`SkillDef`, gated by the **same** `skills:` allowlist (you may author
only names your allowlist admits; `-*` = author nothing) and requiring
you to also hold the `SkillDef` tool. See `help(topic="skills-evolution")`
for the create / fork / promote loop.

## Cross-references

- `help(topic="skills-evolution")` — author + version skill bodies at
  runtime (the `SkillDef` substrate).
- `help(topic="scopes")` — agent vs user vs tenant scope.
- `Context op=permissions` — your effective `skills` allowlist + tools.
- `Context op=tools` — confirm you actually hold the `Skill` tool.
