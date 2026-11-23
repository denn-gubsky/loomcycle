---
name: Workspace/reset_kernel
description: "Workspace op=reset_kernel — stop a workspace's interpreter so the next run_cell starts clean."
---
`reset_kernel` stops the workspace's interpreter of one kind. Its variables
are gone; the files in the workspace stay. The next `run_cell` starts a
clean interpreter, and does not say `kernel_restarted`, since you asked.

## Arguments

- `name` (required) — the workspace.
- `kernel` — `python` (the default) or `node`.

## Returns

`{kernel, stopped}`. `stopped` is false when no interpreter of that kind
was running.

## Errors

- `not_granted` — resetting needs the `exec` grant, as running a cell does.

## Examples

```json
{"op": "reset_kernel", "name": "analysis"}
```

```json result
{"kernel": "python", "stopped": true}
```
