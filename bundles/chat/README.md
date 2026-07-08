# chat bundle

Two general-purpose **conversational agents** for direct chatting in the
loomcycle interactive `/run` terminal:

| Agent | Routing | Use it for |
|---|---|---|
| **`chat/medium`** | `tier: middle` | Everyday chat that routes per your `provider_priority` (cloud-capable, falls back across providers). |
| **`chat/local`** | pinned `model: local-medium` → **ollama-local** | Private / offline chat that runs entirely on your local model and never leaves the box. |

This is a **first-class, reusable bundle** — not an `examples/` experiment.

```
bundles/chat/
├── loomcycle.yaml   # the chat/medium + chat/local agents (identical to the embedded copy)
└── README.md
```

The binary ships an **embedded** copy at
[`cmd/loomcycle/embedded/bundles/chat.yaml`](../../cmd/loomcycle/embedded/bundles/chat.yaml)
(`go:embed`). `bundles/chat/loomcycle.yaml` is the in-repo source mirror — keep the
two identical (`chat` has no split-out skills, so they are byte-for-byte the same).

## Enable

```sh
# the bundle carries no provider matrix — pair it with `base`
LOOMCYCLE_PRESETS=base,chat
```

then start an interactive run with `chat/medium` or `chat/local` from the `/run` terminal.

## Toolset

Both agents get: `Read, Write, Edit, Grep, Glob, WebSearch, WebFetch, Bashbox,
Bash, Document, Memory, Path, Skill` (plus `Context`, auto-added).

### Requirements

- **A read-write volume.** The agents declare no `volumes:`, so they use the
  operator's **default** volume — define one with `default: true, mode: rw` (or
  bind one per agent in your overlay). Without it, the file/shell tools are
  registered-but-idle.
- **`LOOMCYCLE_SQLMEM_ENABLED=1`** — Document (and SQL-backed Memory) need it.
- **`BRAVE_API_KEY`** + **`LOOMCYCLE_HTTP_HOST_ALLOWLIST`** — for WebSearch/WebFetch.
- **`LOOMCYCLE_BASHBOX_ENABLED=1`** (and `LOOMCYCLE_BASH_ENABLED=1` for raw Bash).
- **`chat/local` context window** is the GLOBAL `LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX`
  env (e.g. `131072` for 128K) — it is not a per-agent yaml field. To force GPU
  offload on a box where Ollama falls back to CPU, set
  `LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU=99`.

## Overriding

The operator overlay can re-declare any field (last layer wins per field) — swap
the tier, change the model, adjust sampling, narrow `tools`. It cannot
**widen** `tools` beyond what the bundle grants.
