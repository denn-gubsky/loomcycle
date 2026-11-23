---
name: Workspace/start
description: "Workspace op=start — run a command that keeps running after the call (a server) in a workspace, and get its id."
---
`start` runs a shell command in the workspace that keeps running after the
call returns: a web server, a watcher, anything that does not end by itself.
Use `exec` for a command that ends; use `start` for one that must stay up.

The call waits half a second. A server that ended in that time (a typo, a
port already in use) comes back as an error with its output, so you see
why at once.

## Arguments

- `name` (required) — the workspace.
- `command` (required) — a shell command line, run with `/bin/sh -c`, its
  standard error joined to its standard output.
- `cwd` — the directory to start in, relative to the workspace.
- `port` — the port the server listens on. It is recorded with the server
  and shown by `logs`.

## Returns

`{server, command, port, running, started_at}`. `server` (`srv-1`, `srv-2`,
...) is the id `stop` and `logs` take.

## Limits

- At most 4 servers run in one workspace at once; stop one to start another.
- The latest 256 KiB of a server's output is kept.
- A server ends with `stop`, when the workspace is released, when your run
  ends, and when its worker stops. It is never in a snapshot.
- Nothing outside the workspace reaches a server's port.

## Errors

- `not_granted` — servers need the `exec` grant, as commands do.
- `server_limit` — the workspace runs 4 servers already.
- `refused` — the worker's operator lists the commands it runs, and this
  one is not listed (or uses `;`, `&`, `|` and the like).
- `workspace_over_quota` — the workspace is over its disk quota; remove
  files first.

## Examples

```json
{"op": "start", "name": "site", "command": "python3 -m http.server 8000", "port": 8000}
```

```json result
{"server": "srv-1", "command": "python3 -m http.server 8000", "port": 8000, "running": true, "started_at": "2026-11-23T20:00:00Z"}
```
