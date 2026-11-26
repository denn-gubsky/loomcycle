# Workspaces

A **workspace** is a directory on a *workspace worker*, a process apart from the runtime, where an agent runs shell commands and works on files through the `Workspace` tool. It is for work that needs a real shell and real toolchains (Python, Node): run a script, build something, test it, read the result. The `File` tool moves files in and out of a workspace and converts them there, by reference, so a file's bytes never pass through the model. This guide covers what workspaces are in this release (RFC GW, phases 1 to 3), how to run a worker, the `container` engine that isolates a workspace's commands, the agent grant, the tool, moving and converting files, its limits, and what its refusals mean.

## What a workspace is, and what it is not

**What it is.** An agent granted `workspace` opens a workspace by name, runs commands in it (`exec`), reads and changes its files (`read`, `write`, `edit`, `glob`, `grep`) and may `release` it. With the `File` tool it copies files and folders in and out of it, converts files inside it, imports a Markdown file from it as a Document and renders a Document into it (see "Moving files" and "Converting files in a workspace"). A workspace belongs to the **run that opened it**: no other run sees it, not a sub-agent of that run, not a sibling of the same tree, not another run of the same user. It is deleted, files and all, when that run ends, however it ends (completed, failed, cancelled). A run that is paused or parked waiting for input keeps it. If the runtime dies mid-run, a sweeper and the worker's next attach remove what is left.

**What it is not, yet.** Several things are left to later phases:

- **No isolation on the `host` engine.** On `host` (the default, and the only engine of `local` mode) a command runs as the worker's own OS user. It can read and write anything that user can, including the worker's token, and in `local` mode the runtime's data directory, config and secrets files and environment. It is not a sandbox. The `container` engine isolates: see "The container engine".
- **No network but routes, and only on the `container` engine.** A session on the `container` engine has no network except the egress routes its workspace holds, when the worker turns egress on (see "Egress"); without it, `pip install` and `npm install` of anything outside the image fail at once. On `host` nothing blocks the network, and a command has whatever network the worker's machine has.
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

### A personal worker's roots

A personal worker (RFC GW §4.5) runs on a user's own machine and offers folders of it, its roots, by name:

```sh
loomcycle worker --role workspace --personal \
  --root code=~/code/myproject --root notes=~/notes
```

`--root name=path` may be repeated; `~` is the home folder and a relative path is taken from the current folder. The worker refuses to start when a root does not exist or is not a folder, when one root lies inside another, when a root is, holds or lies inside its own `LOOMCYCLE_WORKSPACE_ROOT`, when a name is used twice, and when `--personal` is given without a root, with the `container` engine, or `--root` without `--personal`. It reports each root to the runtime as a label carrying its name only; a root's path never leaves the machine, and no answer names it.

A workspace opened on a root is that folder, not a copy. One workspace is open on a root at a time; a second is refused as busy. Releasing the workspace, or the runtime removing it, only unbinds the folder: the worker never deletes a root or anything in it. Checkpoints, restores, forks and templates are refused on a root, and the disk quota does not apply to it. The worker stages uploads in a `.loomcycle-work` folder inside the root, as in any workspace, and never clears it: what a worker killed mid-upload left there stays for the user to remove. A personal worker runs the `host` engine, so a command runs with the user's own rights and can reach the rest of that user's files by absolute path; the worker logs this at start.

The runtime side, a personal worker token and opening a workspace on a root, comes with later parts of RFC GW P13; until then a personal worker's roots are not reachable from a run.

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
      templates: true            # may save and delete templates (default false)
      connect: true              # may reach a peer's exposed ports through a share, and share with connect (default false)
      durable: true              # may open durable workspaces, kept for the user's next run (default false)
      engine: host               # least isolating engine it accepts; unset = any
```

`exec`, `run_cell`, `reset_kernel`, `start`, `stop`, `logs`, `request` and `await` need `exec`; `write` and `edit` need `write`; `read`, `view`, `glob`, `grep`, `list` and `status` need `read`; `open` needs `open: true`; `expose` and `unexpose` need `expose: true`; `open` with `lifetime: "durable"` needs `durable: true`; `request` on a workspace shared with the run needs `connect: true` instead of `exec`. A sub-agent holds only what both it and its parent hold, before and after a resume. The grant is not per-run overridable, and an agent a tenant member authors may not set it. A grant on an agent without the tool, or while `workspace.mode` is off, does nothing; the boot log and `loomcycle doctor` name such agents.

## Egress

A workspace has no network of its own. What it may reach is a list of **routes**. A route is a name, an `https` upstream URL, the methods it allows (`GET` and `HEAD` unless it lists others), and optionally a stored credential that the core adds on the way out. A session asks the worker for `<base><route>/<path>`. The worker passes the request to the core over its own connection. The core decides whether to send it, builds the upstream request itself, adds the credential and streams the answer back. The worker and the session never see a route's URL list or a credential.

- **Who writes routes.**
  - The operator, in yaml, under `workspace.egress.routes`. The embedded `workspace` preset (`LOOMCYCLE_PRESETS=…,workspace`) declares `pypi`, `pypi-files` and `npm`, all read-only and without a credential.
  - A tenant operator, with `EgressRouteDef`, when the operator sets `workspace.egress.tenant_routes: true`. Port 443 only, and only hosts on `workspace.egress.tenant_hosts` when that list is set.
  - Never an agent: `EgressRouteDef` is not an in-run tool, so a model cannot widen its own way out.
  - See [CONFIGURATION.md](CONFIGURATION.md) for the fields and their checks.
- **Which routes a workspace holds.**
  - The agent grant names them: `workspace: {ops: [...], open: true, egress: [pypi, pypi-files, npm, crm]}`.
  - `open` takes every route the grant names, or the subset it lists in `egress`. A name outside the grant is refused `not_granted`.
  - `status` and `list` show the routes a workspace holds.
  - A sub-agent holds only the routes that both it and its parent hold.
  - A workspace given no route has no egress.
- **What a name means, at each request.**
  - If the operator has a route of that name, the name is the operator's route, even when a tenant wrote a route of the same name.
  - Otherwise, with tenant routes on, it is the workspace's own tenant's route of that name. It is never another tenant's.
  - A tenant route is checked again at each request against `tenant_hosts` as the operator has it now and against the 443-only rule. Deleting it, or narrowing the list, stops it at once.
  - A name that resolves to nothing is refused with 403: `route "<name>" is not one this workspace holds`.
- **From a script.**
  - `LOOMCYCLE_EGRESS` holds the base: `curl "${LOOMCYCLE_EGRESS}crm/deals?since=2026"`.
  - The core maps the path under the route's URL, and the path never leaves it. A `.` or `..` segment, an encoded slash, a backslash and an absolute URL are refused. The one exception is the encoded slash in npm's `@scope%2fname`, allowed in the first segment only.
  - The request's `Authorization`, `Cookie`, `Host`, `Forwarded`, `X-Forwarded-*` and hop-by-hop headers are dropped.
  - The answer's `Set-Cookie` and authentication headers are removed, so an API cannot hand the session a token either.
  - A redirect is relayed only when it points under another route the workspace holds, rewritten to that route. Any other redirect is refused with 502.
- **pip and npm.**
  - The session's environment points pip and npm at the routes:
    - `PIP_INDEX_URL=<base>pypi/simple/` and `PIP_TRUSTED_HOST`;
    - `npm_config_registry=<base>npm/`;
    - npm's audit, fund and update-notifier calls turned off.
  - pip needs both `pypi` and `pypi-files`. PyPI's index links to `files.pythonhosted.org`, and the core rewrites those links to the `pypi-files` route. It does this only on an operator route, never a tenant's.
  - Without `pypi-files`, pip fails at once with a 403 naming the route.
  - The hashes beside the links are not touched, so pip still checks what it downloads, and npm still checks `integrity`.
  - Routes for Go and cargo are the operator's to add.
- **On the `container` engine.** Egress is turned on on the worker with `LOOMCYCLE_WORKSPACE_CONTAINER_EGRESS_DIR`, and needs a Linux daemon ([WORKERS.md](WORKERS.md)).
  - Each session gets a network of its own, with no route out. Its only other member is a gateway that answers as `egress`.
  - No other name resolves, and a lookup fails at once.
  - A session cannot reach another workspace, the worker or the host.
  - The base is `http://egress/`.
  - Without the setting, a session keeps `--network none`.
- **On the `host` engine.** Egress is turned on with `LOOMCYCLE_WORKSPACE_HOST_EGRESS=1` on the worker; a local worker takes it from the runtime's environment.
  - Each workspace gets a loopback port, and the base is `http://127.0.0.1:<port>/<secret>/`.
  - The port answers only a request whose `Host` is its own address and whose path starts with the secret. The secret is a random value made each time the port opens and given to the workspace's commands only in their environment. That keeps out a web page reaching the port through DNS rebinding, and other users' processes.
  - **It is not a boundary.** The host engine's commands have the host's network anyway. Every process of the worker's own OS user can read a command's environment, so it can send requests as the workspace through its routes, a credentialed route included. That includes other workspaces on the same host worker.
  - Turn it on only where every agent is trusted.
- **Credentials.**
  - A route's credential is resolved for the workspace's run, from its agent scope, then user scope, then tenant scope.
  - The credential must be restricted (`allowed_urls`) to URLs covering the route's URL. An unrestricted credential, or one restricted elsewhere, refuses the request.
  - This is checked when a tenant creates the route (for the tenant credential) and again at every request (for whichever credential the run resolves).
  - The token reaches neither the worker nor the session. The end-to-end suite searches the worker's disk, environment and log, the session's environment, the run's events, the store and the audit log for it.
- **Limits and audit.**
  - Each workspace may send 600 requests a minute and 4 GiB of bodies through each route. `workspace.egress.limits` changes both. Over either, the request is refused with 429. The limits are counted per replica.
  - Every request through a credentialed or tenant-written route writes a `workspace_egress` audit line. The line holds the route, the method, the path without its query, the status and the bytes; never a header, a body or a credential.
- **What routes do not bound.** A script is untrusted code holding the route's authority. On a route that allows `POST` it can do whatever the credential can do at that API, and it can send anything it read to any route it holds. Routes bound where data can go, not what is sent. Give a route the narrowest credential the API offers.

| Answer | What it means |
|---|---|
| 403 `route "x" is not one this workspace holds` | The grant, `open`'s `egress`, or the routes as they are now do not hold `x`. |
| 403 `route "x" does not allow POST` | The route lists the methods it allows. |
| 403 `credential "c" … is not found for this run`, `… does not cover route …`, `… is not restricted to URLs` | The route's credential cannot be sent. |
| 403 `route "x": host … is not on the operator's workspace.egress.tenant_hosts`, `… port … is not allowed` | A tenant route the operator's ceiling no longer allows. |
| 429 | Over the route's request rate or byte budget for this workspace. |
| 502 `route "x" redirected to <host>, which is not under a route this workspace holds` | The upstream sent the session elsewhere. |
| 503 `egress is not linked to the core on this worker` | The worker has no way to the core for egress. |

## The tool

One tool, `Workspace`, with an `op`:

| op | What it does | Arguments |
|---|---|---|
| `open` | open (or return) a workspace of this run | `name`, `toolchain` (`python` or `node`; omit for a worker with both), `lifetime` (`durable`: see Durable workspaces) |
| `list` | this run's workspaces | — |
| `status` | its state, and whether its directory is on the worker | `name` |
| `release` | delete it now; a durable one is kept for the user's next run unless `discard` | `name`, `discard` |
| `exec` | run a shell command in it (`/bin/sh -c`) | `name`, `command`, `cwd`, `timeout_s` |
| `run_cell` | run code in an interpreter that keeps its variables between calls | `name`, `code`, `kernel` (`python` or `node`), `timeout_s` |
| `reset_kernel` | stop the interpreter; the next cell starts clean | `name`, `kernel` |
| `checkpoint`, `checkpoints`, `restore`, `delete_checkpoint`, `fork` | record the files, put them back, open a copy; see "Checkpoints and forks" | `name`, `label`, `checkpoint`, `as` |
| `save_template`, `templates`, `delete_template` | keep a checkpoint as a template, list them, delete one; `open` takes `template`; see "Templates" | `name`, `checkpoint`, `as`, `scope`, `template` |
| `view` | return a picture in it to the model as an image | `name`, `path` |
| `start` | run a command that keeps running after the call, a server (see Servers) | `name`, `command`, `cwd`, `port` |
| `stop` | end a server and what it started | `name`, `server` |
| `logs` | a server's latest output, or the list of servers | `name`, `server`, `tail_bytes` |
| `request` | send one HTTP request to a port a running server declared (see Sending a request to a server) | `name`, `port`, `method`, `path`, `headers`, `body` |
| `expose`, `unexpose` | show a server's port to the run's user as a page, and withdraw it (see Showing a page to the user) | `name`, `port`, `title`, `kind` |
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

### A fork for each child

`Agent op=spawn` and each entry of `op=parallel_spawn` take `workspaces: [{fork: <name>, as?}]` beside the shares: the child gets its own fork of the parent's workspace `name`, named `as` in the child (default the same name), made after its run exists and before its first turn.

```json
{"op": "parallel_spawn", "spawns": [
  {"name": "trial", "prompt": "Try plan A.", "workspaces": [{"fork": "py"}]},
  {"name": "trial", "prompt": "Try plan B.", "workspaces": [{"fork": "py"}]}]}
```

- **The child owns it.** It counts against the child's `max_per_run`, the child may run commands in it as its own grant allows, and it is released when the child ends. Nothing a child does reaches the parent's workspace or a sibling's fork.
- **Who may.** The parent needs `workspace.open`, and the source must be the parent's own workspace: one shared with it cannot be forked. The fork's routes are the source's that the child's `workspace.egress` also names; a child whose `workspace.engine` is stronger than the source's is refused.
- **Failures.** A fork that cannot be made (a name the parent does not hold, a limit, a full worker) fails the child before its first turn.
- **Results.** The parent reads what the children made through a share each child makes (`workspace.share`), or the children copy it back with `File`.
- **Audit.** Every fork made, by `Workspace op=fork` or at spawn, writes one `workspace_fork` audit line: the source workspace and its run, the copy's id and name and the run that owns it, the tenant, and the path (`fork` or `fork at spawn`). Never a file's content. A refused fork writes none.

## Templates

A template is a named starting state: a checkpoint's files, installed packages included, kept by the runtime after the run that saved it ends (RFC GW §5.5, P6). `open` with `template` starts a new workspace as a copy of it, on any worker that has its toolchains.

```json
{"op": "save_template", "name": "py", "checkpoint": "ck_3f9a1c0e5b7d2a64", "as": "pandas-ready", "scope": "user"}
```
```json
{"op": "open", "name": "analysis", "template": "pandas-ready"}
```

| op | What it does | Grant | Arguments |
|---|---|---|---|
| `save_template` | keep a checkpoint of the run's own workspace as a template | `templates` | `name`, `checkpoint`, `as`, `scope` |
| `templates` | the templates the run may open | `open` or `templates` | `all_users` (a tenant operator's session) |
| `delete_template` | delete a template | `templates` | `template`, `scope`, `owner` (a tenant operator's session) |
| `open` with `template` | a new workspace as a copy of a template | `open` | `name`, `template` (not with `toolchain`) |

- **Who sees one.** A `user` template is open to its author's runs only (runs of the same user); a `tenant` one to every run of the tenant. Saving or deleting a `tenant` template also needs the tenant value in the agent's `memory_scopes`. A template never leaves its tenant; a run with no user id sees tenant templates only and cannot save a user one. An isolated member (`substrate:user`) lists and opens its tenant's templates, but cannot save or delete one. `open` takes the user's own template of a name first, then the tenant's. Over the MCP server a session uses templates as its token's tenant and subject (the subject in the user's place, so a run of that user and the session share user templates); saving or deleting a tenant template there needs a tenant-operator token (`substrate:tenant` or `substrate:admin`). Such a session may also clean up its tenant's user templates: `templates` with `all_users: true` lists every user's (each with its `owner`), and `delete_template` with `scope: user` and `owner` deletes one; the audit line names both the operator and the owner. Never another tenant's, never a member token, and never from inside a run, whatever bearer started it.
- **Never overwritten.** A name already in that scope is refused with `exists`: delete it first, or save as another name.
- **Where it is kept.** As one archive (tar, gzip) under the core's data directory (`<LOOMCYCLE_DATA_DIR>/workspace-templates/`), with its tenant, scope, toolchains and SHA-256 on a row (`workspace_templates`). The core copies the bytes and never opens them. A worker that opens a template it does not hold is sent the archive once, checks its digest, unpacks it into a cache beside its workspaces (`.loomcycle-templates/<digest>/`, at most 8 kept, the least recently used removed) and copies each new workspace from there by its checkpoint mechanism. Unpacking refuses a name that leaves the template and never follows a link.
- **Bounds.** `workspace.template.max_mb` (default 1024) bounds one archive; a save past it keeps nothing (`template_too_large`). `workspace.template.max_per_tenant` (default 20) bounds a tenant's templates, user and tenant ones together, and `workspace.template.max_per_user` (default 5) one user's user templates, a run's and an MCP session's of that subject together, so one member cannot fill the tenant's bound (`template_limit` for either). A workspace opened from a template is held to its own `disk_quota_mb`.
- **Python environments.** On the `host` engine an environment's paths are moved to the new workspace's folder, and on the `container` engine to `/workspace`, as for a fork; the answer's `rewritten` is not shown for an open.
- **Audit.** A save and a delete each write an audit line (`workspace_template_save`, `workspace_template_delete`) naming the template, its scope, digest and size, the run, and for a user template its owner. Never a file's content.
- **Erasure.** A subject's erasure (`POST /v1/_erasure` and its twins) reports the subject's user templates in tier 1 (`workspace_templates`) and deletes each row and its archive; a dry run only counts them. A tenant template the subject saved is the tenant's and stays: the report's notes and the result's `retained` (`workspace_templates_tenant`) count it, and a tenant operator removes it with `delete_template`.
- **Not yet.** The worker image ships no templates of its own yet.

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
- **Who reaches its port.** In a `container` session no one but the session: the session has no network, and a server listens on its own loopback. The owning run reaches it with `request` (below); pages served to a user come with exposure (RFC GW P9, later PRs). On the `host` engine every process of the worker shares one loopback, so a port is not isolated from the worker's other workspaces; the host engine is for a trusted single-tenant install.

## Sending a request to a server

`request` sends one HTTP request to a port a running server of the workspace declared at `start`, and returns the answer: a quick way for the agent to try the API or page it built (RFC GW §5.3, D26). It is one request and one answer; nothing stays open. It needs the `exec` grant, as servers do, and a share never carries it.

```json
{"op": "request", "name": "api", "port": 3000, "method": "POST", "path": "/items", "headers": {"Content-Type": "application/json"}, "body": "{\"title\": \"milk\"}"}
```
```json
{"status": 201, "headers": {"Content-Type": "application/json"}, "body": "{\"id\": 1, \"title\": \"milk\"}"}
```

- **Which port.** Only a port a running server of this workspace declared with `start ... port`; anything else is `port_not_served`, and nothing is dialled. On the `host` engine the port must also be listened on by that server's own processes (its process group), and by nothing else: declaring a port does not make it the server's, and on the host's shared loopback it could be the runtime's API or any other local service. The worker reads the listener's owner from `lsof` on macOS and `/proc` on Linux, refuses the loopback dispatcher ports it attaches to, and never reaches a listener in its own process. A worker with an operator command list (`LOOMCYCLE_WORKSPACE_COMMANDS`) still serves `request`: a server can only be a listed program, and the tunnel reaches only what that program bound. A server stopped, restored away or not yet started in a fork serves no port. `port_unreachable` is a declared port nothing listens on yet: read the server's `logs`.
- **What is sent.** The method, the request target (`path`, starting with `/`, with its query), the headers given and a body of up to 1 MiB. `Host` is `localhost:<port>` unless set; `Connection: close` is the runtime's, and so are `Content-Length`, `Transfer-Encoding` and the other framing headers, which are refused as arguments. No credential of the run or its user is added.
- **What comes back.** `status`, `headers` (several values joined by `, `), and `body` when it is text, otherwise `body_base64`; a body over 1 MiB is cut there with `truncated: true`. A redirect is returned, not followed. The answer is untrusted tool output.
- **Time.** The call waits as long as a command does by default (`workspace.exec.default_timeout_s`), then answers `timeout`.
- **How it travels.** The core opens a tunnel down one of the worker's idle op streams, carrying bytes both ways at once. On the `container` engine the worker starts a small relay inside the session (`python3`, which the session image must have) that connects to `127.0.0.1:<port>` there: the session keeps the network it had (none, unless egress gave it its own) and the worker never dials into it. On the `host` engine the worker dials `127.0.0.1:<port>` itself, which is a check, not isolation: every workspace of a host worker shares one loopback, so a port two workspaces' servers both declare is refused.

## Showing a page to the user

`expose` publishes a port a server of the workspace listens on to the run's user: loomboard shows it in the chat as a page or a panel (RFC GW §5.3, D21–D24). It needs the agent's `workspace.expose: true` grant and the operator's `workspace.expose.enabled`; both are off unless set, since a page is model-written active content put in front of a person.

```json
{"op": "expose", "name": "site", "port": 8000, "title": "Sales by region"}
```
```json
{"name": "site", "exposure_id": "4f1c0a9e2b7d3c5e6a8b9d0f", "port": 8000, "title": "Sales by region", "kind": "page", "base_path": "/v1/workspaces/exposures/4f1c0a9e2b7d3c5e6a8b9d0f/"}
```

- **Who opens it.** `/v1/workspaces/exposures/<id>/…` (every method) serves it to one person: a bearer in the `Authorization` header whose tenant and subject are the workspace's tenant and user. Anyone else, an admin, the legacy token, open mode and a token limited to certain agents included, gets the answer an unknown id gets, so an id cannot be probed. A Web UI cookie, a token in the address and a request with no `Authorization` header are refused, so a browser sent to the address runs nothing.
- **What reaches the application.** The prefix is removed (the application receives `/…`); `Host` and `Origin` arrive as the client sent them; `Authorization`, `Proxy-Authorization` and `Cookie` are removed. Each request travels down its own tunnel to the worker, which reaches the port only while a running server of the workspace declared it and, on the `host` engine, its own processes listen on it; anything else is a `502`.
- **What reaches the client.** Of the application's headers only `Content-Type`, `Content-Encoding`, `Content-Length`, `Content-Disposition`, `ETag`, `Last-Modified`, `Accept-Ranges` and `Content-Range` (every other header, `Set-Cookie` and the origin-wide ones such as `Clear-Site-Data` and `Strict-Transport-Security` among them, is dropped, as are trailers and any `1xx`); `Cache-Control` is `private, no-store`; a redirect to `/x` is rewritten under the prefix, and one to another site, or whose second character is `/` or `\`, is left as it is. Every answer, errors included, carries `Content-Security-Policy: sandbox allow-scripts allow-forms; connect-src 'self'` (in place of the application's own) and `X-Content-Type-Options: nosniff`. Links inside a page are the application's: make them relative, or start them with `base_path`.
- **When it closes.** With `unexpose {name, port}`, with the workspace's release and the owning run's end, when no request used it for `workspace.expose.idle_timeout_s` (default 15 minutes), and, all at once, when the operator turns `workspace.expose` off. A server that stops leaves the page answering `502` until a server listens on the port again.
- **WebSocket.** A page's socket opens on the same address. A browser cannot set an `Authorization` header on a socket, so it offers the bearer as the subprotocol `bearer.<token>`, beside the marker `loomcycle.surface.v1` and the application's own protocols (as the client-tools socket does). A cookie never opens one, so a page of another site cannot open a socket in the user's name; the `Origin` is passed to the application as sent, for it to judge. The core dials the application first, through the same tunnel and listener check (an application that is not there, or refuses the upgrade, is a plain `502`, never a half-open socket). It offers the application only the client's protocols for it, never the marker or the bearer, and answers the client with the application's choice or the marker. Nothing else of the application's `101` reaches the client. Messages cross one by one, text and binary, with their close codes; each hop answers its own pings. Bounds: a message is at most `max_message_bytes` (16 MiB) each way, the byte rate holds both ways, a socket counts as one of `max_connections` while open, and it closes after `idle_timeout_s` with no message (1001), after `socket_lifetime_s` (4 hours, 1001), and within 15 seconds of an `unexpose` or the workspace's release (1008). A browser may instead present a one-time ticket as the subprotocol `ticket.<t>`: `POST /v1/workspaces/exposure-tickets/<id>` with the bearer in the `Authorization` header mints one for the page (a cookie never mints one), good once and for 30 seconds, on any replica sharing `LOOMCYCLE_SECRET_KEY`; the socket it opens follows the credential that minted it. In strict sign-in mode no bearer is read from a subprotocol, so a browser opens a page's socket there with a ticket (or a client that can set the header, with the header). Any other upgrade (`h2c`) is `501`.
- **Limits.** Per workspace: `max_per_workspace` ports (default 2), `max_connections` requests and sockets open at once (32), `requests_per_second` (20, burst twice that) and `bytes_per_second` (8 MiB), the last three kept per replica. Over a bound the route answers `429`.
- **How the user learns of it.** `expose` puts a `surface` event on the run's own stream: `{exposure_id, workspace, title, kind, base_path, expires_at}`, where `expires_at` is the idle deadline at that moment and moves on with every use. A `surface_closed` event, `{exposure_id, reason}`, follows when it goes: `unexposed` (the run's `unexpose`), `released` (the workspace's release or the run's end), `idle` (the idle timeout, met by the sweep or by the next request) or `lost` (its worker went away: the workspace was marked lost). Both are on SSE, gRPC (`Event.surface`), the stored transcript and both adapters (`SurfaceInfo`); neither carries a bearer, a ticket or a host path, and the model never sees either. A close the run did not make in a tool call (a sweep, the run's end) is written to the run's transcript, which a re-attached stream (`GET /v1/runs/{id}/stream`) reads; it is not pushed to a stream already open. A sub-agent's page is told on the sub-agent's own stream, not its parent's. Turning the operator's switch off tells nothing at that moment; each page is told `idle` when the sweep removes its row.
- **What the person did (`await`).** A server the workspace started finds an address in `LOOMCYCLE_UPDATE_URL`; the application posts an update there (a `POST` whose body is the update, up to 64 KiB) when a person does something in the page, and `Workspace op=await {name, timeout_s?}` returns the next one, once, in order, or `timed_out` (RFC GW §5.3, P9 plan §3.4). The answer is `{name, update, pending}` (`update_base64` for a body that is not text; `dropped` when posts were refused). The post is answered `202` when queued, `413` past 64 KiB and `429` when 64 updates already wait, so a server cannot flood the run. `await` needs `exec`, is the owner's alone (not through a share, not over the MCP server), and waits at most the operator's command timeout and never over an hour. On the `container` engine the address is a small relay inside the session (`python3`), on the session's own loopback, which nothing outside the session reaches. On the `host` engine it is a loopback port of the worker with a secret path of 256 random bits, compared in constant time; a wrong path is answered `404` with nothing else, and one workspace's path never posts into another's queue. That is a check, not isolation: every process of the worker's user shares the loopback and can read a server's environment (see the host engine's limits). The address is given only to the workspace's servers, never to a command, the model, an event or a log, and it closes with the workspace's release, a restore and the worker's stop.
- **Not yet.** A peer reaches a page with `request` through a share made with `connect` (see Sharing a workspace). `expose` is a run's: a workspace opened over the MCP server and a shared one take none.

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

**Making a share.** `Workspace op=share {name, with, access, connect?}`: `with` is the other run's agent id (a parent, a child, a sibling, or a run of another tree of the same user); `access` is `read` or `read_write`. `op=unshare {name, with}` ends it. `Agent op=spawn` and each entry of `op=parallel_spawn` take `workspaces: [{name, access, connect?}]`, shared with the child before its first turn (an entry `{fork, as?}` gives the child its own copy instead; see "Checkpoints and forks"):

```json
{"op": "spawn", "name": "reviewer", "prompt": "Review the change in build.",
 "workspaces": [{"name": "build", "access": "read"}]}
```

**Using one.** The peer names the workspace `<name>@<owner agent id>`; its `Workspace op=list` shows it under `shared_with_me`. With `read` it may use `Workspace read`, `view`, `glob` and `grep` on it, and `File` ops that read from `workspace:<name>@<agent id>:<path>` (copy out, stat, ls, extract, import). `read_write` adds `File` writes into it and `Workspace write` and `edit`.

**Reaching its pages (`connect`).** A share made with `connect: true` (in `op=share` or a spawn's `workspaces` entry) lets the peer send single HTTP requests to the ports its owner **exposed**, with `Workspace op=request {name: "<name>@<owner agent id>", port, method, path, headers?, body?}` (RFC GW §5.2, D20, D26). The owner needs `workspace.connect` to hand it over, the peer needs `workspace.connect` to use it (not `exec`), and the port must be one the owner exposed with `expose`, while the operator's `workspace.expose` is on and the page has not gone idle; a port a server listens on but the owner did not expose is refused `not_exposed`. The request then travels as the owner's does: down the tunnel, to a port a running server of the workspace declared and, on the `host` engine, its own processes listen on. A request through a share keeps the page from going idle, as a use by its user does. The owner's own `request` still needs `exec`, and reaches its servers' ports whether exposed or not.

**What a share never allows:** `exec`, a release, a status, and sharing it on; and, with `connect`, never `expose`, `start`, `stop` or `logs`. Commands in a workspace are its owner's alone, so two agents never race in one session.

**Who may share with whom.**

- The owner needs `workspace.share` and the operations it hands over: `read` for a read share, `read` and `write` for `read_write`, and `connect` for a share with connect.
- The peer needs the `workspace` operation each use asks for, in its own grant: a share never gives an agent more than its definition does.
- The peer is a live run of the same user, in the same tenant. A run of another user, an ended run and an unknown agent id are refused alike; a run with no user id neither shares nor is shared with.
- A workspace is shared with at most 16 runs.

**How long it lasts.** A share is stored on the workspace's row, so every replica honours it and it survives a pause and a resume of either run. It ends with `unshare`, with the workspace's release, or when either run ends.

**Audit.** Each share, unshare and each tool call through a share writes an audit event (`workspace_share`, `workspace_unshare`, `workspace_share_use`) naming the workspace, both runs and the access; a share with connect says so (`connect`), and each request through one is a use naming the port (`request :<port> via Workspace`).

## Durable workspaces

A durable workspace is kept after its run ends, under the tenant, the user and its name (RFC GW §4.2, P12). A later run of the same user opens it again and finds its files, on the worker that holds them.

```yaml
workspace:
  durable:
    enabled: true          # off unless set
    max_per_user: 8        # durable workspaces a user keeps, pinned or parked (default 8)
agents:
  builder:
    tools: [Workspace]
    workspace:
      ops: [read, write, exec]
      open: true
      durable: true        # may open durable workspaces (default false)
```

```json
{"op": "open", "name": "site", "lifetime": "durable"}
```

- **Who reaches it.** Only runs of the user who opened it, in its tenant, by its name and `lifetime: "durable"`. An `open` without `lifetime` never reaches it: it makes an ephemeral workspace of its own. Another user of the tenant who opens the same name gets a workspace of their own.
- **One run at a time.** A run that opens it holds it (its pin). A second live run of the user is refused `held`, naming the holder's agent id. The pin is taken by a compare-and-set on the workspace's row, so two runs on two replicas never both hold it. A run that holds it and is paused keeps it.
- **When its run ends, or releases it,** it is parked: its files stay; what runs in it ends (servers, interpreters, the session); its shares end, its pages close, and a Documents checkout in it is forgotten, so commit before you release. A parked workspace is reached by no run, takes no place in its worker's capacity, and sends nothing out through egress.
- **Opening it again** pins it to the new run on its worker. The worker must be attached to the replica the run is on (else `worker_lost`; try again), and the run's grant must cover the engine and the egress routes it was opened with (else `not_granted`). Its routes stay the ones it was opened with; a template is for a new one only.
- **Removing it.** `release {name, discard: true}` deletes it, files and all; it also deletes, by name, a durable workspace of the user that no live run holds. Discarding needs `workspace.durable` and a grant that covers the engine and routes it was opened with, as re-opening it does; otherwise it is refused `not_granted` and nothing is touched. Otherwise it is kept until the user's erasure: nothing deletes a parked workspace by age. A subject's erasure (`POST /v1/_erasure` and its twins) counts their durable workspaces in tier 1 (`workspaces_durable`) and removes each, held or parked, row and files; a worker that is away removes the files when it next attaches. A dry run only counts them. A transport with no workspace layer removes none and names them in `retained`.
- **Its worker.** A durable workspace lives on one worker's disk. If the worker goes away, the workspace is marked lost after `lost_after_s` and `open` answers `lost`; when the worker comes back with its disk, the workspace is ready again, whatever became of its last run.
- **Not over the MCP server.** A workspace opened over loomcycle's MCP server is never durable; it ends by the session's release rule.
- **In a snapshot.** A workspace's row travels in a snapshot; its files never do. Captured: run-owned and parked workspaces that are ready or lost, with their shares and egress routes. Not captured: workspaces opened over the MCP server, ones opening or releasing, pages, templates and checkouts. On restore a row is kept only where it points at what is its own: a worker of its own tenant (or a shared worker; a personal worker only if it is its own user's), a run of its own tenant and user, routes the target defines, and a git credential that is a reference. A durable row whose run was not restored is parked; an ephemeral one is skipped, as is a row the target already holds. Every restored row is `lost` until its worker attaches holding it. The restore result counts `workspaces_restored` and `workspaces_skipped`.

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
- **An operator's grant.** An admin and a tenant operator (`substrate:tenant`) reach the tool. A member does not by default: a tenant operator grants it to a member, a group or the whole tenant with an access-control list that allows `invoke` on `mcp-tool:workspace` (see [CONFIGURATION.md](CONFIGURATION.md#managing-groups-acl-lists-and-bindings)). The grant does not lift the switch or the scope above.
- **What it may do** is the operator's `workspace.mcp.ops`: `read` and `write` by default; `exec` only when listed. Opening is implied. The ops are `open` (with `template` too), `release`, `list`, `status`, `exec`, `read`, `write`, `edit`, `glob`, `grep`, `checkpoint`, `checkpoints`, `delete_checkpoint`, `save_template`, `templates` and `delete_template` (see Templates); sharing, `restore`, `fork`, interpreters and servers are a run's (`not_over_mcp`).
- **Who reaches it.** Every session and token of the same tenant and subject reaches the same workspaces by name; another subject of the tenant, the same subject in another tenant, an admin and every run get `not_found`, as for a name nobody uses. A workspace opened over MCP cannot be shared, and reaches no share.
- **When it is released.** At the first of: `release`; no op on it for `workspace.mcp.idle_release_s` (default 3600); `workspace.mcp.max_lifetime_s` after it opened, however busy (default 86400); the token definition that opened it retired, expired or deleted. The workspace sweeper applies the last three on every tick, whether or not the tool is on now, so turning the tool off still ends what it opened. A token that cannot be read (a store outage) keeps the workspace until the next tick. A principal declared in config (`principals:`) has no token definition, so only the two times end its workspaces.
- **Egress.** None unless the operator lists routes in `workspace.mcp.egress`. Each name must be one of the operator's own routes (`workspace.egress.routes`). A tenant's routes are not offered here, because a session's workspace has no run. `open` takes every listed route, or a subset named in `egress`. A route the list does not name is refused (`not_granted`). The list is read again at each request, so removing a route from it stops that route for workspaces already open. A route's credential resolves for the session's tenant and subject, from its user scope and then its tenant scope. A session has no agent, so an agent-scope credential is never used.
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
| `workspace.mcp.egress` | `[]` | The operator routes (`workspace.egress.routes`) a workspace opened over the MCP server may reach the network through. Empty, none. |
| `workspace.kernel.interrupt_grace_seconds` | 5 | How long a cell interrupted at its timeout has to stop before its interpreter is killed (1 to 300). |
| `workspace.lost_after_s` | 300 | How long a worker may be away before its workspaces are marked `lost`. |
| `workspace.disk_quota_mb` | 2048 | What one workspace may hold on its worker's disk (1 MiB to 16 TiB); see "The disk quota". |
| `workspace.copy.max_files` | 2000 | The files one folder copy may move. |
| `workspace.copy.max_bytes` | 1 GiB | The bytes one folder copy may move. |
| `workspace.checkout.max_documents` | 500 | The documents one `File op=checkout` writes. |
| `workspace.checkout.max_bytes` | 256 MiB | The bytes one checkout writes. |
| `workspace.checkpoint.max_per_workspace` | 8 | The checkpoints one workspace holds (1 to 100); each also counts against its disk quota. |
| `workspace.personal_workers` | false | Let a user's own machine attach as a worker of that user's runs (RFC GW §4.5). Refused unless the mode is `remote` or `local+remote` and `worker_endpoint.listen` is on loopback. |
| `workspace.template.max_mb` | 1024 | One template's archive, in MiB (1 to 65536). |
| `workspace.template.max_per_tenant` | 20 | The templates a tenant holds, user and tenant ones together (1 to 1000). |
| `workspace.template.max_per_user` | 5 | One user's user templates, runs and MCP sessions of the subject together (1 to `max_per_tenant`). |
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

**`lost` workspaces.** When no replica has seen a worker for `workspace.lost_after_s`, its workspaces are marked `lost`. If the worker comes back while the owning run is still going, the workspace is put back in service; if the run has ended, it is removed, except a durable one, which comes back whatever became of its last run (the sweeper parks it). A worker that comes back without a ready workspace's directory (a new disk) leaves it lost: its files are gone, and it never answers ready again (one made in the last two minutes is left alone, since another replica may still be opening it). A workspace marked lost closes its pages, and its run is told `surface_closed` with the reason `lost`. A lost workspace is never removed on a guess: the directory goes only when its worker says so.

**A worker that keeps reconnecting.** A token the runtime cannot verify (unknown, revoked, retired, or checked while the store was down) is never final: the worker logs `could not verify this worker's token` and retries with backoff, and is served only once the token verifies again. A token that verifies but may not attach a worker (an admin token, the legacy secret, a token with other scopes, a shared token on the host engine) is refused for good, and the worker exits saying why. The runtime checks an attached worker's token again every minute, and detaches it when the token no longer holds.

**Leftover directories.** On attach the worker lists the workspaces on its disk, and the runtime tells it which to delete: those released while it was away, those it does not know, and lost ones whose run has ended. Staging folders a copy left behind are removed at release and when the worker starts. A sweeper every 60 seconds releases the workspaces of runs that ended without the release (a runtime killed mid-run).

**`loomcycle doctor`** reports `workspace.mode`, the local worker's tenant and why, and the agents whose grant does nothing. It reads the configuration only; which workers are attached is the running server's `loomcycle_workspace_workers` metric and its log (`workspace worker attached`).
