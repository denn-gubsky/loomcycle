---
name: Identity/groups
description: "Identity op=groups — list the groups of your tenant you may read, by name prefix or by the groups one person is in."
---
`groups` lists the groups of your tenant, sorted by name, with their
descriptions. A group you may not read is left out of the list (a member
without a grant to read groups sees the groups they are in).

## Arguments

- `prefix` — a prefix of the group name.
- `member` — only the groups this person is in. Needs the right to read that
  person (always yours).
- `limit` — rows per page, 1 to 200 (default 50).
- `cursor` — the `next_cursor` of the previous page.

## Returns

`{groups: [{name, description?}], next_cursor?}`.

## Examples

```json
{"op": "groups", "prefix": "sup"}
```

```json result
{"groups": [{"name": "support", "description": "Tier 1"}]}
```
