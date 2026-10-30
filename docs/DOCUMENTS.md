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
id; it cannot key on an empty component.) Releases before v1.123.0 keyed these
rows by the SQL Memory scope id instead (the tenant, or `default` for the empty
tenant), where no tenant-scope Memory read looked:

- Postgres migration `0114` moves them, embeddings included; SQLite does the same
  on open, and every boot repeats it to catch rows a replica on the older binary
  wrote during a rolling upgrade. The boot log reports rows moved, conflicts
  (both coordinates held the key — the later write is kept, except that an
  old-keyspace chunk body whose `body` is empty never replaces the destination;
  fields alone do not count as content) and strays (a tenant document row at any
  other scope id, left where it is).
- A tenant document read looks at the tenant keyspace only. A row still at the
  old scope id reads as missing (an empty body, absent from search) until a boot
  moves it. (v1.123.0 and v1.124.0 also retried the old scope id on a miss; that
  fallback is gone as of v1.125.0.)
- Snapshot restore re-keys an older snapshot's tenant document rows the same way.
- **Do not run a v1.122-or-older replica beside v1.125.0 or later.** Upgrade
  through v1.123.0 or v1.124.0, or stop the older replicas before starting the
  new ones. In a mixed cluster the older replica reads moved bodies as empty, and
  a tenant chunk it writes lands at the old scope id, where the newer replica
  reads it as missing until the next boot's sweep moves it. If the older replica
  writes an empty body back (a fields-only `update_chunk`), the sweep discards it
  in favour of the moved body; a real edit made through it is kept if it is the
  later write.
- Rolling the binary back past `0114` needs the rows moved back by hand (the down
  migration is a no-op).

**Memory and document rows.** Because chunk bodies share the Memory keyspace,
the Memory tool keeps them behind the Document tool's checks:

- Writes: `set`, `incr`, `merge`, `append_dedupe` and `bounded_list` refuse a
  `doc.chunk:` (and `doc.unit:`) key with a validation error; `delete` and
  `supersede` refuse a unit and a provenance-free (prose) chunk body. A
  consolidated fact's body carries provenance, and the consolidation pass may
  still retire or remove a fact by its `doc.chunk:` id. The off-run key routes
  (`PUT` / `DELETE /v1/_memory/scopes/{scope}/{scope_id}/keys/{key}`) apply the
  same rule to every caller and answer `400 document_row`, pointing at
  `/v1/_document`; they share the Memory tool's whole write rule, so a
  `trace.turn:` key is refused there as well (`400 reserved_key`), and a
  `core/` key (a core memory block, which an operator seeds) is written or
  removed there only by a tenant operator or an admin (`403 permission_denied`
  for a member).
- Tenant reads: reading tenant document rows through Memory needs
  `sql_scopes: [tenant]` as well as `memory_scopes: [tenant]`, the pair the
  Document tool needs. Without it, a call that asks for documents (`documents` in
  `sources`, a `doc.chunk:` prefix, a document filter, a `get` of a prose body)
  is a permission error naming the grant; a search or list that does not ask
  leaves the document class out. Facts, notes and traces are unaffected.
- Access control: for a caller whose access-control set denies reading a
  document, Memory `get` answers that document's chunk, homed fact or unit
  exactly as a missing key, and `list`, `search` and `recall` leave those rows
  out. This holds in a run, over MCP and gRPC, and on the off-run key routes and
  search. A caller whose set cannot deny anything sees no change.

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
- **Response.** The same `filter: {applied, matched_documents, matched_chunks, inverted}` block beside `entries` / `memories`, only when a filter was given. A path is a name in one scope's tree, so a caller recalling in several scopes makes one call per scope and gets each scope's own counts — except for documents, which `documents_scope` (next section) brings into the same call, the filters then aimed at that scope.
- **Access control on the hits.** Independently of any filter, a Memory search's or recall's hits are checked against the caller's access-control set by key: every `doc.chunk:` hit, and every unit through its chunk, whatever the row's class, is kept only when `Document op=search` would keep it. A chunk with no document row is dropped. A caller whose set cannot deny anything gets no lookup.
- **Off-run search.** The operator console's `POST /v1/_memory/search` takes the same four lists (TS `memorySearch`: `underPaths`, `excludeUnderPaths`, `documentIds`, `excludeDocumentIds`) and answers with the same `filter` block, only when a filter was sent. It runs the Memory tool's own resolution step with the Document tool as resolver, under the caller's authoritative identity: the principal's tenant, the request's `scope_id` as the subject, the operator-plane grants the off-run Document routes use, and the principal's access-control set for the narrowing. The same identity checks the hits, as above, and a failed check fails the request (500) rather than returning hits unchecked. A refusal is answered by its class: `400 invalid_filter` (validation), `403 permission_denied`, `400 filter_unavailable` (no SQL Memory, filter too wide). A `tenant` search resolves its filter in the tenant's tree.

**Another scope's documents in the same call (`documents_scope`).** An agent often remembers in one scope and consults its reference corpus in another: its own notes in `user`, the organisation's documents in `tenant`. `Memory op=search` and `op=recall` take `documents_scope: agent | user | tenant` to search both in ONE call, instead of two calls merged by hand.

- **Two legs, one list.** The primary leg is the call as it stands: its `scope`, `sources`, `prefix` and `when`. The document leg is the prose of `documents_scope`, and only its documents. The query is embedded once. Both legs are fused into one pool, with one rerank (when the operator enabled it) and one `top_k`. `when`, the rerank setting and the derived-unit setting apply to both legs; `prefix` applies to the primary leg only.
- **Every hit names its scope.** With a `documents_scope` different from `scope`, each entry (search) or memory (recall) carries `scope`. Without it, no key is added and the response is byte-identical.
- **Filters aim at the document leg.** `under_paths`, `exclude_under_paths`, `document_ids` and `exclude_document_ids` resolve in the `documents_scope` tree and narrow its documents only. The `filter` block then carries `scope`. The primary scope's documents, if its `sources` include them, are not filtered.
- **Authority.** The document leg needs the Memory tool's own `memory_scopes` grant for that scope AND everything `Document op=search` in that scope needs (`Document.resolveScope`: both tenant grants for `tenant`, isolated-member confinement, the user id). A refusal of either refuses the whole call with a permission error naming the missing grant; the call never answers from one scope. The tenant is the run's own.
- **Access control.** The caller's access-control set is applied to each leg's hits in that leg's scope, by key (`doc.chunk:` and every unit through its chunk), whatever the row's class: a fact homed in a denied document's chunk is dropped too. A caller whose set cannot deny anything gets no lookup.
- **Equal scopes.** `documents_scope` equal to `scope` is shorthand for adding `documents` to `sources`, and the answer is that call's, byte for byte, on any memory backend. A search with no `sources` already returns documents and is left alone; a recall's default (facts and notes) gains documents.
- **Refused:** an unknown value; `sources: [traces]` with a different `documents_scope`; a different `documents_scope` when the agent's memory is served by another backend, or when this server has no documents (no SQL Memory).
- **Reads only.** Nothing the leg finds is written anywhere.

**Off-run.** `POST /v1/_memory/search` takes `documents_scope` and `documents_scope_id` (TS `memorySearch`: `documentsScope`, `documentsScopeId`). `documents_scope_id` names the second subject as `scope_id` names the first, under the same rules: required for `agent` and `user`, ignored for `tenant`, and the tenant always comes from the token. The leg goes through the Document tool's gate under the caller's identity (the identity the first scope's filter uses), and each scope's hits are checked against the caller's access-control set in that scope. Hits carry `scope`, and the response echoes `documents_scope` and `documents_scope_id` (`""` for tenant), only when a second scope was searched.

- `documents_scope` equal to `scope` with the same subject is the sugar above. With a different `documents_scope_id` it is refused (`400 invalid_documents_scope`), because hits name their scope, not their subject.
- Other refusals: `400 invalid_documents_scope` (unknown value, or combined with `[traces]`), `400 missing_documents_scope_id`, `400 filter_unavailable` (no documents on this server), and `403 permission_denied` from the Document gate.

#### The word leg's language — `memory.fulltext`

The full-text leg matches words through a Postgres *text search configuration*. A configuration decides how a word is reduced before it is compared: `english` turns "contracts" and "contract" into the same lexeme, `simple` only lowercases. Until this block, the leg always used `english`. On a corpus in another language an inflected term therefore never matched its other forms, and the vector leg carried the search alone.

```yaml
memory:
  fulltext:
    config: english   # any configuration Postgres knows; default english
    unaccent: false   # strip diacritics on both sides (the `unaccent` extension)
```

- **Server-wide.** The words live in one generated column on `memory_embeddings`, shared by every tenant and scope, so there is one setting per server.
- **A change rebuilds the column at the next boot.** Startup compares the setting with the one recorded on the column. When they differ it drops the column and its GIN index, re-adds them under the new configuration and records the new setting, all in one transaction. The table is locked while this runs, for a time proportional to its rows; startup logs the old setting, the new one and the row count first. A restart with the same setting does nothing. An unset block never touches the column the migrations built. Replicas booting together serialise on an advisory lock, so only the first rebuilds. A rebuild needs a restart; a config reload does not apply it.
- **Who applies it.** The server boot, and `loomcycle migrate up --config <yaml>` (with `--dsn` alone it has no setting to apply). Other subcommands that open the store, such as `loomcycle tokens`, never rebuild the column. The rebuild is DDL on `memory_embeddings`, so the role that runs it must own that table. If the server's role does not, apply the change with `migrate up --config` as the role that owns the schema; the server then finds the column already matching and changes nothing.
- **The query side follows the column**, not the process's own config. The search reads its query text with the same configuration and the same unaccent pass the column was built with, re-reading the setting recorded on the column at most every 30 seconds. An accented query and an unaccented row (or the reverse) both match.
- **Rolling a change across replicas is last-writer-wins.** A replica that boots with a different setting rebuilds the column and logs a warning naming the column's setting and its own. The replicas already running follow the new column within 30 seconds; one still configured with the old setting rebuilds it back when it next boots. Put the new setting in every replica's config before restarting any of them.
- **An in-place change to a text search configuration** (`ALTER TEXT SEARCH CONFIGURATION … ALTER MAPPING`, a new dictionary) keeps its name, so no boot sees a change, and the stored words stay as the old mapping reduced them. Force a rebuild by overwriting the recorded setting, then restart or run `migrate up --config`:

  ```sql
  COMMENT ON COLUMN memory_embeddings.embed_text_tsv IS 'rebuild';
  ```
- **A configuration Postgres does not know fails the boot**, naming it. A search that silently stayed English is the defect this setting fixes. The value must be a name (`letters, digits, _`, optionally `schema.name`) because it is written into DDL.
- **Languages Postgres does not ship.** Postgres ships Snowball stemmers for about thirty languages (`\dF` in psql lists them), but not for Ukrainian. For Ukrainian, install a hunspell dictionary on the database server and create a configuration from it:

  ```sql
  -- files uk_ua.affix / uk_ua.dict / uk_ua.stop in $SHAREDIR/tsearch_data
  CREATE TEXT SEARCH DICTIONARY ukrainian_hunspell (TEMPLATE = ispell, DictFile = uk_ua, AffFile = uk_ua, StopWords = uk_ua);
  CREATE TEXT SEARCH CONFIGURATION ukrainian (COPY = simple);
  ALTER TEXT SEARCH CONFIGURATION ukrainian ALTER MAPPING FOR word, hword, hword_part WITH ukrainian_hunspell, simple;
  ```

  Then set `config: ukrainian`. Without a dictionary, `simple` plus `unaccent: true` matches exact word forms only.
- **`unaccent`** creates the extension in `public` if it is missing (trusted since Postgres 13, so the database owner may), and an `IMMUTABLE` wrapper, `loomcycle_fts_unaccent`, that the generated column can use. An extension that already exists is used where it is.
- **Tiers.** Postgres with pgvector only, the one tier with a word leg. sqlite ignores the block.

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

## Relations as statements

A relation between two chunks (`link_chunks`, a `[[name]]` link, an import's edges) is a **statement**: a `chunk_edges` row that says not only *that* chunk A relates to chunk B by a kind, but who or what made the claim, from which text, since when, and whether anyone has checked it.

**Why statements, not bare edges.** An edge used to be `(from_id, to_id, kind)` and nothing else. It could not say why it existed, so every consumer that needed an answer (a reviewer, a links pass, a judge) kept its own ledger beside the store and rebuilt edges from it: one fact in two places, with nothing keeping them in step. A statement carries those answers itself, and every reader the runtime owns (`get_edges`, `backlinks`, `graph_recall`, `export_md`, `judge_fact`, `sync`) sees them without a second table. An existing scope is rebuilt into the statement shape on first use: every old edge keeps its endpoints, kind and age, and reads back as an asserted statement with no evidence (`wikilink` for an inline `[[name]]` edge, `manual` otherwise).

The per-op reference is in the tool's own help: `Context op=help topic=Document/<op>` (for example `topic=Document/propose_statement`).

### What a statement is

| field | meaning |
|---|---|
| `id` | the statement's own id, which every review and maintenance op takes |
| `from_id` / `to_id` / `kind` | the relation; direction matters (`backlinks` lists it at `to_id`) |
| `status` | `asserted`, `proposed` or `rejected` (below) |
| `method` | how it was made, stamped by the server (below) |
| `confidence` | the writer's own 0..1. It is recorded and never hides the statement |
| `valid_at` / `invalid_at` | world time: when the relation began and stopped holding (unix ns). Unset `invalid_at` = still holds |
| `attributes` | the relation's qualifiers, a small JSON object (at most 4 KiB) |
| evidence | verbatim quotes that support it (below) |
| verdict | `verdict_confidence`, `judged_at`, `judge_reason`, `judged_by`: what a judge found. Only a verdict withholds |
| `operator_ruled_at` | set when an operator accepted or rejected it |
| `actor`, `run_id`, `created_at`, `verified_at`, `expired_at` | who wrote it, when it was last re-checked, when it was retired |

**One statement per relation.** A statement is identified by `from_id`, `to_id`, `kind`, `valid_at` and, where the tenant's vocabulary declares one for the kind, a discriminating qualifier. Linking the same relation again does not make a second row; it adds any new evidence to the existing one. A statement that differs in `valid_at` (a dated amendment) or in its discriminator coexists with the first.

**Status lifecycle.**

```
propose_statement ──▶ proposed ──accept_statement──▶ asserted ◀── link_chunks, imports, sync
                         │  ▲                           │
         reject_statement│  └──maintain_statement demote┘
                         ▼
                      rejected ──accept_statement (rejected in error)──▶ asserted
```

- **asserted** is live: every reader shows and follows it.
- **proposed** is filed for review. It shows in `get_edges` and the review queue, but `backlinks` and `graph_recall` leave it out unless asked (`include_proposed`).
- **rejected** is kept, never deleted, as the record that the relation was considered and refused. It is withheld from every reader, and the same relation is refused (`statement_rejected`) if anyone proposes or links it again, at any `valid_at`. Only an operator's own `link_chunks` can re-assert it.

An asserted statement is never *rejected*. It leaves the live set in one of three ways: `unlink_chunks` deletes it with its evidence; `judge_fact` with the statement's id and `verdict: unsupported` keeps it but withholds it; `maintain_statement action: expire` retires it (readable with `include_retired`). A `[[name]]` relation is removed by editing the link out of the body, because the body re-creates it on every write.

**Methods.** The server stamps every statement's method. None of the fixed ones is accepted from the wire:

| method | written by |
|---|---|
| `manual` | `link_chunks` and `propose_statement` without a method: a person's (or a model's) own write |
| `wikilink` | a `[[name]]` link in a chunk body |
| `import` | `import_md` (its `<!-- loom-edges -->` block, and the `amends` statements of an edition) |
| `sync` | a link pulled from a peer by `sync` |
| `canvas` | `import_canvas` |
| `runtime` | the runtime's own links: `about`, `derived_from` and the like from `upsert_chunk` |
| `consolidator` | the relation a fact states (`upsert_chunk` `relation`), when no method is named |
| `ontology` | a relation declared in the tenant ontology's Instances section |
| `rule:<name>` / `extractor:<name>` | a code agent granted `statement_methods` (see below): code found it, or a model the code drives found it. `<name>` is 1–64 of `a-z 0-9 _ . / -` |

A later pass treats a `manual` statement as a person's and never undoes it (see `maintain_statement`).

**Reserved kinds.** `about`, `supersedes`, `references`, `mentions`, `derived_from` and `supported_by` are written only by the runtime, because a caller-made one would be read as the runtime's: a manual `supersedes` would block a real correction, a manual `references` would be mistaken for an inline link. `link_chunks`, `propose_statement` and the import, canvas and sync paths refuse them with `kind_reserved`. The one exception: `link_chunks` still accepts `about` when `from_id` is a fact.

**Vocabulary.** Where the tenant ontology declares relation kinds, each write is checked against them. A person's own `link_chunks` that does not fit is refused. Every other writer is annotated instead: the statement is filed with `ontology_valid: false` and `warnings`, for the reviewer to see. The kinds themselves (inverse names, qualifiers, discriminators, which kinds a walk follows, which a rule may auto-assert) come from the ontology's Relations section; `Document op=describe_ontology` returns them as data, and the ontology has its own documentation.

**Evidence.** Each evidence row has exactly one source:

- `{chunk_id, quote}`: a chunk of this scope;
- `{session_id, event_seq, quote}`: a turn of a chat the caller can read;
- `{scope, natural_key, quote}`: a fact in another scope.

The quote must be verbatim. For a chunk or a chat turn the server finds it in the source at write time, records `char_start` / `char_end`, and refuses a quote that does not occur there (`evidence_quote_not_found`). A reference into another scope is kept as written: this scope cannot read that one to check it, so such a row is not ground for a verdict either.

- **Bounds.** A quote is at most 4,000 characters. A statement keeps at most 32 rows (`LOOMCYCLE_DOCUMENT_EVIDENCE_MAX_PER_EDGE`); past that, a write reports `evidence_capped: true` and adds no more.
- **Readers.** `get_edges`, `backlinks` and `list_statements` carry the first three rows and `evidence_count`; `get_edge` carries every row.
- **Withheld when unreadable.** A row whose source is a document the caller may not read is left out of every answer, and `evidence_count` counts only what is shown. At write time, citing a chunk the caller may not read answers exactly as a chunk that does not exist, so a quote check cannot reveal text in a document the caller cannot open.
- **Stale.** `stale: true` marks a quote that no longer occurs in its source. A maintainer can mark a row stale or re-point it (`maintain_statement`).

### Writing and reviewing

**`link_chunks`** asserts a relation: method `manual`, optionally with evidence, validity, confidence and attributes. It is a person's act, so it refuses a `method`. Linking a relation that is awaiting review asserts it.

**`propose_statement`** files a relation as `proposed`. Use it when a relation was found in text and should be checked rather than asserted. It cannot change or remove anything that exists:

- proposing a relation that is already proposed adds the new evidence to it;
- a relation that already holds is refused (`statement_asserted`); pass its `id` to propose a correction instead, filed beside it with `attributes.revision_of` naming the original, which stays as it is;
- a `rule:` statement of a kind the vocabulary marks auto-asserting, with every quote in a chunk, is asserted directly. A correction proposed with `id` always stays proposed.

```json
{"op": "propose_statement", "scope": "tenant",
 "from_id": "e1f3a5b7c9d14e2f8a4b6c8d0e2f4a6b", "to_id": "5f1c9e2a7b3d4e6f8a9b0c1d2e3f4a5b", "kind": "implements",
 "evidence": [{"chunk_id": "e1f3a5b7c9d14e2f8a4b6c8d0e2f4a6b", "quote": "This order implements Directive 2019/1937"}],
 "confidence": 0.8}
```

```json
{"id": "7c2e9a1b3d4f4e6a8b0c2d4e6f8a1b3c", "status": "proposed", "created": true, "evidence_written": 1, "ontology_valid": true}
```

**`accept_statement` / `reject_statement`** are the operator's ruling on a proposal. They are refused inside an agent run, so a model can never accept its own proposals. `reject_statement` needs a `reason`, which is shown to whoever proposes the relation again. `accept_statement` also reverses a rejection made in error, and overrules a judge who found the relation unsupported (recording the operator as the judge). Of two operators ruling at once, the second gets `statement_changed`. A ruling also needs the `link` permission on the documents at both ends.

**Two permissions, `propose` and `link`.** The access-control model has a `propose` action on documents, narrower than `link`, so a principal can be allowed to suggest relations without being allowed to make them live:

| action | ops |
|---|---|
| `link` | `link_chunks`, `unlink_chunks`, `accept_statement`, `reject_statement`; also `maintain_statement` `expire` / `demote` / `merge` (on both ends) |
| `propose` | `propose_statement`, every `maintain_statement` action, `lease`, `release` |
| `read` | `get_edges`, `get_edge`, `list_statements`, `backlinks` |

A principal holding only `read` and `propose` can file proposals but cannot add evidence to a relation that already holds: evidence on an asserted statement is live data.

**Reading.**

- `get_edges` takes a `document_id` and lists every statement in or out of the document's chunks, with both endpoints' titles, types and documents.
- `backlinks` takes a chunk `id` and lists what points at it, with `inverse_kind` when the vocabulary names the relation from this end.
- `get_edge` reads one statement with all its evidence.

They differ in what they show by default:

| | proposed | rejected or judged unsupported | retired |
|---|---|---|---|
| `get_edges` | shown (`include_proposed: false` hides) | `include_refuted` | `include_retired` |
| `backlinks` | `include_proposed` | `include_refuted` | `include_retired` |
| `graph_recall` | `include_proposed` | `include_refuted` follows a judged-unsupported relation; an operator-rejected one is never followed | — |

**The review queue: `list_statements`.** It lists statements of one `status` (default `proposed`) across every document of the scope, each with its endpoints' titles and documents, `auto` (made by a `[[name]]` link) and up to three evidence rows, so a reviewer can decide without opening each one. The same filters let a maintainer pick what to re-check:

- `kind` / `kinds`; `document_id` (either end in the document); `evidence_document_id` (an evidence row quotes the document); `method_prefix` (exact, case-sensitive, e.g. `rule:` or `extractor:cite`); `under_path` (either end named under a Path);
- `include_retired` adds expired relations and ones whose `invalid_at` has passed;
- `order: created` (default) is newest first; `order: verified` is least recently verified first, never-verified ones before any. A cursor from one order is refused under the other.

Filters combine with AND, and never show more than the plain listing would: naming a document the caller may not read selects nothing.

**The review route.** Rulings are made from the off-run operator plane: `POST /v1/_document` with the op in the body (and its twins, the gRPC `Document` RPC, the MCP `document` meta-tool and the adapters' `document()`). The route is tenant-confined: a `substrate:tenant` operator reviews its own tenant's statements.

```bash
# the tenant's proposals awaiting review
curl -sS -X POST localhost:8080/v1/_document -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -d '{"op":"list_statements","scope":"tenant","status":"proposed","limit":50}'
# rule on one
curl -sS -X POST localhost:8080/v1/_document -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -d '{"op":"reject_statement","scope":"tenant","id":"7c2e9a1b3d4f4e6a8b0c2d4e6f8a1b3c","reason":"The quote cites the 2015 act, not this one."}'
```

**The Web UI.** The document viewer's cross-references panel (the `@loomcycle/explorer` package) shows each statement's status, an unusual method, whether it was judged (the reason as a tooltip), its validity and its first evidence quotes, with a quote whose source changed marked as such. The relationship graph draws a proposed statement dotted. A proposed statement gets **Accept** / **Reject** buttons; Reject opens a reason field, and a refusal from the runtime is shown inline. There is no separate review-queue page: the queue is `list_statements`.

### Code agents that maintain relations

A links pass (find citations in imported text, propose the relations, keep them true as the documents change) is deterministic work, so it runs as a `provider: code-js` agent. Three pieces make that safe.

**`statement_methods`** (an agent definition field; see `docs/CONFIGURATION.md` §7) lists the method kinds the agent's runs may stamp: `rule`, `extractor`, or both. A run then names its method per write: `method: "rule:citations"`.

- **code-js only.** A `rule:` method claims that code made the relation, and only a code-js body is code. The field is refused at config load and at AgentDef create/fork for any other provider, and a model-driven definition that reached a run some other way holds no grant.
- **Fork narrows, never widens.** A new definition authored from a run may list only methods its author may stamp; a fork may list only what the version it forks already holds, even when the author holds more. The grant is what lets a rule's writes skip review (auto-assert), so the operator chooses which bodies hold it.
- Not per-run overridable, and a sub-agent holds only its own definition's grant. Authority, not content: not part of `content_sha256`.
- `method` is taken by `propose_statement`, `maintain_statement` and `upsert_chunk` (for its `relation`). A malformed method, or one outside the grant, is refused with `method_not_granted`; `link_chunks` refuses a method outright. Without the grant, every relation an agent writes is `manual`.

**`maintain_statement`** is the upkeep of a relation that already exists. It never asserts one (acceptance is an operator's) and never deletes one or an evidence row. It is open to a code agent holding statement methods and to an operator session.

| action | effect |
|---|---|
| `verify` | stamp `verified_at`; nothing else changes |
| `expire` | retire it; `reason` required, recorded with the method as why and by what |
| `demote` | an asserted relation goes back to `proposed`; `reason` required |
| `merge` | move its evidence onto `survivor_id` (a live statement of the same relation and status), then retire it as `merged`. Rows the survivor already holds stay put |
| `stale_evidence` | mark the row `evidence_id` stale: its quote is no longer where it points |
| `relocate_evidence` | re-point the row `evidence_id` to `chunk_id`, where its quote must occur; the span is recomputed and the stale mark cleared |
| `flag` | set `attributes.needs_review` (and the reason) for a person |

- A relation a person wrote takes only `verify` and `flag`, from anyone (`statement_manual`). That is a `manual` one, and a `sync` one unless its `attributes.origin_method` names a machine method (`rule:` / `extractor:`, `import`, `canvas`, `wikilink`, `runtime`, `consolidator`, `ontology`): a link a person made on a peer is still a person's here, and one whose origin is unknown is treated as one. So does one an operator ruled on, from inside a run (`statement_operator_ruled`): flag it and let a person decide.
- Every action needs `propose` on the relation's documents; `expire`, `demote` and `merge` change what readers see as live, so they also need `link` on both ends.
- A relation that changed while the pass looked at it is refused with `statement_changed`: reload it (`get_edge`) and decide again.

**`lease` / `release`** make a pass exclusive on a scope. A lease is named (`linker`, `citations`, …): a second pass of the same name finds it held and stops before writing, while leases of different names never block each other, so a links pass does not stall memory consolidation of the same scope. It expires after `ttl_ms` (default 60 s, at most one hour), so a pass that dies never wedges the scope; the holder renews by calling `lease` again and ends it with `release`, on every exit path. The holder is the run that took it, never a name the caller supplies. Only the operator plane and a code agent holding statement methods may take one.

A citation pass, end to end:

1. subscribe to the scope's `documents/ingested` channel (below), or run on a schedule;
2. `lease` `{"name": "citations", "ttl_ms": 300000}`; stop if `acquired` is false;
3. `extract_citations` on the new or changed document: the class's citation grammar finds each citation with its span and resolves it to a work and part (read-only);
4. `resolve_anchor` with `create_placeholder: true` for a work the scope does not hold yet;
5. `propose_statement` with `method: "rule:citations"` and the citation as evidence `{chunk_id, quote}`; later passes `maintain_statement` what they find stale;
6. `release`.

### Editions

A law, a standard or a contract is amended and the new edition imported. Imported as a new document, every statement, tag and citation stays on the old one; imported over it, every chunk is recreated and they are all lost. An edition is neither: most parts are unchanged and keep everything attached to them, a few changed and are superseded with dates, a few were removed and close, a few are new.

**Work keys.** A work is recognised by its key, e.g. `law:ua/2297-vi`: claimed on the root chunk by the first keyed import, or the identity the document's ontology class derives. A key is unique in its scope (`identity_collision` otherwise).

**`import_md` with `reimport: true`** and the document's `document_id` or its `work_key` reconciles the incoming Markdown onto the held document part by part. Each part is recognised by its **anchor** and by its text, in five passes:

1. same anchor, same text: untouched, keeping its id, statements, evidence and tags;
2. held text now under another anchor (renumbered or moved): the part keeps its id and everything linked to it, and takes the new anchor. This runs before pass 3, so inserting an article and renumbering the ones after it moves them, rather than handing one article's links to the next;
3. same anchor, new text: **superseded**. The old version closes at `edition_date` (it is still what the work said before) and a new one opens there, joined by `supersedes`. A statement follows the new version where its evidence still holds; the others stay on the old version, marked `proposed` for re-checking. A statement a person made and one an operator ruled on always follow, with their ruling: a rejection stays a rejection on the new version, so the relation stays refused there. Inline `[[name]]` links are not carried: the new text links what it names;
4. a held part the edition no longer has closes; nothing is deleted;
5. a new part opens at `edition_date`.

The title and the preamble are updated in place (the earlier text stays readable with `history`). The response's `reimport` block counts `unchanged`, `superseded`, `moved`, `closed`, `created`, `statements_copied` and `statements_proposed`, and `changed_chunk_ids` names what changed. Running the same reimport twice changes nothing, a reimport that stopped part way is completed by running it again, and two reimports of one work run one after the other. A reimport reads headings, text and each part's type, status and fields; it ignores the `loom-edges` block, `valid_at` / `invalid_at` in `loom:` lines (a part's period comes from `edition_date`) and pictures.

**Anchors and `stable_id`.** An anchor is the part's heading path (`article-12/paragraph-3`), or what a class-declared anchor grammar produces. A heading with no letters (a bare `1.2`) adds nothing to the path, so renumbering a list does not re-key everything under it; a repeated heading under one parent becomes `notes~2`. A part keeps its anchor once it has one, so renaming a heading later does not silently re-key what links to it, and a renumbered part is still found by the anchor it first had. Alongside the anchor each part carries a `stable_id`: the identity of the provision itself, which a moved part keeps and a superseding version inherits.

**`edition_date`** is when the edition came into force (unix ns; default now). Import editions in date order: an `edition_date` earlier than the edition in force is refused before anything is written.

**Placeholders and `resolve_anchor`.** `resolve_anchor` turns a citation into a chunk: the work by `work_key`, the part by `anchor`, as of `as_of`, so a citation to a past edition resolves to the text in force then. A work the scope does not hold (or holds in a document the caller may not read) answers `found: false`; with `create_placeholder: true` an empty document is filed for it (status `unresolved`). Link citations to the placeholder's root: its backlinks are the list of works still to import, and the first keyed import of that work fills the placeholder in. After a renumbering a heading path can name two parts; the part now under the heading wins, and an `as_of` before the renumbering reaches the other.

**Who made the edition.** With `amending_document_id` (the amending act, a document of the same scope) on a reimport by `work_key`, each part the edition renumbered or moved gets one `amends` statement from that document's root, with `attributes: {mod_type: renumbering}`, method `import`, valid from `edition_date`. Untouched, superseded and new parts get none, and running the edition again adds none. The caller must be able to read and link both documents. Where the vocabulary declares relation kinds and does not accept the statement, the edition is still imported and the skip is counted (`amends_skipped`).

**From a file.** `File op=import` takes the same `reimport`, `work_key`, `edition_date` and `amending_document_id`, so an extracted PDF of a new edition reconciles onto the held work exactly as `import_md` does. `doc_path` and `source` apply only to a first edition. See `Context op=help topic=File/import`.

**The `documents/ingested` event.** `create_document`, `import_md` and `File op=import` publish `{document_id, reason, timestamp, changed_chunk_ids?}` to the scope's `documents/ingested` channel, with `reason` `created`, `imported` or `reimported`. It is what makes "on import" something a links pass can subscribe to rather than poll. The event names the document to everyone on the channel, so it is not published when the caller's access-control rules hide part of the scope from reading; the result then says why in `not_announced`.

### Facts and documents

A fact (a chunk with fact metadata, written by `upsert_chunk`) and the documents it was taken from are joined by statements, so a fact can be checked against its passage and a passage can show what was learned from it.

- **`source_chunk_id`**: the passage the fact was taken from. The fact is linked to it by `derived_from`, with the fact's `source_quote` as evidence and its span (`char_start` / `char_end`, which can also select the quote). The quote must occur in that chunk, and the caller must be allowed to read and link its document. A fact has one source; passing another replaces it. `judge_fact` then checks the fact against the passage as it is now, and `Memory op=recall` returns `source_chunk_id` and `source_document_id` (left out when the caller may not read that document).
- **`about_ids`**: the subject nodes the fact is about, each linked by `about`. The list is the whole set: a subject an earlier upsert named and this one does not is no longer linked. One the caller may not read and link is skipped and counted in `about_skipped`.
- **`mentions`**: a `[[name]]` link in prose that resolves to a subject node is a `mentions` statement, not `references`, with the link's span as evidence. Facts are *about* a subject; passages only *mention* it. `list_facts` with `about` returns the facts and, separately, `mentioned_in`.
- **A fact's `relation`**: with two or more `about_ids`, `relation` names the relation the fact states between the first two (e.g. `works_at`). It is recorded as a statement whose evidence is the fact, with method `consolidator` unless a granted code agent names its own. A kind the tenant has not declared is recorded for review. Another live relation of the same kind from the same subject to a different one is reported in `relation_conflicts`, never removed. A later upsert with `about_ids` replaces the relation the fact stated, and retiring or deleting the fact ends it, unless another fact still states it or an operator accepted it.

```json
{"op": "upsert_chunk", "scope": "tenant", "natural_key": "person:ada|works_at|acme", "document_id": "b4407b522dc11495d3de371311db17f0",
 "title": "Ada works at Acme", "type": "employment", "subject": "Ada",
 "source_chunk_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "source_quote": "Ada joined Acme as head of research.",
 "about_ids": ["2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f", "7a6b5c4d3e2f10a9b8c7d6e5f4a3b2c1"], "relation": "works_at"}
```

The answer adds `derived_from: {statement_id, chunk_id, char_start, char_end}`, `about_written` / `about_skipped`, `relation: {statement_id, kind, status}` and any `relation_conflicts`.

### Following relations in retrieval

Search finds chunks by what they say; statements let retrieval also reach what they are *related to*: the article a clause cites, the term it is defined by.

**`graph_recall`** walks statements hop by hop from seed chunks (or a `query`):

- `walk: declared` follows only the kinds the tenant declared for walking, and is the default where any are declared; `walk: all` follows every relation (the default otherwise). `kinds` overrides both, and is the best way to keep a walk on topic.
- `decay` (with `budget_chars`) lets each hop spend at most `decay`^hop of the budget, so far hops cannot crowd out near ones: default 0.7 for a declared walk, else 1.
- `include_proposed` also follows proposals; a row reached over one carries `via_status: proposed`.
- `as_of`: a relation is followed only while its own `valid_at` / `invalid_at` contain the moment asked about (now by default), so a past question walks the relations that held then.
- Each row reached over a relation carries `via_kind`, `via_id` and `via_quote`, the relation's first evidence quote, to cite.

**`search` with `expand`** (`{kinds?, budget_chars?}`) adds, after ranking, the chunks the top five hits are related to (one hop), each with `expanded_via: {kind, from_chunk_id, quote?}`. Without `kinds` it follows the declared walk kinds, or every relation except `about` and `mentions`. The search's filters apply to what it adds. A reader denied part of the scope is searched only over what it may read, so its page is as full as anyone's and `expand` starts from that page.

**`related` with `related_to`** returns a chunk's neighbours by relation (what it cites, what cites it) as `linked`, each with `via_kind` and `via_quote`. It needs no embedder; with `id` too, the by-meaning neighbours come back beside them.

**`concept` / `concept_prefix`** on `search` and `related` keep only chunks about a concept of the tenant's concept schemes (or a narrower one). In the tenant scope, where the concepts live, a chunk matches through a live `subject` statement (or a narrower kind) to the concept; a proposed, rejected, refuted or retired statement does not count, and neither does a tag alone. In a `user` or `agent` scope a statement cannot reach a concept, so a chunk matches by carrying the concept's tag. `filter.applied.concept_matched_by` says which (`statement` or `tag`). The schemes themselves are part of the ontology, documented with it.

### Sync

`sync` between two loomcycles carries statements between keyed chunks with their envelope (`confidence`, `valid_at` / `invalid_at`, `attributes`) and their evidence.

- **Asserted only.** A proposal is one side's review state, and a relation a judge refuted or an operator rejected stays where it is. A pull never undoes a local rejection: such a link is left out and counted in `edges_skipped_rejected`.
- **Pull.** A pulled link is asserted here with method `sync`; the method it was made by on the peer is kept in `attributes.origin_method` (for a link the peer itself pulled, the method it was first made by).
- **Evidence is re-verified.** A row is matched by the cited chunk's `natural_key`, and its quote is checked again against this side's text. A row that cannot be checked here (an unkeyed chunk, a chat turn, another scope, a key this side does not hold or may not read, a quote not in this side's text) is dropped and counted in `evidence_dropped`.
- **Updates.** A link pulled before follows the peer's `confidence`, `invalid_at` and `attributes` (`edges_updated`). A link written here, or one an operator here ruled on, keeps its own envelope and gains only the evidence it lacks (`edges_envelope_kept`). `valid_at` is part of a statement's identity, so a peer link with another `valid_at` arrives as a second statement.

**What push cannot do.** A pushed link arrives on the peer as the peer's own `manual` statement, its method here in `attributes.origin_method`, and the peer checks every quote again; only quotes from keyed chunks of this document whose text the peer now holds are sent. The peer has no way to change an existing link's envelope, so a push adds only the evidence a peer link lacks, and counts an envelope that differs in `edges_envelope_kept`.

## Imported documents — origin and source link

A document built from an outside file or message says so, and points back to where it came from.

- **`import_md` with a source.** `source_url`, `source_kind` (`sharepoint`, `email`, `attachment`, `web`, `other`; default `other`) and `source_title` mark the document as imported — for example an email body an agent imports as Markdown. The url must be http(s), at most 2 KiB, with no user information; signed query parameters are masked and the fragment is dropped. Pass the item's web link (a SharePoint item's `webUrl`, a message's `webLink`), never a download or upload link. A source cannot be combined with `document_id` (an import under an existing chunk). Without a source, `import_md` imports as before, unmarked.
- **`File op=import`** (an extracted file from a run's inbox) takes the same link as `source: {url, kind, title}` and adds what the runtime recorded when the file was captured, while the bytes still hash to that record: `file`, `media_type`, `sha256`, `captured_from` (`mcp:<server>/<tool>`, `download:<host>`, `http`, `webfetch`), `captured_at`, `run_id`. A downloaded file with no source links to its download URL as recorded (scheme, host, path; `?…` for any query).
- **Where it is kept.** One field on the root chunk, `provenance`, plus a `source/<kind>` tag. `get_document` and `export_md` report it; a clean export adds a `> Source: … · File: …` line under the title.
- **Only the runtime writes it.** Every chunk write — create, update, set_asset, upsert, sync, and loom metadata in imported Markdown, on every transport — drops a caller's `provenance` and keeps the stored one, and it is shown only while the root's text carries the `import` origin. Sync between peers and re-importing an export do not carry it over (name the source again on the re-import).
- **Imported text is document text, never a fact.** Each chunk body of an imported document is a memory row with origin `import`. A row is a fact when its origin is set and is **not** `import`, so imports are found by `Memory op=search` with `sources=documents` and never returned by the fact surfaces.

Since #2161 (`/loomcycle/rfcs/files-by-reference` D18).

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
