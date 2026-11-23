---
name: Workspace/run_cell
description: "Workspace op=run_cell — run Python or Node code in an interpreter that keeps its variables between calls, as a notebook does."
---
`run_cell` runs code in an interpreter that stays alive in the workspace and
keeps its variables between calls. Load a file once, then ask it questions
cell by cell, rather than starting a new process with `exec` each time.

## Arguments

- `name` (required) — the workspace.
- `code` (required) — the code, up to 1 MiB. The value of its last
  expression is returned.
- `kernel` — `python` (the default) or `node`. Each workspace has one of
  each, apart: a Python variable is not seen from Node.
- `timeout_s` — seconds before the cell is interrupted. The default and the
  ceiling are the operator's, as for `exec`.

## Returns

`{kernel, execution_count, output, value?, error?, pictures?, truncated?,
interrupted?, timed_out?, kernel_killed?, kernel_died?, kernel_restarted?}`.

- `output` is what the cell printed, stdout and stderr in the order written
  (in Python, a subprocess's output too).
- `value` is the last expression's value as text (`repr` in Python,
  `util.inspect` in Node); absent when there is none.
- `error` is the traceback of an exception the cell raised.
- `pictures` names the figures a Python cell left open (matplotlib): each is
  saved as a PNG under `.loom/cells/` in the workspace.
- `output`, `value` and `error` together are kept up to 1 MiB (`truncated`).

A cell that raised, timed out or lost its kernel is an error result with the
same fields.

## Stopping a cell

At its timeout the cell is interrupted and the interpreter's variables are
kept (`timed_out`, `interrupted`). A call inside native code may ignore the
interrupt; after a grace period (the operator's, 5 seconds by default) the
interpreter is killed (`kernel_killed`) and its variables are lost.

## The interpreter's state is not durable

The interpreter dies with its kernel being killed, with a crash, with its
workspace, and when the worker restarts. The next cell then runs in a clean
interpreter and says `kernel_restarted: true`: reload your data rather than
trust variables that are gone. Files the cells wrote are still there.

## What is different from a notebook

- `input()` fails at once: there is nobody to answer.
- Python: the workspace's `.venv` is used when it has one, so packages you
  installed there with `exec` are importable.
- Node: `let`, `const` and top-level `await` work across cells, and
  `require` resolves from the workspace. `await import()` does not work in a
  cell; use `await loomImport("package")`.
- Cells of one interpreter run one at a time; a second call waits for the
  first.

## Errors

- `kernel_unavailable` — the worker has no such interpreter.
- `workspace_over_quota` — the workspace holds more than its disk quota;
  remove files first, as for `exec`.
- `not_granted` — a cell needs the `exec` grant.

## Examples

Load data once:

```json
{"op": "run_cell", "name": "analysis", "code": "import csv\nrows = list(csv.DictReader(open('sales.csv')))\nlen(rows)"}
```

```json result
{"kernel": "python", "execution_count": 1, "output": "", "value": "1204"}
```

Then use it in the next cell:

```json
{"op": "run_cell", "name": "analysis", "code": "sum(float(r['amount']) for r in rows)"}
```

```json result
{"kernel": "python", "execution_count": 2, "output": "", "value": "48211.5"}
```

In Node:

```json
{"op": "run_cell", "name": "site", "kernel": "node", "code": "const pkg = JSON.parse(require('fs').readFileSync('package.json', 'utf8')); pkg.name", "timeout_s": 30}
```

```json result
{"kernel": "node", "execution_count": 1, "output": "", "value": "'site'"}
```
