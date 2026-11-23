# Workspaces

A **workspace** is a directory on a *workspace worker*, a process apart from the runtime, where an agent runs shell commands and works on files through the `Workspace` tool. It is for work that needs a real shell and real toolchains (Python, Node): run a script, build something, test it, read the result. The `File` tool moves files in and out of a workspace and converts them there, by reference, so a file's bytes never pass through the model. This guide covers what workspaces are in this release (RFC GW, phases 1 and 2), how to run a worker, the agent grant, the tool, moving and converting files, its limits, and what its refusals mean.

## What a workspace is, and what it is not

**What it is.** An agent granted `workspace` opens a workspace by name, runs commands in it (`exec`), reads and changes its files (`read`, `write`, `edit`, `glob`, `grep`) and may `release` it. With the `File` tool it copies files and folders in and out of it, converts files inside it, imports a Markdown file from it as a Document and renders a Document into it (see "Moving files" and "Converting files in a workspace"). A workspace belongs to the **run that opened it**: no other run sees it, not a sub-agent of that run, not a sibling of the same tree, not another run of the same user. It is deleted, files and all, when that run ends, however it ends (completed, failed, cancelled). A run that is paused or parked waiting for input keeps it. If the runtime dies mid-run, a sweeper and the worker's next attach remove what is left.

**What it is not, yet.** Phases 1 and 2 have the `host` engine only, and leave several things to later phases:

- **No isolation.** On the `host` engine a command runs as the worker's own OS user. It can read and write anything that user can, including the worker's token, and in `local` mode the runtime's data directory, config and secrets files and environment. It is not a sandbox. The `container` engine, which isolates, is phase 3.
- **No managed network.** No network is granted or routed for a workspace. On the `host` engine nothing blocks it either: a command has whatever network the worker's machine has. Do not rely on either.
- **No hard disk limit under a command.** The disk quota stops what `File` and the `Workspace` tool write before a byte is written, but a command's own writes are only measured after it ends (see "The disk quota"). One command can pass the quota while it runs; the next step is stopped. The `container` engine (phase 3) can put a file-system limit under it.
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
- **Grants.** checkout: the Document tool and the scope's read grants, the `workspace` grant `write`. commit: the Document tool and the scope's write grants (tenant scope: `tenant` in both `memory_scopes` and `sql_scopes`), the `workspace` grant `read`, and `write` for a real commit, which updates the manifest. The `Document` hooks judge every Document call either op makes (the exports, the lookups, each import, its tags), the `Workspace` hooks each file, all before anything is written: one refusal refuses the whole call.

## Limits

| Setting | Default | What it bounds |
|---|---|---|
| `workspace.max_per_run` | 4 | Workspaces one run holds at once. |
| `workspace.max_per_tenant` | 0 (unlimited) | A tenant's workspaces across its runs. |
| `workspace.exec.default_timeout_s` | 120 | A command that names no `timeout_s`. |
| `workspace.exec.max_timeout_s` | 900 | The longest `timeout_s` a command may ask for. |
| `workspace.lost_after_s` | 300 | How long a worker may be away before its workspaces are marked `lost`. |
| `workspace.disk_quota_mb` | 2048 | What one workspace may hold on its worker's disk (1 MiB to 16 TiB); see "The disk quota". |
| `workspace.copy.max_files` | 2000 | The files one folder copy may move. |
| `workspace.copy.max_bytes` | 1 GiB | The bytes one folder copy may move. |
| `workspace.checkout.max_documents` | 500 | The documents one `File op=checkout` writes. |
| `workspace.checkout.max_bytes` | 256 MiB | The bytes one checkout writes. |
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
| `workspace_over_quota` | The write would pass the workspace's disk quota, or the workspace is marked full after a command. | Remove files (`rm`, `clear`, or a command), then go on; or raise `workspace.disk_quota_mb`. |
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
