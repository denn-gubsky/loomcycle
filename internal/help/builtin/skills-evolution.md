---
name: skills-evolution
description: The SkillDef substrate for evolving skill bodies at runtime (mirror of AgentDef for skills).
---
The v0.8.22 substrate lets agents version skill bodies at runtime
without binary or filesystem redeploys. Mirror of `AgentDef` —
same six ops, same lineage model, same active pointer. Use it
when you want to iterate on a skill's prompt content as fast as
you iterate on agent definitions.

Static `SKILL.md` files (loaded at boot from
`LOOMCYCLE_SKILLS_ROOT`) remain the operator's immutable ground
truth; `SkillDef` produces a DERIVED layer of agent-authored
versions on top.

## Six operations

```
create   — declare a brand-new skill name with a v1 definition.
           Refused if `name` already exists in the static
           skills.Set (use `fork` to derive a new version).
fork     — make a new version from an existing parent. Bootstraps
           v1 from the static SKILL.md when neither a DB row nor
           an active pointer exists yet.
get      — fetch one row by def_id.
list     — list versions for a name, version DESC.
retire   — flip the retired flag. Row stays visible.
promote  — set the active pointer for a name to a specific def_id.
```

`create` defaults `promote=true`. `fork` defaults `promote=false`
— operators promote explicitly after evaluation (the same
non-auto-promote stance as `AgentDef`; loomcycle is a substrate,
not Hermes).

## The overlay shape

```
{"body": "## skill markdown body",
 "description": "what this skill is for",
 "tools": ["Read", "WebFetch"]}
```

`body` is required on `create` / `fork` (empty / whitespace-only
is rejected — a zero-body skill is silent prompt corruption).

`tools` is the per-skill tool ceiling. It must be a
SUBSET of the calling agent's effective `tools` — skills
may NARROW but never widen, same rule as `AgentDef`.

## How the body lands in the model

Skills are loaded **on demand** — never baked into the
system prompt. When the agent calls `Skill({"name": "..."})`
(or `Skill({"op":"list"})` to discover), the tool consults
`SkillDefGetActive(name)` first and falls back to the static
SKILL.md body on miss (DB-first). The `Skill` tool is auto-added
to every agent whose `skills:` allowlist permits any skill. A
promoted new version is picked up by the next `Skill` invoke; an
in-flight call already loaded keeps the body it read.

## The fork-and-promote loop

```
{"op":"fork", "name":"voice-applier",
 "overlay":{"body":"## Revised voice rules\n..."},
 "description":"v2: tighter editorial tone"}
→ {"def_id":"sdf_v2_abc...", "version":2, "promoted":false, ...}
```

Then test the fork by passing the `def_id` to a sub-agent (or
have the calling agent reference the active version after
promotion). When satisfied:

```
{"op":"promote", "def_id":"sdf_v2_abc..."}
```

Any agent that lists `voice-applier` in its `skills:` allowlist
now loads the v2 body when it next invokes the `Skill` tool.

## Authoring gate

Authoring is gated by the same `skills:` **pattern allowlist**
that governs listing + use — one policy for all three.
The agent must also hold the `SkillDef` tool. Empty/absent
`skills:` = author anywhere; `-*` = author nothing.

```yaml
agents:
  curator:
    tools: [Read, Skill, SkillDef]
    skills:
      - voice-applier      # may list/use/author exactly these two
      - cv-voice-applier
```

Entries are `/`-globs with an optional `+`/`-` sign: `doc/*`
allows the whole `doc/` group, `-doc/secret` carves one out.

## When NOT to use SkillDef

- The skill body is stable. Plain `SKILL.md` checked into the
  operator's repo is simpler than a versioned DB row.
- You want to A/B test two versions in the SAME run. The active
  pointer is per-name, so both branches see the same active body.
  Fork both, then have each sub-agent load its intended `def_id`.

## Cross-references

- `help(topic="skills")` — how to USE skills (the `Skill` tool:
  discover + load on demand + the `skills:` allowlist). Start there
  if you just want to run a skill, not author one.
- `help(topic="experimentation")` — the AgentDef + Evaluation
  cousin for whole-agent evolution.
- `help(topic="scopes")` — agent vs user scope across Memory +
  Channel + DefScopes.
- `Context.permissions` — surfaces the active `skills` allowlist
  for the calling agent.
