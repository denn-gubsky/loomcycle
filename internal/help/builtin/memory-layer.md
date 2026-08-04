---
name: memory-layer
description: The Memory tool's add / recall ops — how the default backend now serves them (add enqueues for background consolidation; recall is semantic search), and how they differ from key/value and vector search.
---
loomcycle's `Memory` tool has three retrieval paradigms, not one. All
three now work on the **default in-process backend** — `add` / `recall`
no longer require a special external backend.

| You want… | Op | Needs |
|---|---|---|
| "what did I store under this exact key?" | `get` / `list` | any backend |
| "what notes are *close* to X?" | `search` (`embed: true` on `set`) | an embedder + a vector-capable store |
| "remember this conversation; recall what you learned" | `add` / `recall` | `add`: any backend · `recall`: the vector stack |

## Calling them (the exact shape)

Both ops take a `scope`, and `recall` also requires a `query`. Omitting either
is the most common way an agent stalls here, so:

```
Memory op=recall scope=user query="which medicine does the user take"
Memory op=add    scope=user messages=[{"role":"user","content":"..."}]
```

`recall` is the ONLY semantic search over remembered facts. Two things it is
often confused with:

- **`Document graph_recall` is not a substitute.** It walks *relations* out from
  facts that already carry entity metadata, so on a store without those it
  returns `seeds: 0` and tells you nothing — which reads like "there is nothing
  remembered" when there is. Use it *after* `recall` hands you a fact worth
  expanding.
- **Document TEXT is a different surface.** Chunk bodies live in the same
  keyspace under a reserved `doc.chunk:` prefix, and the way to search them on
  purpose is `Memory op=search scope=user prefix="doc.chunk:" query="..."`.
  Because they share the keyspace, `recall` currently *can* surface a chunk body
  alongside consolidated facts — a `doc.chunk:` id in a recall result is document
  prose, not something the user told you, so weigh it accordingly.

If you are unsure of any tool's arguments, `Context op=doc name=Memory` prints
the schema rather than making you guess from an error message.

## What the memory-layer paradigm is

A key/value store is faithful: you `set` a key, you `get` exactly that
value back. A vector store adds similarity ranking over rows you wrote.
The **memory-layer** paradigm (`add` / `recall`) is a different contract:
you hand it conversation messages and let the system decide what is worth
remembering as durable facts — you don't choose keys, and identity is
server-assigned.

On the default backend this is **background consolidation**: `add`
enqueues the messages onto a durable queue and returns immediately; a
scheduled consolidator (when configured) later distils them into durable
facts. `recall` is then a semantic search over stored memories. An
external memory-layer backend may instead extract facts server-side with
its own LLM — same `add` / `recall` contract, different engine.

## add — ingest a conversation

```json
{"op": "add", "scope": "user",
 "messages": [
   {"role": "user", "content": "I prefer dark mode and I'm based in Berlin"},
   {"role": "assistant", "content": "Noted."}
 ]}
```

- `infer` (default **true**) hands the messages to the memory layer for
  consolidation — on the default backend they are enqueued for the
  background consolidator. Pass `infer: false` to store the joined turns
  verbatim as one row immediately.
- `metadata` is opaque key/value context attached to the ingestion.
- The result is `{"status": "pending" | "done", "event_id"?}`.
  **`infer: true` is asynchronous** — it returns `pending` before any
  consolidation runs. Do **not** assume read-after-write: a `recall`
  immediately after an `add` will not see the new facts until they are
  consolidated. `event_id`, when present, is a correlation handle.

## recall — semantic search over stored memories

```json
{"op": "recall", "scope": "user", "query": "ui preferences", "top_k": 5}
```

Returns ranked memories, each with an `id`, the `memory` text, and a
0..1 `score`:

```json
{"facts": [{"id": "mem_ab12…", "memory": "user prefers dark mode", "score": 0.91}]}
```

- `top_k` defaults to 10, capped at 50.
- `threshold` (0..1) is a relevance floor; facts below it are dropped.
  0 means "use the backend's default".
- `recall` needs the vector stack (an embedder + a vector-capable store).
  Without it, it refuses with `vector_unsupported` /
  `embedder_not_configured` rather than a silent empty result.

Unlike `search`, the results are *stored memories* addressed by opaque
ids — treat them as facts, not the exact rows you wrote.

## Scope, tenancy, and failure modes

`add` / `recall` honor the agent's `memory_scopes` exactly like every
other op: `scope: agent` is keyed to this agent, `scope: user` to the
run's `user_id`. The tenant is always the run's own — one tenant can
neither write into nor recall from another's memories.

- **`vector_unsupported` / `embedder_not_configured`** — `recall` (or a
  vector-backed `add`) on a deployment without an embedder + vector store.
  Configure an embedder and a vector-capable store (e.g. pgvector).
- **`capability_unsupported`** — the resolved backend does not implement
  the memory-layer contract at all. The default in-process backend does,
  so this is only reached for a backend explicitly wired without it.

See `MEMORY-BACKENDS.md` for backend wiring, and the `vector-memory` /
`memory-ranking` topics for the `search` paradigm.
