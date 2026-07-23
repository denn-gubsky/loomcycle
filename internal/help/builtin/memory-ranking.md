---
name: memory-ranking
description: Memory retrieval tuning — the hybrid ranker (semantic + recency weights) and search-time dedup on Memory.search, pluggable memory backends (MemoryBackendDef + Mem9), per-agent memory_backend routing, and the loomcycle memory-eval scoring harness.
---

# Memory ranking, dedup & pluggable backends

`Memory.search` retrieves by relevance to the query. On a backend with a
full-text index (Postgres+pgvector) retrieval is **hybrid by default**: a
vector (semantic) leg and a keyword (full-text) leg are fused by Reciprocal
Rank Fusion (RRF), so an entry that lexically matches the query but sits
outside the nearest-neighbour set still surfaces. A backend **without** a
full-text index (SQLite) keeps pure cosine similarity — the original Vector
Memory behavior, with no over-fetch or second round-trip for a plain semantic
search. On top of retrieval, loomcycle layers three **opt-in** tunables:
recency/frequency ranking weights, search-time dedup, and a pluggable backend
layer (so an agent's memory can live in an external store like Mem9 instead of
loomcycle's own sqlite-vec/pgvector).

## Why tune retrieval at all

Pure semantic similarity has two failure modes in long-lived agents:

- **Stale-but-similar wins.** A fact written six months ago can out-score a
  fresh correction that says the opposite, because both embed near the query.
- **Repetition wastes context.** Three paraphrases of the same fact all match
  and all get returned, burning tokens on redundancy.

The recency/frequency ranking weights address the first, dedup the second.
Neither fires unless you ask for it — an agent that sends no `rank`/`dedup`
block gets the default weights (pure semantic) and no dedup. Hybrid fusion
itself, however, runs by default wherever a full-text index is available.

## The hybrid ranker

`Memory.search` accepts an optional `rank` block:

```jsonc
{
  "op": "search", "scope": "user", "query": "...", "top_k": 10,
  "rank": {
    "semantic_weight":        1.0,   // default — scales the semantic signal
    "recency_weight":         0.0,
    "recency_half_life_hours": 24,
    "source_weight":          0.0,   // reserved (0 today)
    "frequency_weight":       0.0    // wired — rewards frequently-recalled rows
  }
}
```

The computed rank is:

```
rank_score = semantic_weight·semantic_signal
           + recency_weight·exp(-age·ln2 / recency_half_life_hours)
           + source_weight·source_score              (reserved — 0 today)
           + frequency_weight·log(1+access_count)
```

`semantic_signal` is the **fused RRF rank** where hybrid retrieval ran, or the
raw cosine on a pure-vector (no-full-text) backend. A non-zero `recency_weight`
blends in an exponential recency decay: an entry at one half-life scores 0.5 on
that term, so a fresh entry can overtake an older, slightly-more-similar one. A
non-zero `frequency_weight` rewards a frequently-recalled entry (sub-linear, so
a runaway access count can't dominate the semantic signal). When any of these
weights is set, loomcycle re-ranks the candidate pool, so recency or frequency
can promote an entry the semantic top-k would have missed.

Each result carries two distinct fields: **`score`** — always the raw cosine
similarity, unchanged by fusion or ranking (a stable value you can compare
across searches) — and **`rank_score`** — the computed rank above (fused
semantic + recency + frequency) that the results were ordered by.

**Reserved weight.** `source_weight` is part of the locked wire shape but
contributes 0 today (there's no per-entry source-score signal yet). Setting it
is accepted, not rejected — the response carries a `rank_note` so the weight
isn't silently ignored. (`frequency_weight` is no longer reserved: it is wired
to each entry's access count.)

## Search-time dedup

An optional `dedup` block collapses near-duplicate results AFTER ranking and
BEFORE the top-k trim, so the highest-ranked member of a duplicate cluster
survives:

```jsonc
{ "dedup": { "enabled": true, "threshold": 0.92, "mode": "drop" } }
```

- `threshold` is a **cosine-similarity floor**: two results whose embeddings
  are ≥ threshold similar are duplicates. Default `0.92`. (Higher = stricter,
  only near-identical rows collapse.)
- `mode`:
  - `drop` (default) — keep only the highest-ranked of a cluster.
  - `merge` — drop the duplicate but record its key+value under the survivor's
    value as `merged_from` provenance (nothing is lost).
  - `keep` — retain duplicates but count them, so you can measure the
    duplication rate without losing data.

The response reports how many entries were collapsed. Dedup is a no-op when
disabled (the default) and degrades gracefully to a no-op for backends that
can't supply per-entry vectors (e.g. the Mem9 REST backend re-ranks/dedups on
whatever candidates it returns).

## Pluggable backends

By default every agent's memory lives in loomcycle's in-process store
(sqlite-vec or pgvector). `MemoryBackendDef` (the sixth substrate primitive)
lets an operator register an external backend and point specific agents at
it:

```yaml
memory_backends:
  default:      { kind: inprocess }
  mem9-team:
    kind: mem9
    config:
      base_url:    "https://mem9.internal.example"
      api_version: "v1alpha2"
      api_key_env: "LOOMCYCLE_MEM9_TEAM_API_KEY"   # env-allowlist-gated
    tenancy_strategy: { kind: "shared_key_with_prefix", prefix_pattern: "tenant-{tenant_id}::" }
    fallback_on_error: "inprocess"

agents:
  shared-research:
    memory_backend: mem9-team     # this agent's Memory.* ops route to Mem9
  # agents with no memory_backend use the operator-default backend
```

`AgentDef.memory_backend` names the backend; absence means the operator
default. The backend name is resolved from operator config, never from model
input — same trust posture as `memory_scopes`. The ranker + dedup work
uniformly across backends (in-process backends rank/dedup in-Go; external
backends re-rank client-side on the candidates they return).

**Credentials** for an external backend mirror the scheduler exactly: the API
key is an env-var NAME (`api_key_env`) gated by the shared env-allowlist
(`LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST`), resolvable per-run from
`${run.credentials.*}` or the env. Never plaintext in the Def, never
logged. **Fallback:** `fallback_on_error: inprocess` keeps an agent working
against local memory if the external backend is unreachable, rather than
failing the run.

> ⚠ The Mem9 wire mapping is implemented against the documented v1alpha2 REST
> shape + a CI stub; **verify it against your Mem9 version before production**
> — see `docs/MEMORY-BACKENDS.md`.

## Full CRUD + admin

`MemoryBackendDef` is a 5-op substrate tool (`create`/`fork`/`get`/`list`/
`retire`) with the standard 4-transport admin (HTTP `/v1/_memorybackenddef`,
gRPC, MCP meta-tool, TS `memoryBackendDef()`). The `/ui/memory` Web UI tab
shows per-key embedding metadata (model + dimension) when the store supports
vectors.

## The eval harness — tuning is measured, not guessed

A ranker/dedup change is only an improvement if the numbers say so. The
`loomcycle memory-eval` CLI scores retrieval against a dataset of
`{query, expected_recall}` tuples:

```
$ loomcycle memory-eval --dataset bundled
  precision@k        0.19
  recall@k           0.94
  duplication_rate   0.16
  recall_latency_p50 0.05 ms
```

It seeds a corpus into the real in-process backend (ranker + dedup included),
runs the queries, and reports precision@k / recall@k / duplication_rate /
recall-latency percentiles. The bundled dataset uses a **deterministic stub
embedder** — reproducible in CI with no provider key, but NOT a semantic
benchmark. For real numbers, pass `--dataset <file.jsonl>` (one-line corpus
header + query lines) and run against your real embedder, optionally with
`--rank-config <file.json>` to A/B-test ranker weights.

**This is the gating tool for ranker/dedup changes:** run it before and after,
compare the metrics.

## What's deferred

The "show recalls for run X" UI overlay (joining memory accesses against OTEL
traces) is **not** shipped — it needs an access-log subsystem that is its own
future RFC. Write-time dedup (merging a near-duplicate at `set`) is also
deferred: search-time dedup covers the user-facing pain without an LLM judge
on the hot write path.
