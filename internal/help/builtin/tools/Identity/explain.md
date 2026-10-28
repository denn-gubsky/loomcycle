---
name: Identity/explain
description: "Identity op=explain — which access rules decide one question for a person, rule by rule."
---
`explain` answers one `can` question and says why: every rule of the
resource's type that applies to the person, whether it matched, and its
effect. Use it to tell someone which list and rule to ask an operator about.

The rules of a list are shown only when you may read that list; otherwise a
trace names the list, the rule's number and its effect. The same locks as
`can` apply. Omit `subject` to explain your own access.

## Arguments

- `subject` — the person (omit for yourself).
- `action`, `resource` — one question, as in `can`.

## Returns

`{subject, decision: {decision, outcome, mode, reasons?}, traces: [...],
missing_acls}`. `missing_acls` names lists bound to the person that no longer
exist; they match nothing.

## Examples

```json
{"op": "explain", "subject": "alice", "action": "run", "resource": {"type": "agent", "id": "admin/triage"}}
```

```json result
{"subject": "alice", "decision": {"decision": false, "outcome": "deny", "mode": "enforce"},
 "traces": [{"acl": "support-tier-1", "rule": 1, "effect": "deny"}], "missing_acls": []}
```
