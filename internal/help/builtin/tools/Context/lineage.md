---
name: Context/lineage
description: "Context op=lineage — walk an agent definition's fork tree: its ancestors (parent chain) and its descendants, up to a depth."
---
`lineage` shows where an agent definition came from and what was forked from
it. Use it when you evolve agents: to find the parent of a version you are
judging, or every variant forked from one.

## Arguments

- `def_id` (required) — an agent definition id (`def_...`). Get one from
  `{"op":"agents"}` (`active_def_id`), from your own `agent_def_id` in
  `{"op":"self"}`, or from `AgentDef`.
- `depth` — how many levels to walk in each direction (default 10, at most
  100).

## Returns

`{root, ancestors, descendants, depth, truncated}`. Each entry is
`{def_id, name, version, parent_def_id?, retired, description?}`.
`ancestors` runs from the parent upward. `descendants` is breadth-first and
stops at 500 entries, in which case `truncated` is `true`.

## Errors

- `lineage: missing required field: def_id ...`.
- `lineage: def_id "..." not found` — check the id; retrying is pointless.
- `lineage: not configured (no Store backend)` — this runtime keeps no
  definitions.

## Examples

Show the family of the active `cv-adapter` definition:

```json
{"op": "lineage", "def_id": "def_b4407b522dc11495"}
```

```json result
{"root": {"def_id": "def_b4407b522dc11495", "name": "cv-adapter", "version": 3, "parent_def_id": "def_0c9e7a15f3d2b884", "retired": false},
 "ancestors": [{"def_id": "def_0c9e7a15f3d2b884", "name": "cv-adapter", "version": 2, "retired": false}],
 "descendants": [], "depth": 10, "truncated": false}
```

Only the immediate parent and children:

```json
{"op": "lineage", "def_id": "def_b4407b522dc11495", "depth": 1}
```
