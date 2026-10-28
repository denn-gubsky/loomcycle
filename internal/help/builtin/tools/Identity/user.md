---
name: Identity/user
description: "Identity op=user — a person's profile plus their status and groups, for a caller allowed to read principals."
---
`user` is `profile` plus where the person stands: their account `status` and
their `groups`, each with `via` — `direct`, or the group through which they
are in it. A caller who may also manage principals sees `access_mode`.

You may always look yourself up. Anyone else needs a grant to read
principals for the person you act for; without one they read as not found.

## Arguments

- `subject` — the person's subject id.

## Returns

`{subject, display_name?, names?, public?, status?, status_note?, groups,
access_mode?}`. `status` is `active`, `disabled` or `pending`; a `pending`
account is waiting for an operator to approve it and can do nothing yet.

## Errors

- `identity_not_found` — no such person, or you may not look them up.

## Examples

```json
{"op": "user", "subject": "alice"}
```

```json result
{"subject": "alice", "display_name": "Alice Doe", "status": "active",
 "groups": [{"name": "staff", "via": "support"}, {"name": "support", "via": "direct"}]}
```
