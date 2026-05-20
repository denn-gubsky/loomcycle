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
 "allowed_tools": ["Read", "WebFetch"]}
```

`body` is required on `create` / `fork` (empty / whitespace-only
is rejected — a zero-body skill is silent prompt corruption).

`allowed_tools` is the per-skill tool ceiling. It must be a
SUBSET of the calling agent's effective `allowed_tools` — skills
may NARROW but never widen, same rule as `AgentDef`.

## How the body lands in the model

Two consumption paths:

**Approach A — baked into the system prompt.** When the skill
name is in the agent's `skills:` yaml list, the loomcycle
run-creation handler resolves `SkillDefGetActive(name)` at
session start. A DB-active row's body OVERRIDES the static
SKILL.md body for the duration of that run. The next run picks
up the latest active row; existing in-flight runs keep their
locked system prompt — there is no mid-run skill body swap.

**Approach B — via the `Skill` tool.** When the agent calls
`Skill({"name": "..."})`, the tool consults
`SkillDefGetActive(name)` first; falls back to the static
SKILL.md body on miss. Same DB-first semantics as Approach A,
but per-call.

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

New runs of any agent that lists `voice-applier` in its `skills:`
now get the v2 body baked into their system prompt.

## Scope policy

The yaml gate is `skill_def_scopes` (mirror of
`agent_def_scopes`). Default-deny:

```yaml
agents:
  curator:
    skills: [voice-applier, cv-voice-applier]
    allowed_tools: [Read, Skill, SkillDef]
    skill_def_scopes:
      - named:voice-applier      # may fork/promote this skill
      - named:cv-voice-applier   # ...and this one
```

Closed set: `any` / `named:<skill-name>` / `descendants`. No
`self` scope — skills have no agent identity, so a "self"
constraint is meaningless.

## When NOT to use SkillDef

- The skill body is stable. Plain `SKILL.md` checked into the
  operator's repo is simpler than a versioned DB row.
- You want to A/B test the skill across agents in the SAME run.
  Approach A locks the body at session start, so two sub-agents
  spawned in the same call would both see the same active body.
  For mid-run A/B, use Approach B and pass the desired version
  as an explicit parameter.

## Cross-references

- `help(topic="experimentation")` — the AgentDef + Evaluation
  cousin for whole-agent evolution.
- `help(topic="scopes")` — agent vs user scope across Memory +
  Channel + DefScopes.
- `Context.permissions` — surfaces the active `skill_def_scopes`
  for the calling agent.
