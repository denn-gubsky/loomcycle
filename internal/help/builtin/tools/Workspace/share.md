---
name: Workspace/share
description: "Workspace op=share — let another run of your user read, or read and write, the files of one of your workspaces; never run commands in it."
---
`share` lets another run of your user reach one of your workspaces: your
parent, a child, a sibling, or a run of another tree your user started. It
names the workspace `<name>@<your agent id>` (the answer's `their_name`).

- `read` lets it read, `glob` and `grep` the files, and copy them out with
  `File op=copy`.
- `read_write` also lets it write files in: `File` writes and
  `Workspace write` and `edit`.
- A share never lets it run a command, start a server, release the
  workspace or share it on. Commands in a workspace are yours alone.

Your grant must allow sharing (`workspace.share`) and hold what the access
hands over: `read` for a read share, `read` and `write` for `read_write`. The
other run's own grant still applies: a share never gives it more than its
definition does. A run of another user is never a share's target.

A share ends when you `unshare`, when the workspace is released, or when
either run ends. Sharing again with the same run replaces its access. To
share with a child as it starts, use `Agent op=spawn` with `workspaces`.

## Arguments

- `name` (required) — your workspace.
- `with` (required) — the other run's agent id.
- `access` (required) — `read` or `read_write`.

## Returns

`{name, shared_with, access, their_name}`.

## Errors

- `not_granted` — your grant does not allow sharing, or not what `access`
  hands over.
- `invalid_share_target` — `with` is not a live run of your user (another
  user's run, one that ended, or an agent id nobody holds), or it is you.
- `not_found` — this run has no workspace of that name.
- `limit` — the workspace is shared with as many runs as allowed (16).
- `not_yet` — `connect` (exposed ports) is not available yet.

## Examples

```json
{"op": "share", "name": "build", "with": "a_7f3c2e", "access": "read"}
```

```json result
{"access": "read", "name": "build", "shared_with": "a_7f3c2e", "their_name": "build@a_19d0b4"}
```
