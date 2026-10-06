---
name: TeamDef/fork
description: "TeamDef op=fork — store a new version of a team: your overlay merged field by field over its active version (or one you pin); not promoted unless you say so."
---
`fork` makes a new version of an existing team. Each top-level field of the
overlay replaces the parent's (`states`, `transitions`, `local`, `vars` are
replaced whole, not merged); fields you omit are kept. The result is checked
as `create` checks a graph, and refused with nothing stored if it fails.

The parent is the team's active version in your tenant, else the shared one;
`parent_def_id` pins another version of the same team.

## Arguments

- `name` (required) — the team to fork.
- `overlay` — the fields to change. Omitted: a copy of the parent.
- `parent_def_id` — fork this version instead of the active one.
- `description` — why this version exists.
- `promote` — `true` makes the new version active (default `false`).

## Returns

The stored version, as `create` returns it, with `parent_def_id` set.

## Errors

- `fork: no parent — name "..." has no DB version to fork ...` — create the
  team first.
- `fork: parent_def_id "..." not found` / `... has name "...", refusing to
  fork under name "..."` — pin a version of this team.
- `fork: ...` — the merged graph was refused; the message says why.

## Examples

Raise the per-state cycle cap and make the new version active:

```json
{"op": "fork", "name": "pr-review", "overlay": {"max_iterations": 5}, "promote": true,
 "description": "allow more rework rounds"}
```
