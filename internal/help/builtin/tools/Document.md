---
name: Document
description: "Document tool — chunked-graph documents (a tree of chunks with types, fields, tags, edges and Markdown bodies) plus a bi-temporal fact tier on the same chunks. The index of all 60 operations."
---
The `Document` tool stores **chunked-graph documents**. A document is not one
Markdown blob: it is a tree of **chunks**. Each chunk has its own id, a parent
and a position among its siblings, a title, an optional `type` and `status`,
structured `fields`, `tags`, a Markdown `body`, and graph **edges** to other
chunks (in this document or another one in the same scope). You restructure a
document by moving and linking chunks, and you find things by querying and
searching rather than by reading it top to bottom.

The same chunks also carry the **fact tier**: a chunk written with
`upsert_chunk` (or `remember`) gets fact metadata — a stable `natural_key`,
timestamps, a source quote, a confidence — and the fact operations read,
correct, verify and walk those facts.

Used as a **knowledge base** — source documents, facts about subjects,
relations between parts with the quotes that show them, and a tenant
ontology — read the guide first: `{"op":"help","topic":"knowledge-base"}`. It
explains how the pieces fit, who rules on what, and walks through whole call
sequences.

## When to use it — and when not

- **Use Document** for structured, multi-part content that people and agents
  edit together (specs, plans, task boards, notes), and for facts you want to
  correct without losing history.
- **Use Memory** for a single value under a key, for semantic recall across
  everything a scope remembers, or for plain SQL tables. To search document
  text, use this tool's `search`.
- **Use Path** to browse or move document paths (`ls /documents`). Path only
  names a document in the tree; reading and writing it is always this tool.
  To rename a document's title, `update_chunk` its root chunk's `title`.
- **Use Read/Write** for files on a volume. A document is not a file.

## Ids — get these right

Three different ids come back from `create_document`, and mixing them up is the
most common mistake:

| id | What it names | Where you pass it |
|---|---|---|
| `document_id` | the document as a whole | `document_id` for `create_chunk`, `query_chunks`, `export_md`, `get_edges`; `id` for `get_document`, `delete_document`, `set_path` |
| `root_chunk_id` | the document's top chunk (it holds the title) | `get_chunk`/`update_chunk` on the title itself. A chunk created or moved with no parent goes under it |
| chunk `id` | one chunk | `id` for `get_chunk`, `update_chunk`, `delete_chunk`, `move_chunk`; `parent_id`, `after_id`, `from_id`, `to_id`, `seed_ids` |

`id` means a DOCUMENT for `get_document`, `delete_document` and `set_path`, and
a CHUNK for every chunk operation. Ids are opaque hex strings: never invent
one. If you do not have an id, find it first with `search`, `query_chunks`,
`query_documents` or `get_document` with a `path`.

## Operations

Every call takes `op`. Fetch one operation's article, with arguments, errors
and examples, as `Document/<op>` — for example
`{"op":"help","topic":"Document/create_chunk"}`.

**Documents**

| op | What it does | Required besides `op` |
|---|---|---|
| `create_document` | new document with an empty root chunk, named in the Path tree | `title` |
| `get_document` | a document's metadata: title, root chunk, type, status, tags | `id` or `path` |
| `documents_summary` | title/type/status/colour for many documents in one call | `document_ids` or `under_path` |
| `query_documents` | list documents filtered by path, type, status or tag | — |
| `delete_document` | delete a document, every chunk in it, and its path | `id` or `path` |
| `set_path` | give an existing document a (further) Path-tree name | `id`, `path` |

**Chunks**

| op | What it does | Required besides `op` |
|---|---|---|
| `create_chunk` | add a chunk to a document | `document_id`, `title` |
| `get_chunk` | one chunk with its body, fields, tags and current `revision` | `id` |
| `update_chunk` | change a chunk's title/type/status/body/fields/tags/dates | `id`, `revision` |
| `delete_chunk` | delete a chunk and everything under it | `id` |
| `move_chunk` | give a chunk a new parent and/or position | `id` |
| `reorder_chunk` | move a chunk one place up or down among its siblings | `id`, `direction` |

**Relations (statements)**

Every edge is a statement: an id, a status (`proposed`, `asserted`,
`rejected`), how it was made, and the quotes that support it.

| op | What it does | Required besides `op` |
|---|---|---|
| `link_chunks` | assert a typed relation between two chunks, with optional evidence | `from_id`, `to_id`, `kind` |
| `propose_statement` | file a relation for review; nothing live changes until an operator accepts it | `from_id`, `to_id`, `kind` (or `id` to revise one) |
| `unlink_chunks` | remove a relation and its evidence | `id`, or `from_id`, `to_id`, `kind` |
| `get_edges` | the relations touching a document, with each end's title/type/status | `document_id` |
| `get_edge` | one relation with all its evidence | `id` |
| `backlinks` | the relations arriving at one chunk (manual and `[[name]]` links) | `id` |
| `unlinked_mentions` | chunks that mention a chunk's title but do not link to it | `id` |
| `list_statements` | the review queue: relations of one status across the scope | — |
| `accept_statement` / `reject_statement` | an operator's ruling on a proposal; refused inside a run | `id` (+ `reason` to reject) |
| `maintain_statement` | upkeep by a code agent granted statement methods: verify, expire, demote, merge, re-point evidence, flag | `id`, `action` |

**Tags, types**

| op | What it does | Required besides `op` |
|---|---|---|
| `add_tags` | add tags to a chunk (`id`) or a document (`document_id`) | `tags` |
| `remove_tags` | remove tags from a chunk or a document | `tags` |
| `list_tags` | distinct tags with counts for a chunk, a document, or the scope | — |
| `define_type` | declare a chunk type and its fields | `name` |
| `list_types` | the declared chunk types | — |

**Citations, works and the ontology**

| op | What it does | Required besides `op` |
|---|---|---|
| `describe_ontology` | the tenant ontology in force: classes, relation kinds, concept schemes, instances | — |
| `extract_citations` | find and resolve the citations in a document's or a chunk's text; read-only | `document_id` or `id` |
| `resolve_anchor` | find a work by its key and a part of it by anchor, as of a date; optionally file a placeholder | `work_key` |
| `classify` | say what a document is from the ontology; applies a single rule match, proposes the rest | `document_id`, `id` or `path` |
| `accept_classification` | an operator accepts a document's pending classification; refused inside a run | `document_id`, `id` or `path` |
| `propose_entity` | suggest a class, relation kind, concept or instance for the tenant ontology | `name` |
| `propose_subject` | suggest a new shared subject for the tenant | `subject`, `natural_key` |

**Finding things**

| op | What it does | Required besides `op` |
|---|---|---|
| `search` | semantic search over chunk bodies — start here when you do not know which document; aim it with path, document, type, status, tag or `as_of` filters | `query` |
| `related` | chunks whose bodies are semantically closest to a chunk's; takes the same filters as `search` | `id` |
| `query_chunks` | structured filters (document, type, status, tag, path) or a read-only `sql` SELECT | — |

**History**

| op | What it does | Required besides `op` |
|---|---|---|
| `history` | the revisions at which a chunk's body changed | `id` |
| `get_version` | a chunk's exact body at one revision | `id`, `revision` |
| `diff` | a unified diff between two body revisions | `id`, `from_revision`, `to_revision` |

**Import, export, images**

| op | What it does | Required besides `op` |
|---|---|---|
| `export_md` | render a document as Markdown | `document_id`, `id` or `path` |
| `import_md` | build a document (or a subtree) from Markdown; `reimport: true` reconciles a new edition | `markdown` |
| `export_canvas` | render a document as a JSON Canvas spatial graph | `document_id`, `id` or `path` |
| `import_canvas` | build a new document from a JSON Canvas | `canvas` |
| `set_asset` | attach image bytes to a chunk, making it an image chunk | `id`, `media_type`, `data` |
| `get_asset` | an image chunk's asset metadata | `id` |

**Remote documents**

| op | What it does | Required besides `op` |
|---|---|---|
| `set_remote` | bind a document to a document on a peer loomcycle | `source`, `remote_ref`, and `id` or `path` |
| `sync` | reconcile the bound documents' keyed chunks, `pull` (default) or `push` | `id` or `path` |
| `diff_remote` | dry run: what a `sync` would change | `id` or `path` |

A source declared in the operator's yaml reaches its peer wherever it runs. A
source authored at runtime (`DocumentSourceDef`) reaches only a host the
operator lists in `LOOMCYCLE_HTTP_HOST_ALLOWLIST` or
`LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST`; otherwise `sync` and `diff_remote`
fail with `... is not in LOOMCYCLE_HTTP_HOST_ALLOWLIST ...`, and nothing is
sent. A peer on a private network needs the private list:
without it they fail with `blocked: <host> has no public addresses`.
A source whose `api_key_env` is `$cred:<name>` sends the tenant-level
credential of the tenant that owns the source; if that tenant has no such
credential, `sync` and `diff_remote` fail with `... has no tenant-level
credential of that name`, and nothing is sent.

**Facts**

| op | What it does | Required besides `op` |
|---|---|---|
| `upsert_chunk` | write a fact by `natural_key`: creates it once, updates it after | `natural_key` (+ `document_id`, `title` to create) |
| `supersede_chunk` | retire an old fact in favour of a new one, keeping the old one | `id`, `supersedes_id` |
| `list_facts` | browse facts, newest first, metadata only | — |
| `graph_recall` | recall facts by following edges, optionally as of a past moment; chunks that are not facts carry the start of their text | `seed_ids` or `query` |
| `remember` | store a sentence a person told you as a self-citing fact | `text` |
| `verbatim_answer` | answer a lookup question with one verified fact, quoted exactly | `query` |
| `judge_fact` | record whether a fact's (or a relation's) evidence supports it | `id` or `natural_key`, `verdict`, `reason` |
| `verification_stats` | how much of the scope's fact store is quoted and verified | — |

**Passes over a scope**

| op | What it does | Required besides `op` |
|---|---|---|
| `lease` | take a named, expiring lease so only one pass of a kind works on the scope | `name` |
| `release` | end a lease this run holds | `name` |

## Scopes

`scope` picks which store a call reads or writes, and it **defaults to
`user`**:

- `user` (the default) — this end-user's documents. Needs a user id on the run.
- `agent` — this agent's own documents. Needs a yaml-declared agent.
- `tenant` — shared by every user and agent in the tenant. The operator must
  grant BOTH `memory_scopes: [tenant]` and `sql_scopes: [tenant]` on the agent,
  because a document's structure and its chunk bodies live in two stores.

One call reads one scope. A chunk id from one scope is "no such chunk" in
another, so pass the same `scope` on every call about the same document. For how
scopes work across every tool, read `{"op":"help","topic":"scopes"}`.

## Facts and time

A fact carries three independent clocks, all in **unix nanoseconds**:

- `valid_at` — when it became true in the world. Omit it when you do not know:
  an undated fact still answers an `as_of` question.
- `invalid_at` — when it stopped being true in the world. A future value is
  allowed and the fact stays current until then.
- `observed_at` — when it was said or written. "Yesterday I was in Boston",
  said on the 4th, is observed the 4th and valid the 3rd.

The server also records when the store started believing a fact (`created_at`)
and when it stopped (`expired_at`, set by `supersede_chunk`). You cannot set
those two.

By default the fact reads (`list_facts`, `graph_recall`) return what is true
now. `as_of` asks what was true at a past moment: a fact matches when its
`valid_at` is unset or at or before that moment, and its `invalid_at` is unset
or after it — so a fact corrected later still answers a question about before
the correction. A chunk with its own `valid_at`/`invalid_at` must contain the
moment too; on `graph_recall` it must also contain the present when there is no
`as_of`. `include_retired: true` drops the time filter
entirely. A fact a
judge marked `unsupported` is withheld from these reads until you pass
`include_refuted: true`; it is never deleted.

Correct a fact with `supersede_chunk`, not `delete_chunk`: the old fact stays
readable for questions about the past.

## Dated content

Any chunk — not only a fact — can say when its content is true: pass
`valid_at` / `invalid_at` (unix nanoseconds, `invalid_at` exclusive) to
`create_chunk` or `update_chunk`. Use this for an edition, a tariff or a policy
clause: content that is a passage of a document, read as a whole, and replaced
by a newer version. `search`, `related` and `query_chunks` with `as_of` then
return only what was in force at that moment; a chunk with no dates always
matches. When an edition is replaced, set the old one's `invalid_at` (and its
`status`, e.g. `repealed`, for people) — the status is not derived from the
dates, or the other way round. Dating a chunk does not make it a fact: it does
not appear in `list_facts`.

Use the fact tier instead (`upsert_chunk`, `remember`) for a claim about
something with a stable identity — a person's employer, a service's owner —
that is corrected by a later claim and checked against the text it came from.
A fact can carry both periods; `as_of` then needs both to hold. Dates are
content time, never the time you write them: omit what you do not know, since
a guessed date is worse than none.

## Paths

`create_document` always names the document in the Path tree:
`/documents/<title>` when you pass no `path`. Renaming the document later (its
root chunk's `title`) does not move that path; `Path op=mv` does, and
`set_path` adds another name. `get_document`, `delete_document`,
`export_md`, `sync` and friends accept `path` instead of `id`, and
`query_documents`, `query_chunks` and `documents_summary` take `under_path` to
limit themselves to a subtree. `search` and `related` take `under_path(s)` and
`exclude_under_path(s)` too, applied before ranking, so a search aimed at one
subtree finds that subtree's best matches even when the rest of the scope
matches better. Paths follow the Path tool's rules
(`{"op":"help","topic":"Path"}`).

## Change events

Every chunk create, update or delete publishes `{op, chunk_id, timestamp,
actor}` on the channel `documents/<document_id>/chunks`, so a live viewer can
follow edits without polling; a relation written or ruled on publishes `{op,
edge_id, kind, status, actor}` there for both ends' documents. Imports are
announced on the scope's `documents/ingested` channel, and subject changes
(`adopted`, `merged`, `retyped`, `removed`, `erased`) on `memory/subjects` —
see `{"op":"help","topic":"knowledge-base"}`. The feed is kept while the document is edited:
`delete_document` purges it, and a feed with no event for a day is purged.
`list_channels` does not list these feeds.

## Requirements

The server needs SQL Memory enabled: a document's structure lives there and its
chunk bodies live in Memory. Without it every call fails with "not configured".
`search`, `related` and `verbatim_answer` also need an embedder.
