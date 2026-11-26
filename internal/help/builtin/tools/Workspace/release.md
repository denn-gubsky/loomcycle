---
name: Workspace/release
description: "Workspace op=release — end this run's hold on a workspace: an ephemeral one is deleted, a durable one is kept for your next run."
---
`release` ends your run's hold on a workspace now, rather than when your
run ends, and frees one of your run's workspace slots.

- An **ephemeral** workspace is deleted, files and all.
- A **durable** workspace is kept with its files, for a later run of yours
  to open again (`open {name, lifetime: "durable"}`). What runs in it ends:
  servers, interpreters, the session. Its shares end, its pages close, and a
  Documents checkout in it is forgotten (commit before you release).
- `discard: true` deletes a durable workspace instead. It also deletes a
  durable workspace of yours that no live run holds, by name. It needs your
  grant's `workspace.durable`, and a grant that covers the engine and the
  egress routes the workspace was opened with.

## Arguments

- `name` (required) — the workspace.
- `discard` — delete a durable workspace, files and all (default false).

## Returns

`{name, released: true}`, with `parked: true` for a durable workspace kept,
or `discarded: true`. If its worker is away, a deleted workspace's files go
when it comes back; the workspace is gone for you either way.

## Errors

- `not_found` — this run has no workspace of that name (with `discard`: nor
  is there a durable one of yours of that name).
- `held` — with `discard`: another live run of yours holds that durable
  workspace.
- `not_granted` — with `discard`: your grant has no `durable`, or does not
  cover the workspace's engine or routes. Nothing is deleted.

## Examples

```json
{"op": "release", "name": "analysis"}
```

```json result
{"name": "analysis", "released": true}
```
