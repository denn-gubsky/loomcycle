# Pluggable memory backends

loomcycle's `Memory` tool stores agent- and user-scoped key/value state.
By default that state lives **in-process** — in loomcycle's own SQLite or
Postgres store, embedded for semantic search by the configured embedder.
RFC I MR-4 adds a **pluggable backend** seam so the same `Memory` tool can
route somewhere else, per agent, without any agent prompt change.

This page covers the backend model and the `MemoryBackendDef` schema.
**One backend kind ships today — `inprocess`.** The seam is what stays
documented: it is the extension point an external backend would land on,
and it is what makes the routing, the substrate, and the per-agent
`memory_backend:` field meaningful.

## Why pluggable backends (and why not a memory subsystem)

We chose a *pluggable backend* over coupling loomcycle to one memory
product. A subsystem coupling would regress loomcycle's per-user +
per-agent + per-tenant isolation (external products tend toward a flat
"one API key = one shared space" model), break the single-Go-binary
posture, and bind loomcycle's roadmap to a third party's pricing and
longevity. A pluggable backend keeps the upside available — an operator who
runs other memory-consumer products could share a memory pool across them —
with none of those couplings. The in-process backend stays the default and
the unconditional fallback.

That trade-off is also why the seam outlived the one external backend built
on it: retiring that backend cost a `case` arm and a package, not a
refactor of the `Memory` tool, and no agent prompt changed.

**Thesis:** the backend is an operator deployment choice, expressed in
config; agents are backend-agnostic and never see which backend served a
recall.

## The backend model

Every key/value `Memory` op (get/set/delete/list/search) routes through
a `memory.Backend`. An agent's `memory_backend: <name>` field selects a
named backend; absent, the agent uses the operator-default (in-process).
The backend NAME is operator-resolved and stamped onto the run — it is
**never** model/tool input (same trust posture as the memory scope).

One backend kind ships today:

| kind        | where state lives                        | when to use |
|-------------|------------------------------------------|-------------|
| `inprocess` | loomcycle's own store + embedder (default) | single binary, no external dependency, lowest latency |

An external REST backend (`mem9`) shipped between v0.15.0 and the removal
below. It was retired once the in-process backend became a native memory
layer — `add` enqueues to a durable consolidation queue and `recall` is
hybrid search, which is exactly what the external product had been imported
to provide. Authoring `kind: mem9` is now **refused** by the validator; a def
persisted by an older build still resolves and **degrades to `inprocess`**
(logged), so an upgrade cannot fail a run over a stale row.

Backends are declared under `memory_backends:` in operator yaml, or
authored at runtime via the `MemoryBackendDef` tool (forks of the static
roots). Resolution precedence: static yaml first, then the active
substrate def.

## MemoryBackendDef schema

```yaml
memory_backends:
  <name>:
    kind: inprocess
```

That is the whole live surface. The persisted definition shape also carries
`config` (`base_url` / `api_version` / `api_key_env`), `tenancy_strategy`,
`fallback_on_error`, and `health_check_interval_seconds`. **No shipped kind
reads any of them** — they are retained because the shape is content-addressed
and mirrored three ways (operator yaml, the substrate write shape, the
substrate read shape), so removing them is a storage change rather than a
docs change. Treat them as **reserved**: set them and nothing happens.

Two are still *validated* at authoring time, deliberately, so the persisted
shape can never hold a state a future external kind would act on unsafely:

- `tenancy_strategy.kind: shared_key_with_prefix` requires a
  `prefix_pattern` containing `{tenant_id}`. An empty or token-less prefix
  would resolve to an empty key prefix and collapse every tenant into one
  keyspace — a cross-tenant read+write leak.
- `tenancy_strategy.kind: key_per_tenant` requires any `env_pattern` it sets
  to contain `{tenant_id}`.

`api_key_env` is an env-var **name**, never a plaintext key.

## Memory layer: add / recall

Beyond the six key/value ops the Memory tool serves the `add` / `recall`
paradigm: you hand it conversation messages and let the system distil
durable facts, then answer natural-language recall queries over them.
There are no caller keys; identity is server-assigned. **The default
in-process backend now serves this natively** — `add` enqueues the
messages onto a durable queue for background consolidation; `recall` is a
semantic search over stored memories. An external memory-layer backend
may instead extract facts server-side with its own LLM.

```jsonc
// ingest a conversation — infer defaults to true (queued for consolidation)
{"op": "add", "scope": "user",
 "messages": [{"role": "user", "content": "I prefer dark mode"}]}
// → {"status": "pending"}   (async — no read-after-write guarantee)

// recall stored memories by meaning
{"op": "recall", "scope": "user", "query": "ui preferences", "top_k": 5}
// → {"facts": [{"id": "mem_ab12…", "memory": "user prefers dark mode", "score": 0.91}]}
```

`recall` needs the vector stack (an embedder + a vector-capable store);
without it, it refuses with `vector_unsupported` / `embedder_not_configured`.
`capability_unsupported` is reached only for a backend that does not
implement the memory-layer contract at all. `add` / `recall` honor the
agent's `memory_scopes` and the run's tenant exactly like the key/value
ops. See the `memory-layer` `Context.help` topic for the full op reference.

## Observability

Each `memory.search` emits an OTEL span (`loomcycle.memory.search`) tagged
with `memory.backend` (the resolved backend kind), `memory.top_k`, and
`memory.recall_latency_ms` — so if a backend that crosses the network is ever
added, an operator can split its latency against in-process on the existing
trace dashboards. No secrets, query text, or transcripts are placed on spans.

## When to use which

**in-process** — the default, and currently the only kind. Lowest latency,
no external dependency, full per-scope isolation, native to the
single-binary deployment, and (since the memory layer landed natively) it
serves `add` / `recall` as well as the six key/value ops.

Naming a backend explicitly via `memory_backend:` is still worth doing when
you want the routing to be *declared* rather than implicit — the name resolves
through the substrate, so it is the seam an operator or a future external kind
plugs into without touching an agent prompt.
