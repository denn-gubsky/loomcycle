---
name: Workspace/git_push
description: "Workspace op=git_push — push your git workspace's own branch, and only it, to the forge."
---
`git_push` pushes the workspace's own branch (the `branch` its open
answered) to the branch of the same name on the forge. Nothing else is
pushed: not another branch, not the forge's default branch, not a
deletion, and a tag only where the operator allows tags. The runtime
checks every push it relays, whatever sent it. Commit first, with `exec`
(`git commit`) in the workspace.

## Arguments

- `name` (required) — the git workspace.
- `force` — let the push rewrite your own branch on the forge (after an
  amend or a rebase). Default false: a push that is no fast-forward is
  refused before it is sent.

## Returns

`{name, op, up_to_date, updated}`. `updated` names the forge's ref that
moved.

## Errors

- `git_remote_not_allowed` — your git_remote does not include push.
- `git_push_disabled`, `host_not_allowed` — the operator allows no push,
  or none to this host.
- `push_rejected` — the forge (or, without `force`, the fast-forward
  check) refused the push; its words go to the operator's log only.
- `not_a_git_workspace` and the access errors of `git_fetch`.

## Examples

```json
{"op": "git_push", "name": "app"}
```

```json result
{"name": "app", "op": "git_push", "up_to_date": false, "updated": ["refs/heads/loom/app-3f9a1c"]}
```
