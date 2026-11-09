---
name: knowledge-base-questions
description: "Answering questions from a knowledge base with the Document tool — typical questions (what did we prepare for a client, how did we estimate X, where do we discuss a topic, what else belongs to this deal, are there copies, what did it say on a date) and the calls that answer each, with where every id comes from."
aliases: [kb-questions, questions, asking, ask-kb, kb-answers]
---
A knowledge base is answered with the **`Document` tool**, not with file tools.
Its documents are not files: a name such as `/library/proposals/...` is a Path
tree name for a document, and `Grep`, `Glob`, `Read` and `Bash` cannot open it.
Search it with `Document op=search`, follow its relations with `backlinks` and
`get_edges`, and read a document with `get_chunk` or `export_md`.

Four rules carry most of the work:

- **Pass `scope` on every call.** Imported documents usually live in the
  tenant scope, and a call without `scope` reads your own user scope, where the
  id does not exist: `get_chunk`, `get_document` and `export_md` then answer
  "no such chunk/document" for an id that is perfectly good. Repeat the scope
  the id came from.
- **Copy ids from results; never write one yourself.** Every id below comes
  from an earlier result: `document_id` and `chunk_id` from `search`,
  `root_chunk_id` from `get_document`. An id you compose does not exist.
- **Choose the call by the shape of the question,** using the table below. A
  question about links (prepared for, issued by, copy of) is answered by a
  relation, not by searching for the name again.
- **Read before you answer, and cite** each document you use as
  `[title](loom:doc/<document_id>?scope=tenant)`. If nothing answers the
  question, say so.

When you do not know the document classes, relation kinds or topics this
knowledge base uses, ask once: `{"op": "describe_ontology"}`. The names you
pass to `types`, `kind` and `concept` come from there.

| The question | The call |
|---|---|
| What did we prepare for a client? What did they issue? | `get_document` by the subject's `path`, then `backlinks` on its `root_chunk_id` with the relation `kind` |
| How did we do X (only estimates, only proposals)? | `search` with `types` (and `under_path` for one folder) |
| Where do we talk about a topic? | `search` with `concept`, or `graph_recall` from the topic's chunk |
| What else belongs to the deal this document is part of? | `graph_recall` from the document's root, `hops: 2` |
| Are there copies or versions of this document? | `get_edges` with `kind: "duplicate_of"` |
| What did it say on a past date? | `search` (or `graph_recall`) with `as_of`; `history` for one chunk's revisions |
| My notes and the shared documents together | `Memory op=recall` with `documents_scope` |
| A phrase you need to find word for word | `search` with the phrase as `query` |

## Examples — what did we prepare for a client?

A client or a partner is a subject document, here under `/facts`. Read it to
get its root chunk:

```json tool=Document
{"op": "get_document", "scope": "tenant", "path": "/facts/northwind-health"}
```

The result carries `root_chunk_id`. List every document linked to it by the
relation, each with the reason the link was made:

```json tool=Document
{"op": "backlinks", "scope": "tenant", "id": "4f1c9a7e2b3d4c5e8f6a7b8c9d0e1f2a", "kind": "prepared_for"}
```

The same two steps answer "what did the company itself issue?" with the kind
`issued_by` on the company's own subject document. Do not search for the
client's name instead: it also appears in documents that are not about them.

## Examples — how did we estimate a kind of project?

Search only the documents of one class, and only one folder when the question
names one:

```json tool=Document
{"op": "search", "scope": "tenant", "query": "estimate for a machine learning pilot", "types": ["Estimate"], "under_path": "/library/presale", "limit": 5}
```

Each hit carries `chunk_id`, `document_id` and its title. Read the parts you
use, in the same scope:

```json tool=Document
{"op": "get_chunk", "scope": "tenant", "id": "8e2d6b1a9c4f4e7d8a3b2c1d0e9f8a7b"}
```

or a whole document at once:

```json tool=Document
{"op": "export_md", "scope": "tenant", "document_id": "c3a9e5f1b7d24c6e9a8b7c6d5e4f3a2b"}
```

If a search comes back empty, change the filter (another class, no folder),
not just the wording.

## Examples — where do we talk about a topic?

A topic from the knowledge base's concept scheme, named by its tag:

```json tool=Document
{"op": "search", "scope": "tenant", "query": "continuous integration and deployment pipeline", "concept": "topic/engineering/devops", "limit": 10}
```

To get the exact passages that use a term rather than whole documents, walk
into the topic's chunk (its id comes from `describe_ontology` or from a
search hit):

```json tool=Document
{"op": "graph_recall", "scope": "tenant", "seed_ids": ["2b7e4d9a1c3f4a5b8c6d7e8f9a0b1c2d"], "kinds": ["uses_term"], "direction": "in", "hops": 1, "body_chars": 400}
```

## Examples — what else belongs to this deal?

From a document you already found (its root chunk id from `get_document`),
two hops reach the client and then the client's other documents:

```json tool=Document
{"op": "graph_recall", "scope": "tenant", "seed_ids": ["6a1f3c8e5b2d4f7a9c0e1b2a3d4c5e6f"], "kinds": ["prepared_for"], "direction": "both", "hops": 2, "body_chars": 0}
```

Each row carries `via_kind`, so you can say how it is connected.

## Examples — are there copies of this document?

```json tool=Document
{"op": "get_edges", "scope": "tenant", "document_id": "d4b8f2a6c1e94a3b8d7c6b5a4f3e2d1c", "kind": "duplicate_of"}
```

Each pair points from the copy to the original, with its basis (`sha256` for
identical text, `title` for the same title) and whether it is still proposed.

## Examples — what did it say on a past date?

`as_of` is a moment in unix nanoseconds (1 March 2025 00:00 UTC is
`1740787200000000000`). It returns the text in force then, from earlier
editions of the same document:

```json tool=Document
{"op": "search", "scope": "tenant", "query": "phases and pricing", "under_path": "/library/presale/contoso", "as_of": 1740787200000000000}
```

For the revisions of one chunk, with when each changed:

```json tool=Document
{"op": "history", "scope": "tenant", "id": "8e2d6b1a9c4f4e7d8a3b2c1d0e9f8a7b"}
```

## Examples — my notes and the shared documents together

```json tool=Memory
{"op": "recall", "scope": "user", "documents_scope": "tenant", "query": "decision on the pilot budget", "top_k": 10}
```

Your notes and the shared documents are ranked together; each result carries
its `scope`. One empty recall is enough: if it finds nothing, go to
`Document op=search` rather than rewording it.
