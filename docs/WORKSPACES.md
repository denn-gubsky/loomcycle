# Workspaces

A **workspace** is a directory on a *workspace worker*, a process apart from the runtime, where an agent runs shell commands and works on files through the `Workspace` tool. It is for work that needs a real shell and real toolchains (Python, Node): run a script, build something, test it, read the result. This guide covers what workspaces are in this release (RFC GW, phase 1), how to run a worker, the agent grant, the tool, its limits, and what its refusals mean.

## What a workspace is, and what it is not

**What it is.** An agent granted `workspace` opens a workspace by name, runs commands in it (`exec`), reads and changes its files (`read`, `write`, `edit`, `glob`, `grep`) and may `release` it. A workspace belongs to the **run that opened it**: no other run sees it, not a sub-agent of that run, not a sibling of the same tree, not another run of the same user. It is deleted, files and all, when that run ends, however it ends (completed, failed, cancelled). A run that is paused or parked waiting for input keeps it. If the runtime dies mid-run, a sweeper and the worker's next attach remove what is left.

**What it is not, yet.** Phase 1 has the `host` engine only, and leaves several things to later phases:

- **No isolation.** On the `host` engine a command runs as the worker's own OS user. It can read and write anything that user can, including the worker's token, and in `local` mode the runtime's data directory, config and secrets files and environment. It is not a sandbox. The container engine, which isolates, is a later phase.
- **No managed network.** Phase 1 grants or routes no network for a workspace. On the `host` engine nothing blocks it either: a command has whatever network the worker's machine has. Do not rely on either.
- **No file transfer** other than `write` and `read`. The `File` tool does not reach workspaces yet; copy what you need out with `read` before the run ends.
- **No disk quota.** A workspace can fill the worker's disk; `workspace.max_per_run` and `max_per_tenant` bound how many there are, not their size.
- **No durable, git or shared workspaces.** Every workspace is ephemeral, empty when opened and gone when its run ends, and owned by one run.
- **Not on loomcycle's MCP server.** A workspace is pinned to a run, and an MCP session has none.

## Running a remote worker

A remote worker is `loomcycle worker --role workspace` on a machine of its own, attached to the runtime's worker endpoint.

On the runtime, in its yaml:

```yaml
worker_endpoint:
  listen: 0.0.0.0:7788     # off loopback this needs worker_endpoint.tls (or insecure: true on a private network)
workspace:
  mode: remote
```

Mint the worker a token whose **only** scope is `workers:attach`, bound to the tenant it serves:

```sh
loomcycle operator-token create --name ws-acme --tenant acme --subject ws-acme --scopes workers:attach
```

The runtime refuses the legacy `LOOMCYCLE_AUTH_TOKEN`, any `substrate:admin` token, a token with other scopes and a person's sign-in credential. A worker serves only its token's tenant. An admin can mint a **shared** token (`--worker-shared`) that serves every tenant, but a shared worker must run an isolating engine, and on the `host` engine it is refused at attach: until the container engine arrives, run one tenant-bound worker per tenant.

On the worker's machine:

```sh
LOOMCYCLE_WORKER_DISPATCHER=runtime.internal:7788 \
LOOMCYCLE_WORKER_TOKEN=<the workers:attach token> \
LOOMCYCLE_WORKSPACE_ROOT=/var/lib/loomcycle/workspaces \
loomcycle worker --role workspace
```

The worker reads:

| Variable | Default | Meaning |
|---|---|---|
| `LOOMCYCLE_WORKER_DISPATCHER` | — | The runtime's worker endpoint (`host:port`, `unix:///path`, `dns:///name:port`); or `--dispatcher`. |
| `LOOMCYCLE_WORKER_TOKEN` | — | The `workers:attach` token. |
| `LOOMCYCLE_WORKSPACE_ROOT` | — (required) | Where it makes its workspaces, one directory each, mode 0700. |
| `LOOMCYCLE_WORKSPACE_ENGINE` | `host` | The only engine in this release. |
| `LOOMCYCLE_WORKSPACE_CAPACITY` | 8 | The workspaces it holds at most. |
| `LOOMCYCLE_WORKSPACE_IDLE_STREAMS` | 16 | Idle op streams it keeps open, 1–32. |
| `LOOMCYCLE_WORKSPACE_COMMANDS` | any | Comma-separated programs a command may start with, e.g. `python3,node`; a listed interpreter still runs any code. |
| `LOOMCYCLE_WORKER_TLS_CA`, `_CERT`, `_KEY`, `_SERVER_NAME`, `LOOMCYCLE_WORKER_INSECURE` | — | TLS to the endpoint, as for conversion workers. |

A worker is known by its token's grant and its name (`--id`, by default the host name): `tenant:<tenant>/<name>`. Keep the name the same across restarts, or the runtime will read every workspace on its disk as unknown and remove it. The worker reports `python` and `node` when it finds them on its `PATH`; a workspace opened without a `toolchain` goes only to a worker with both. [WORKERS.md](WORKERS.md#workspace-workers) and [CONFIGURATION.md](CONFIGURATION.md#workspace-workers-workspace) have the full settings.

## Local mode

For a single trusted machine, `workspace.mode: local` has the runtime start one worker itself:

```yaml
workspace:
  mode: local
  local:
    tenant: acme           # optional: the one tenant it serves (default: see below)
    root: /srv/loomcycle/workspaces   # default <LOOMCYCLE_DATA_DIR>/workspaces
```

The runtime starts `loomcycle worker --role workspace` as its child on a Unix socket in a private (0700) directory. The child presents a random token made for this boot, which the runtime accepts on that socket only, never on the network endpoint; it needs no worker endpoint and no minted token. The child is restarted with backoff if it exits, exits itself if the runtime dies, and is stopped at shutdown. Its workspaces live under the root, so after a crash the next start reconciles them. It takes `LOOMCYCLE_WORKSPACE_CAPACITY`, `_IDLE_STREAMS` and `_COMMANDS` from the runtime's environment when set, and nothing else of it; its toolchains are whatever the host has.

**It runs every command as the runtime's own user, with no isolation.** The child's environment is scrubbed and a command sees only `PATH`, but it runs as the same user: it can read the runtime's data directory and database, its config and secrets files, and on Linux the runtime's environment through `/proc`, provider keys included. The boot log says so, and so does `loomcycle doctor`. Use local mode only where you trust every agent that may use it.

The local worker serves **one tenant**. With `workspace.local.tenant` unset it serves the tenant a run on this install carries when its request names none, worked out at boot (the boot log and `loomcycle doctor` name it and say why):

- **Open mode** (no `LOOMCYCLE_AUTH_TOKEN`, no config principals): a run keeps the `tenant_id` its request names, and one that names none has the empty tenant. The local worker serves the empty tenant, so a run needs no `tenant_id` at all. Only the local worker ever serves the empty tenant; a network worker never does.
- **The legacy `LOOMCYCLE_AUTH_TOKEN`**: every run is of tenant `default`, whatever its request names, and the local worker serves `default`.
- **Config principals without the legacy token**: their tenant when they all share one; with several, `default`, and you should write `workspace.local.tenant`.

Tokens minted into the store are not seen at boot. If your runs carry a minted token's tenant (including once an admin token has turned an open-mode install into an authenticated one), write `workspace.local.tenant`.

`mode: local+remote` runs both: the local worker for its tenant, and remote workers on the endpoint.

## The agent grant

An agent reaches workspaces only with `Workspace` in its `tools` and a `workspace` grant:

```yaml
agents:
  builder:
    tools: [Workspace]
    workspace:
      ops: [read, write, exec]   # any of read, write, exec
      open: true                 # may open workspaces of its own
      engine: host               # least isolating engine it accepts; unset = any
```

`exec` needs `exec`; `write` and `edit` need `write`; `read`, `glob`, `grep`, `list` and `status` need `read`; `open` needs `open: true`. A sub-agent holds only what both it and its parent hold, before and after a resume. The grant is not per-run overridable, and an agent a tenant member authors may not set it. A grant on an agent without the tool, or while `workspace.mode` is off, does nothing; the boot log and `loomcycle doctor` name such agents.

## The tool

One tool, `Workspace`, with an `op`:

| op | What it does | Arguments |
|---|---|---|
| `open` | open (or return) a workspace of this run | `name`, `toolchain` (`python` or `node`; omit for a worker with both) |
| `list` | this run's workspaces | — |
| `status` | its state, and whether its directory is on the worker | `name` |
| `release` | delete it now | `name` |
| `exec` | run a shell command in it (`/bin/sh -c`) | `name`, `command`, `cwd`, `timeout_s` |
| `read`, `write`, `edit`, `glob`, `grep` | the file tools' operations, relative to the workspace | as the Read, Write, Edit, Glob and Grep tools, without `volume` |

`..`, an absolute path elsewhere and a symlink out of the workspace are refused, on the worker. An argument an op does not take is refused.

A worked example: an agent writes a script, runs it and reads what it wrote.

```json
{"op": "open", "name": "py", "toolchain": "python"}
```
```json
{"name": "py", "state": "ready", "engine": "host"}
```
```json
{"op": "write", "name": "py", "path": "square.py",
 "content": "import sys\nn = int(sys.argv[1])\nopen('out.txt', 'w').write(str(n * n))\nprint('squared', n)\n"}
```
```json
{"op": "exec", "name": "py", "command": "python3 square.py 12"}
```
```json
{"exit_code": 0, "output": "squared 12\n"}
```
```json
{"op": "read", "name": "py", "path": "out.txt"}
```

The read returns `144`. When the run ends, the workspace is deleted. A command that exits non-zero, or times out (`"timed_out": true`), is returned as a failed call with its output, as the Bash tool does.

## Limits

| Setting | Default | What it bounds |
|---|---|---|
| `workspace.max_per_run` | 4 | Workspaces one run holds at once. |
| `workspace.max_per_tenant` | 0 (unlimited) | A tenant's workspaces across its runs. |
| `workspace.exec.default_timeout_s` | 120 | A command that names no `timeout_s`. |
| `workspace.exec.max_timeout_s` | 900 | The longest `timeout_s` a command may ask for. |
| `workspace.lost_after_s` | 300 | How long a worker may be away before its workspaces are marked `lost`. |
| worker capacity | 8 | Workspaces a worker holds (`LOOMCYCLE_WORKSPACE_CAPACITY`). |

A command and everything it started is killed at its timeout. Its output is kept up to 1 MiB. `write` takes a file of up to 1 MiB; `read` returns what the Read tool would.

## Troubleshooting

**Refusals.** Every refusal is `{"error": <class>, "detail": ...}`:

| Class | Meaning | What to do |
|---|---|---|
| `workspaces_off` | `workspace.mode` is off, or the runtime has no store. | Set `workspace.mode`. |
| `not_granted` | The agent's grant does not allow the op. | Widen its `workspace` grant (Context op=self shows it). |
| `no_worker` | No attached worker serves the run's tenant with that toolchain and engine. A run with no tenant (open mode, no `tenant_id`) is served only by a local worker whose tenant resolved to the empty one. | Check the worker attached (its log, `loomcycle_workspace_workers`), its tenant (the boot log and `loomcycle doctor` name the local worker's), and that it reported the toolchain. |
| `at_capacity` | Every worker that could take it is full. | Release workspaces, or raise the worker's capacity. |
| `limit` | The run or the tenant holds as many workspaces as allowed. | `release` one, or raise `max_per_run` / `max_per_tenant`. |
| `not_found` | This run has no workspace of that name. Another run's workspace is reported the same way. | Open it in this run. |
| `not_ready` | The workspace is opening, being released, or `lost`. | See `lost` below. |
| `lost` | The workspace's directory is no longer on its worker. | Open a new one. |
| `refused` | The worker will not run the command (`LOOMCYCLE_WORKSPACE_COMMANDS`). | Use a listed program, or widen the list. |
| `invalid_argument` | The runtime refused the call: an unknown op, an argument the op does not take, a missing one, a bad timeout. | Fix the call. |
| `invalid`, `failed` | The worker refused the arguments (a path or `cwd` out of the workspace, a malformed argument) or a file system operation failed. | Read the detail. |
| `busy`, `output_too_large` | The worker had no free stream in time; the op sent more output than its bound. | Retry; narrow the op. |
| `cancelled` | The run was cancelled before the worker answered. | — |

**`worker_lost`.** The worker went away during the call (killed, restarted, its connection dropped). Whether the op took effect is unknown, and the workspace may be gone. The run itself goes on; the agent can check with `status`, or open a new workspace. `worker_revoked` is the same, when the operator retired or re-bound the worker's token.

**`lost` workspaces.** When no replica has seen a worker for `workspace.lost_after_s`, its workspaces are marked `lost`. If the worker comes back while the owning run is still going, the workspace is put back in service; if the run has ended, it is removed. A lost workspace is never removed on a guess: the directory goes only when its worker says so.

**A worker that keeps reconnecting.** A token the runtime cannot verify (unknown, revoked, retired, or checked while the store was down) is never final: the worker logs `could not verify this worker's token` and retries with backoff, and is served only once the token verifies again. A token that verifies but may not attach a worker (an admin token, the legacy secret, a token with other scopes, a shared token on the host engine) is refused for good, and the worker exits saying why. The runtime checks an attached worker's token again every minute, and detaches it when the token no longer holds.

**Leftover directories.** On attach the worker lists the workspaces on its disk, and the runtime tells it which to delete: those released while it was away, those it does not know, and lost ones whose run has ended. A sweeper every 60 seconds releases the workspaces of runs that ended without the release (a runtime killed mid-run).

**`loomcycle doctor`** reports `workspace.mode`, the local worker's tenant and why, and the agents whose grant does nothing. It reads the configuration only; which workers are attached is the running server's `loomcycle_workspace_workers` metric and its log (`workspace worker attached`).
