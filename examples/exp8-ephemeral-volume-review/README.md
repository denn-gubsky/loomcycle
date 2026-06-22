# exp8 — self-contained code review fan-out (RFC AH Phase 2b ephemeral volume)

An in-loomcycle **dispatcher agent** creates an ephemeral run-scoped volume, clones a Go
repository into it, fans out **8 read-only reviewers** via `Agent op=parallel_spawn`, then a
consolidator merges the findings and writes a report. The ephemeral volume **auto-purges** when
the dispatcher run ends — no `rm -rf` needed.

```
 POST /v1/runs → exp8-dispatcher (rw default + dynamic-root)
   ├─ VolumeDef op=create name="lc-src" mode=rw ephemeral=true  → <EPHEMERAL_PATH>
   ├─ Bash: git clone https://github.com/denn-gubsky/loomcycle <EPHEMERAL_PATH>/loomcycle
   ├─ Agent op=parallel_spawn: 8 × exp8-reviewer (inherit ephemeral vol, Read/Grep/Glob only)
   │    each writes Memory key review:<slice>:findings (confidence ≥ 80)
   └─ Agent op=spawn: exp8-consolidator → reads Memory → Write work/exp8/review-report.md
 ← run ends → ephemeral volume auto-purged (work/dynamic/_ephemeral/ is empty)
```

## What it demonstrates

| Primitive | Role |
|---|---|
| **`VolumeDef op=create ephemeral=true`** | Phase 2b: create a run-scoped rw volume; auto-purged when the creating run ends |
| **`Agent op=parallel_spawn`** | in-loomcycle fan-out — spawns N sub-agents with a join barrier (no MCP round-trip) |
| **Spawn narrowing** | sub-agents (reviewers) inherit the dispatcher's volume bindings including `lc-src` |
| **`volume=` parameter** | reviewer Read/Grep/Glob calls address `lc-src` explicitly by name |
| **`Write` to default volume** | consolidator writes to the default rw volume (`./work/exp8/`) |
| **`Memory` user scope** | findings ledger — `review:<slice>:findings` per reviewer |
| **`Context op=self`** | each reviewer confirms `lc-src` is in its bound volume list |

### Contrast with exp7

exp7 uses an **external** MCP fan-out (`spawn_runs`) over a **pre-cloned static ro volume** —
the orchestrator drives the fan-in barrier and the repo lives outside loomcycle. exp8 is
**self-contained**: loomcycle owns the full lifecycle (provision → clone → review → purge).

> **Use exp7** when the repo is large/shared or you need a pre-existing workspace.
> **Use exp8** when you want zero-setup, zero-cleanup on-demand code review.

## Prerequisites

- **loomcycle ≥ v1.0.0** on PATH (or `LOOMCYCLE_BIN`). Check: `loomcycle --version`.
- **A provider** — Anthropic OAuth (`loomcycle anthropic login`, kept enabled by `run.sh`) or
  `DEEPSEEK_API_KEY` in `.env.local`.
- **`git`** on PATH (the dispatcher clones from GitHub).
- Internet access to `github.com/denn-gubsky/loomcycle`.

Fully self-contained — no external services, no MCP servers to install, no pre-cloned repo.

## Step 1 — start the server

```bash
./run.sh serve
# (or: ./run.sh serve --log-level debug)
```

On first run, `run.sh` copies `.env.local.example` → `.env.local`. Edit it to add your provider.

Verify the server is up:

```bash
./loomcurl.sh http://127.0.0.1:8787/healthz
```

## Step 2 — trigger the dispatcher

The `/v1/runs` endpoint streams **Server-Sent Events** (SSE, `text/event-stream`). The run
stays alive as long as the HTTP connection is open — disconnecting cancels the run. Use a
persistent SSE client, not a one-shot curl:

```bash
# Simple trigger (stays connected; prints events until completed):
python3 - <<'EOF'
import json, socket, time

HOST, PORT = "127.0.0.1", 8787
TOKEN = ""  # set if LOOMCYCLE_AUTH_TOKEN is non-empty in .env.local

payload = json.dumps({
    "agent": "exp8-dispatcher",
    "user_id": "exp8",
    "segments": [{"role": "user", "content": [{"type": "trusted-text", "text":
        "Run the full code review fan-out. Clone denn-gubsky/loomcycle, "
        "fan out 8 reviewers, consolidate, write exp8/review-report.md."
    }]}]
})

req = (
    f"POST /v1/runs HTTP/1.1\r\nHost: {HOST}:{PORT}\r\n"
    f"Content-Type: application/json\r\nAccept: text/event-stream\r\n"
    + (f"Authorization: Bearer {TOKEN}\r\n" if TOKEN else "")
    + f"Content-Length: {len(payload)}\r\n\r\n{payload}"
)

sock = socket.create_connection((HOST, PORT), timeout=30)
sock.sendall(req.encode())
sock.settimeout(900)  # 15 min max
buf, in_body = b"", False
while True:
    chunk = sock.recv(4096)
    if not chunk: break
    buf += chunk
    if not in_body:
        if b"\r\n\r\n" in buf:
            h, _, buf = buf.partition(b"\r\n\r\n")
            print(h.split(b"\r\n")[0].decode())
            in_body = True
        continue
    while b"\n" in buf:
        line, buf = buf.split(b"\n", 1)
        line = line.rstrip(b"\r").decode(errors="replace")
        if line: print(line)
        if line.startswith("event: completed"): break
    else: continue
    break
sock.close()
EOF
```

The run takes ~3–5 minutes (8 concurrent reviewers + consolidator).

## Step 3 — verify ephemeral purge (F-V1)

After the run completes:

```bash
ls work/dynamic/_ephemeral/   # must be empty — volume auto-purged
cat work/exp8/review-report.md
```

The report lands at `work/exp8/review-report.md` (the consolidator's Write resolves against the
default rw volume root = `./work`).

## Verification checklist

| Check | Expected |
|---|---|
| **F-V1** `ls work/dynamic/_ephemeral/` after run | empty — auto-purge confirmed |
| **F-V2** reviewer logs `Context op=self` | `lc-src` listed in bound volumes |
| **F-V3** reviewer Glob/Read/Grep with `volume="lc-src"` | files resolved correctly |
| **F-V4** `work/exp8/review-report.md` exists | consolidator Write succeeded |

## Troubleshooting

**Dispatcher fails at git clone**: check internet access and that `git` is on PATH.

**"dynamic-root volume not found"**: `run.sh` creates `work/dynamic/` automatically; if you
started the server without `run.sh`, create it manually and restart.

**Reviewers see 0 files**: the slice path may be wrong. Check that `loomcycle/internal/api` etc.
exist inside the cloned repo. The EPHEMERAL_PATH printed by the dispatcher is the volume root.

**Run cancelled immediately**: the SSE connection closed before the run finished. Use the Python
client above (not curl) — curl closes the connection as soon as it flushes output.
