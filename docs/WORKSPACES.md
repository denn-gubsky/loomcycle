# Workspaces

A **workspace** is a directory on a *workspace worker*, a process apart from the runtime, where an agent runs shell commands and works on files through the `Workspace` tool. It is for work that needs a real shell and real toolchains (Python, Node): run a script, build something, test it, read the result. The `File` tool moves files in and out of a workspace and converts them there, by reference, so a file's bytes never pass through the model. This guide covers what workspaces are in this release (RFC GW, phases 1 to 3), how to run a worker, the `container` engine that isolates a workspace's commands, the agent grant, the tool, moving and converting files, its limits, and what its refusals mean.

## What a workspace is, and what it is not

**What it is.** An agent granted `workspace` opens a workspace by name, runs commands in it (`exec`), reads and changes its files (`read`, `write`, `edit`, `glob`, `grep`) and may `release` it. With the `File` tool it copies files and folders in and out of it, converts files inside it, imports a Markdown file from it as a Document and renders a Document into it (see "Moving files" and "Converting files in a workspace"). A workspace belongs to the **run that opened it**: no other run sees it, not a sub-agent of that run, not a sibling of the same tree, not another run of the same user. It is deleted, files and all, when that run ends, however it ends (completed, failed, cancelled). A run that is paused or parked waiting for input keeps it. If the runtime dies mid-run, a sweeper and the worker's next attach remove what is left.

**What it is not, yet.** Several things are left to later phases:

- **No isolation on the `host` engine.** On `host` (the default, and the only engine of `local` mode) a command runs as the worker's own OS user. It can read and write anything that user can, including the worker's token, and in `local` mode the runtime's data directory, config and secrets files and environment. It is not a sandbox. The `container` engine isolates: see "The container engine".
- **No network routes.** On the `container` engine a session has no network at all; on `host` nothing blocks it, and a command has whatever network the worker's machine has. Package downloads through routes the operator declares come with egress, phase 4; until then, on the `container` engine, `pip install` and `npm install` of anything outside the image fail at once.
- **No hard disk limit under a command.** The disk quota stops what `File` and the `Workspace` tool write before a byte is written, but a command's own writes are only measured after it ends (see "The disk quota"), on either engine. One command can pass the quota while it runs; the next step is stopped.
- **No durable, git or shared workspaces.** Every workspace is ephemeral, empty when opened and gone when its run ends, and owned by one run. A reference into another agent's workspace (`workspace:<name>@<agent_id>:<path>`) comes with sharing, phase 8; checking a Document out into a workspace and committing it back, phase 7.
- **Not on loomcycle's MCP server.** A workspace is pinned to a run, and an MCP session has none: the MCP `file` tool refuses a workspace reference.

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

The runtime refuses the legacy `LOOMCYCLE_AUTH_TOKEN`, any `substrate:admin` token, a token with other scopes and a person's sign-in credential. A worker serves only its token's tenant. An admin can mint a **shared** token (`--worker-shared`) that serves every tenant; a shared worker must run the `container` engine, and on the `host` engine it is refused at attach.

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
| `LOOMCYCLE_WORKSPACE_ENGINE` | `host` | `host`, or `container` (next section). |
| `LOOMCYCLE_WORKSPACE_CAPACITY` | 8 | The workspaces it holds at most. |
| `LOOMCYCLE_WORKSPACE_IDLE_STREAMS` | 16 | Idle op streams it keeps open, 1–32. |
| `LOOMCYCLE_WORKSPACE_COMMANDS` | any | Comma-separated programs a command may start with, e.g. `python3,node`; a listed interpreter still runs any code. |
| `LOOMCYCLE_WORKER_TLS_CA`, `_CERT`, `_KEY`, `_SERVER_NAME`, `LOOMCYCLE_WORKER_INSECURE` | — | TLS to the endpoint, as for conversion workers. |

A worker is known by its token's grant and its name (`--id`, by default the host name): `tenant:<tenant>/<name>` or `shared/<name>`. Keep the name the same across restarts, or the runtime will read every workspace on its disk as unknown and remove it. On `host` the worker reports `python` and `node` when it finds them on its `PATH`, on `container` when its session image has them; a workspace opened without a `toolchain` goes only to a worker with both. [WORKERS.md](WORKERS.md#workspace-workers) and [CONFIGURATION.md](CONFIGURATION.md#workspace-workers-workspace) have the full settings.

## The container engine

With `LOOMCYCLE_WORKSPACE_ENGINE=container` a worker runs each workspace's commands in a container of its own, the workspace's **session** (phase 3). The worker drives a container daemon through its command line (`docker`, or `podman`); its session image is `LOOMCYCLE_WORKSPACE_CONTAINER_IMAGE` (required). The published one, `denngubsky/loomcycle-workspace:<version>` (each minor release after v1.165.0), carries Python 3 with pip and venv, Node.js with npm, git, the document and media helpers, and a fixed set of data, document and chart libraries ([WORKERS.md](WORKERS.md#the-workspace-session-image) lists them); use the tag of the runtime's own version.

```sh
LOOMCYCLE_WORKER_DISPATCHER=runtime.internal:7788 \
LOOMCYCLE_WORKER_TOKEN=<the workers:attach token> \
LOOMCYCLE_WORKSPACE_ROOT=/var/lib/loomcycle/workspaces \
LOOMCYCLE_WORKSPACE_ENGINE=container \
LOOMCYCLE_WORKSPACE_CONTAINER_IMAGE=denngubsky/loomcycle-workspace:<version> \
loomcycle worker --role workspace
```

**What a session is.**

- The workspace is mounted at **`/workspace`**, always the same path, and nothing else of the worker's disk: a command cannot see another workspace. A Python environment made in `/workspace` keeps working when the workspace is forked or restored later.
- **No network.** A session has no route anywhere: a connection or a name lookup fails at once, not after a timeout.
- **A read-only root.** A command writes into `/workspace` and the session's own `/tmp` (a size-capped tmpfs) only; what is in `/tmp` ends with the session.
- **No privilege.** Every capability dropped, `no-new-privileges`, and the worker's own uid, never root.
- **Limits** on memory (no swap past it), CPUs and processes.
- **Its own environment**: `HOME=/workspace`, the pip and npm caches under it, `TMPDIR=/tmp`. Nothing of the worker's environment, its token included, reaches a command.

A session starts at a workspace's first command and stays for the next ones, so what a command installs into the workspace (a virtual environment, `node_modules`) is there for the next. A session that is gone is started again at the next command: the files are kept, a running server and `/tmp` are not. A command past its timeout, or one the runtime stopped waiting for, is killed inside the session with everything it started; a process that leaves its process group lives until the workspace is released, which removes the session.

**Running it.** The worker will not run this engine as root: run it as a user that may use the container daemon (the `docker` group, or a rootless daemon), on a machine or VM given to it, since starting containers is close to root on the host. The sessions have no such right. At start the worker runs one throwaway session to check the daemon, the runtime and the image, reports the image's `python3` and `node` as its toolchains, and removes the sessions a previous run left. A worker that itself runs in a container and drives the host's daemon sets `LOOMCYCLE_WORKSPACE_CONTAINER_HOST_ROOT` to its root as the host sees it.

| Variable | Default | Meaning |
|---|---|---|
| `LOOMCYCLE_WORKSPACE_CONTAINER_IMAGE` | — (required) | The session image: `denngubsky/loomcycle-workspace:<version>`, or any image with `/bin/sh`. |
| `LOOMCYCLE_WORKSPACE_CONTAINER_RUNTIME` | the daemon's | `runc`, `runsc` (gVisor) or a Kata runtime: gVisor and Kata put a kernel boundary under each session. |
| `LOOMCYCLE_WORKSPACE_CONTAINER_CLI` | `docker` | The container command. |
| `LOOMCYCLE_WORKSPACE_CONTAINER_MEMORY_MB` | 2048 | A session's memory. |
| `LOOMCYCLE_WORKSPACE_CONTAINER_CPUS` | 2 | A session's CPUs. |
| `LOOMCYCLE_WORKSPACE_CONTAINER_PIDS` | 512 | A session's processes. |
| `LOOMCYCLE_WORKSPACE_CONTAINER_TMP_MB` | 256 | A session's `/tmp`, counted in its memory. |
| `LOOMCYCLE_WORKSPACE_CONTAINER_HOST_ROOT` | the root | The root as the daemon sees it. |

An agent that must only ever run on this engine sets `engine: container` in its grant; a shared worker must run it. The disk quota works as on `host` (measured after each command), and a workspace's files are still converted by the worker's conversion child, not in the session.

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

`exec`, `run_cell`, `reset_kernel`, `start`, `stop` and `logs` need `exec`; `write` and `edit` need `write`; `read`, `view`, `glob`, `grep`, `list` and `status` need `read`; `open` needs `open: true`. A sub-agent holds only what both it and its parent hold, before and after a resume. The grant is not per-run overridable, and an agent a tenant member authors may not set it. A grant on an agent without the tool, or while `workspace.mode` is off, does nothing; the boot log and `loomcycle doctor` name such agents.

## The tool

One tool, `Workspace`, with an `op`:

| op | What it does | Arguments |
|---|---|---|
| `open` | open (or return) a workspace of this run | `name`, `toolchain` (`python` or `node`; omit for a worker with both) |
| `list` | this run's workspaces | — |
| `status` | its state, and whether its directory is on the worker | `name` |
| `release` | delete it now | `name` |
| `exec` | run a shell command in it (`/bin/sh -c`) | `name`, `command`, `cwd`, `timeout_s` |
| `run_cell` | run code in an interpreter that keeps its variables between calls | `name`, `code`, `kernel` (`python` or `node`), `timeout_s` |
| `reset_kernel` | stop the interpreter; the next cell starts clean | `name`, `kernel` |
| `checkpoint`, `checkpoints`, `restore`, `delete_checkpoint`, `fork` | record the files, put them back, open a copy; see "Checkpoints and forks" | `name`, `label`, `checkpoint`, `as` |
| `view` | return a picture in it to the model as an image | `name`, `path` |
| `start` | run a command that keeps running after the call, a server (see Servers) | `name`, `command`, `cwd`, `port` |
| `stop` | end a server and what it started | `name`, `server` |
| `logs` | a server's latest output, or the list of servers | `name`, `server`, `tail_bytes` |
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

## The interpreter

`run_cell` runs code in an interpreter that stays alive in the workspace and keeps its variables between calls, as a notebook does (RFC GW §5.7). A workspace has one Python and one Node interpreter, apart from each other; `kernel` picks one (default `python`). It needs the `exec` grant, as a command does.

```json
{"op": "run_cell", "name": "py", "code": "import csv\nrows = list(csv.DictReader(open('sales.csv')))\nlen(rows)"}
```
```json
{"kernel": "python", "execution_count": 1, "output": "", "value": "1204"}
```

- **The answer.** `output` is what the cell printed, stdout and stderr in the order written (in Python a subprocess's output too); `value` the last expression's value as text; `error` the traceback of an exception. A Python figure left open (matplotlib, on the `Agg` backend) is saved as `.loom/cells/cell-<n>-<i>.png` in the workspace and named in `pictures`. Output, value and error together are kept up to 1 MiB (`truncated`). A cell that raised, timed out or lost its interpreter is a failed call with the same fields.
- **Stopping a cell.** At its timeout (`timeout_s`, bounded as `exec`'s) the cell is interrupted with SIGINT and the variables are kept (`timed_out`, `interrupted`). A call inside native code may ignore the interrupt: after `workspace.kernel.interrupt_grace_seconds` (default 5) the interpreter and its process group are killed (`kernel_killed`).
- **Not durable.** The interpreter dies with a kill, a crash, its workspace and a worker restart. The next cell runs in a clean one and says `kernel_restarted: true`, so the agent reloads its data rather than trusting variables that are gone; files stay. `reset_kernel` stops it on purpose and is not reported as a restart.
- **Python** runs the workspace's `.venv/bin/python` when there is one, so what was installed there is importable, else `python3`. `input()` fails at once.
- **Node** runs cells through the V8 inspector's REPL mode: `let`, `const` and top-level `await` persist across cells, and `require` resolves from the workspace. `await import()` does not work in that mode; `await loomImport("pkg")` does.
- **One cell at a time** per interpreter: a second call waits for the first.
- **The quota.** A cell is a command: it is refused with `workspace_over_quota` while the workspace is marked full, and the workspace is measured after it.
- **How it runs.** Each interpreter is a small driver of ours (one Python file, one Node file, carried in the binary and written into the worker's root for the `host` engine) that the worker starts through its engine and speaks JSON lines with. A cell runs inside the driver's process and can write to that stream itself; the worker reads it with a bounded reader and takes a frame only as the current cell's own output, under a fresh id per cell, so a forged frame changes nothing but that cell's result.

## Checkpoints and forks

`checkpoint` records a workspace's files on its worker, installed packages included, and answers an id; `restore` puts them back; `fork` opens a second workspace from a checkpoint or from the present files (RFC GW §5.4, P6).

```json
{"op": "checkpoint", "name": "py", "label": "packages installed"}
```
```json
{"id": "ck_3f9a1c0e5b7d2a64", "label": "packages installed", "bytes": 48213, "files": 37, "mechanism": "clone", "usage": {"used_bytes": 48213, "quota_bytes": 2147483648, "checkpoint_bytes": 48213}}
```
```json
{"op": "fork", "name": "py", "as": "py-b", "checkpoint": "ck_3f9a1c0e5b7d2a64"}
```

| op | What it does | Grant | Arguments |
|---|---|---|---|
| `checkpoint` | record the workspace's files | `write` | `name`, `label` |
| `checkpoints` | its checkpoints, newest first | `read` | `name` |
| `restore` | put the files back to a checkpoint | `write` | `name`, `checkpoint` |
| `delete_checkpoint` | remove a checkpoint and free its room | `write` | `name`, `checkpoint` |
| `fork` | open a new workspace of the run as a copy | `open` | `name`, `as`, `checkpoint` |

- **Files only.** An interpreter's variables and a running command are not in a checkpoint. `checkpoint` leaves them running; a command still writing may leave a file half written in the checkpoint. `restore` stops the workspace's interpreters and its session first (a server, a background command), and the next cell or command starts them new; the checkpoint stays, to restore again.
- **The quota.** A checkpoint counts its full size against the workspace's `disk_quota_mb`, beside the workspace's files, whatever the mechanism shares on disk: a clone or a reflink stops sharing as soon as either side is rewritten, so a checkpoint is never a way round the quota. A checkpoint that does not fit, and a restore whose files do not fit beside the checkpoints held, are refused with `workspace_over_quota`; `delete_checkpoint` makes room. A workspace holds at most `workspace.checkpoint.max_per_workspace` checkpoints (default 8; past it `checkpoint_limit`).
- **A fork is a workspace like any other:** the caller's own, on the original's worker (refused with `at_capacity` when that worker is full), with its own session and the operator's disk quota (a source that does not fit is refused, nothing made), counted against `max_per_run` and `max_per_tenant`. It starts with no checkpoints, no shares, no interpreter, and no record of Documents checked out into the original: check them out again to commit from the fork. A workspace shared with the run cannot be forked; an agent whose `workspace.engine` is stronger than the original's engine cannot fork it.
- **How the worker records one.** It probes its disk at start and picks the first mechanism that works, reported as the label `checkpoint:<mechanism>` and in every answer: `clone` (a whole-folder clone on APFS, macOS), `reflink` (each file shares its blocks: xfs with reflink, btrfs, bcachefs), else `copy` (each file copied; a checkpoint or a fork then takes its full size on disk and seconds for a large tree). Never hard links. A worker that serves fan-outs wants a disk with reflinks. Links are copied as links, never followed; a pipe, a socket or a device is left out and named in `skipped`.
- **Where checkpoints live.** In the worker's root, beside the workspaces (`.loomcycle-checkpoints/<workspace id>/`), never inside one: no command or file op reaches them. They go with their workspace, and a worker's start removes what an interrupted checkpoint left.
- **The `host` engine has no fixed path.** A fork's Python environment would still name the original's folder, so its `pip` would install into the original. On `host` the worker rewrites the paths in each environment's `pyvenv.cfg` and the text files of its `bin/`, and names them in the answer's `rewritten`. The `container` engine mounts every workspace at `/workspace`, so nothing needs it.

## Servers

`start` runs a command that keeps running after the call: a web server, a file watcher, anything that does not end by itself (RFC GW §5.3). `exec` stays what it is, one command and one result. Servers need the `exec` grant, as commands do; a share never reaches them.

```json
{"op": "start", "name": "site", "command": "python3 -m http.server 8000", "port": 8000}
```
```json
{"server": "srv-1", "command": "python3 -m http.server 8000", "port": 8000, "running": true, "started_at": "2026-11-23T20:00:00Z"}
```

- **The answer.** `server` is the id `stop` and `logs` take (`srv-1`, `srv-2`, ...). The call waits half a second: a server that ended in that time (a typo, a port in use) is a failed call carrying its output.
- **`stop {name, server}`** kills the server and every process it started (its process group); `stopped` is false when it had ended by itself.
- **`logs {name, server?, tail_bytes?}`** returns a server's latest output, standard output and standard error in the order written (default 64 KiB, at most 256 KiB), or, without `server`, lists the workspace's servers with their state.
- **Limits.** At most 4 servers run in one workspace at once; the latest 256 KiB of each one's output is kept. A server is a command for the quota: `start` is refused with `workspace_over_quota` while the workspace is marked full.
- **The command list.** A worker whose operator lists its commands (`LOOMCYCLE_WORKSPACE_COMMANDS`) holds `start` to the list exactly as `exec`: a command that `exec` refuses, `start` refuses.
- **When it ends.** With `stop`, with the workspace's release, when the owning run ends, when the worker removes the workspace at attach (released or marked lost while it was away), and when the worker stops. Nothing of a server survives a worker restart or a snapshot.
- **Who reaches its port.** In a `container` session no one but the session: the session has no network, and a server listens on its own loopback. Pages served to a user, and requests from an agent, come with exposure (RFC GW P9, later PRs). On the `host` engine every process of the worker shares one loopback, so a port is not isolated from the worker's other workspaces; the host engine is for a trusted single-tenant install.

## Looking at a picture

`view` returns a picture from the workspace to the model as an image, so an agent can look at what it made: a chart, a rendered page, a diagram (RFC GW §5.8). It needs the `read` grant and works on a workspace shared with the run through either kind of share, since it only reads.

```json
{"op": "view", "name": "py", "path": ".loom/cells/cell-3-1.png"}
```
```json
{"bytes": 48213, "name": "py", "path": ".loom/cells/cell-3-1.png"}
```

- **What it sends.** A PNG, JPEG, GIF or WebP, its type read from the file, not its name. A picture over 1568 pixels on its long side is scaled down first; one whose decoding would take too much memory, judged from its header, is refused. Scaling runs in a conversion worker when the runtime has any. The tool's text gains a line naming the picture's type, size and pixels.
- **Bounds.** The file may be at most 20 MiB (`too_large` otherwise); a picture as sent at most 3,750,000 bytes.
- **A model that cannot see images.** The picture is not sent and the call fails saying so; the run goes on. The same applies to any tool that returns a picture.
- **Kept with the run.** The picture is stored with the run's transcript, so a resumed run shows the model the same picture. The live event carries only the line naming it.
- **Not yet:** `screenshot`, which loads a page of the workspace's own server in a headless browser, arrives with servers (RFC GW P9) and a worker image that carries a browser.

## Moving files

The `File` tool reaches a workspace through a **reference**, `workspace:<name>:<path>`: a file or folder in a workspace this run opened (`workspace:<name>:` is its root). The bytes stream between the worker and the runtime, never through the model, which sees only where they went.

| op | On a workspace |
|---|---|
| `stat`, `ls` | size, type and sha256 of a file; the files of a folder |
| `copy` | a file or a folder between the inbox, a volume and a workspace, either way, or between two workspaces of the run |
| `rm`, `clear` | remove a file or a folder; empty a folder |
| `import` | build a Document from a Markdown file in the workspace |
| `render` | write a Document (or a Markdown or HTML file) into the workspace with `to` |
| `download`, `upload` | GET a URL into the workspace (`to`); send a file from it (`from`) |
| `extract`, `transcribe`, `unpack` | convert a file in the workspace, on its worker (next section) |

The `Crypto` tool's `from` and `to` take a workspace reference too. `keep` stays inbox to volume; `copy` is the general form.

**Grants and hooks.** A workspace reference needs the agent's `workspace` grant, not the `Workspace` tool and not the Read and Write grants that govern volumes: `read` to read from a workspace, `write` to write into it or remove from it. The operator's `Workspace` pre-hooks judge every use as a `Workspace` call `{op: read|write|remove, name, path, via: "File"}` (or `via: "Crypto"`), on the final path, after a folder target is expanded. For a folder copy every file's own source and target path is judged before anything is written, and one refusal refuses the whole copy. Only the run that opened the workspace reaches it: another run's workspace answers "not found", and a run without the grant, or with `read` only and a write target, is refused before a byte moves.

**Copy.** A `to` ending in `/`, or naming a folder that exists, gets the source's name inside it; otherwise `to` is the new name. A folder target in a volume must already exist.

- **A folder is copied all or nothing.** It is listed first and refused before anything is written when it holds more files or bytes than the operator allows (`workspace.copy.max_files`, default 2000; `workspace.copy.max_bytes`, default 1 GiB), when one of its files passes the per-file limit (`LOOMCYCLE_FILE_MAX_BYTES`), or when the target's quota or the inbox's budget would be passed. The refusal (`copy_limit: …`) names the bound. Into a workspace or a volume the folder is written to a hidden staging folder and renamed into place at once, so a failed copy leaves nothing; into the inbox each file is captured and, on failure, the ones already written are forgotten. Links and special files are not copied, and the answer names them in `skipped`.
- **`overwrite`** is off by default: a copy onto an existing file or folder is refused. With `overwrite: true` a file is replaced at once, never left half written, and a folder is merged into file by file.
- **The inbox is never overwritten.** It is a store of captures: a name already taken there gets a numbered variant (`report-2.pdf`), `overwrite` or not, and the answer's `ref` says which.
- **Two workspaces of the run on one worker** are copied by that worker (`same_worker: true`), exactly the files the runtime listed and checked; on two workers the bytes pass through the runtime.

**`rm` and `clear`.** `rm workspace:<name>:<path>` removes a file or a folder with what it holds; `clear` with `path` empties a folder. Both need `write`, give the space back to the workspace's quota, and never remove the workspace's root: release the workspace with `Workspace op=release`.

**Import.** A Document imported from a workspace records where it came from, and the agent cannot set or change it: `captured_from: "workspace:<name>"`, the workspace's id (`workspace_id`), the run (`run_id`), the file's name and sha256. It is tagged `source/workspace` and `untrusted`. Pictures the Markdown links by a relative path inside the same workspace are attached as image chunks (at most 64, each within the image limit); any other link stays text.

The data pass RFC GW §11 describes is these ops in a row: render a Document into a workspace as Markdown, run a script over it, import the result as a new Document, render that as a PDF into a volume.

```json
{"op": "render", "document": "56f3…", "format": "md", "to": "workspace:data:in/source.md"}
{"op": "exec", "name": "data", "command": "python3 rewrite.py"}
{"op": "import", "path": "workspace:data:out/result.md", "doc_path": "/reports/result"}
{"op": "render", "document": "bb4f…", "format": "pdf", "to": "volume:reports:result.pdf"}
```

(The second call is the `Workspace` tool; the others are `File`.)

### The disk quota

Each workspace has a disk quota, `workspace.disk_quota_mb` (default 2048, up to 16 TiB), sent to the worker when the workspace is opened and kept by it. The worker counts a workspace's bytes by walking it the first time it is written in a session, bounded in time (past the bound the count is an estimate, and its refusals say so), and keeps the count as files are written and removed.

- **Before a write.** A write that would pass the quota is refused with `workspace_over_quota` before a byte is written: a `File` copy, render, download, unpack or extract into the workspace, a folder commit, a `Crypto` output, and the `Workspace` tool's `write` (its content's size) and `edit` (what it adds). A refused write leaves no partial file.
- **After a command.** What a command writes is not stopped while it runs. After every `exec` the worker measures the workspace again; one found over its quota is marked **full**. While it is full, further writes (by `File`, `Crypto` and the `Workspace` tool) and the next `exec` are refused with `workspace_over_quota`, and reading, listing, `rm` and `clear` still work, so the agent can clean up and go on. The mark clears when a measurement finds the workspace under its quota again: after an `rm` or a `clear`, or at the next check.

## Converting files in a workspace

A file in a workspace is converted in the workspace's own space, on its worker, never pulled across to the runtime to be converted (RFC GW D34). A file in the inbox or a volume is still converted by the runtime's conversion workers (RFC GO), as before.

**Where it runs.** The workspace worker starts a **conversion child** of its own at start: the same binary run as a conversion worker, on a private socket, with a memory limit (`workspace.convert.memory_limit_mb` for the local worker, `LOOMCYCLE_WORKSPACE_CONVERT_MEMORY_LIMIT_MB` on a remote one; default 1024). It is never the worker's supervisor and never the agent's command session. A file that crashes the child fails that one call; the worker and its workspaces go on, and the child is started again.

**What runs there.**

- `extract` of a file in a workspace: its text layer and pictures. With `to` a folder of a workspace, the Markdown and its `images/` are written there and the result carries no excerpt; on the same worker neither the file nor the text leaves it. Without `to`, the Markdown goes to the inbox, with an excerpt.
- `unpack` of an archive in a workspace, into a folder of a workspace (all or nothing, every file counted against the quota and judged by the `Workspace` hooks), a volume or the inbox, under the usual unpack limits.
- `render` of an HTML file in a workspace: the HTML is read on the worker and only its Markdown comes back to be typeset.
- the byte work of OCR and transcription (below).

A Document rendered into a workspace is still typeset where the runtime converts files: the runtime builds the bundle from the store, and only the finished file streams into the workspace. `import` and a Markdown `render` read the Markdown through a bounded stream (32 MiB); nothing is converted.

**OCR and transcription are split.** They call a model with a provider key, and a key never reaches a worker. The worker does the byte work in its child and sends back only what the model reads; the runtime calls the model, under the run's page and minute caps, and records the usage.

- `extract` with OCR: the worker extracts the text layer and cuts out (or renders to PNG) only the pages that need OCR; those page images go to the runtime, which calls the OCR model. With the Markdown in a workspace on the same worker, only the OCR text comes back to be written into it.
- `transcribe`: the worker converts the recording to the audio the speech-to-text model takes (ffmpeg); only that audio reaches the runtime, which calls the model and writes the transcript where `to` says.

No op the runtime sends a worker names the provider or the model in its arguments, and no provider key or credential ever reaches it. The provenance line of Markdown written into a workspace names the engine, the provider and the model, as one in the inbox does: a model's name is not a secret.

**Helper programs.** The child reads every format loomcycle reads in pure Go. Helper programs are used when they are on the worker's `PATH`: poppler (`pdftoppm`, to render a page for an OCR model that reads images), ffmpeg (recordings), DjVuLibre (DjVu), Typst. The worker reports what its child has when it attaches, and a conversion that needs a missing helper is refused with `conversion_helper_missing`, naming the program to install where the worker runs. On `host` workers that is whatever each machine has, so a file that converts on one worker can be refused on another; the loomcycle-worker image carries them all. OCR of a DjVu book in a workspace is not done yet: its pages that need it are reported; copy it into the inbox to read them.

**Quarantine.** A file that crashed the conversion child is refused, by its sha256, in that workspace for `conversion.quarantine_ttl_s`, with a message saying it crashed the converter; other files and other workspaces are unaffected. The worker checks the file still hashes to what the runtime checked before it converts it, and never follows a link.

## Sharing a workspace

A workspace belongs to the run that opened it. Its owner can share it with another run of the same user, so a reviewer or a tester can read what a builder made, or a helper can write results into it, without copying the files first.

```yaml
agents:
  builder:
    tools: [Workspace, Agent]
    workspace:
      ops: [read, write, exec]
      open: true
      share: true                # may share a workspace it owns
  reviewer:
    tools: [Workspace, File]
    workspace:
      ops: [read]                # reads what it is given; opens nothing
```

**Making a share.** `Workspace op=share {name, with, access}`: `with` is the other run's agent id (a parent, a child, a sibling, or a run of another tree of the same user); `access` is `read` or `read_write`. `op=unshare {name, with}` ends it. `Agent op=spawn` and each entry of `op=parallel_spawn` take `workspaces: [{name, access}]`, shared with the child before its first turn:

```json
{"op": "spawn", "name": "reviewer", "prompt": "Review the change in build.",
 "workspaces": [{"name": "build", "access": "read"}]}
```

**Using one.** The peer names the workspace `<name>@<owner agent id>`; its `Workspace op=list` shows it under `shared_with_me`. With `read` it may use `Workspace read`, `view`, `glob` and `grep` on it, and `File` ops that read from `workspace:<name>@<agent id>:<path>` (copy out, stat, ls, extract, import). `read_write` adds `File` writes into it and `Workspace write` and `edit`.

**What a share never allows:** `exec`, a release, a status, and sharing it on. Commands in a workspace are its owner's alone, so two agents never race in one session.

**Who may share with whom.**

- The owner needs `workspace.share` and the operations it hands over: `read` for a read share, `read` and `write` for `read_write`.
- The peer needs the `workspace` operation each use asks for, in its own grant: a share never gives an agent more than its definition does.
- The peer is a live run of the same user, in the same tenant. A run of another user, an ended run and an unknown agent id are refused alike; a run with no user id neither shares nor is shared with.
- A workspace is shared with at most 16 runs.

**How long it lasts.** A share is stored on the workspace's row, so every replica honours it and it survives a pause and a resume of either run. It ends with `unshare`, with the workspace's release, or when either run ends.

**Audit.** Each share, unshare and each tool call through a share writes an audit event (`workspace_share`, `workspace_unshare`, `workspace_share_use`) naming the workspace, both runs and the access.

Not yet: `connect` (a share's reach to a workspace's exposed ports) arrives with servers; a share asking for it is refused.

## Documents as a working tree

A set of Documents can be checked out into a workspace as Markdown files, changed there by a script or by hand, and committed back as new editions (RFC GW P7). A bulk edit of a knowledge base becomes a script and a dry run.

```json
{"op": "checkout", "under_path": "/kb/policies", "scope": "tenant", "to": "workspace:work:policies/"}
```

- **What is written.** Each Document you may read under `under_path` (or each one named by id in `documents`), at its Path name relative to it, as `<name>.md`: `/kb/policies/travel` is `travel.md`. The text is `Document op=export_md`'s round-trip form of the parts in force: each part's id, type and status in a `<!-- loom: … -->` line, the relations at the end. A part a later edition closed is not written.
- **The manifest.** `.loom/checkout.json` lists each file's document id, the document's revision when taken (a fingerprint of its text, relations aside) and the sha256 of the file as written. It is for people and scripts to read; the runtime keeps its own record of the checkout.
- **All or nothing.** The folder must not exist; it appears whole or not at all.
- **Bounds.** `workspace.checkout.max_documents` (500) and `workspace.checkout.max_bytes` (256 MiB). What passes either is left out and listed in `left_out` with the bound named; so is a document with no Path name, and one over the 32 MiB a commit reads back.

```json
{"op": "commit", "from": "workspace:work:policies/"}
```

- **The dry run is the default.** Each file is reported unchanged, changed (lines added and removed), new, missing, conflict or ignored. Pass `dry_run: false` to commit.
- **Changed** files become new editions (`import_md` with `reimport`): unchanged parts keep their ids and relations, changed parts are superseded, nothing is deleted. **New** `.md` files become new Documents at the Path their place maps to, under the checkout's `under_path`; a checkout of named `documents` takes no new file. **Missing** files do nothing: a commit never deletes a Document.
- **Conflicts** are refused and listed while the rest go through: the Document changed in the store after the checkout, it is no longer named at the file's path, or a new file's path already names a Document.
- **The text is a script's.** Its `<!-- loom -->` metadata and relations block are dropped (ids, types and relations come from the store), the rest is neutralised as every import is, and pictures are attached only as `data:` URLs. Each edition's provenance names the workspace, the run and the checkout, and the Document is tagged `source/workspace` and `untrusted`.
- **The manifest chooses nothing.** It is in the workspace, where a command can rewrite it, so a commit goes by the runtime's own record of the checkout. A manifest whose scope, root or files no longer match that record is refused whole. The record is kept in the process that took the checkout: a commit asked on another replica, or after the runtime restarted, finds none and is refused whole, nothing written; check the documents out again.
- **After a real commit** the record and the manifest are updated for what was committed, so the next commit finds those files unchanged.
- **Only in a workspace the run owns.** checkout and commit refuse a shared workspace's name (`<name>@<agent id>`): the record is the owner's, and a share hands over file reads and writes only.
- **Grants.** checkout: the Document tool and the scope's read grants, the `workspace` grant `write`. commit: the Document tool and the scope's write grants (tenant scope: `tenant` in both `memory_scopes` and `sql_scopes`), the `workspace` grant `read`, and `write` for a real commit, which updates the manifest. The `Document` hooks judge every Document call either op makes (the exports, the lookups, each import, its tags), the `Workspace` hooks each file, all before anything is written: one refusal refuses the whole call.

## Workspaces over the MCP server

A session of loomcycle's MCP server (`loomcycle mcp`, `/v1/_mcp`) has no run, so the `workspace` meta-tool gives it workspaces of its own (RFC GW D35). A workspace opened there **belongs to the token identity the session signed in with: its tenant and subject**, not to a run.

- **Off unless the operator turns it on.** `workspace.mcp.enabled: true`, with `workspace.mode` serving workspaces. Every call needs a principal with a tenant and a subject (open mode and the shared `LOOMCYCLE_AUTH_TOKEN` have none and are refused `no_principal`) and the `runs:create` scope. Each call takes one unit of the token's run rate.
- **What it may do** is the operator's `workspace.mcp.ops`: `read` and `write` by default; `exec` only when listed. Opening is implied. The ops are `open`, `release`, `list`, `status`, `exec`, `read`, `write`, `edit`, `glob` and `grep`; sharing, interpreters and servers are a run's (`not_over_mcp`).
- **Who reaches it.** Every session and token of the same tenant and subject reaches the same workspaces by name; another subject of the tenant, the same subject in another tenant, an admin and every run get `not_found`, as for a name nobody uses. A workspace opened over MCP cannot be shared, and reaches no share.
- **When it is released.** At the first of: `release`; no op on it for `workspace.mcp.idle_release_s` (default 3600); `workspace.mcp.max_lifetime_s` after it opened, however busy (default 86400); the token definition that opened it retired, expired or deleted. The workspace sweeper applies the last three on every tick, whether or not the tool is on now, so turning the tool off still ends what it opened. A token that cannot be read (a store outage) keeps the workspace until the next tick. A principal declared in config (`principals:`) has no token definition, so only the two times end its workspaces.
- **Limits.** `workspace.max_per_run` bounds the workspaces one subject holds over MCP; `workspace.max_per_tenant` counts them with the tenant's runs'.
- **A lost worker.** A workspace opened over MCP whose worker comes back after it was marked `lost` is put back in service, and the release rule above decides when it ends.

## Limits

| Setting | Default | What it bounds |
|---|---|---|
| `workspace.max_per_run` | 4 | Workspaces one run holds at once. |
| `workspace.max_per_tenant` | 0 (unlimited) | A tenant's workspaces across its runs. |
| `workspace.exec.default_timeout_s` | 120 | A command that names no `timeout_s`. |
| `workspace.exec.max_timeout_s` | 900 | The longest `timeout_s` a command may ask for. |
| `workspace.mcp.idle_release_s` | 3600 | How long a workspace opened over the MCP server may go without an op before it is released (1 to 2592000). |
| `workspace.mcp.max_lifetime_s` | 86400 | How long after it opened a workspace opened over the MCP server is released, however busy (1 to 2592000). |
| `workspace.kernel.interrupt_grace_seconds` | 5 | How long a cell interrupted at its timeout has to stop before its interpreter is killed (1 to 300). |
| `workspace.lost_after_s` | 300 | How long a worker may be away before its workspaces are marked `lost`. |
| `workspace.disk_quota_mb` | 2048 | What one workspace may hold on its worker's disk (1 MiB to 16 TiB); see "The disk quota". |
| `workspace.copy.max_files` | 2000 | The files one folder copy may move. |
| `workspace.copy.max_bytes` | 1 GiB | The bytes one folder copy may move. |
| `workspace.checkout.max_documents` | 500 | The documents one `File op=checkout` writes. |
| `workspace.checkout.max_bytes` | 256 MiB | The bytes one checkout writes. |
| `workspace.checkpoint.max_per_workspace` | 8 | The checkpoints one workspace holds (1 to 100); each also counts against its disk quota. |
| `workspace.convert.memory_limit_mb` | 1024 | The local worker's conversion child (`LOOMCYCLE_WORKSPACE_CONVERT_MEMORY_LIMIT_MB` on a remote worker). |
| worker capacity | 8 | Workspaces a worker holds (`LOOMCYCLE_WORKSPACE_CAPACITY`). |

A command and everything it started is killed at its timeout. Its output is kept up to 1 MiB. `write` takes a file of up to 1 MiB; `read` returns what the Read tool would. A file `File` moves is bounded by `LOOMCYCLE_FILE_MAX_BYTES` (64 MiB), as any file it handles.

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
| `kernel_unavailable` | The worker cannot start the interpreter `run_cell` asked for: no `python3` or `node` on its `PATH`. | Install it on the worker, or use the other kernel. |
| `workspace_over_quota` | The write, checkpoint, restore or fork would pass a disk quota (checkpoints count against their workspace's), or the workspace is marked full after a command. | Remove files (`rm`, `clear`, or a command) or checkpoints (`delete_checkpoint`), then go on; or raise `workspace.disk_quota_mb`. |
| `checkpoint_limit` | The workspace holds as many checkpoints as `workspace.checkpoint.max_per_workspace` allows. | `delete_checkpoint` one. |
| `checkpoint_not_found` | The workspace has no checkpoint of that id. | `checkpoints` lists them. |
| `exists` | A `fork`'s `as` names a workspace the run already holds. | Fork as another name. |
| `invalid_argument` | The runtime refused the call: an unknown op, an argument the op does not take, a missing one, a bad timeout. | Fix the call. |
| `invalid`, `failed` | The worker refused the arguments (a path or `cwd` out of the workspace, a malformed argument) or a file system operation failed. | Read the detail. |
| `busy`, `output_too_large` | The worker had no free stream in time; the op sent more output than its bound. | Retry; narrow the op. |
| `cancelled` | The run was cancelled before the worker answered. | — |

**`File` on a workspace.** The `File` tool's refusals name the op and say why: `this run has no workspace named …` (open it first; only the run that opened it reaches it), `… already exists` (pass `overwrite: true`), `copy_limit: …` (a folder over `workspace.copy.max_files` or `max_bytes`, named; nothing was copied), `workspace_over_quota: …`, `conversion_helper_missing` (the worker lacks the program named; install it where the worker runs), and a file that crashed the workspace's converter, refused in that workspace for `conversion.quarantine_ttl_s`.

**`worker_lost`.** The worker went away during the call (killed, restarted, its connection dropped). Whether the op took effect is unknown, and the workspace may be gone. The run itself goes on; the agent can check with `status`, or open a new workspace. `worker_revoked` is the same, when the operator retired or re-bound the worker's token.

**`lost` workspaces.** When no replica has seen a worker for `workspace.lost_after_s`, its workspaces are marked `lost`. If the worker comes back while the owning run is still going, the workspace is put back in service; if the run has ended, it is removed. A lost workspace is never removed on a guess: the directory goes only when its worker says so.

**A worker that keeps reconnecting.** A token the runtime cannot verify (unknown, revoked, retired, or checked while the store was down) is never final: the worker logs `could not verify this worker's token` and retries with backoff, and is served only once the token verifies again. A token that verifies but may not attach a worker (an admin token, the legacy secret, a token with other scopes, a shared token on the host engine) is refused for good, and the worker exits saying why. The runtime checks an attached worker's token again every minute, and detaches it when the token no longer holds.

**Leftover directories.** On attach the worker lists the workspaces on its disk, and the runtime tells it which to delete: those released while it was away, those it does not know, and lost ones whose run has ended. Staging folders a copy left behind are removed at release and when the worker starts. A sweeper every 60 seconds releases the workspaces of runs that ended without the release (a runtime killed mid-run).

**`loomcycle doctor`** reports `workspace.mode`, the local worker's tenant and why, and the agents whose grant does nothing. It reads the configuration only; which workers are attached is the running server's `loomcycle_workspace_workers` metric and its log (`workspace worker attached`).
