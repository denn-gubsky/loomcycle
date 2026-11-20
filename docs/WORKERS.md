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

Turn it on in the runtime's yaml with `workspace: {mode: remote}` and a `worker_endpoint` (see [CONFIGURATION.md](CONFIGURATION.md#workspace-workers-workspace)). Mint the worker a token whose only scope is `workers:attach`, bound to the tenant it serves. A worker that serves every tenant is minted by an admin with `worker_shared: true`, and it must run an isolating engine: a shared worker on the `host` engine is refused at attach and exits, because there one tenant's commands could reach every other tenant's workspaces on its disk. Until the container engine arrives (RFC GW P3), shared workspace workers cannot be used; run one tenant-bound worker per tenant. Then run, on the worker's machine:

```sh
LOOMCYCLE_WORKER_DISPATCHER=runtime.internal:7788 \
LOOMCYCLE_WORKER_TOKEN=<the workers:attach token> \
LOOMCYCLE_WORKSPACE_ROOT=/var/lib/loomcycle/workspaces \
loomcycle worker --role workspace
```

The worker reports `python` and `node` when it finds them on its `PATH`. A token that may not attach a worker (an admin token, the runtime's legacy secret, a token with other scopes) is refused, and the worker exits saying why. A token the runtime cannot verify, such as an unknown or revoked one, or one checked while the store is down, is refused for now: the worker logs it and retries with backoff, and is served only once the token verifies. The conversion workers' `LOOMCYCLE_WORKER_TOKEN` is not an operator token and never verifies. The runtime checks the token again every minute and detaches a worker whose token no longer holds.

A worker is known by its token's grant and its own name (`--id`, by default the host name): `tenant:<tenant>/<name>` or `shared/<name>`. The name must stay the same across restarts, so that the worker keeps its workspaces; a worker that came back under another name would have the runtime remove every workspace on its disk as unknown. Two workers on one host each need an `--id` of their own.

**The `host` engine has no isolation.** A command runs as the worker's user and can read anything that user can, the worker's own token included. Run it only on a machine you trust with every agent that may use it; isolated engines come in a later phase.

When a worker attaches it lists the workspaces on its disk, and the runtime tells it which to delete: those released while it was away, those the runtime does not know, and those marked lost whose run has ended. A lost workspace whose run is still going is put back in service. `/metrics` reports `loomcycle_workspace_workers`, `loomcycle_workspace_ops_total{op,outcome}` and `loomcycle_workspace_worker_deaths_total`.

## Versions

A runtime refuses a worker that speaks another worker protocol and logs a warning for a worker of another build. Upgrade the runtime and the workers together; a worker that is refused exits with a message naming both versions.
