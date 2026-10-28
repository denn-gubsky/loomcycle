---
name: Identity/users
description: "Identity op=users — search the people of your tenant by name prefix, group, status or activity, a page at a time."
---
`users` lists the people of your tenant, sorted by subject id. Every row has
the subject and display name. Status and groups are added to a row when you
may read that person (always for yourself).

## Arguments

- `query` — a prefix of the subject id or of the display name, any case.
- `group` — only the members of this group, at any depth of nesting. Needs
  the right to read the group's members (as `op=group` shows them); a group
  you may not read reads as not found.
- `status` — `active`, `disabled` or `pending`.
- `active_only` — only people who have started a run, with
  `running_count`, `total_count` and `last_started_at`. Someone who runs
  without being registered appears here with no display name.
- `limit` — rows per page, 1 to 200 (default 50).
- `cursor` — the `next_cursor` of the previous page, passed back as it came.

`status` and `active_only` need a grant to read the people of the tenant.

## Returns

`{users: [{subject, display_name?, status?, groups?, running_count?,
total_count?, last_started_at?}], next_cursor?}`.

## Examples

The members of the support group whose name starts with "al":

```json
{"op": "users", "query": "al", "group": "support"}
```

```json result
{"users": [{"subject": "alice", "display_name": "Alice Doe", "status": "active",
 "groups": [{"name": "support", "via": "direct"}]}]}
```
