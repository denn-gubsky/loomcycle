# Conversion workers

loomcycle converts files: it extracts a document's text, cuts and renders PDF pages for OCR, turns a recording into a WAV for speech-to-text, encodes synthesized speech, and re-encodes images a vision model must have scaled. These are the jobs whose memory is set by what someone put in a file. One Office document at every extraction cap can take about 700 MiB, and a hostile one is written to take more.

By default every conversion runs inside the runtime's own process. A file that exhausts memory then takes every run on the replica with it. **Conversion workers** move the conversions out (RFC GO): a worker is the same binary, run as `loomcycle worker`, that receives a file's bytes, converts them and sends the result back. A worker that dies fails that one tool call and nothing else.

## Choosing a mode

`conversion.mode` in the runtime's config:

| Mode | Where conversions run | Use it when |
|---|---|---|
| `inprocess` (default) | in the runtime | a single-user install, or you accept the risk |
| `local` | children of the runtime, started and restarted by it | one host or one container; nothing else to deploy |
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
     listen: 0.0.0.0:7788   # never the API port; reachable from the workers only
   ```

3. Start workers from `denngubsky/loomcycle-worker` (or `ghcr.io/denn-gubsky/loomcycle-worker`), the same release as the runtime:

   ```sh
   docker run -d --name lc-worker-1 \
     --memory 1g --read-only --tmpfs /scratch:size=1g,mode=0700,uid=65532,gid=65532 \
     --cap-drop ALL --security-opt no-new-privileges \
     -e LOOMCYCLE_WORKER_DISPATCHER=loomcycle:7788 \
     -e LOOMCYCLE_WORKER_TOKEN \
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

The worker image ships ffmpeg and poppler, and sets these commands:

| Variable | Default in the image | The runtime setting it stands for |
|---|---|---|
| `LOOMCYCLE_WORKER_MEDIA_COMMAND` | `ffmpeg -nostdin -i {in} -ac 1 -ar 16000 -f wav {out}` | `transcription.stt.media.command` |
| `LOOMCYCLE_WORKER_RASTERIZE_COMMAND` | `pdftoppm -r 150 -png {in} {out_prefix}` | `transcription.ocr.rasterize.command` |
| `LOOMCYCLE_WORKER_ENCODE_COMMAND` | `ffmpeg -nostdin -i {in} -c:a libopus -b:a 32k {out}` | `speech.encode.command` |

A worker reads its commands from its own environment, never from the runtime: a job says what to convert, not what to run. The runtime's own setting still decides whether a conversion is wanted at all (an OCR pass renders pages only when `transcription.ocr.rasterize` is set, a recording is converted only when `transcription.stt.media` is), so set both. A worker offers a helper's jobs only when the program resolves on its `PATH`; when no attached worker has it, the call is refused as if the setting were unset. In `local` mode the runtime hands its own commands to its children, so there is one place to set them.

The helpers run as they do in the runtime: no shell, a scrubbed environment, a timeout, in the worker's scratch directory. `LOOMCYCLE_HELPER_WRAPPER` confines them further, as it does in the runtime.

## The endpoint is not encrypted

The worker endpoint speaks gRPC without TLS. The token keeps strangers from attaching, but anyone who can read the traffic sees the files sent for conversion. Keep the endpoint on a network only the runtime and its workers share: a Docker internal network, a Kubernetes namespace with a NetworkPolicy, a WireGuard or Tailscale link between hosts. Never publish its port.

## Watching the pool

`/metrics` reports `loomcycle_conversion_workers`, `_slots`, `_idle_slots`, `_inflight`, `_waiting`, `_quarantined_files`, `loomcycle_conversion_jobs_total{kind,outcome}`, `loomcycle_conversion_worker_deaths_total` and `loomcycle_conversion_worker_respawns_total`. A rising `outcome="crashed"` count is files killing workers; `outcome="busy"` is callers turned away after `queue_wait_s` with every worker occupied; `outcome="no_worker"` is calls made while no worker was attached.

## Versions

A runtime refuses a worker that speaks another worker protocol and logs a warning for a worker of another build. Upgrade the runtime and the workers together; a worker that is refused exits with a message naming both versions.
