# Skybits Document Federation Bridge — Specification (RFC draft)

Status: **draft for review** (RFC-first per loomcycle conventions — approval before implementation)
Author: slysiuchenko-waverley
Date: 2026-08-19
Related: `docs/SKYBITS.md` (MCP integration), `docs/EXTERNAL_API.md` § RFC CD/CE, `examples/exp11-skybits-documents/`

## 1. Summary

A standalone bridge service that lets a loomcycle instance treat **Skybits as a federated
document source** (RFC CE), with **zero changes to loomcycle core**. The bridge implements
the peer side of loomcycle's documented document-federation protocol
(`POST /v1/_document`) and translates it to the Skybits API. Once a source is declared,
loomcycle's existing machinery does the rest: agents bind documents with
`Document op=set_remote`, dry-run with `op=diff_remote`, and reconcile with `op=sync`
(`direction:pull|push`) — Skybits content becomes first-class in loomcycle's document
plane (chunks, viewer, embeddings, `sources:[documents]` search), and loomcycle-authored
documents can be pushed up to Skybits.

This complements, and does not replace, the MCP integration (`mcp__skybits__*` tools):
MCP is for live agent work on Skybits documents; federation is for making Skybits content
part of loomcycle's own document/memory plane.

## 2. Why a bridge, not a loomcore feature

Per loomcycle's own convention ("loomcycle stays domain-agnostic; the consumer owns its
own surface"), third-party integrations ship as consumer-side services speaking a
documented loomcycle protocol — exactly how jobs-search-agent exposes `/api/mcp`.
The federation peer protocol is a plain documented HTTP contract; a Skybits-specific
driver inside loomcycle core would couple the core to one SaaS. The bridge keeps the
coupling on the consumer side, where it belongs.

## 3. Architecture

```
loomcycle instance                    bridge (this project)              Skybits
─────────────────                     ─────────────────────             ─────────
Document op=sync  ──POST /v1/_document──▶  op router              ──▶  GET/POST
  direction:pull      (bearer auth,        → Skybits client            skybits.ai
  diff_remote,        SSRF-guarded by      → chunk projection          /v1/tools/*
  set_remote)         caller)              → state store (SQLite)
                                           (chunk metadata: tags,
                                            revision, title/status)
```

- **Bridge**: single small service (Python 3.12 + FastAPI + SQLite), one endpoint
  (`POST /v1/_document`) plus `/healthz`. Stateless except the chunk-metadata store.
- **loomcycle side**: config only —
  ```yaml
  document_sources:
    skybits:
      config:
        base_url: "https://bridge.example.com"
        api_key_env: LOOMCYCLE_SKYBITS_BRIDGE_TOKEN   # env NAME, resolved at use time
  ```
- **Skybits side**: connector API key (server-side env `SKYBITS_API_KEY`), same one the
  MCP integration already uses. The bridge is the only party holding Skybits credentials.

## 4. Protocol surface the bridge must implement

All calls are `POST /v1/_document`, one op per request, HTTP 200 + op-result JSON on
success. loomcycle sends `Authorization: Bearer <token>` when `api_key_env` resolves.
No version markers, no handshake; first contact is the first `get_document`.
Body cap 16 MiB; client timeout 30 s per op.

Ops in wire order (exact payloads from `internal/tools/builtin/document_sync.go` and
`internal/docremote/client.go`, loomcycle v1.59):

### Read path (needed for `diff_remote` AND both sync directions)

1. **`get_document`** — once per sync/diff, resolves the binding:
   ```json
   {"op":"get_document","path":"<remote_ref>","scope":"<scope>"}
   ```
   Must return `{"document_id": "<id>", "root_chunk_id": "<id>", ...}`.
   `document_id` is **required** (empty → "remote document not found"). All later ops
   address the document by this id. `remote_ref` is opaque to loomcycle — for this
   bridge it is a Skybits `doc_url` or bare document UUID; the bridge returns
   `document_id = "sb_" + <uuid>` and `root_chunk_id = "sbroot_" + <uuid>`.

2. **`list_facts`** — the reconciliation keyspace:
   ```json
   {"op":"list_facts","document_id":"<id>","scope":"<scope>","include_refuted":true,"limit":10000}
   ```
   Must return `{"facts":[{"id","title","type","status","entity":{"natural_key","withheld"}}...]}`.
   Facts without `entity.natural_key` are skipped by the reconciler.
   **The bridge MUST honor `limit:10000` literally** — a real loomcycle peer silently
   caps at 200 and the federation client never checks `truncated`; a bridge that caps
   will silently half-sync large documents.

3. **`get_chunk`** — one per keyed fact:
   ```json
   {"op":"get_chunk","id":"<chunk id>","scope":"<scope>"}
   ```
   Must return `{"id","body","parent_id","position","revision","tags":[...]}`.
   Missing fields degrade to zero values silently (no error) — the bridge must emit all.

4. **`get_edges`** — manual cross-reference links:
   ```json
   {"op":"get_edges","document_id":"<id>","scope":"<scope>"}
   → {"edges":[{"from_id","to_id","kind","auto"}]}
   ```
   `auto:true` edges are never synced (regenerated from `[[name]]` body links per side).
   Bridge-state edges via `link_chunks`/`get_edges` are implemented (idempotent,
   `auto:false`, stored in the bridge's own state store).

5. **`query_chunks`** — unkeyed-chunk accounting:
   ```json
   {"op":"query_chunks","document_id":"<id>","scope":"<scope>","limit":10000}
   → {"chunks":[{"id"}...]}
   ```
   Only `id` is read (counted as `excluded_unkeyed` when neither root nor keyed).
   Same literal-`limit` requirement as `list_facts`.

### Write path (sync `direction:push` only — phase 2)

6. **`upsert_chunk`** — create/refresh a keyed chunk:
   ```json
   {"op":"upsert_chunk","document_id","scope","natural_key","title","type","status","body","tags":[...]}
   → {"id": "<chunk id>"}   // tags is never null; natural_key is the idempotency handle
   ```
7. **`update_chunk`** — diverged chunk, presence-based (only supplied fields are
   written; `tags` is a replace-set):
   ```json
   {"op":"update_chunk","id","revision","scope","title","type","status","tags","body"?}
   ```
   Contract: refuse with revision conflict if the chunk's current revision ≠ the passed
   one (HTTP 422, see §8); when `body` is present, snapshot the overwritten body into
   history (Skybits whole-doc revisions satisfy this — §5.4); omitting `body` must not
   record history.
8. **`move_chunk`** — hierarchy/position: `{"op":"move_chunk","id","new_parent_id","position","scope"}`;
   `new_parent_id:""` = document root. Must be idempotent.
9. **`link_chunks`** — manual edge, additive, idempotent:
   `{"op":"link_chunks","from_id","to_id","kind","scope"}`. Phase 2; Skybits has only
   doc-level links, so the bridge stores chunk edges in its own state store.

Reads always run `get_document → list_facts → get_chunk×N → get_edges → query_chunks`.
`scope` is forwarded verbatim in every payload; the MVP treats it as an opaque namespace
hint and validates presence only.

## 5. Data-model mapping (Skybits ↔ loomcycle)

### 5.1 Chunks and natural keys

Reconciliation unit = keyed chunk. Mapping rule:

- **Skybits section `nodeId` → `natural_key` = nodeId.** Skybits nodeIds are stable
  section anchors, which is exactly the reconciliation handle `op=sync` needs
  (`entity.natural_key` is the scope-wide idempotency key on both sides).
- Skybits documents created via the API are frequently **flat** (a single `<p>` node —
  observed live: `create_doc` does not build heading nodes). For flat docs the bridge
  projects **virtual chunks** by splitting the document text on Markdown headings
  (`^#{1,6} `); each virtual chunk gets a deterministic natural key
  (`h_<sha1(heading text)[:12]>`). A doc with no headings projects as a single chunk
  keyed `doc_<uuid>`. Re-projection is deterministic, so repeated syncs are stable.
- Chunk `id` on the wire: `"sbc_" + sha1(document_id + natural_key)[:16]` (stable,
  collision-free per document).

### 5.2 Chunk metadata Skybits does not have

Skybits chunks have no tags/title/type/status and no per-chunk revision. The bridge
keeps a small SQLite store keyed `(document_id, natural_key)`:
`{title, type, status, tags[], revision:int}`.
- Defaults on first projection: `title` = heading text (or doc title), `type:"section"`,
  `status:"active"`, `tags:[]`, `revision:1`.
- `update_chunk`/`upsert_chunk` write metadata here; `body` goes to Skybits.

### 5.3 Bodies

Chunk `body` (Markdown) ↔ the section's HTML content converted to/from Markdown by the
bridge. Skybits `edit_document` batch ops (`replace`/`insert_after`/`remove`) are the
write primitives; one bridge write = one Skybits edit op per chunk.

### 5.4 History / retire-not-delete

The federation contract requires the losing side of a diverged sync to keep the
overwritten body in chunk history. Skybits revisions are whole-document, so every
bridge body write lands as a normal Skybits revision (visible in `document_history`,
revertible via `restore_document`) — this satisfies the guarantee at document
granularity, which the spec accepts; per-chunk history is not reproduced.

### 5.5 Hierarchy

Skybits docs are flat (sections are siblings). Federation hierarchy therefore
degenerates: every chunk's parent is the document root, so top-level chunks
report `get_chunk.parent_id` = `""` (empty = root; emitting the root chunk id
causes perpetual reparenting during loomcycle reconciliation), and
`move_chunk` only ever reorders among siblings (Skybits
`edit_document` `move` op) or is a no-op when already in place.

### 5.6 Out of scope for the data model

Canvas and Spreadsheet document kinds (doc kind only), comments, images/assets,
doc-level tags (they exist in Skybits but have no loomcycle chunk analogue).

## 6. Auth & tenancy

- **loomcycle → bridge**: static bearer, `api_key_env: LOOMCYCLE_SKYBITS_BRIDGE_TOKEN`
  (env-var *name*; must pass loomcycle's credential allowlist — `LOOMCYCLE_` prefix
  qualifies). Bridge refuses 401/403 on mismatch. SSRF: the bridge's own hostname is
  auto-allowlisted by the caller even when private — running the bridge next to
  loomcycle on localhost/LAN works.
- **bridge → Skybits**: connector API key in `SKYBITS_API_KEY` (server-side only,
  minted via the Skybits device flow). Token never crosses the loomcycle boundary.
- **Multi-tenant**: MVP is single-tenant (one Skybits workspace per bridge). The
  protocol's `key_per_tenant` tenancy strategy is noted but deferred.

## 7. Scope of MVP vs later phases

- **MVP (phase 1)**: ops 1–5 (read path) → `diff_remote` and `sync direction:pull`
  work end-to-end; edges return empty; flat-doc projection; single tenant.
- **Phase 2**: ops 6–9 (write path) → `sync direction:push`; bridge-state edges;
  revision-conflict semantics.
- **Phase 3**: scheduled reconciliation (loomcycle `schedule` running
  `Document op=sync` on a cadence — Skybits has no webhooks, so pull-based scheduling
  is the natural fit on both sides), multi-tenant, canvas/spreadsheets.

## 8. Error contract (mirror the real peer)

- Tool-level refusal (unknown op, missing field, not found, revision conflict):
  **HTTP 422** `{"code":"tool_refused","error":"<message>","tool":"Document"}`.
- Bad JSON / empty body: **400** `{"code":"bad_request",...}`.
- Handler fault: **500** `{"code":"internal",...}`. Auth: 401/403 from middleware.
- The client parses any non-200 as `{"code","error"}`; keeping `code` non-empty
  produces the readable `op %q refused (%d): %s` form in loomcycle logs.

## 9. Verification (acceptance)

Against a real loomcycle v1.59 instance + the live Skybits workspace:

1. `Document op=diff_remote` on a bound Skybits doc classifies chunks correctly
   (`only_remote` on first bind; `same` after a pull).
2. `sync direction:pull` imports a Skybits doc into loomcycle: chunk count matches
   projection, bodies match Markdown source, doc is searchable via
   `sources:[documents]` and visible in the document viewer.
3. Idempotence: a second pull reports zero changes (`same` across the board).
4. Drift: external edit in Skybits → `diff_remote` shows `diverged` → pull converges.
5. (Phase 2) `sync direction:push` creates/updates Skybits sections; revision
   conflict on stale `update_chunk` is refused with 422; Skybits `document_history`
   shows the pushed revisions attributed to the connector.

## 10. Open questions for the maintainer

1. Is the federation peer contract (`POST /v1/_document` + the 9 ops above) considered
   a **stable public surface** for third parties, or still internal (RFC CE is young)?
2. `natural_key` semantics: any constraints (charset/length) beyond "scope-wide unique"?
3. The `limit:10000` vs silent server-side truncation gap (§4.2) — worth a client-side
   `truncated` check upstream regardless of this bridge?
4. Should this spec be imported into the loomcycle document store as an RFC for
   approval, per the doc-store-primary convention?
