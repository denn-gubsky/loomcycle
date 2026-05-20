---
name: experimentation
description: The AgentDef + Evaluation fork-and-score pattern for self-improving agents.
---
The v0.8.5 substrate lets agents mutate themselves and rate the
results. Together, `AgentDef` (the mutation surface) and
`Evaluation` (the selection surface) form a single experimentation
loop:

```
1. fork    → create a variant
2. spawn   → run the variant against a workload
3. submit  → score the result
4. aggregate → compare variants
5. promote → make the best one active
```

Loomcycle deliberately does NOT auto-promote based on score —
selection is policy, lives in YOUR orchestration logic (max-score,
GA tournament, RLHF reward model, whatever fits the experiment).

## The fork

```
{"op":"fork", "name":"reviewer",
 "overlay":{"system_prompt":"... new prompt ..."},
 "description":"v2: trying terser feedback style"}
→ {"def_id":"def_v2_abc...", "version":2, "promoted":false, ...}
```

`fork` defaults `promote=false` — the new def is a shadow version,
not the active one. Operator-blessed `cfg.Agents[name]` stays the
ceiling for AllowedTools (forks may NARROW; never widen).

## Spawning the variant

Use `Agent` with `def_id` pinning so the sub-run uses the fork
(not the active pointer):

```
{"name":"reviewer", "prompt":"...", "def_id":"def_v2_abc..."}
→ sub-run uses the v2 system prompt
```

The sub-run's `agent_def_id` is denormalised onto its `runs` row
AND onto any `Evaluation.submit` calls targeting that run — your
aggregate queries downstream automatically partition by def.

## Scoring

After the sub-run completes, score it:

```
{"op":"submit", "run_id":"<sub-run's id>",
 "score":0.83,
 "dimensions":{"correctness":0.9,"terseness":0.7},
 "rationale":"clear feedback, slightly verbose intro"}
```

`emitter_role` is derived server-side from your ctx vs the target
run's identity — `self` / `parent` / `external` / `unrelated`. You
don't supply it; the runtime stamps it. Your agent yaml's
`evaluation_scopes` gates which roles you may submit (default-deny).

A parent agent scoring its own sub-runs gets `emitter_role=parent`
(use `submit_descendants` scope). An unrelated coordinator scoring
any run gets `unrelated` (needs `submit_any`).

## Aggregating

After N forks × M runs each:

```
{"op":"aggregate", "def_id":"def_v2_abc..."}
→ {"def_id", "count", "score": {mean, median, min, max, latest},
   "dimensions": {...}, "by_emitter_role": {...}}
```

Optional `include_lineage` walks `parent_def_id` and includes
ancestors' scores — useful when comparing a recent fork against
the cumulative history of its lineage.

## Promoting the winner

Once your selection policy picks the best fork:

```
{"op":"promote", "def_id":"def_v2_abc..."}
→ active pointer for "reviewer" now points at v2
```

New `Agent` spawns of `reviewer` (without `def_id`) now use v2.
Older sub-runs still in flight are unaffected (they pinned their
own def_id at spawn time).

## Rollback

If v2 degrades production, promote any prior def_id (including v1):

```
{"op":"promote", "def_id":"def_v1_orig..."}
```

Or retire v2 entirely:

```
{"op":"retire", "def_id":"def_v2_abc...", "retired":true}
```

Retired defs can't be promoted; they remain in the lineage for
historical evaluation queries.

## The canonical loop

```
for trial in 1..N:
  fork → variant
  for example in workload:
    spawn(variant, example) → run_id
    submit(run_id, score)
  aggregate(variant) → metrics
pick best metric → promote
```

See `help(topic="subagents")` for spawn mechanics; `help(topic="scopes")`
for how Memory + Channel state lives across forks.

## Sibling substrate: SkillDef

`SkillDef` (v0.8.22) mirrors `AgentDef` for SKILL bodies — same
fork / promote / retire surface, same lineage model, same active
pointer. Use it when you want to evolve a skill's prompt content
without binary or filesystem redeploys. See
`help(topic="skills-evolution")` for the full overlay shape +
how DB-active rows override the static SKILL.md body in both
consumption paths (system-prompt baking AND the `Skill` tool).
