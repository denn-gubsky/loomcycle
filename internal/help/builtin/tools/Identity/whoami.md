---
name: Identity/whoami
description: "Identity op=whoami — yourself: the identity block Context op=self reports, for an agent that holds Identity."
---
`whoami` returns the person this run acts for, exactly as `Context op=self`
reports it in its `identity` block: subject, display name, status, groups
with how you are in each, and the access lists that apply. It is never
audited. A run started without a signed-in person gets a note saying so.

## Arguments

None besides `op`.

## Examples

```json
{"op": "whoami"}
```

```json result
{"tenant_id": "acme", "subject": "alice", "display_name": "Alice Doe", "status": "active",
 "groups": [{"name": "support", "via": "direct"}],
 "acl": {"mode": "enforce", "version": 41, "baseline": "builtin:member", "lists": [], "missing_acls": [], "removed_tools": []}}
```
