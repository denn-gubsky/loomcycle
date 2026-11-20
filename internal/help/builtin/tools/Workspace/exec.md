---
name: Workspace/exec
description: "Workspace op=exec — run a shell command in a workspace and get its exit code and output."
---
`exec` runs a command line with `/bin/sh -c` in the workspace, as the
worker's user, with no isolation. The environment holds only `PATH`.

## Arguments

- `name` (required) — the workspace.
- `command` (required) — the shell command line.
- `cwd` — the directory to start in, relative to the workspace.
- `timeout_s` — seconds before the command, and everything it started, is
  killed. The default and the ceiling are the operator's (120 and 900 unless
  changed).

## Returns

`{exit_code, output, truncated?, timed_out?}`. `output` is stdout and stderr
together, kept up to 1 MiB (`truncated` then). A non-zero exit or a timeout
is reported as an error result, with the same fields, so you can read why.

## Errors

- `refused` — the operator lists which programs a command may start with, and
  this one is not listed (or uses `;`, `|`, `>` and the like).
- `invalid_argument` — `timeout_s` over the ceiling, or `cwd` outside the
  workspace.
- `worker_lost` — the worker went away; whether the command ran is unknown.

## Examples

Run a script the workspace holds:

```json
{"op": "exec", "name": "analysis", "command": "python3 report.py", "timeout_s": 60}
```

```json result
{"exit_code": 0, "output": "rows: 1204\n"}
```
