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

One tool, `Document`, gated by per-agent `allowed_tools: [Document]`. 13 ops:

| group | ops |
|-------|-----|
| Document lifecycle | `create_document` (optional `path:` → a Path dirent), `get_document` (by `id` or `path`), `delete_document` |
| Chunk lifecycle | `create_chunk`, `get_chunk`, `update_chunk`, `delete_chunk`, `move_chunk` |
| Edges | `link_chunks`, `unlink_chunks` |
| Query | `query_chunks` |
| Types | `define_type`, `list_types` |

`scope` is `agent` (this agent, the default) or `user` (this end-user — needs a
`user_id` on the run). **Tenant scope is deferred** — SQL Memory has
`agent`/`user`/`run` scopes, no `tenant` scope (the blocker), so the Document
tool refuses `scope:tenant` with a clear message. Documents are still
tenant-*isolated*: the SQL Memory scope key carries the authoritative tenant.

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

## Caveats

- **SQL Memory required** (`LOOMCYCLE_SQLMEM_ENABLED=1`).
- **Tenant scope deferred** — `agent`/`user` only in v1.
- **Orphaned bodies are best-effort on delete** — see the integrity note above
  (invisible dead K/V; the row side is atomic).
- **Markdown round-trip (`export_md`/`import_md`) and the Web UI tree/editor**
  are RFC AK Phase 2/3 — not in this core.

## Where it lives

`internal/tools/builtin/document.go` (the tool + the 4-table schema). The
`document` help topic (`Context op=help topic=document`) is the in-agent
reference; `docs/SQL_MEMORY.md` covers the backing store and `docs/PATH.md` the
naming layer.

---

**One-sentence thesis:** A Document is a queryable graph of typed, individually
versioned chunks — prose in Memory, structure in SQL Memory, named in the Path
tree — so agents and humans can co-author and query a document the way they
query a database, not diff a blob.
