# Documents (RFC AK)

A Document is a **chunked-graph document**: instead of one opaque blob, it's a
tree of **chunks** — each a first-class unit with a UUID, a position in a
hierarchy, an optional type, structured fields, graph edges to other chunks,
and a Markdown body — that agents and humans co-author and query.

**Why a chunked graph, not "a Memory blob" or "a Markdown file":** a single
blob/file is opaque to the runtime — you can't ask "every `decision` chunk
still in `status:open`", you can't update one section without rewriting the
whole document (and racing another writer), and you can't link a chunk in one
document to a chunk in another. Storing the *structure* in a queryable form
buys: `SELECT … FROM chunks WHERE type=… AND status=…`, **per-chunk optimistic
concurrency** (two agents edit different chunks without clobbering), typed
chunks (supertag-style `fields`), and cross-document **edges** (`promotes`,
`targets`, …). The body text still lives as Markdown, so a chunk round-trips to
something a human reads.

## Mechanism — content/structure split

- **Chunk bodies + fields** live in **Memory**, keyed by the chunk UUID
  (`store.MemorySet/MemoryGet`). This is the prose.
- **Chunk structure** — `parent_id` / `position` / `type` / `status` / `title`
  / `revision`, the **edges**, and the **type schemas** — lives in **SQL
  Memory** across four tables (`documents`, `chunks`, `chunk_edges`,
  `chunk_types`), so it's queryable.
- The Document is **named in the Path tree** via a `document` dirent, so
  `/docs/launch` resolves to it (see `docs/PATH.md`).

Because the structure tables live in SQL Memory, **Documents require SQL Memory
enabled** (`LOOMCYCLE_SQLMEM_ENABLED=1`); without it the tool refuses.

## Surface

One tool, `Document`, gated by per-agent `tools: [Document]`. 15 ops:

| group | ops |
|-------|-----|
| Document lifecycle | `create_document` (optional `path:` → a Path dirent), `get_document` (by `id` or `path`), `delete_document` |
| Chunk lifecycle | `create_chunk`, `get_chunk`, `update_chunk`, `delete_chunk`, `move_chunk` |
| Edges | `link_chunks`, `unlink_chunks` |
| Query | `query_chunks` |
| Types | `define_type`, `list_types` |
| Markdown | `export_md` (render to Markdown; `include_metadata:false` for clean human MD, default `true` embeds round-trippable metadata + edges as HTML comments), `import_md` (build a document from export_md-shaped Markdown — omit `document_id` for a new document; pass `document_id`/`parent_id` to import under an existing chunk) |

`scope` is `agent` (this agent) or `user` (this end-user, the default — needs a
`user_id` on the run). **Tenant scope exists too** (`scope:tenant`, shared by
every user and agent in the tenant) and needs `tenant` granted in **both** the
agent's `memory_scopes` and its `sql_scopes`; without both, the Document
tool refuses `scope:tenant` with a clear message. Documents are still
tenant-*isolated*: the SQL Memory scope key carries the authoritative tenant.

**Where tenant chunk bodies live.** A chunk body (`doc.chunk:<id>`), its
derived units (`doc.unit:`) and their embeddings are Memory rows. In the tenant
scope they are keyed like every other tenant Memory row — `scope_id` empty, the
`tenant_id` column carrying the identity — so `Memory op=search` / `recall` /
`get` at `scope: tenant` reach them. (SQL Memory keeps the tenant as its scope
id; it cannot key on an empty component.) Releases before this one keyed these
rows by the SQL Memory scope id instead (the tenant, or `default` for the empty
tenant), where no tenant-scope Memory read looked:

- Postgres migration `0114` moves them, embeddings included; SQLite does the same
  on open, and every boot repeats it to catch rows a replica on the older binary
  wrote during a rolling upgrade. The boot log reports rows moved, conflicts
  (both coordinates held the key — the later write is kept, except that an
  old-keyspace chunk body whose `body` is empty never replaces the destination;
  fields alone do not count as content) and strays (a tenant document row at any
  other scope id, left where it is).
- For one release a tenant body read that misses retries the old scope id, so a
  chunk an older replica just wrote reads correctly before the next boot moves it.
- Snapshot restore re-keys an older snapshot's tenant document rows the same way.
- During a rolling upgrade an older replica reads a moved body as empty. If it
  writes that emptiness back (a fields-only `update_chunk`), the blank row lands
  at the old scope id and the next sweep discards it in favour of the moved body;
  a real edit made through an older replica is kept if it is the later write. The
  older replica still shows tenant documents as empty until it is upgraded.
  Rolling the binary back past `0114` needs the rows moved back by hand (the down
  migration is a no-op).

**Memory and document rows.** Because chunk bodies share the Memory keyspace,
the Memory tool keeps them behind the Document tool's checks:

- Writes: `set`, `incr`, `merge`, `append_dedupe` and `bounded_list` refuse a
  `doc.chunk:` (and `doc.unit:`) key with a validation error; `delete` and
  `supersede` refuse a unit and a provenance-free (prose) chunk body. A
  consolidated fact's body carries provenance, and the consolidation pass may
  still retire or remove a fact by its `doc.chunk:` id.
- Tenant reads: reading tenant document rows through Memory needs
  `sql_scopes: [tenant]` as well as `memory_scopes: [tenant]`, the pair the
  Document tool needs. Without it, a call that asks for documents (`documents` in
  `sources`, a `doc.chunk:` prefix, a document filter, a `get` of a prose body)
  is a permission error naming the grant; a search or list that does not ask
  leaves the document class out. Facts, notes and traces are unaffected.

### Concurrency, hierarchy, querying

- **Optimistic concurrency.** `update_chunk` takes the chunk's current
  `revision`; the write is a guarded atomic bump
  (`UPDATE … SET revision=revision+1 WHERE id=? AND revision=?`). A stale
  revision affects zero rows → the op returns a conflict instead of a silent
  lost update.
- **`move_chunk`** re-parents (and re-positions) a chunk, with a cycle guard —
  a move that would make a chunk its own ancestor is refused.
- **`query_chunks`** takes structured filters (`document_id` / `type` /
  `status` / `parent_id`, plus `under_path:` which joins the Path tree —
  documents at/under a path → their chunks) **or** a `sql:` escape hatch: a
  raw, read-only `SELECT` against the chunk tables, gated by the SQL Memory
  statement validator (no `ATTACH` / `PRAGMA` / multi-statement / writes).
  It also takes `as_of` (below), so the chunks in force on a date can be
  listed without a semantic query.
- **Dated content.** Any chunk can carry its own validity period: nullable
  `chunks.valid_at` / `chunks.invalid_at` (unix ns, half-open
  `[valid_at, invalid_at)`, NULL = unbounded on that side), set by
  `create_chunk` / `update_chunk` (an explicit JSON `null` clears an end; the
  revision bumps, no `chunk_revisions` row is written) and round-tripped by
  `export_md` / `import_md` in the per-chunk `<!-- loom: … -->` comment and by
  `sync` / `diff_remote`. It is content time, never write time — nothing
  defaults it — and it is independent of the fact tier: a dated chunk gets no
  `chunk_memory_meta` row, so `list_facts` does not list it. Use it for an
  edition, a tariff or a policy clause; use the fact tier for a claim with a
  natural key and a source span. A fact-tier chunk may carry both intervals,
  and `as_of` (`search`, `related`, `query_chunks`) then requires both to
  contain the instant. `upsert_chunk`'s `valid_at`/`invalid_at` remain the
  fact's sidecar interval. Existing scopes gain the columns (and the
  `chunks_validity` index) through an idempotent migration on first use.
- **Change events.** `update_chunk` / `move_chunk` / `link_chunks` /
  `delete_chunk` publish `{op, chunk_id, timestamp, actor}` to the
  `documents/<id>/chunks` channel, so a watcher (or a co-authoring UI) sees
  edits live.

### Integrity — deletes are atomic, edges never dangle

The delete paths were hardened so a cascade can't leave orphans (PR #540):

- `delete_document` and `delete_chunk` run their whole SQL cascade in **one SQL
  Memory transaction** — a mid-cascade failure rolls back, never a
  half-deleted graph.
- `delete_document` cleans edges **bidirectionally** (`from_id IN doc OR to_id
  IN doc`), so an *incoming* cross-document edge from another doc doesn't
  dangle at a deleted chunk.
- `delete_chunk` cascades a chunk + its descendants (and refuses a document's
  **root** chunk — that would orphan the document row; use `delete_document`).
- `link_chunks` validates **both** endpoints exist (cross-document links are
  allowed — both chunks just have to exist), so no edge is born dangling.
- Chunk Memory **bodies** live in a separate store that can't join the SQL
  transaction, so they're deleted **after** a successful commit (best-effort —
  an orphaned body is invisible dead K/V, an orphaned *row* would be visible,
  so SQL-first is the least-bad ordering).

## Off-run: every transport (v1.4.0)

Besides in-band use, Document is a first-class operation on all four wire
surfaces — so a UI or a human can co-author the same documents agents build:

| Transport | Surface |
|-----------|---------|
| HTTP      | `POST /v1/_document` (body = the op-discriminated tool input) |
| gRPC      | `rpc Document(SubstrateRequest) returns (SubstrateResponse)` |
| MCP       | the LoomCycle MCP meta-tool `document` |
| TS        | `client.document(input)` (`@loomcycle/client`) |
| Python    | `await client.document(input)` |

All four dispatch through a single `Connector.Document` method. **Scope and
tenant are resolved server-side from the authenticated principal — never the
wire.** The endpoints are tenant-confined (`ScopeTenant`; `substrate:admin`
also satisfies).

```bash
# create a document, named in the Path tree
curl -sS -X POST localhost:8080/v1/_document \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -d '{"op":"create_document","scope":"user","title":"Launch plan","path":"/docs/launch"}'
# → {"document_id":"…","root_chunk_id":"…","path":"/docs/launch"}
```

```ts
const doc = await client.document({ op: "create_document", scope: "user", title: "Launch plan" });
const chunk = await client.document({
  op: "create_chunk", scope: "user",
  document_id: doc.document_id, parent_id: doc.root_chunk_id,
  type: "decision", title: "Ship date", body: "## Ship date\n2026-07-01",
});
const open = await client.document({
  op: "query_chunks", scope: "user",
  document_id: doc.document_id, type: "decision", status: "open",
});
```

## Search: indexing, re-indexing and reranking

How a question finds a chunk: what text each chunk is indexed under, how that index stays true as the tree changes, what `Document op=search` runs, and the optional model rerank on top.

**Why this shape, not the alternatives.** It was measured before it was built (RFC DM), on QASPER papers and on 911 Natural Questions over Wikipedia pages:

- **An index-only header** naming the document and section beat indexing the bare content: R@5 went from 0.912 to 0.935, p=0.0046. The alternatives cost more and did no better:
  - LLM-written chunk context (the Anthropic contextual-retrieval pattern) had to beat this free header, and did not.
  - A generated document map did not transfer to a held-out set.
- **A listwise rerank** of the top 20 candidates by one local model was the largest lever: R@1 went from 0.515 to 0.754 and R@5 from 0.935 to 0.959. It costs a model call per search, so it is opt-in per agent, never a default.

The header changes only what is FOUND. A chunk's body, and everything `get_chunk`, `export_md` and search results return, are unchanged.

### How a chunk is indexed

Every chunk is embedded, and full-text indexed where the store has a word index, under its **index text**:

    <document title> — <heading path>
    <content>

The heading path is the chunk's ancestor titles below the root, joined with " > ". For example, a chunk under *Setup › Install* in a document titled *Guide* is indexed as `Guide — Setup > Install` followed by its body.

**Measured (2026-09-29, through the built feature).** The header is on by default.
- **Corpus:** 652 UK government guidance pages in one pooled store; the header was compared against the same text indexed without it.
- **Retrieval:** the right section was in the top 5 for 86.0% of questions, against 76.8% without it (p = 8e-42).
- **Answers:** a reader's answers improved (F1 +0.043).
- **Where the gain comes from:** mostly from choosing the right section within a page, since finding the page itself moved only +2.2pp. The gain is largest under distinctive headings.
- **Research doc:** `/documents/research/document-retrieval-conditionalqa`.

- **Derived, never stored.** The header is built from the chunk tree at index time, so there is no stored copy to go stale. The chunk's body is exactly what the author wrote.
- **Every writer uses the same builder**: the write path, `/v1/_memory/backfill_embeddings`, `/v1/_memory/reembed` and the re-index pass.
- **Special cases:**
  - Image and Mermaid chunks keep their own embed text (description or caption; diagram labels) and gain the same header line.
  - A bodyless heading is indexed under its header alone, if its own title has letters.
  - A title part with no letters in it (an empty title, "---", a bare number) is left out of the header.
  - A bodyless document root is **not** indexed. It used to outrank every section for a query that named the document. The title stays findable through every section's header.
- **If the tree cannot be read**, a chunk is indexed under its content alone, never under nothing. An empty index text is what the purge treats as "un-index this chunk".

### Keeping the index current — POST /v1/_document/reindex

A header depends on the titles above a chunk, so the index follows the tree.

- **Renaming** a heading or a document (`update_chunk` with `title`) re-indexes that chunk's whole subtree. A root rename is a document rename and re-indexes every chunk in it.
- **`move_chunk`** re-indexes the moved subtree.
- **How it runs:** up to 32 chunks are re-indexed during the call; beyond that, in the background. It is embedding calls only, with no model call. A failed embed leaves the chunk under its old text until the next pass, and never fails the write.
- **A chunk that derives to nothing** (renamed to a letterless heading, a bodyless root) has its stored vector deleted, rather than keeping the vector of what it used to say.

**Existing stores.** Chunks written before the header existed keep their old vectors until something rewrites them. Nothing migrates automatically: re-embedding a large store is the operator's decision about embedder load. The operator pass is:

    # what WOULD change (the default — writes nothing)
    curl -X POST -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
      "http://127.0.0.1:8787/v1/_document/reindex?scope=user&scope_id=<subject>"
    # do it
    curl -X POST ... "…/v1/_document/reindex?scope=user&scope_id=<subject>&dry_run=false&limit=200"

| param | meaning |
|---|---|
| `scope` | `agent` \| `user` (default) \| `tenant`; `scope_id` / `tenant` choose whose, as for the other browse routes |
| `dry_run` | `true` (default) reports without writing |
| `limit` | chunks to **change** per call (default 200). Chunks already up to date are stepped over and do not count, so a mostly-current store still makes progress. A call also stops after examining 5,000 chunks. |
| `after` | resume cursor: pass back `next_cursor` while `more` is true |

The response is `{scope, dry_run, examined, up_to_date, reindexed, unindexed, failed, failed_keys, next_cursor, more}`.

- The pass is **idempotent**: a chunk already indexed under its current text costs one read and no embedding call.
- It needs SQL Memory (503 `sqlmem_unavailable`), and for a real run an embedder (503 `embedder_unavailable`).
- Operator-admin scope today.

### Document op=search — the memory search pipeline

`Document op=search` runs the same search as `Memory op=search`, restricted to chunk bodies (`prefix: doc.chunk:`), on the server's own memory backend. Both tools share one backend instance, so they rank the same chunks in the same order, and whatever that backend is configured with (the rerank below) applies to both.

- **Hybrid where the store has a word index** (Postgres with pgvector): the vector leg and a full-text leg are fused by reciprocal-rank fusion (RRF). A name, a code or an error string that the embedding misses is found by its words.
  - Verified live: a chunk holding `E4711` ranks first for "E4711", in both legs.
- **Meaning-only without a word index.** Every `rank_score` then equals its `score`.
- **Hits** are `{chunk_id, score, rank_score, title, type, document_id}`:
  - `rank_score` is what the order used.
  - `score` is the raw strength of the match that first found the chunk: the cosine similarity, or, for a chunk found only by its words, a word-match weight on a different scale.
- Every chunk body is searched, including chunks memory's facts are homed in. That is why the op filters by the key prefix and not by `sources=[documents]`.
- **Side effect:** as with `Memory op=search`, a returned chunk's access count is bumped, which feeds the ranker's frequency term.

#### Filtered search — aiming `search` and `related` at one corpus

One scope often holds several corpora — board files, regulator norms, decisions, a gate's own reports. Each retrieval leg keeps at most 51 candidates, fused and then reranked, so for wording common to all of them the pool can hold none of the corpus a caller wants, and no amount of post-filtering fixes a pool filled from the wrong place. `search` and `related` therefore take structural filters that are applied **inside every leg, before its limit**:

| Argument | Meaning |
|---|---|
| `under_path` / `under_paths` | chunks of documents named at or under any of these paths (this scope's tree) |
| `exclude_under_path` / `exclude_under_paths` | never chunks of documents named at or under these paths |
| `document_id` / `document_ids` | only these documents |
| `exclude_document_ids` | never these documents |
| `type` / `types` | chunk type, each widened to its ontology subtypes (reported as `type_expanded_to`) |
| `status` / `statuses` | exact status |
| `tag` / `tag_prefix` | exact tag / the tag or anything nested under it |
| `as_of` | unix ns; chunks whose validity contains the instant — the chunk's own `valid_at`/`invalid_at` AND, for a fact-tier chunk, its sidecar interval (a chunk with no interval always passes — the `graph_recall` rule) |

Kinds combine with AND, values within a list with OR; a singular and its plural twin are one filter (their union).

**How it works — resolve in SQL Memory, restrict in the main store.** The filter is resolved once per call, in the scope's SQL Memory (`chunks`, `chunk_tags`, `chunk_memory_meta`, the Path tree), to a set of chunk ids. That set travels to the store as `MemorySearchFilter.IDs` (or `ExcludeIDs`), an *id segment* relative to the key prefix: for `doc.chunk:<id>` it is the chunk id and for a derived unit `doc.unit:<id>:<kind>:<n>` the same chunk id, so one list restricts the vector leg, the full-text leg and both unit legs alike. Postgres applies it in the statement (`split_part(...) = ANY($ids)`), before `LIMIT`; fusion, unit resolution, dedup and the rerank then only ever see filtered rows. Resolving at query time against the tables that own the structure means a filter is correct the moment a chunk is moved, retyped, retagged or renamed — there is no denormalised copy to keep in step.

- **Response.** Only when a filter was given, the response carries `filter: {applied, matched_documents, matched_chunks, type_expanded_to?, inverted}`. A filter-free call is byte-identical to before (no resolver runs). `matched_chunks: 0` returns `{chunks: [], filter}` without embedding the query.
- **Size.** When the matched set is more than half the scope, its complement is sent as a denylist instead (`inverted: true`); an exclusion-only filter is always a denylist. The list sent is capped by `LOOMCYCLE_DOCUMENT_SEARCH_FILTER_MAX_IDS` (default 50,000); over it the call is refused with a business error `filter_too_wide` carrying `matched_chunks`, `scope_chunks` and the cap — never a silent post-filter. The list's size is recorded on the `loomcycle.memory.search` span as `loomcycle.memory.filter.ids` / `loomcycle.memory.filter.exclude_ids`.
- **Isolation and access control.** Paths resolve in the caller's own tenant + scope tree; a foreign document id resolves to nothing. For a caller whose access-control set can deny it reading part of the scope, the filtered set is narrowed to readable documents before anything is counted or sent, and always travels as an allowlist (never inverted — a denylist would have to name the unreadable chunks): no id of a denied document reaches the store, and no count or refusal moves when a denied document changes. The listing post-filter still runs on the answer. A filter-free search is not narrowed this way; it keeps the plain post-filter.
- **Tiers.** The id predicate is Postgres-only, which is the only tier with vector and full-text legs; sqlite ignores it.

**The same filters on `Memory op=search` and `op=recall`.** A caller that recalls through the Memory tool — where chunk bodies come back beside facts and notes — can aim the documents half with `under_paths`, `exclude_under_paths`, `document_ids` and `exclude_document_ids` (lists only; the chunk-level predicates stay on `Document op=search`). The Memory tool does not reach SQL Memory: it holds the Document tool as a `DocumentFilterResolver` (`internal/memory`), wired at boot and read at call time, which runs the same resolution above — access-control narrowing, inversion and the cap included — in the SQL Memory scope matching the call's memory scope, and returns the chunk-id lists `SearchQuery` already takes.

- **Scope gate.** Resolving is a SQL Memory read, so it goes through the Document tool's own scope gate (`resolveScope`: the grants and the isolated-member confinement) and provisions nothing — a scope with no SQL Memory or no document tables simply matches nothing. A gate refusal is reported as `Document op=search` reports it: a **permission** error naming the missing grant (e.g. `sql_scopes: [tenant]`) or the confinement. Every scope, each in its own tree: on `tenant` the filters resolve in the tenant's SQL Memory tree, and need both tenant grants — a tenant-scope Memory call without `sql_scopes: [tenant]` is refused with the **permission** error of the tenant read rule above before anything is resolved.

- **Documents only.** In a mixed search the lists must not drop a note or a fact for not being a chunk id, so the store filter carries `IDsPrefixes` (`doc.chunk:`, `doc.unit:`): only provenance-free rows under those namespaces are held to the lists, the segment cut after whichever prefix matched; a fact homed in a chunk carries provenance and passes. A search on exactly the chunk namespace (Document `search`) keeps the plain rule.
- **Accepted only where documents can come back.** No `sources` or `sources` including `documents` on `search`; on `recall`, whose default is facts and notes, `documents` must be asked for. Otherwise the call is refused with a validation error naming the selector — never run with a filter it would ignore. Refused too when no resolver is wired or SQL Memory is off ("documents not configured on this server"), and when the agent's memory backend is not this server's in-process one (a remote backend cannot interpret the ids).
- **Nothing matched.** A documents-only call answers `{entries|memories: [], filter}` without embedding; a mixed call keeps its facts and notes and drops the document class at the store (an empty id list would mean "everything").
- **Response.** The same `filter: {applied, matched_documents, matched_chunks, inverted}` block beside `entries` / `memories`, only when a filter was given. A path is a name in one scope's tree, so a caller recalling in several scopes makes one call per scope and gets each scope's own counts.
- **Off-run search.** The operator console's `POST /v1/_memory/search` takes the same four lists (TS `memorySearch`: `underPaths`, `excludeUnderPaths`, `documentIds`, `excludeDocumentIds`) and answers with the same `filter` block, only when a filter was sent. It runs the Memory tool's own resolution step with the Document tool as resolver, under the caller's authoritative identity: the principal's tenant, the request's `scope_id` as the subject, the operator-plane grants the off-run Document routes use, and the principal's access-control set for the narrowing. A refusal is answered by its class: `400 invalid_filter` (validation), `403 permission_denied`, `400 filter_unavailable` (no SQL Memory, filter too wide). A `tenant` search resolves its filter in the tenant's tree.

### The opt-in rerank — memory.reranker + memory_rerank

One model call per search reorders the first N candidates. The model reads the question plus each candidate's **index text** (so every candidate arrives labelled with its document and section), each truncated to a character budget.

**Two kinds** (`memory.reranker.kind`):
- **`listwise`** (the default): a chat model answers with a JSON array of candidate numbers, and the reply is repaired.
- **`decision`**: a decision model (Ollama ≥ 0.35, `/v1/systemone`; e.g. `nimble`) answers one typed choice over the candidates with a probability for each, and the candidates are ordered by probability.
  - There is no reply to parse, so it cannot fail as `unparseable` or name a passage outside the pool.

**Measured.** The rerank is the largest retrieval lever the document work found, on four corpora:
- On QASPER (held out), NQ and PolicyQA, it was tested as a probe stand-in.
- On ConditionalQA, through this feature, with 20 candidates × 1,200 characters (all 2,515 questions):

| kind | model | right section in top 5 (none → reranked) | in top 1 | median rerank time |
|---|---|---|---|---|
| `listwise` | `qwen3.8` (27B) | 86.0% → 93.3% | 58.8% → 81.2% | about 4.4 s |
| `decision` | `nimble` (9B) | 86.0% → 93.3% | 58.8% → 77.0% | **1.45 s** |

- **How the kinds compare:** the decision kind reaches the same top 5 at a third of the latency with a third of the model, and is somewhat worse at rank 1.
- **Answers:** with the listwise kind, a reader's answers improved (F1 +0.047), to within 0.025 F1 of reading the gold sections directly.
- **Memory recall** (LoCoMo): reranking recall lifted recall@5 by about +15 points with either kind. For the listwise kind, a 40-row pool lifted multi-hop recall@5 by a further +7.8 points over 20 (0.517 → 0.595).
  - **It did not change an agent's answers:** with recall reranked over consolidated facts, LoCoMo binary-J went 0.780 → 0.782 over 383 paired questions (p = 1.0).
  - Treat memory reranking as a retrieval improvement, not an answer-quality one.
- See `/documents/research/document-retrieval-conditionalqa`, `/documents/research/document-rerank-decision-models`, `/documents/research/memory-rerank-locomo` and `/documents/research/memory-rerank-answers`.

**The operator declares the model**, shaped like the embedder. There is no default: without this block no search is reranked anywhere.

```yaml
memory:
  reranker:
    kind: listwise                # the default; or decision (below)
    provider: ollama-local        # a provider declared in providers:
    model: qwen3.6:latest
    effort: low                   # thinking off on a hybrid local model (as measured)
    timeout_ms: 30000             # default 30000 — raise it on a shared GPU (see below)
    context_tokens: 16384         # default; Ollama num_ctx (it silently truncates to 4096 otherwise)
    sources: [documents]          # the default; add facts, notes to rerank agents' recall
    # base_url / api_key_env: override the provider's, as for the embedder
```

```yaml
memory:
  reranker:
    kind: decision
    provider: ollama-local        # must be an Ollama provider (/v1/systemone is Ollama's)
    model: nimble                 # `ollama pull nimble`
    timeout_ms: 30000
```

**Rules for the decision kind:**
- **Load-time refusals:** `effort` and `context_tokens` do not apply, and setting them fails config load. So does a non-Ollama provider.
- **At most 26 candidates** are shown, the model's options A–Z. A larger `candidates` is cut to 26, the rest keep their places, and the report's candidate count says 26. This is why its default pool is 20, not the listwise kind's 40.
- **The model never truncates its input.** A prompt over its window (8,192 tokens for `nimble`) is refused, and the call is retried with every candidate cut to half, then a quarter, of its characters. If it still does not fit, the search keeps its order and reports `prompt_too_large`. Measured: no ConditionalQA pool needed a cut at 20 × 1,200 characters.
- **The probabilities stay internal:** they rank well but are overconfident (of the pairs scored above 0.9, only 42% were relevant), so a caller must not threshold on them.

A declared block that cannot be built fails boot. Like the embedder, it is not rebuilt on config reload.

**On a GPU shared with other workloads, `timeout_ms` must cover a model reload, not just the call.**
- When another workload loads its own models, Ollama evicts the reranker's model. Reloading it took about 13 s in the measurement, plus the queue.
- The 30 s default then timed out on every rerank, and every search silently fell back to its unreranked order. `reranked: false` / `timeout` in the response is the tell.
- 120000 held there.
- For the listwise kind, matching `context_tokens` to the num_ctx other callers use for the same model avoids a reload on every switch.

**Each agent opts in** (in yaml, agent frontmatter, or the AgentDef create/fork overlay, which merges per field):

```yaml
agents:
  researcher:
    memory_rerank: { enabled: true, candidates: 40, max_chars: 1200 }
    # candidates: default 40 for listwise, 20 for decision [2-50]; max_chars: default 1200 [200-20000]
```

- **The `candidates` default follows the reranker's kind:**
  - **40 for listwise**, the measured gain above. At 1,200 characters this is about 12,000 prompt tokens, which the 16,384-token `context_tokens` default holds.
  - **20 for decision**, which can show at most 26.
  - An agent that sets `candidates` gets exactly that, whatever the kind.
- There is **no tool parameter and no per-run override**. The model cannot request a rerank, change its budget or switch it off.
- The setting is content-identifying: it is part of `content_sha256`. The kind-dependent default is resolved at search time, so leaving `candidates` unset does not change an agent's hash.
- By operator decision, there is no operator-level ceiling on an agent's `candidates` / `max_chars`. Local models cost nothing, and cloud reranks are covered by the run ledger and token budgets.

**Where it applies — `memory.reranker.sources`** (operator-wide; values `documents`, `facts`, `notes`, `traces`):
- **The default `[documents]`:** `Memory op=search`, `Document op=search`, and `recall` when its `sources` include `documents`. Only a search that can return chunk bodies is reranked. A facts/notes-only selector, or a `prefix` outside `doc.chunk:`, reports `not_a_document_search`.
- **With `facts` and `notes` added:** an agent's default `Memory op=recall` is reranked too. A search that can return none of the listed kinds reports `source_not_enabled`.
- The trace reach-through and the run-start memory injection are never reranked.
- The rerank runs on the fused pool **before** the top-k cut. Its value is promoting a candidate from below `top_k`, so the pool always holds `candidates` rows whatever `top_k` is (at most 51).

**What the response says** (only for an agent that enabled it; every other response keeps its shape): `reranked: true`, or `reranked: false` with a `rerank_reason`. The reasons are:

| reason | meaning |
|---|---|
| `not_configured` | no `memory.reranker` |
| `not_a_document_search` | the search could not return chunk bodies (default `sources`) |
| `source_not_enabled` | the search could return none of the kinds in `memory.reranker.sources` |
| `too_few_candidates` | fewer than two candidates |
| `timeout` | no answer within `timeout_ms` |
| `call_failed` | a transport, provider or key failure |
| `unparseable` | listwise only: the reply held no usable ranking |
| `prompt_too_large` | decision only: the candidates did not fit the model's window even cut to a quarter |
| `not_supported_by_memory_backend` | the agent's memory backend does not rerank |

**It fails open.** Every fault returns the search's own order, never an error. A listwise reply is repaired, not trusted:
- unknown or repeated numbers are dropped;
- candidates the model left out keep their relative order after the ranked ones;
- of several bracketed arrays, the one naming the most candidates is the answer;
- an unfinished reply (cut off by the timeout, or unclosed reasoning) counts as no answer.

A decision answer needs no repair: candidates are ordered by probability, and ties keep the search's order.

**Cost and keys:**
- Each rerank is booked as a `token_usage` row on the **run that searched** and counts against its token budget.
  - Measured for listwise: about 250 prompt tokens for a handful of short sections, up to about 6,000 for 20 × 1,200 characters (about 12,000 at the default 40).
  - Measured for decision: about 2,650 input tokens and 1 output token for 20 sections.
- A rerank is counted against its provider like any other call: a listwise rerank is a chat call (`providers.<id>.max_concurrent` / `lanes.chat`), a decision rerank a decision call (`lanes.decision`). One that waits for a slot past `timeout_ms` keeps search's own order.
- `memory.reranker.max_concurrent` was removed (it defaulted to 4), and a config that still sets it fails to load, naming the lane key that replaces it. Its bound stays as a floor: while the reranker's provider has no lane for the call (chat for a listwise rerank, decision for a decision rerank), reranks are held to 4 in flight. A lane on that provider replaces the floor.
- A tenant's own provider key pays for its reranks when the reranker uses the provider's own endpoint.
- With `base_url` overridden and no `api_key_env`, only a credential named `LOOMCYCLE_RERANKER_API_KEY` can override, so a tenant's vendor key never travels to an operator endpoint.
- The operator-key restriction holds for both kinds: a restricted run with no key of its own gets `call_failed` and search's order, and no request leaves. A keyless local Ollama endpoint has no operator key to protect, so it is never restricted.

**Trust:** stored document text reaches the reranker. The worst a document author can do is reorder candidates the searcher could already see: a listwise reply is read only as in-range numbers, a decision answer only as a probability per known option, and neither reaches the model or the logs.

### Derived search units — memory.unit_generator + derive_units

**Status: optional, no measured benefit yet.**
- On PolicyQA (20 privacy policies, 2,643 questions written by legal experts), units did not beat the header alone on R@5: 0.6065 vs 0.6008, p = 0.39.
- With the rerank on, units did not beat the rerank alone: 0.7075 vs 0.6875, p = 0.29.
- The rerank is the lever that measured: it adds 9–11pp R@5.
- See the research doc `/documents/research/document-units-policyqa` and `bench/results/docs/2026-09-28-policyqa-units/`.
- Nothing is generated unless an operator configures a generator AND marks documents.

**What a unit is.** A short text a model writes about ONE chunk, which is searchable in its place. There are three kinds:
- `description`: one or two sentences on what the section states.
- `claim`: an atomic, self-contained fact from the section.
- `question`: a question the section answers, phrased the way a reader would ask it.

Each unit is stored as a memory row keyed `doc.unit:<chunk>:<kind>:<n>` and embedded under its chunk's index header. Units are not content. They are never returned on their own, and memory's own write ops refuse the `doc.unit:` prefix.

**How a search uses them.**
- Units compete in the same hybrid pool as chunks. After ranking, before dedup, rerank and trimming, a unit hit **resolves to its chunk**: one slot per chunk, carrying `matched_unit {kind, text}` so the caller can see why the chunk matched.
- A search aimed at a chunk prefix also runs a separate unit leg, fused by reciprocal rank.
- Searches over notes or facts never see units, and units do not count toward a scope's memory quota.
- `matched_unit` appears on the in-run search, `Document op=search`, and the operator `POST /v1/_memory/search`.

**Per-agent opt-out.** Set `memory_units: false` on an agent to search without units, even where units exist. This setting is content-identifying and cannot be overridden per run.

**Configuring generation — `memory.unit_generator`.** The block is shaped like `memory.embedder` and `memory.reranker`:
- `provider`/`model` (a `models:` alias works; a `model_pattern` alias does not), `base_url`, `api_key_env`.
- `timeout_ms` (default 120000), `effort`, `context_tokens` (default 16384), `max_output_tokens` (default 2000).
- There is no default model. If `base_url` is overridden without `api_key_env`, the generator uses only `LOOMCYCLE_UNIT_GENERATOR_API_KEY`, so a tenant's key never reaches an operator endpoint.
- `subtrees: [{path, kinds}]` lists the Path subtrees whose documents get units. The deepest covering subtree wins, and `kinds` defaults to all three. A subtree covering `/facts` or `/memory` fails config load.

**Which documents get units.**
- A document's own root-chunk field `index_units` wins: `true` means all kinds, `false` or `[]` means none, and a list means those kinds.
- Otherwise, the deepest marked subtree under ANY of the document's Path names applies.
- Otherwise, the document is never touched.
- Memory trees are always refused, whatever is set: a document named under `/facts` or `/memory`, or one whose chunks carry entity metadata. An opted-in document refused this way is reported in `skipped_by_rule`.

**Running generation — `POST /v1/_document/derive_units`** (operator admin).
- Nothing is generated on write. This is an explicit, bounded, resumable pass modelled on `describe_images`.
- `dry_run` defaults to **true**: it makes no model call, writes nothing, and reports what would happen.
- `limit` is model calls per request (default 25, max 500). `after` is a cursor (`next_cursor` in the report). `scope`/`scope_id`/`tenant` select the store.
- A real run needs the generator and an embedder, and returns a 503 naming whichever is missing. An operator-key-restricted principal gets 403.
- Staleness is judged by the **hash of the chunk body** the units were written from, so editing one body rewrites exactly that chunk's units, and a pass over an unchanged scope makes no model call.
- If a chunk's generation fails, it keeps its old units and is named in the report.
- The report fields are: `documents_examined`, `documents_opted_in`, `skipped_by_rule`, `chunks_examined`, `up_to_date`, `generated`, `rewritten`, `units_written`, `failed`, `failed_chunks`, `first_failure`, `samples`, `next_cursor`, `more`.

**Cost.** Two model calls per chunk: one for the description, and one JSON call for claims and questions. On a local qwen3.6 (`effort: low`) that measured 12.3 s per chunk and 8.6 units per chunk.

## Caveats

- **SQL Memory required** (`LOOMCYCLE_SQLMEM_ENABLED=1`).
- **Tenant scope needs two grants** — `tenant` in both `memory_scopes` and `sql_scopes`.
- **Orphaned bodies are best-effort on delete** — see the integrity note above
  (invisible dead K/V; the row side is atomic).
- **`import_md` chunk boundaries are heading lines** — a chunk body containing
  ATX headings (`## …`) re-chunks on import. For loomcycle exports this is
  faithful; for arbitrary prose use the Document Assistant's semantic chunking.

## The Web UI + the Document Assistant

The Web UI exposes Documents through the **`paths`** tab (open a `document`
node) and the `/documents/:id` deep link: a chunk sub-tree, a Markdown view, MD
download, and a single-chunk content editor (body/fields, optimistic revision).
Structural editing — restructuring, semantic import, linking — is done by the
**Document Assistant** (the viewer's `assistant` toggle): you type instructions
and the **`doc/manager`** agent performs the `Document` ops. That agent +
skills ship as the **[`bundles/document-agent/`](../bundles/document-agent/)**
bundle; register it (see its README) for the Assistant to work — it degrades to
a hint if absent. (RFC AM: Phase 1 = Path console, Phase 2 = viewer + `export_md`,
Phase 3 = `import_md` + the agent.)

## Where it lives

`internal/tools/builtin/document.go` (the tool + the 4-table schema). The
`Document` tool article (`Context op=help topic=Document`, one article per
operation as `Document/<op>`) is the in-agent reference; `docs/SQL_MEMORY.md` covers the backing store and `docs/PATH.md` the
naming layer.

---

**One-sentence thesis:** A Document is a queryable graph of typed, individually
versioned chunks — prose in Memory, structure in SQL Memory, named in the Path
tree — so agents and humans can co-author and query a document the way they
query a database, not diff a blob.
