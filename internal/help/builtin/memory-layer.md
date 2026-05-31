---
name: memory-layer
description: The Memory tool's add / recall ops — the LLM-extract memory-layer paradigm (mem9), and how it differs from key/value and vector search (RFC K).
---
loomcycle's `Memory` tool has three retrieval paradigms, not one.
Most agents only need the first two; `add` / `recall` exist for the
third — an **external memory layer** like Mem9 that runs its own LLM
to distil conversations into durable facts.

| You want… | Op | Backend |
|---|---|---|
| "what did I store under this exact key?" | `get` / `list` | any |
| "what notes are *close* to X?" | `search` (`embed: true` on `set`) | any with an embedder |
| "remember this conversation; recall what you learned" | `add` / `recall` | memory-layer only (`memory_backend` kind=mem9) |

## What a memory layer is

A key/value store is faithful: you `set` a key, you `get` exactly
that value back. A vector store adds similarity ranking over rows you
wrote. A **memory layer** is a different contract entirely: you hand
it conversation messages, and the backend *itself* decides what is
worth remembering — running an LLM to extract, merge, update, or drop
facts. There are no caller keys; identity is server-assigned. The
whole point is that the backend is smarter than a dictionary.

That is why `add` / `recall` are a separate capability and not just
new ops on the flat store. The default in-process backend is a
key/value + vector store, **not** a memory layer — so against it,
`add` / `recall` refuse with `capability_unsupported` rather than
silently faking it. Wire a `memory_backend` of a memory-layer kind
(today: `mem9`) to the agent to enable them.

## add — ingest a conversation

```json
{"op": "add", "scope": "user",
 "messages": [
   {"role": "user", "content": "I prefer dark mode and I'm based in Berlin"},
   {"role": "assistant", "content": "Noted."}
 ]}
```

- `infer` (default **true**) asks the backend to LLM-extract durable
  facts from the messages. Pass `infer: false` to store them verbatim.
- `metadata` is opaque key/value context attached to the ingestion.
- The result is `{"status": "pending" | "done", "event_id"?}`.
  **A memory-layer add is frequently asynchronous** — the backend
  returns before extraction finishes. Do **not** assume
  read-after-write: a `recall` immediately after an `add` may not see
  the new facts yet. `event_id`, when present, is a correlation handle.

## recall — semantic search over extracted facts

```json
{"op": "recall", "scope": "user", "query": "ui preferences", "top_k": 5}
```

Returns ranked facts the backend has distilled, each with a
server-assigned `id`, the `memory` text, and a 0..1 `score`:

```json
{"facts": [{"id": "uuid-1", "memory": "user prefers dark mode", "score": 0.91}]}
```

- `top_k` defaults to 10, capped at 50.
- `threshold` (0..1) is a relevance floor; facts below it are dropped.
  0 means "use the backend's default".

Unlike `search`, the results are *derived facts* with opaque IDs, not
the rows you wrote — you cannot `get` them back by key.

## Scope, tenancy, and failure modes

`add` / `recall` honor the agent's `memory_scopes` exactly like every
other op: `scope: agent` is keyed to this agent, `scope: user` to the
run's `user_id`. The mem9 backend then namespaces that under its
tenant prefix, so one tenant can neither write into nor recall from
another's facts.

- **`capability_unsupported`** — the resolved backend is not a memory
  layer (e.g. the default in-process store). Wire a `mem9`
  `memory_backend`.
- **`fallback_on_error: inprocess` is mutually exclusive with add /
  recall.** The in-process fallback is a key/value store and cannot
  honor a semantic `add` / `recall`, so a fallback-wrapped mem9
  backend reports `capability_unsupported` for these ops — fail-closed
  by design, never a silent degrade to a store that would lose the
  facts. Use fallback for the six key/value ops, or the memory layer
  for `add` / `recall`, but not both on one backend.

See `MEMORY-BACKENDS.md` for wiring a `memory_backend`, and the
`vector-memory` / `memory-ranking` topics for the `search` paradigm.
