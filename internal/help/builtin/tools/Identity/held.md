---
name: Identity/held
description: "Identity op=held — what this deployment holds for one person: chats, memory rows, documents, token budget and usage."
---
`held` counts what the deployment keeps for one person of your tenant: their
run activity, chats, user-scope memory rows, documents, token budget and
usage, in one call. It needs a grant to manage principals; anyone else reads
as not found. It changes nothing — removing a person's data is an operator's
erasure, not this tool.

## Arguments

- `subject` — the person's subject id. A subject that has run without being
  registered can be asked about too.

## Returns

`{tenant, subject, activity, chats, memory, documents?, budget?, usage,
errors?, notes?}`. A plane that could not be read is named in `errors`, and
its count is then a lower bound — never read a failed plane as empty.

## Examples

```json
{"op": "held", "subject": "alice"}
```

```json result
{"tenant": "acme", "subject": "alice", "activity": {"subject": "alice", "running_count": 0, "total_count": 12},
 "chats": 4, "memory": {"user_scope_rows": 37}, "documents": 2, "usage": {"calls": 210, "cost": 1.84}}
```
