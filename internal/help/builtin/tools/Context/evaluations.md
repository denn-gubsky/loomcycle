---
name: Context/evaluations
description: "Context op=evaluations — score statistics (mean, median, min, max, latest) for the evaluations submitted against one agent definition, optionally including its ancestors."
---
`evaluations` summarises the scores given to runs of one agent definition.
Use it to compare versions of an agent before promoting one. To submit a
score, use the `Evaluation` tool.

## Arguments

- `def_id` (required) — the agent definition id (`def_...`). Discover ids with
  `{"op":"agents"}` or `{"op":"lineage"}`.
- `include_lineage` — `true` adds the ancestors' evaluations to the
  aggregate. Default `false`.

## Returns

`{def_id, count, score, dimensions?, by_emitter_role?, lineage_included}`.
`score` and each entry of `dimensions` / `by_emitter_role` are
`{mean, median, min, max, latest, count}`. **`count: 0` is not an error**: no
evaluations have been submitted yet, and every statistic is 0.

## Errors

- `evaluations: missing required field: def_id ...`.
- `evaluations: not configured (no Store backend)`.

## Examples

How has this version scored?

```json
{"op": "evaluations", "def_id": "def_b4407b522dc11495"}
```

```json result
{"def_id": "def_b4407b522dc11495", "count": 12, "lineage_included": false,
 "score": {"mean": 0.74, "median": 0.77, "min": 0.41, "max": 0.93, "latest": 0.81, "count": 12}}
```

Include the scores of the versions it was forked from:

```json
{"op": "evaluations", "def_id": "def_b4407b522dc11495", "include_lineage": true}
```
