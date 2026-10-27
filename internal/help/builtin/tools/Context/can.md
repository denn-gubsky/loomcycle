---
name: Context/can
description: "Context op=can — ask whether your access rules allow an action on a resource (one question, or up to 32), answered the way the access checks will answer it, before you try."
---
`can` answers "may I do this?" from the access rules of the person this run
acts for, without doing it. Ask it before you plan around another agent, a
tool, a volume or a shared scope, instead of trying the action and failing
halfway through a task. The answer comes from the same evaluation the access
checks make when you act, so it cannot disagree with them. It only reads: it
answers for you, never for anyone else.

## Arguments

- `action` — a verb of the resource type: `run`, `spawn` (agent); `invoke`
  (tool, skill, mcp); `read`, `write` (volume, memory, document, sqlmem);
  `use` (model, tier, search); and so on.
- `resource` — `{type, id}`. `type` is the kind of thing (`agent`, `tool`,
  `volume`, `skill`, `mcp`, `memory`, `sqlmem`, `document`, `history`,
  `channel`, `team`, `credential`, `model`, `tier`, `run`, `principal`, ...).
  `id` is its name in that kind's shape: an agent `chat/support`, a tool
  `Bash`, a memory scope `tenant`, a document `tenant:/kb/pricing`, a
  credential `user:github`. Omit `id` to ask about the kind as a whole.
- `queries` — instead of `action` and `resource`, a list of up to 32
  `{action, resource}` questions, answered in order.

An unknown type, an action that is not one of the type's verbs, or a
malformed `id` is refused before anything is evaluated.

## Returns

`{decision, outcome, mode, allowed, reasons?, fallback?, tenant_floor?}`, or
`{evaluations: [...]}` of those for `queries`.

- `outcome` — `allow` (a rule allowed it), `deny` (a rule denied it; a deny
  always wins over an allow), or `no_match` (no rule speaks about it).
- `decision` — `true` only when a rule allowed it.
- `allowed` — what the access check will actually do. With `mode` `enforce`
  it equals `decision`. With `mode` `compat`, `no_match` is not a refusal:
  `fallback: "scopes"` says your scopes and your agent's own grants decide,
  so `allowed` is `true` and `op=self` / `op=permissions` tell you the rest.
  With `mode` `shadow` the rules are only being observed: your scopes and
  grants decide every question (`fallback: "scopes"`). A `deny` is a refusal
  in `compat` and `enforce`.
- `reasons` — every rule that matched, `{acl, rule, effect, version?}`,
  denies first: the list and the rule number to quote to an operator.

Being allowed here does not grant anything your agent was not given: a tool
must still be in your tools, a scope in your scopes.

## Errors

- `access control is not active for this run` — no rules apply, so there is
  nothing to ask; your definition's grants decide (`op=permissions`).
- `"<x>" is not a resource type` / `"<a>" is not an action on <type>` — fix
  the question; `op=help topic=Context/can` lists the shapes.
- `the limit is 32` — split a larger batch, or use `reach` for a whole kind.

## Examples

May I run the triage agent?

```json
{"op": "can", "action": "run", "resource": {"type": "agent", "id": "admin/triage"}}
```

```json result
{"decision": false, "outcome": "deny", "mode": "enforce", "allowed": false,
 "reasons": [{"acl": "support-tier-1", "version": 3, "rule": 2, "effect": "deny"}]}
```

Several questions at once, before planning a task:

```json
{"op": "can", "queries": [
  {"action": "invoke", "resource": {"type": "tool", "id": "Bash"}},
  {"action": "write", "resource": {"type": "memory", "id": "tenant"}}]}
```
