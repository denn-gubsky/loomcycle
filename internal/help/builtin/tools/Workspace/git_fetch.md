---
name: Workspace/git_fetch
description: "Workspace op=git_fetch — bring a git workspace's forge branches in as origin/*, without touching your files."
---
`git_fetch` fetches the repository of a git workspace (one opened with
`repo`) and makes its `origin/<branch>` refs the forge's branches. Your
branches, your index and your files are left as they are; merge or rebase
with `exec` when you want the new commits. A branch the forge deleted
leaves `origin/*` too. The credential is the one the workspace was opened
with, resolved again for this call.

## Arguments

- `name` (required) — the git workspace.

## Returns

`{name, op, up_to_date, updated}`. `updated` names the `origin/*` refs
that moved, appeared or went.

## Errors

- `not_a_git_workspace` — the workspace was not opened with `repo`.
- `git_remote_not_allowed` — your git_remote does not include fetch.
- `host_not_allowed`, `acl_denied`, `credential_url_not_allowed` — the
  operator's host list, your user's access-control rules or the
  credential's own URL list refuse the repository.
- `auth_failed`, `repository_not_found`, `fetch_failed`, `timeout` — the
  forge refused or the fetch failed; the forge's words go to the
  operator's log only.

## Examples

```json
{"op": "git_fetch", "name": "app"}
```

```json result
{"name": "app", "op": "git_fetch", "up_to_date": false, "updated": ["refs/remotes/origin/main"]}
```
