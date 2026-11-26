# Conversion workers

loomcycle converts files: it extracts a document's text, cuts and renders PDF pages for OCR, turns a recording into a WAV for speech-to-text, encodes synthesized speech, and re-encodes images a vision model must have scaled. These are the jobs whose memory is set by what someone put in a file. One Office document at every extraction cap can take about 700 MiB, and a hostile one is written to take more.

Inside the runtime's own process, a file that exhausts memory takes every run on the replica with it. **Conversion workers** move the conversions out (RFC GO): a worker is the same binary, run as `loomcycle worker`, that receives a file's bytes, converts them and sends the result back. A worker that dies fails that one tool call and nothing else.

## Choosing a mode

`conversion.mode` in the runtime's config:

| Mode | Where conversions run | Use it when |
|---|---|---|
| `inprocess` (default off Linux) | in the runtime | a single-user install, or you accept the risk |
| `local` (default on Linux) | children of the runtime, started and restarted by it | one host or one container; nothing else to deploy |
| `remote` | workers you run as their own containers | you want hard memory limits, no shared filesystem, and workers on other hosts |
| `local+remote` | both, in one pool | moving from `local` to `remote`, or keeping a floor of local capacity |

`local` is described in [CONFIGURATION.md §9n](CONFIGURATION.md#9n-conversion-workers-conversion). This guide is about `remote`.

## What a worker can and cannot reach

A worker is handed bytes and returns bytes. It is never handed a path, a tenant, a run or a credential. It holds no provider key, opens no store and mounts no volume; the runtime keeps every grant check, every fence, the inbox and every model call. So a worker container needs:

- the worker endpoint of each runtime it serves, and nothing else on the network;
- a writable scratch directory (`/scratch` in the image, a tmpfs) for the input and the helper programs' output;
- a memory limit. A file that exceeds it kills the worker, the runtime answers that one call with "this file crashed the converter", and the same file is refused for `quarantine_ttl_s` instead of being sent to the next worker.

## Running remote workers

1. Pick a token: `openssl rand -hex 32`. Put it in the runtime's environment as `LOOMCYCLE_WORKER_TOKEN` (with the runtime's other secrets) and give the workers the same value. The runtime refuses to start a network endpoint with no token or one under 32 characters.
2. Turn the endpoint on in the runtime's config:

   ```yaml
   conversion:
     mode: remote
   worker_endpoint:
     listen: 0.0.0.0:7788   # never the API port; reachable from the workers only
     tls:
       cert_file: /config/worker-endpoint.crt
       key_file: /config/worker-endpoint.key
   ```

   See [Encryption](#encryption) for the choice between TLS and a private network. A config written before `worker_endpoint` existed sets the same keys as `conversion.listen`, `conversion.tls` and `conversion.insecure`; they still work, and a value set in both places must be the same.

3. Start workers from `denngubsky/loomcycle-worker` (or `ghcr.io/denn-gubsky/loomcycle-worker`), the same release as the runtime:

   ```sh
   docker run -d --name lc-worker-1 \
     --memory 1g --read-only --tmpfs /scratch:size=1g,mode=0700,uid=65532,gid=65532 \
     --cap-drop ALL --security-opt no-new-privileges \
     -e LOOMCYCLE_WORKER_DISPATCHER=loomcycle:7788 \
     -e LOOMCYCLE_WORKER_TOKEN \
     -e LOOMCYCLE_WORKER_TLS_CA=/certs/ca.crt -v ./certs:/certs:ro \
     denngubsky/loomcycle-worker:<release>
   ```

[`deploy/workers/docker-compose.yaml`](../deploy/workers/docker-compose.yaml) is a complete example: the runtime and two workers, the workers on an internal network that reaches the runtime's endpoint and nothing else.

Capacity is the number of workers. A worker runs one job at a time by default (`LOOMCYCLE_WORKER_SLOTS`), so a worker that dies takes exactly the job that killed it. Start more workers for more capacity; a new one takes work the moment it attaches. The runtime never dials a worker and needs no list of them.

## Several runtime replicas

A worker attaches to every runtime it is given and holds an idle stream to each, but runs only its own slots' worth of jobs in all: a runtime whose job arrives while the worker is full is told so, and its job waits for the next free worker. Give the worker either a list or one name:

- `LOOMCYCLE_WORKER_DISPATCHER=core-a:7788,core-b:7788`
- `LOOMCYCLE_WORKER_DISPATCHER=dns:///loomcycle-workers.internal:7788` — every address the name resolves to is one runtime, looked up again every 30 seconds. This is the shape of a Kubernetes headless service in front of the runtime replicas.

Each runtime replica keeps its own quarantine: a file that killed a worker is refused by the replica that sent it, and could kill one more worker through another replica.

## Helper programs

The worker image ships ffmpeg, poppler, Typst (with the Noto Sans, Noto Sans Mono and Noto Serif fonts; Noto Serif is for a house style that asks for serif type) and merman (`merman-cli`, which draws Mermaid diagrams without a browser), and sets these commands:

| Variable | Default in the image | The runtime setting it stands for |
|---|---|---|
| `LOOMCYCLE_WORKER_MEDIA_COMMAND` | `ffmpeg -nostdin -i {in} -ac 1 -ar 16000 -f wav {out}` | `transcription.stt.media.command` |
| `LOOMCYCLE_WORKER_RASTERIZE_COMMAND` | `pdftoppm -r 150 -png {in} {out_prefix}` | `transcription.ocr.rasterize.command` |
| `LOOMCYCLE_WORKER_ENCODE_COMMAND` | `ffmpeg -nostdin -i {in} -c:a libopus -b:a 32k {out}` | `speech.encode.command` |
| `LOOMCYCLE_WORKER_WRITE_COMMAND` | `typst compile --root {dir} {in} {out}` | none: a Document rendered as a PDF (`File op=render format: pdf`) |
| `LOOMCYCLE_WORKER_MERMAID_COMMAND` | unset: `merman-cli render -q -s 2 -o {out} {in}` | none: a Mermaid diagram drawn as a picture in a rendered file |

A worker reads its commands from its own environment, never from the runtime: a job says what to convert, not what to run. The runtime's own setting still decides whether a conversion is wanted at all (an OCR pass renders pages only when `transcription.ocr.rasterize` is set, a recording is converted only when `transcription.stt.media` is), so set both. A worker offers a helper's jobs only when the program resolves on its `PATH`; when no attached worker has it, the call is refused as if the setting were unset. In `local` mode the runtime hands its own commands to its children, so there is one place to set them.

The PDF typesetter is the exception to "set both": a PDF render has no runtime setting, and a worker takes PDF renders whenever `typst` is on its `PATH` (or `LOOMCYCLE_WORKER_WRITE_COMMAND` names another program). It is handed `{in}`, the Typst source the runtime wrote (`main.typ`, which reads its pictures from `images/` beside it and imports nothing), `{out}`, the PDF to write, and `{dir}`, the job's folder. The runtime checks what comes back: a PDF that opens, of at most 2000 pages and 64 MiB. A worker built before PDF rendering never receives a PDF job. In `local` mode, and in process, the runtime runs `typst` from its own `PATH`; where it is missing, a PDF render answers that the server cannot write PDF.

Diagrams work the same way. A worker draws them whenever `merman-cli` is on its `PATH`, with `merman-cli render -q -s 2 -o {out} {in}`: a PNG at twice the diagram's size, which every format shows at half its pixel size so it stays sharp. `LOOMCYCLE_WORKER_MERMAID_COMMAND` replaces that command with your own: `{in}` is the diagram's source and `{out}` the PNG to write. The runtime checks what comes back: a PNG that decodes whole, at most 8 MiB and 4096 px a side. A diagram that does not draw, or whose picture fails the check, stays code in the file and the render goes on; with no worker carrying a renderer, every diagram stays code. One render draws at most 200 diagrams, each within 10 seconds and all within 2 minutes; after two diagrams run out of time the rest stay code. In `local` mode, and in process, the runtime runs `merman-cli` from its own `PATH`.

The helpers run as they do in the runtime: no shell, a scrubbed environment, a timeout, in the worker's scratch directory. `LOOMCYCLE_HELPER_WRAPPER` confines them further, as it does in the runtime.

## Encryption

Everything a worker converts crosses the worker endpoint, and so does the token. The runtime refuses a `listen` address off its own loopback unless one of these is set:

- **`worker_endpoint.tls`**: the endpoint's certificate and key. Workers set `LOOMCYCLE_WORKER_TLS_CA` to the CA that signed it (or `system`). Add `client_ca_file` to make every worker also present a client certificate (`LOOMCYCLE_WORKER_TLS_CERT`, `LOOMCYCLE_WORKER_TLS_KEY`), so a leaked token alone does not attach. For `dns:///` targets the certificate is checked against the name, so issue it for the service name.
- **`worker_endpoint.insecure: true`**: plain gRPC. Use it only where no one else can read the traffic: a Docker network marked `internal`, a Kubernetes namespace with a NetworkPolicy, a WireGuard or Tailscale link. Workers then need `LOOMCYCLE_WORKER_INSECURE=1`, because a worker on its own refuses to send its token in the clear to another host.

In both cases, never publish the endpoint's port.

## Watching the pool

`/metrics` reports `loomcycle_conversion_workers`, `_slots`, `_idle_slots`, `_inflight`, `_waiting`, `_quarantined_files`, `loomcycle_conversion_jobs_total{kind,outcome}`, `loomcycle_conversion_worker_deaths_total` and `loomcycle_conversion_worker_respawns_total`. A rising `outcome="crashed"` count is files killing workers; `outcome="busy"` is callers turned away after `queue_wait_s` with every worker occupied; `outcome="no_worker"` is calls made while no worker was attached.

## Workspace workers

The same endpoint serves a second kind of worker (RFC GW): one that holds workspaces, directories where an agent runs commands and works on files, away from the runtime. Agents use them through the `Workspace` tool ([TOOLS.md](TOOLS.md)); a workspace belongs to the run that opened it and is removed from the worker's disk when that run ends. Set `LOOMCYCLE_WORKSPACE_COMMANDS` on the worker to limit which programs a command may start with. [WORKSPACES.md](WORKSPACES.md) is the guide: what a workspace is and is not, the agent grant, the tool, limits and troubleshooting.

On a single trusted machine, `workspace: {mode: local}` has the runtime start one workspace worker as its own child instead, on a private Unix socket with a per-boot token, serving one tenant (`workspace.local.tenant`; unset, the tenant a run of this install carries when it names none, the empty tenant in open mode); no endpoint and no minted token are needed. It runs commands as the runtime's own user, so a command can read the runtime's data, secrets files and environment. See [WORKSPACES.md](WORKSPACES.md#local-mode).

Turn it on in the runtime's yaml with `workspace: {mode: remote}` and a `worker_endpoint` (see [CONFIGURATION.md](CONFIGURATION.md#workspace-workers-workspace)). Mint the worker a token whose only scope is `workers:attach`, bound to the tenant it serves. A worker that serves every tenant is minted by an admin with `worker_shared: true`, and it must run an isolating engine: a shared worker on the `host` engine is refused at attach and exits, because there one tenant's commands could reach every other tenant's workspaces on its disk. A shared worker therefore runs the `container` engine (below). Then run, on the worker's machine:

```sh
LOOMCYCLE_WORKER_DISPATCHER=runtime.internal:7788 \
LOOMCYCLE_WORKER_TOKEN=<the workers:attach token> \
LOOMCYCLE_WORKSPACE_ROOT=/var/lib/loomcycle/workspaces \
loomcycle worker --role workspace
```

The worker reports `python` and `node` when it finds them on its `PATH`. A token that may not attach a worker (an admin token, the runtime's legacy secret, a token with other scopes) is refused, and the worker exits saying why. A token the runtime cannot verify, such as an unknown or revoked one, or one checked while the store is down, is refused for now: the worker logs it and retries with backoff, and is served only once the token verifies. The conversion workers' `LOOMCYCLE_WORKER_TOKEN` is not an operator token and never verifies. The runtime checks the token again every minute and detaches a worker whose token no longer holds.

A worker is known by its token's grant and its own name (`--id`, by default the host name): `tenant:<tenant>/<name>` or `shared/<name>`. The name must stay the same across restarts, so that the worker keeps its workspaces; a worker that came back under another name would have the runtime remove every workspace on its disk as unknown. Two workers on one host each need an `--id` of their own.

**A workspace's way out.** A workspace reaches the network only through named egress routes (RFC GW §6.3), and only through the runtime: the worker carries each request a session sends to `http://egress/<route>/<path>` to the runtime on an `Egress` stream over its own connection, and the runtime decides from the workspace's record which routes it holds, builds the upstream request, adds any credential and streams the answer back. The worker holds no route's URL and never a credential. The runtime accepts such a stream only from the token that attached the worker, for a workspace that is ready on that worker. [WORKSPACES.md](WORKSPACES.md) describes the routes.

**A personal worker** (RFC GW P13) is a user's own machine as a workspace worker: `loomcycle worker --role workspace --personal --root code=~/code`, attached with a token a tenant operator mints for that user (`POST /v1/_users/{subject}/tokens` with `{"purpose": "personal_worker"}`). It serves that user's runs only, and only workspaces opened on one of its roots, which are the user's folders themselves, never copies. The runtime takes one only with `workspace.personal_workers: true` and its worker endpoint on loopback. Its id is `user:<tenant>/<hash of the user>/<name>`. [WORKSPACES.md](WORKSPACES.md#a-personal-workers-roots) is the guide.

**The `host` engine has no isolation.** A command runs as the worker's user and can read anything that user can, the worker's own token included. Run it only on a machine you trust with every agent that may use it; for anything else use the `container` engine.

### The container engine

`LOOMCYCLE_WORKSPACE_ENGINE=container` runs each workspace's commands in a container of its own, its session (RFC GW P3). The worker drives a container daemon through its command line (`docker`, or `podman` with `LOOMCYCLE_WORKSPACE_CONTAINER_CLI=podman`); nothing in a session can reach the worker.

| A session has | Why |
|---|---|
| its workspace mounted at `/workspace`, and nothing else of the worker's disk | it cannot see another workspace; one fixed path keeps a Python environment correct after a fork or a template |
| a read-only root; a private `/tmp` (tmpfs, `LOOMCYCLE_WORKSPACE_CONTAINER_TMP_MB`) | nothing it writes outside `/workspace` survives the session |
| no network (`--network none`) | no address and no name is reachable; a lookup or a connection fails at once. Package routes come with egress (RFC GW P4) |
| every capability dropped, `no-new-privileges`, the worker's own non-root uid | a command cannot gain privileges; files it writes have the worker's owner |
| a memory limit with no swap past it, a CPU and a process limit | one workspace cannot starve the others |
| an environment the engine writes (`HOME=/workspace`, caches under it, `TMPDIR=/tmp`) | nothing of the worker's environment, its token included, reaches a command |

A session is started at a workspace's first command and started again when it is gone; files and anything installed into `/workspace` (a virtual environment, `node_modules`) stay, a running server and `/tmp` do not. A command past its timeout, or one the runtime stopped waiting for, is killed inside the session with everything in its process group; when that cannot be done the session is removed. A process that leaves its group lives until the workspace is released, which removes the session. At start, and when it stops, the worker removes the sessions it left.

The worker refuses to start with this engine as root (its sessions would run as root): run it as a user that may use the daemon (the `docker` group, or a rootless daemon). That user can start containers, which on most hosts is as good as root on the host, so give the worker a machine (or a VM) of its own; the sessions hold no such right. At start the worker runs one throwaway session to check the daemon, the runtime and the image, and reports the image's toolchains (`python3`, `node`) as its own.

| Variable | Default | |
|---|---|---|
| `LOOMCYCLE_WORKSPACE_CONTAINER_IMAGE` | — (required) | The session image: `denngubsky/loomcycle-workspace:<version>` (or `ghcr.io/denn-gubsky/loomcycle-workspace:<version>`), the workspace session image, published with each minor release after v1.165.0; use the runtime's own version (below). Any image with `/bin/sh` works. |
| `LOOMCYCLE_WORKSPACE_CONTAINER_RUNTIME` | the daemon's | The OCI runtime: `runc`, `runsc` (gVisor), a Kata runtime, or any name the daemon knows. gVisor or Kata put a kernel boundary under each session. |
| `LOOMCYCLE_WORKSPACE_CONTAINER_CLI` | `docker` | The container command. |
| `LOOMCYCLE_WORKSPACE_CONTAINER_MEMORY_MB` | 2048 | A session's memory, 64–1048576 MiB; no swap beyond it. |
| `LOOMCYCLE_WORKSPACE_CONTAINER_CPUS` | 2 | A session's CPUs, 0.01–1024. |
| `LOOMCYCLE_WORKSPACE_CONTAINER_PIDS` | 512 | A session's processes, 16 and up. |
| `LOOMCYCLE_WORKSPACE_CONTAINER_TMP_MB` | 256 | The size of a session's `/tmp`, counted in its memory. |
| `LOOMCYCLE_WORKSPACE_CONTAINER_HOST_ROOT` | the root | `LOOMCYCLE_WORKSPACE_ROOT` as the daemon sees it, when the worker itself runs in a container and drives the host's daemon through its socket. |
| `LOOMCYCLE_WORKSPACE_CONTAINER_EGRESS_DIR` | unset | Gives each session a way out through the worker (RFC GW P4): a short absolute directory where the worker keeps one Unix socket per workspace. Unset, a session has no network. Each session then joins a Docker network of its own (`--internal`, no host address on its bridge) shared only with a gateway container that answers as `egress` and forwards to that workspace's socket; no other name resolves. Needs a Linux daemon: Docker Desktop on macOS cannot connect a container to a host socket. Until the core's egress link lands, every request is answered 503. |
| `LOOMCYCLE_WORKSPACE_CONTAINER_EGRESS_HOST_DIR` | the directory | The egress directory as the daemon sees it, for a worker that runs in a container itself. |
| `LOOMCYCLE_WORKSPACE_CONTAINER_EGRESS_IMAGE` | the session image | The gateway's image; it needs `python3`. |

On the `host` engine:

| Variable | Default | |
|---|---|---|
| `LOOMCYCLE_WORKSPACE_HOST_EGRESS` | unset | `1` gives each workspace of a host-engine worker a loopback port through which its commands reach the egress routes the workspace holds (RFC GW P4): `LOOMCYCLE_EGRESS=http://127.0.0.1:<port>/<secret>/`, with pip and npm pointed at it, as a container session's are at `http://egress/`. The port answers only a request whose `Host` is its own `127.0.0.1:<port>` and whose path starts with the secret, a random value made each time the port opens and given to the workspace's commands only in their environment; anything else gets a bare 403. That keeps out a web page reaching the port through DNS rebinding and other users' processes. **It is not a boundary:** the host engine's commands have the host's own network anyway, and every process of the worker's own user can read a command's environment, the other workspaces on that worker included, and so send requests as the workspace, through its routes, including one that sends a credential. Turn it on only on a machine where the agents are trusted. Ignored by a container worker. A local worker (`workspace.mode: local`) takes it from the runtime's environment. |

**Git mirrors** (RFC GW P11, on either engine). A git workspace is a working tree over a bare mirror the worker keeps per (tenant, repository) under `LOOMCYCLE_WORKSPACE_ROOT/.loomcycle-mirrors`, which a container session sees read-only at `/mirror/objects`. Each workspace pins the commits it names in the mirror; its pins go with it. `LOOMCYCLE_WORKSPACE_MIRROR_IDLE_S` (default 3600, 1 to 2592000) is how long a mirror no workspace pins is kept after its last use; the worker sweeps them. A local worker gets `workspace.git.mirror_idle_s`. A mirror's size cap is the runtime's (`workspace.git.mirror_max_mb`), sent with each fetch.

The disk quota (`workspace.disk_quota_mb`) is kept as on `host`: the worker measures a workspace after each command and refuses further writes and commands while it is over. A container gives no cheap hard limit on a mounted directory (`--storage-opt size` limits a container's own layer, and only on xfs with project quotas), so a single command can still pass the quota while it runs. A workspace's files are still converted by the worker's conversion child (D34), not in the session; the session image carries the same helpers for an agent to run itself.

When a worker attaches it lists the workspaces on its disk, and the runtime tells it which to delete: those released while it was away, those the runtime does not know, and those marked lost whose run has ended. A lost workspace whose run is still going is put back in service. `/metrics` reports `loomcycle_workspace_workers`, `loomcycle_workspace_ops_total{op,outcome}` and `loomcycle_workspace_worker_deaths_total`.

### What a workspace worker converts

A file in a workspace is converted on its workspace's worker, never in the runtime (RFC GW D34): `File op=extract` (its text layer and pictures) and the HTML read of `File op=render`. The worker does not parse the file itself. At start it runs one conversion worker as its own child, exactly as the runtime does in conversion `local` mode: this binary run as `loomcycle worker`, on a private Unix socket with a token made at each start, with a memory limit (`LOOMCYCLE_WORKSPACE_CONVERT_MEMORY_LIMIT_MB`, default 1024; the runtime's local workspace worker gets `workspace.convert.memory_limit_mb`), restarted with backoff and ending when the worker does. A file that kills the child fails that one call; the worker and its workspaces go on, and the runtime refuses that file (by its sha256) in that workspace for `conversion.quarantine_ttl_s`. The worker checks the file still hashes to what the runtime checked before it converts it, and never follows a link.

The child reads every format loomcycle reads in pure Go. Helper programs are used when they are on the worker's `PATH`, with the `LOOMCYCLE_WORKER_*_COMMAND` settings above read from the worker's own environment (the local workspace worker gets the runtime's own helper settings): DjVuLibre (`djvutxt`, `djvused`, `ddjvu`) for DjVu, poppler, ffmpeg, Typst and merman-cli for what they do in a conversion worker. The worker reports what its child has as labels when it attaches, and a conversion that needs a helper it lacks is refused with `conversion_helper_missing`, naming the program to install. With a result written into a workspace on the same worker, nothing of the file or the result crosses to the runtime; into the inbox, or a workspace on another worker, only the result does.

OCR and speech-to-text of a workspace's file are split between the worker and the runtime, because they call a model with a provider key and a key never reaches a worker. The worker does the byte work in its child and sends back only what the model reads; the runtime calls the model, under the run's page and minute caps, and records the usage. No op the runtime sends a worker names the provider or the model in its arguments, and no provider key or credential ever reaches a worker. The provenance line of Markdown written into a workspace names the engine, the provider and the model, as a result in the inbox does: a model's name is not a secret.

- **OCR** (`File op=extract` with OCR): the worker cuts the pages that need OCR out of the PDF (in pure Go, no helper) for a model that reads PDFs, or renders each to a PNG for a model that reads images, which needs poppler's `pdftoppm` (`LOOMCYCLE_WORKER_RASTERIZE_COMMAND`). With the Markdown in a workspace on the same worker, the runtime sends back only the OCR text and the worker writes it into the Markdown in place of its pages. OCR of a DjVu book in a workspace is not done yet: its thin pages are reported.
- **Speech-to-text** (`File op=transcribe`): the worker converts the recording with ffmpeg (`LOOMCYCLE_WORKER_MEDIA_COMMAND`) into the WAV the runtime cuts and sends; the runtime holds that WAV in a file of its conversion spool directory for the length of the call, never in memory.

So a workspace worker that should OCR scans and transcribe recordings needs poppler and ffmpeg on its `PATH` (and DjVuLibre to read DjVu at all); the loomcycle-worker image carries them. Without one, the call is refused with `conversion_helper_missing`, naming it.

### The workspace session image

`denngubsky/loomcycle-workspace` (and `ghcr.io/denn-gubsky/loomcycle-workspace`), built from `Dockerfile.workspace`, is the image a workspace's commands run in when the worker runs the `container` engine (`LOOMCYCLE_WORKSPACE_ENGINE=container`, RFC GW P3): one container of it per workspace, with a read-only root, no capabilities, no network and the workspace at `/workspace`. It is not the worker's own image: it holds no loomcycle binary, and the worker's conversions still run in the worker's conversion child above, not in it. It is published at each minor release after v1.165.0, for amd64 and arm64, tagged with the release's version and `latest`; set `LOOMCYCLE_WORKSPACE_CONTAINER_IMAGE` to the tag of the runtime's own version, so a session has what that release's guides promise.

It carries what a general workspace needs without a download (RFC GW §4.4):

- Python 3 with `pip` and `venv`; Node.js with `npm`; `git`.
- poppler-utils, ffmpeg, DjVuLibre and Typst (with Noto Sans, Noto Sans Mono and Noto Serif), for a script that works on documents and media.
- The fixed library set. Python: numpy, pandas, matplotlib (drawing without a display, `MPLBACKEND=Agg`), openpyxl, python-docx, pypdf, Pillow, lxml, PyYAML. Node: express (a web server) and esbuild (a bundler).

Every package comes from Debian's archive (trixie), so building the image downloads nothing from PyPI or npm. Anything else an agent needs it installs into its workspace (a virtual environment, `node_modules`), never into the image. The image is about 1.3 GB unpacked; an operator who needs less builds a slimmer one and names it in the worker's settings.

Its `browser` target (`docker build -f Dockerfile.workspace --target browser .`) is the same image plus Chromium, for `Workspace op=screenshot` ([WORKSPACES.md](WORKSPACES.md#looking-at-a-picture)). A container-engine worker whose session image has `chromium` finds it at its start and reports the `browser` label; only such a worker takes screenshots. It is a second image, so a general workspace stays the size it is.

## Versions

A runtime refuses a worker that speaks another worker protocol (the workspace worker protocol is at version 2 since the egress stream) and logs a warning for a worker of another build. Upgrade the runtime and the workers together; a worker that is refused exits with a message naming both versions.
