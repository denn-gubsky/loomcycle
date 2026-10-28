---
name: Identity/can
description: "Identity op=can — ask whether a person's access rules allow an action on a resource, answered as the access checks would answer it."
---
`can` is `Context op=can` for another person of your tenant: may alice run
this agent, use this tool, read this memory scope? The answer comes from the
same evaluation the access checks make when that person acts.

You need the right to read that person, and you may only ask about what you
are allowed yourself: the directory does not tell you who can do what you
cannot. A question you could not answer yes to for yourself reads as not
found. Omit `subject` to ask about yourself.

## Arguments

- `subject` — the person to ask about (omit for yourself).
- `action`, `resource` — as in `Context op=can`: a verb of the type and
  `{type, id}`.
- `queries` — instead, up to 32 `{action, resource}` questions.

## Returns

`{subject, decision, outcome, mode, allowed, reasons?, fallback?}`, or
`{subject, evaluations: [...]}` for `queries`. For an account that is not
active, `account_status` is set and `allowed` is false: it is refused
everything until an operator activates it.

## Errors

- `identity_not_found` — no such person, you may not read them, or you ask
  about something you may not do yourself.

## Examples

```json
{"op": "can", "subject": "alice", "action": "run", "resource": {"type": "agent", "id": "support/triage"}}
```

```json result
{"subject": "alice", "decision": true, "outcome": "allow", "mode": "enforce", "allowed": true,
 "reasons": [{"acl": "support-tier-1", "version": 3, "rule": 0, "effect": "allow"}]}
```
