---
name: Workspace
description: "Workspace tool — open a directory on a workspace worker, run shell commands and file operations in it; it belongs to your run and is deleted when the run ends."
---
The `Workspace` tool gives you a **workspace**: a directory on a workspace
worker, a machine apart from the runtime, where you can run shell commands
(`exec`), run code in an interpreter that keeps its variables (`run_cell`),
read and change files (`read`, `write`, `edit`, `glob`, `grep`), and look at
pictures it holds (`view`).
Use it for work that needs a real shell and toolchains such as Python and
Node. For files you keep, use your volumes and the Read, Write and Edit tools.

## What you may do

Your agent's grant decides which operations work: `open` needs the grant to
open workspaces; `exec`, `run_cell` and `reset_kernel` need `exec`; `write`
and `edit` need `write`; `read`, `view`, `glob`, `grep`, `list` and `status` need
`read`; `share` and `unshare` need the grant to share. Anything else is
refused with `not_granted`. Context op=self shows your grant.

## Lifetime

A workspace belongs to the run that opened it, by its `name`. No other run
can see or use it, not even a sub-agent you start or another run of the same
user, unless you share it with them (see Workspace/share): to them it does
not exist. It is deleted, files and all, when your run
ends; a run that is paused or waiting for input keeps it. Copy out anything
you need to keep with `read` before you finish, or `release` it early.

## Not a sandbox

Commands run on the worker's host engine, as the worker's own user, with no
isolation from the worker's machine. The environment holds only `PATH`; a
command and everything it started is killed at its timeout; output is kept
up to 1 MiB.

## Operations

| op | What it does | Main arguments |
|---|---|---|
| `open` | open (or return) a workspace of this run | `name`, `toolchain` |
| `list` | this run's workspaces | — |
| `status` | its state, and whether its directory is on the worker | `name` |
| `release` | delete it now | `name` |
| `exec` | run a shell command in it | `name`, `command`, `cwd`, `timeout_s` |
| `run_cell` | run Python or Node code in an interpreter that keeps its variables | `name`, `code`, `kernel`, `timeout_s` |
| `reset_kernel` | stop the interpreter; the next cell starts clean | `name`, `kernel` |
| `read` | a file's text | `name`, `path`, `offset`, `limit`, `line_numbers` |
| `view` | look at a picture (PNG, JPEG, GIF, WebP) | `name`, `path` |
| `write` | create or replace a file whole | `name`, `path`, `content` |
| `edit` | replace exact text in a file | `name`, `path`, `old_string`, `new_string`, `replace_all` |
| `glob` | find files by name | `name`, `pattern`, `path` |
| `grep` | search file contents | `name`, `pattern`, `path`, `glob`, `output_mode`, ... |
| `share` | let another run of your user read, or read and write, its files | `name`, `with`, `access` |
| `unshare` | end a share | `name`, `with` |

The file operations take the same arguments as the Read, Write, Edit, Glob
and Grep tools, without `volume`, and paths are relative to the workspace.
`..`, an absolute path elsewhere and a symlink out of the workspace are
refused. An argument an operation does not take is refused.

## A workspace shared with you

Another run of your user may share one of its workspaces with you. `list`
shows it under `shared_with_me`, named `<name>@<its agent id>`. Pass that as
`name` to `read`, `view`, `glob` and `grep`, and with a `read_write` share to `write`
and `edit`. Nothing else takes it: a share never lets you run a command,
release the workspace or share it on. In `File` it is
`workspace:<name>@<agent id>:<path>`. Your own grant still applies.

## Errors

Every refusal is `{"error": <class>, "detail": ...}`:

- `not_granted` — your grant does not allow the operation.
- `not_found` — this run has no workspace of that name, or none of that
  name is shared with it.
- `read_only_share` — a write through a share that gives only `read`.
- `not_shared_op` — an operation a share never allows (a command, a status,
  a release, a share).
- `limit` — the run or the tenant holds as many workspaces as allowed.
- `no_worker`, `at_capacity` — no attached worker can take a new workspace.
- `worker_lost` — the worker went away during the call; whether it took
  effect is unknown, and the workspace may be gone.
- `workspaces_off` — this runtime serves no workspaces.
- `refused` — the worker will not run that command.
