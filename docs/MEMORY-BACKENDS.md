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
The embedder does not have to be a vendor one: `provider: ollama-local`
embeds against your own Ollama (pull an embedding model first —
`ollama pull embeddinggemma` — a stock Ollama ships none), and
`provider: openai` with a `base_url` reaches any OpenAI-compatible server.
DeepSeek offers no embeddings API, so a DeepSeek deployment pairs with one
of those. See `docs/TOOLS.md` → "Self-hosted embedders".
`capability_unsupported` is reached only for a backend that does not
implement the memory-layer contract at all. `add` / `recall` honor the
agent's `memory_scopes` and the run's tenant exactly like the key/value
ops. See the `memory-layer` `Context.help` topic for the full op reference.

## Background consolidation

`add` returns `pending` because it only **enqueues**. What turns a queued
conversation into a durable fact is a **consolidation pass**: a scheduled
agent run that reads settled chats past a per-target watermark, drains the
queue, and writes the facts — `set` for new or refined ones, `supersede` for
ones the conversation contradicts.

It is a **pair** of agents rather than a Go subsystem, and the split is where
the interesting decision is. `memory/consolidator` is a deterministic `code-js`
agent that owns the sequence — lease, scan, drain, read, band, write, ack,
advance, release — and calls a model exactly once per transcript, by spawning
the tool-less `memory/extractor`. Deciding that "I prefer tabs" is durable
while "leave the ticket in-progress" is not is a judgement call, so it stays in
a prompt an operator can read and change; deciding to pass `scope` on a read,
or to advance the watermark only after the writes land, is not, so it is code.
Both parts stay operator-editable config: one is a prompt, the other is a
`code:` body in the same bundle.

> ⚠️ The bundle therefore requires `LOOMCYCLE_CODE_AGENTS_ENABLED=1` — a
> `provider: code-js` agent selected without it fails boot by design.
>
> It also dispatches **serially** by default. The scheduler decides
> parallel-vs-serial by resolving the *scheduled* agent's provider, and an
> in-process provider like `code-js` makes no model call at all — so its id
> says nothing about where this batch's load actually lands, which is entirely
> in the extractor children the probe cannot see. That is the same "unknown
> backend" case an unresolvable provider hits, so it takes the same
> conservative answer. Raise `LOOMCYCLE_MAX_CONSOLIDATION_CONCURRENCY` only if
> you know those children are not all pointed at one box.

Two properties make the pass operationally safe:

- **Re-runnable.** Facts are keyed by SUBJECT, not by extraction time, so
  re-consolidating the same chats overwrites the same rows instead of growing
  near-duplicates beside them.
- **Resumable.** The watermark is forward-only and composite
  (`completed_at`, `session_id`), so an interrupted pass costs at most a
  repeat of the chats it had not finished — and two chats settling in the same
  instant cannot collapse into one.

**Consolidation is opt-in.** Without a schedule pointing at a consolidator
agent, `add` still queues durably and nothing drains it; queued items are not
lost, just not yet distilled. Every consolidated row carries `origin`,
`class`, and the source chat/run ids, which is what makes a fact traceable to
the words that produced it — and what makes removing everything derived from a
person possible. See the `memory-consolidation` `Context.help` topic for the
op-level reference and the operator knobs.

| Setting | Effect |
|---|---|
| `LOOMCYCLE_MAX_CONSOLIDATION_TARGETS` | Most targets one tick may dispatch (default 32). The rest wait for the next tick; the watermark makes that safe. |
| `LOOMCYCLE_MAX_CONSOLIDATION_CONCURRENCY` | Parallel passes per tick (default 4). Forced to 1 when the scheduled agent resolves to a local runtime **or to an in-process provider** (`code-js`, `mock`), whose id carries no information about where the load lands. The bundled consolidator is a code agent, so it is serial unless you raise this. |
| `memory.consolidation.merge_threshold` | Similarity at or above which two facts are the same fact reworded, and get merged (default `0.95`). |
| `memory.consolidation.related_threshold` | Lower edge of "overlapping subject, different claim", which is added rather than merged (default `0.85`). |

**Calibrate the bands against your embedding model — `loomcycle
memory-calibrate`.** Cosine scale is a property of the model, so the defaults
are right for at most one. Measured on a 768-dim `embeddinggemma` with a
12-fact corpus and 24 labelled probes, the **highest** genuine paraphrase
scored **0.9487** — just under the 0.95 default, so the shipped band merges
**0 of 12** duplicates and duplicates accumulate forever with nothing logged.
The safe window on that model is `(0.6775, 0.7181]`.

The defaults do not change, because the risk is asymmetric: too high leaves
duplicates lying around (recoverable), too low merges distinct facts (data
loss). 0.95 fails safe. The problem was never the number — it was having no
way to learn the number was inert for your model. Run:

```bash
loomcycle memory-calibrate --config loomcycle.yaml   # add --check in CI
```

It exits non-zero when no threshold can separate duplicates from related
facts. Note that the RELATED and UNRELATED classes **overlap** on this model
(gap −0.2136), so `related_threshold` is a recall/false-positive trade-off
rather than a clean split — a property of the model, not a tuning failure.
**Re-run after any change of embedding model or dimension.** Full table and
flags: `docs/TOOLS.md` → "Calibrating the bands".

### The eval gate

`make memory-eval-mock` runs the consolidation + retrieval gate: a
deterministic offline harness (mock provider, stub embedder, in-memory SQLite
— no network, no API key, a few seconds) pinning the pipeline's runtime
invariants. Run it before and after any change to the consolidation pipeline,
the ranker, or dedup. It is also covered by plain `go test ./...`, so it gates
every PR; the make target exists to run just the memory gate quickly.

## Observability

Each `memory.search` emits an OTEL span, `loomcycle.memory.search`, whose
**duration is the retrieval latency** (a downstream spanmetrics connector
derives the p50/p95 histogram from it — there is no separate latency
attribute). It is tagged with `loomcycle.memory.backend` (the resolved backend
kind), `loomcycle.memory.mode` (`hybrid` | `vector`), `loomcycle.memory.top_k`,
and `loomcycle.memory.deadlink_dropped` — so if a backend that crosses the
network is ever added, an operator can split its latency against in-process on
the existing trace dashboards.

Each consolidation pass emits `loomcycle.memory.consolidate`, with the child
run's own spans nested underneath (so tokens and model come from where they are
already authoritative). Its counts are a **diff of observed store state**, not
a parse of the pass's report — the pass is an LLM, and a metric derived from
its prose would make a pass that silently wrote nothing look healthy:

| Attribute | Meaning |
|---|---|
| `…consolidate.added` / `.updated` / `.superseded` | Rows the pass created, rewrote in place, and archived. A duplicate the pass chose to merge appears as `.updated` — under subject-derived keys that IS an in-place rewrite, and the runtime cannot observe the decision itself. |
| `…consolidate.sessions_read` | Settled chats past the watermark when the pass started — the backlog it was handed. |
| `…consolidate.pending_drained` | Queue rows the pass acked. |
| `…consolidate.noop` | The pass changed nothing. The signature of a wedged target, which otherwise looks healthy. |
| `…consolidate.watermark_lag_ms` | How far behind now() the watermark sits after the pass, also emitted as a span event so a connector can materialise a gauge. **A lag growing without bound is the one signal that a target is stuck.** |

**An unknown value is absent, never 0.** Every counter above is emitted only
when the read behind it succeeded — so absence means "could not be determined"
and 0 means "genuinely zero". This matters because each counter has a
benign-looking zero (nothing added, nothing drained, no lag): a pass whose
observation failed, or a target that has **never** consolidated anything, would
otherwise render as a perfectly healthy pass, and a downstream gauge would
agree. A never-advanced target shows no `watermark_lag_ms` alongside a non-zero
`sessions_read`; a pass whose reads failed carries
`…consolidate.counts_truncated`.

The consolidation span is only observed when tracing is configured — the
before/after reads are gated on the span recording, so an operator without
OTEL pays nothing for them.

No secrets, query text, transcripts, or memory fact text are placed on any
span.

## When to use which

**in-process** — the default, and currently the only kind. Lowest latency,
no external dependency, full per-scope isolation, native to the
single-binary deployment, and (since the memory layer landed natively) it
serves `add` / `recall` as well as the six key/value ops.

Naming a backend explicitly via `memory_backend:` is still worth doing when
you want the routing to be *declared* rather than implicit — the name resolves
through the substrate, so it is the seam an operator or a future external kind
plugs into without touching an agent prompt.
