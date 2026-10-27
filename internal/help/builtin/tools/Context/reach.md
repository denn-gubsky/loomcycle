---
name: Context/reach
description: "Context op=reach — the allow and deny patterns your access rules hold for one kind of resource and one action, for planning before you pick what to act on."
---
`reach` answers "what of this kind may I act on?" for planning: the name
patterns your access rules allow and deny for one resource type and one
action. Use it to choose among agents, tools or volumes before you commit to
one; use `can` to check a single named one. It only reads, and only your own
rules.

## Arguments

- `type` — the resource type: `agent`, `tool`, `volume`, `skill`, `memory`,
  `document`, ... (the same types `can` takes).
- `action` — a verb of that type: `run` for an agent, `invoke` for a tool,
  `write` for a volume.

## Returns

`{type, action, mode, allow, deny, semantics, unmatched, conditional?,
removed_tools?, see_also?}`

- `allow` / `deny` — the name patterns as the rules write them: `*` is any
  one segment, `**` any number of segments (`support/**` matches
  `support/l2/escalation`). `semantics: "deny_overrides"`: a name matched by
  both is denied.
- `unmatched` — what happens to a name neither list matches: `scopes` (your
  scopes and grants decide) or `denied` (refused).
- `conditional: true` — some rules apply only under a condition (time of
  day, transport, ...), so they are in neither list; ask `can` about the
  specific name.
- `removed_tools` (type `tool`) — the tools your rules took out of this
  run; you will not see them in your tool list.
- `see_also` (type `agent`) — `op=agents` lists the agents you may start.

## Errors

- `access control is not active for this run` — no rules apply; your
  definition's grants decide (`op=permissions`).
- `type and action are required` / `"<a>" is not an action on <type>` — fix
  the arguments.

## Examples

Which agents may I run?

```json
{"op": "reach", "type": "agent", "action": "run"}
```

```json result
{"type": "agent", "action": "run", "mode": "enforce", "allow": ["chat/*", "support/**"],
 "deny": ["admin/**"], "semantics": "deny_overrides", "unmatched": "denied",
 "see_also": "op=agents lists the agents this run may start"}
```
