---
name: Identity/group
description: "Identity op=group — one group: its description, members, member groups and the groups it is nested in."
---
`group` returns one group of your tenant: its `description`, its
`external_id` when an identity provider manages it, its direct `members`
(`{kind, id}`, kind `user` or `group`), its member `groups`, and
`contained_in` — every group it is nested in, at any depth.

A caller allowed to read groups sees all of it. A member without that grant
may read a group they are in, without the members (`members_withheld`). Any
other group reads as not found.

## Arguments

- `name` — the group's name, as `op=groups` lists it.

## Returns

`{name, description?, external_id?, contained_in, members?, groups?,
members_withheld?}`.

## Errors

- `identity_not_found` — no such group, or you may not read it.

## Examples

```json
{"op": "group", "name": "support"}
```

```json result
{"name": "support", "description": "Tier 1", "contained_in": ["staff"],
 "members": [{"kind": "user", "id": "alice"}], "groups": []}
```
