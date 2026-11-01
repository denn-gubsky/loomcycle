---
name: knowledge-base
description: "Working with a knowledge base in the Document tool — how documents, chunks, facts, subjects and statements (relations) fit together, the ontology, turning citations into relations, who rules on what, retrieving along relations, keeping it true across editions, and subject changes. With end-to-end call sequences."
aliases: [kb, knowledge, ontology, statements, relations]
---
A **knowledge base** here is not a separate tool: it is the `Document` tool used
for more than text. Documents hold the source material (laws, policies,
minutes, contracts); **facts** record claims about **subjects** (people,
organisations, works); **statements** record how two pieces are related (this
clause *implements* that article, this act *amends* that law) together with the
quoted text that shows it; and the tenant's **ontology** says which kinds of
things and relations exist. This guide explains how the pieces fit, what you may
do and what only an operator may, and ends with call sequences you can follow.
Each operation has its own article (`{"op":"help","topic":"Document/<op>"}`)
with every argument and refusal.

## The pieces

| piece | what it is | made with | read with |
|---|---|---|---|
| document | a tree of chunks with a title and a Path-tree name | `create_document`, `import_md` | `get_document`, `export_md` |
| chunk | one part of a document (an article, a clause, a section) with a body | `create_chunk`, `import_md` | `get_chunk`, `search`, `query_chunks` |
| fact | a chunk carrying fact metadata: a `natural_key`, dates, a `source_quote`, a confidence | `upsert_chunk`, `remember` | `list_facts`, `graph_recall`, `verbatim_answer` |
| subject | a document whose root chunk carries a `natural_key` (`person:ada-lovelace`); facts are `about` it | `create_document` with `type` + `subject` + `natural_key`, or adoption | `get_document`, `list_facts` `about` |
| statement | a directed relation between two chunks: `from_id` → `to_id`, a `kind`, a status, evidence | `link_chunks`, `propose_statement`, `upsert_chunk` `relation` | `get_edges`, `get_edge`, `backlinks`, `list_statements` |
| evidence | the verbatim quote a statement rests on, and where it is (a chunk, a chat turn, another scope's fact) | the `evidence` argument | `get_edge` |
| ontology | the tenant's vocabulary: classes, relation kinds, concept schemes, instances | operators; you suggest with `propose_entity` | `describe_ontology` |

Everything lives in ONE scope per call (`user` by default, `agent`, or
`tenant`). A shared knowledge base is almost always the `tenant` scope: pass
`"scope": "tenant"` on every call about it, because a chunk id from one scope is
"no such chunk" in another. See `{"op":"help","topic":"scopes"}`.

Ids are opaque 32-character hex strings. Never invent one: find it with
`search`, `query_chunks`, `get_document` (by `path`) or `resolve_anchor`.

## Read the ontology first

Before writing into a knowledge base you do not know, call
`describe_ontology` (it always reads the tenant's ontology). Use what it names
instead of making things up:

- **`classes`** — the `type` for a document's root and its parts; the entity
  types `upsert_chunk` accepts with `subject`; the `fields` a class expects (a
  class with identity fields builds a part's `natural_key` for you). A class may
  name a citation grammar, which is what `extract_citations` applies.
- **`relations`** — the relation kinds: the `kind` you pass when relating two
  chunks, the classes each end must be (`domain`, `range`), the qualifiers it
  takes in `attributes`, whether it is `dated`. Kinds marked `walk: true` are
  the ones retrieval follows by default.
- **`schemes`** — the concepts (topics, classifications), each with a `tag`
  such as `topics/risk-management`. Filter `search` and `related` by them with
  `concept` / `concept_prefix`.
- **`instances`** — the real organisations, units and posts the tenant knows.
  An adopted instance has a `subject_chunk_id`: relate things to that chunk;
  never create a second subject for it.

Only a **confirmed** ontology's own entries apply; while it is a draft, only
the standard classes are in force. Where a confirmed ontology has a Relations
section, a relation you assert must fit it, and one you propose that does not
fit is filed marked for review. Missing a class, a relation kind, a concept or
an instance? Suggest it with `propose_entity` (`section: relations`, `schemes`
or `instances`). An agent never edits the ontology document itself.

## Facts and subjects

A fact is one claim, written with `upsert_chunk` under a `natural_key` so a
repeated pass updates the same chunk instead of adding another
(`subject|predicate|object` is a good shape). Give it:

- `source_quote` — the exact sentence it came from, and `source_chunk_id` when
  that sentence is a passage of a document in this scope: the fact is then
  linked to it (`derived_from`) and can be checked against it;
- `about_ids` — the subject chunks it is about (a subject's chunk is its
  document's `root_chunk_id`); with two of them, `relation` names how they are
  related and files that as a statement whose evidence is the fact;
- `valid_at` / `invalid_at` — when it was true in the world, only if you know.

Correct a fact by writing the correction under a new key and calling
`supersede_chunk` (new `id`, old `supersedes_id`): the old one stays readable
for questions about the past. `judge_fact` records whether a fact's quote
supports it; an `unsupported` fact is withheld from reads, never deleted.
`remember` is for a sentence a person asked you to keep.

To make a subject you learned about in a user or agent scope a shared subject
of the tenant, suggest it with `propose_subject`, using the `natural_key` your
facts already point at; an operator adopts it.

## Relations: assert or propose, and who rules

Every relation is a **statement** with its own id, a `status`, how it was made
(`method`) and its evidence. Two ways to write one:

- **`link_chunks` asserts.** The relation is live at once. Use it for links you
  are the authority on (a task blocks another; a section you wrote cites a
  source). Where the tenant's ontology declares relation kinds, a link that
  does not fit them is refused.
- **`propose_statement` proposes.** The relation is filed `proposed`: shown in
  `get_edges` and the review queue (`list_statements`), not followed by
  `graph_recall`, `backlinks` or `search` `expand` until accepted. Use it for
  any relation you read out of text. Always pass `evidence`: each item is one
  source and the verbatim `quote` (`{"chunk_id", "quote", "char_start",
  "char_end"}` for a chunk); a quote that does not occur in its source is
  refused.

Who rules on what:

| decision | who | how |
|---|---|---|
| accept or reject a proposed relation | an operator only | `accept_statement`, `reject_statement` (refused inside a run) |
| accept a document's proposed classification | an operator only | `accept_classification` (refused inside a run) |
| adopt a subject, accept an ontology proposal | an operator only | the console |
| merge two subjects, bind a subject to a user account | an operator only | the operator plane; never a Document op |
| find a relation or fact unsupported by its evidence | any agent | `judge_fact` — a finding, kept and withheld, not a deletion |
| re-check, retire, demote or merge relations a rule made | a code agent granted statement methods, or an operator | `maintain_statement` |
| auto-assert a rule-made relation | the server | a `rule:` proposal of a kind the ontology marks auto-asserting, every quote in a chunk |

A rejected relation is a tombstone: proposing it again, at any `valid_at`, is
refused with `statement_rejected` — do not retry. To dispute a relation that is
asserted, propose a correction beside it (`propose_statement` with its `id` and
a new `kind`), or judge it `unsupported` with `judge_fact` and a reason.

A relation's lifecycle: `proposed` → `asserted` (accepted, or written by
`link_chunks`) or `rejected`; an asserted one can later be retired (expired by
maintenance, or ended by its `invalid_at`) and is then shown only with
`include_retired`; a judged-unsupported one is shown only with
`include_refuted`.

## From citations to relations

A document whose class names a citation grammar has its citations found for
you. `extract_citations` (one `document_id`, or one chunk `id`) returns each
citation with its quote, its position (`char_start`, `char_end`), the cited
work's `work_key`, the `anchor` of the cited part, the relation `kind` the
grammar gives it, and `resolved` — the chunk it resolved to, as of the citing
part's own edition date (or your `as_of`). It writes nothing. For each hit:

1. `resolved.chunk_id` set: `propose_statement` from the hit's `chunk_id` to
   it, `kind` the hit's `kind` (or `cites`), evidence the hit itself.
2. Only `resolved.document_id`: the work is held but the part was not found;
   relate to the work's root chunk, or look the part up with
   `resolve_anchor` (`work_key` + `anchor`).
3. Nothing resolved: the scope does not hold the work. `resolve_anchor` with
   `create_placeholder: true` files an empty placeholder to point at; importing
   the work later (`import_md` `reimport: true` with its `work_key`) fills it,
   and every relation to it stays.

## Retrieving along relations

A passage rarely repeats what it cites, so answering from the passage alone
answers half the question. The retrieval ops reach across relations:

- **`search` with `expand`** — rank chunks by your question, then add what the
  top five hits are related to (one hop, `kinds` to choose which relations,
  `budget_chars` to cap it). Added rows carry `expanded_via: {kind,
  from_chunk_id, quote}`: cite the quote when you use them.
- **`related` with `related_to`** — the chunks a chunk is related to by
  relation (`linked`, with `via_kind` and `via_quote`), beside or instead of the
  ones similar in meaning.
- **`graph_recall`** — a walk of several hops from `seed_ids` (or a `query`),
  following `kinds` (default: the ontology's walk kinds), `direction`, and the
  document tree with `include_hierarchy`; bound it with `budget_chars`, not by
  cutting hops. A hop is one edge: fact → subject → fact is two.
- **`get_edges`** (a whole document, `direction` `in` or `out`) and
  **`backlinks`** (one chunk) — list the relations themselves; **`get_edge`**
  — one relation with all its evidence.

Every one of them reads one scope and takes `as_of` (unix nanoseconds) to
answer as of a past moment: the parts in force then, and the relations that
held then. The walks (`expand`, `related_to`, `graph_recall`) follow only
relations a reader is shown — asserted, not rejected or judged unsupported,
in force (`graph_recall` takes `include_proposed` to follow proposals too);
`get_edges` lists proposals as well unless you pass `include_proposed: false`. `search` and `related` narrow by `under_path(s)`,
`type(s)` (with subtypes), `status(es)`, `tag`, `concept` and document ids
before ranking — filter whenever the scope holds more than one kind of
document.

## Keeping it true across editions

When a work changes — an amended law, a revised policy — import the new
edition onto the one you hold: `import_md` with `reimport: true`, the work's
`document_id` or `work_key`, and `edition_date` (when it came into force).
Nothing is deleted. Unchanged parts keep their ids and relations; a part moved
or renumbered keeps them too (and is still found by its old anchor); a part
whose text changed is **superseded** — the old version closes at
`edition_date`, a new one opens, and relations follow it where their evidence
still holds, otherwise they stay on the old version marked `proposed` for
re-checking. A relation a person made, or one an operator ruled on, always
follows. With `amending_document_id`, each moved part gets an `amends`
relation from the amending act.

After a reimport, re-read the parts listed in `changed_chunk_ids` for
citations (`extract_citations` with `id`), and look at what is now proposed
(`list_statements`). A code agent granted statement methods keeps relations
true with `maintain_statement`: `verify` (stamp it re-checked), `expire` (it no
longer holds), `demote` (back to review), `merge` (fold a duplicate),
`stale_evidence` / `relocate_evidence` (an evidence quote moved), `flag` (for a
person). A relation a person wrote takes only `verify` and `flag`, and so,
from inside a run, does one an operator ruled on.

## Subject changes

When a subject changes identity, an event goes to the scope's
`memory/subjects` channel so whatever indexes relations by subject (a links
pass) can re-point or retire them. Each event is `{op, subject_natural_key,
scope, timestamp, document_id?, root_chunk_id?, ...}`:

| `op` | what happened | made by |
|---|---|---|
| `adopted` | a subject (or an ontology instance) became a shared tenant subject | an operator adopting a proposal, or confirming the ontology |
| `merged` | the subject was folded into another (`surviving_key`, `surviving_document_id`); its root keeps its key, with status `merged` and `fields.merged_into` | an operator |
| `retyped` | the subject's root chunk got a new `type` (`old_type`, `new_type`); its key is kept | `update_chunk` or `upsert_chunk` |
| `removed` | the subject's document was deleted | `delete_document` |
| `erased` | the user account bound to the subject was erased, and the binding cleared | an operator |

Likewise every import is announced on `documents/ingested` (`{document_id,
reason, scope, timestamp, changed_chunk_ids?}`, `reason` `created`, `imported`
or `reimported`). An event is not published when your access-control rules hide
part of the scope from reading; the op's result then carries `not_announced`
saying why — the change was still made.

What you may and may not do to a subject:

- **Never set `principal`.** That `fields` key binds a subject to a user
  account and only an operator sets or clears it; a write naming another value
  is refused with `principal_binding_operator_only`. Leave it out of `fields`
  (the stored value is kept).
- Re-type a subject only to an entity type the ontology declares; any other is
  refused.
- Do not write new facts about a `merged` subject; write them about the one its
  `fields.merged_into` names.
- You may not merge, adopt or erase subjects; propose one with
  `propose_subject`.
- Deleting a subject's document removes it for everyone in the scope and
  leaves other scopes' evidence naming a key that no longer resolves. Correct a
  subject's facts instead.

## Examples — answer a question citing the regulation a clause implements

Find the clause, with the provisions it relies on added after the ranked hits:

```json tool=Document
{"op": "search", "scope": "tenant", "query": "who approves an outsourcing contract", "under_path": "/norms", "limit": 5, "expand": {"kinds": ["implements", "cites"]}}
```

The first hit is a clause; an added row carries `expanded_via: {kind:
"implements", from_chunk_id: <the clause>, quote: "pursuant to Article 41 of
the Law"}`. Read both texts:

```json tool=Document
{"op": "get_chunk", "scope": "tenant", "id": "5e1f0c2a9b8d4e7f8a6b5c4d3e2f1a0b"}
```

```json tool=Document
{"op": "get_chunk", "scope": "tenant", "id": "9c8b7a6f5e4d43c2b1a0f9e8d7c6b5a4"}
```

Answer from both, quoting the clause and naming the article it implements by
the relation's quote. If the article in turn depends on definitions, walk one
more step instead of guessing:

```json tool=Document
{"op": "graph_recall", "scope": "tenant", "seed_ids": ["5e1f0c2a9b8d4e7f8a6b5c4d3e2f1a0b"], "kinds": ["implements", "defines"], "direction": "out", "hops": 2, "budget_chars": 3000}
```

For a question about a past date ("what applied in June 2025?"), pass the
same `as_of` (unix nanoseconds) on every call.

## Examples — file a relation with evidence

Read the ontology for the kind and the classes its ends need:

```json tool=Document
{"op": "describe_ontology"}
```

Find the target part by the work's key and anchor:

```json tool=Document
{"op": "resolve_anchor", "scope": "tenant", "work_key": "law:ua/2297-vi", "anchor": "article-12"}
```

Check what already relates the two, so you do not re-file it:

```json tool=Document
{"op": "backlinks", "scope": "tenant", "id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "kind": "implements"}
```

Then propose it, quoting the sentence that states it, exactly:

```json tool=Document
{"op": "propose_statement", "scope": "tenant", "from_id": "e1f3a5b7c9d14e2f8a4b6c8d0e2f4a6b", "to_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "kind": "implements", "confidence": 0.8,
 "evidence": [{"chunk_id": "e1f3a5b7c9d14e2f8a4b6c8d0e2f4a6b", "quote": "This order implements Article 12 of the Law"}]}
```

The answer's `status` is `proposed` until an operator rules. With
`ontology_valid: false`, read `warnings`: the relation does not fit the
vocabulary (a wrong kind, or an end of the wrong class) and a reviewer will see
that.

## Examples — find what cites act X

Get the act's document id by its work key:

```json tool=Document
{"op": "resolve_anchor", "scope": "tenant", "work_key": "law:ua/2297-vi"}
```

Every relation arriving at any of its parts, of the citing kinds:

```json tool=Document
{"op": "get_edges", "scope": "tenant", "document_id": "0c1d2e3f4a5b4c6d8e9f0a1b2c3d4e5f", "direction": "in", "kinds": ["cites", "implements", "amends"]}
```

Each row names the citing chunk (`from_id`, `from_title`, `from_document_id`)
and up to three evidence quotes. Proposed relations are included by default;
pass `include_proposed: false` for only the accepted ones. For one article
rather than the whole act, ask `backlinks` on that article's chunk. Page with
`next_cursor` when `truncated` is true.

## Examples — record a fact from a passage

```json tool=Document
{"op": "upsert_chunk", "scope": "tenant", "document_id": "3e4f5a6b7c8d49e0a1b2c3d4e5f60718", "natural_key": "person:ada-lovelace|works_at|organization:acme-bank",
 "title": "Ada Lovelace works at Acme Bank", "body": "Ada Lovelace works at Acme Bank.", "type": "employment", "subject": "Ada Lovelace",
 "source_chunk_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "source_quote": "Ada Lovelace, Head of Risk at Acme Bank",
 "about_ids": ["2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f", "9a8b7c6d5e4f40312a1b0c9d8e7f6a5b"], "relation": "works_at"}
```

Later, what is known about Acme Bank, in every scope you can read:

```json tool=Document
{"op": "list_facts", "scope": "tenant", "about": "9a8b7c6d5e4f40312a1b0c9d8e7f6a5b", "claims_only": true, "across_scopes": true}
```

## Examples — after a new edition

```json tool=Document
{"op": "import_md", "scope": "tenant", "reimport": true, "work_key": "law:ua/2297-vi", "edition_date": 1772323200000000000, "markdown": "# On the protection of personal data\n\n## Article 1\n\nTerms.\n\n## Article 2\n\nThe new article."}
```

Re-read each part in `changed_chunk_ids` for citations, then review what the
reimport sent back to review:

```json tool=Document
{"op": "extract_citations", "scope": "tenant", "id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f"}
```

```json tool=Document
{"op": "list_statements", "scope": "tenant", "status": "proposed", "document_id": "0c1d2e3f4a5b4c6d8e9f0a1b2c3d4e5f", "limit": 50}
```
