---
name: Document/graph_recall
description: "Document op=graph_recall — recall facts by starting from seed chunks or a question and following edges outward, optionally as of a past moment."
---
`graph_recall` answers questions that need a chain of facts ("which region does
the person who runs the Cluj office work in?"). It starts from some chunks and
follows edges, in both directions, hop by hop. Start it with `seed_ids` you
already found, or with a `query`: the query is matched semantically against
chunk bodies when an embedder is configured, otherwise against fact titles as a
whole word. It returns each chunk's title, how it was reached and, for chunks
that are not facts (clauses, sections, notes), the start of its text. A fact
comes back as its title alone, because the title is the claim. **A hop is ONE edge, so fact → subject → fact is two
hops: a two-step question needs `hops: 2`, a three-step one `hops: 4`.**

## Arguments

- `seed_ids` — chunk ids to start from (at most 500). Or:
- `query` — text to find the starting chunks. One of the two is required.
- `hops` — 0 to 6; default 1. 0 returns only the starting chunks.
- `seeds` — with a semantic `query`, how many of the best matches start the
  walk (default 5).
- `body_chars` — how much text each chunk carries. Omitted: chunks that are not
  facts carry the first 600 characters of their body, and facts carry none.
  `0`: no text at all, titles only. A number up to 4000: that many characters on
  every chunk, facts included. A cut body ends on a whole word and comes with
  `body_truncated: true`. Read a whole body with `get_chunk`.
- `budget_chars` — cap the answer by total characters (titles plus bodies)
  instead of by rows. Facts are spent before subject names, and leftover budget
  is filled with the next-best semantic matches the walk did not reach. A chunk
  whose title and body do not fit in what is left is skipped. 0 = off.
- `limit` — at most this many chunks (default 50, at most 200). Applies with
  `budget_chars` too; whichever is hit first wins.
- `as_of` — unix nanos: answer as of that moment. A fact counts when its
  `valid_at` is unset or at or before `as_of` and its `invalid_at` is unset or
  after it, so facts corrected since then come back. A chunk with its own
  `valid_at`/`invalid_at` (an edition, a clause in force for a period) must
  contain the moment too; a fact with both kinds of dates needs both to.
- `kind` / `kinds` — follow only these relation kinds, e.g.
  `["cites", "defines", "implements"]`. The best way to keep a walk on topic.
- `walk` — `declared` follows the relation kinds the tenant declared for
  walking (the default where any are declared), `all` follows every relation
  (the default otherwise). `kinds` overrides it; any other value is refused,
  with or without `kinds`.
- `direction` — `out` (relations leaving a chunk), `in` (arriving) or `both`
  (default).
- `include_hierarchy` — also step along the document tree: `up` to a chunk's
  parent (reported as `part_of`), `down` to its children (`has_part`), or
  `both`. Default `none`.
- `include_proposed` — also follow relations proposed for review and not yet
  accepted (default false).
- `decay` — with `budget_chars`, each hop may spend at most `decay`^hop of the
  budget, so far hops cannot crowd out near ones. 0 < decay ≤ 1; default 0.7 for
  a `declared` walk, else 1. Refused without `budget_chars`.
- `include_retired` — `true` turns the time filter off entirely.
- `include_refuted` — `true` also returns facts a judge marked `unsupported`,
  and follows relations a judge marked unsupported. A relation an operator
  rejected is never followed.
- `document_id` — only start from chunks in this document (the walk may still
  leave it).
- `scope` — `user` (default), `agent` or `tenant`. One scope per call: to
  consult user and tenant facts, make two calls.

Without `as_of` you get what is true now: a fact whose `invalid_at` has passed,
including every superseded fact, is left out. So is a chunk whose own dates put
it out of force now (its own `invalid_at` has passed, or its own `valid_at` is
still ahead). `include_retired: true` brings both back; an ended one is marked
`retired: true`.

For a cheap structural walk — which chunks are connected, and how — pass
`body_chars: 0`.

## Returns

`{chunks: [...], seeds, hops, truncated, seeded_by}`. Each chunk is `{id,
title, type?, status?, hop, via_kind?, via_id?, valid_at?, invalid_at?,
chunk_valid_at?, chunk_invalid_at?, retired, body?, body_truncated?}`: `hop` 0
is a starting chunk, `hop` N was reached over the relation `via_kind` from
`via_id`, and `hop: -1` is a budget backfill reached by rank, not by an edge.
`via_quote` is the first evidence quote of that relation when it has one — cite
it when you use the hop — and `via_status` is `proposed` on a row reached over a
proposal (only with `include_proposed`). A relation is followed only while its
own dates contain the moment asked about (now, or `as_of`).

`valid_at`/`invalid_at` are a fact's dates, or, on a chunk that is not a fact,
the chunk's own dates. A fact that also has dates of its own reports those as
`chunk_valid_at`/`chunk_invalid_at`. `body` is absent when a chunk has no text,
its text could not be read, or `body_chars` left it out.

`seeded_by` is `ids`, `semantic` or `title`. `walk` (`kinds`, `declared` or
`all`) is reported when the walk was narrowed or named, and
`kind_expanded_to` when a kind widened to its declared sub-kinds. With `budget_chars` you also get
`budget_chars`, `chars_used` (titles plus bodies) and `backfilled`, and `decay`
when it is below 1.
`truncated: true` means some chunks were cut by a limit.

## Errors

- `graph_recall: give either seed_ids (chunks to start from) or query ...`.
- `graph_recall: hops must be 0..6 (got N) ...` — bound a long walk with
  `budget_chars`, not with fewer hops.
- `graph_recall: N seed_ids is more than the 500 ...` — split the ids across
  several calls.
- `graph_recall: body_chars must be 0..4000 (got N)` — read a whole body with
  `get_chunk` instead.
- `graph_recall: decay needs budget_chars ...` — pass a budget, or drop `decay`.
- `graph_recall: decay must be in (0, 1] (got N)`.
- `graph_recall: walk must be declared or all` / `direction must be out, in or
  both` / `include_hierarchy must be up, down, both or none`.
- An empty `chunks` list with `seeds: 0` means nothing matched the query in
  this scope. With `seeded_by: "title"` the query must appear as a whole word in
  a fact's title; try `seed_ids` from `search` instead.

## Examples

A two-step question, starting from the question itself, with a content budget:

```json
{"op": "graph_recall", "query": "which region does Toma Zoltan work in", "hops": 2, "budget_chars": 1200}
```

```json result
{"chunks": [{"id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00", "title": "Toma Zoltan runs the Cluj office", "hop": 0, "retired": false},
  {"id": "2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f", "title": "Cluj office", "hop": 1, "via_kind": "about", "via_id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00", "retired": false},
  {"id": "7a6b5c4d3e2f10a9b8c7d6e5f4a3b2c1", "title": "The Cluj office belongs to the EMEA region", "hop": 2, "via_kind": "about", "via_id": "2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f", "retired": false}],
 "seeds": 5, "hops": 2, "truncated": false, "seeded_by": "semantic", "budget_chars": 1200, "chars_used": 93, "backfilled": 0}
```

Walking out from a norm to the clauses it links, with their text:

```json
{"op": "graph_recall", "seed_ids": ["9f1e2d3c4b5a69788796a5b4c3d2e1f0"], "hops": 1}
```

```json result
{"chunks": [{"id": "9f1e2d3c4b5a69788796a5b4c3d2e1f0", "title": "Record retention", "type": "norm", "hop": 0, "retired": false},
  {"id": "0a1b2c3d4e5f60718293a4b5c6d7e8f9", "title": "Clause 4.2 Retention period", "hop": 1, "via_kind": "has_clause", "via_id": "9f1e2d3c4b5a69788796a5b4c3d2e1f0", "valid_at": 1704067200000000000, "retired": false,
   "body": "Customer records are kept for seven years after the account closes. Records under a legal hold are kept until the hold is lifted."}],
 "seeds": 1, "hops": 1, "truncated": false, "seeded_by": "ids"}
```

The same walk as structure only:

```json
{"op": "graph_recall", "seed_ids": ["9f1e2d3c4b5a69788796a5b4c3d2e1f0"], "hops": 1, "body_chars": 0}
```

What was true about a subject on 1 March, walking out from its chunk:

```json
{"op": "graph_recall", "seed_ids": ["2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f"], "hops": 1, "as_of": 1772323200000000000}
```

Follow a clause's citations two hops out — what it implements, and what that
cites — with the section each lands in, on a budget that keeps near hops
first:

```json
{"op": "graph_recall", "seed_ids": ["5e1f0c2a9b8d4e7f8a6b5c4d3e2f1a0b"], "kinds": ["implements", "cites", "defines"], "direction": "out", "hops": 2, "include_hierarchy": "up", "budget_chars": 4000, "decay": 0.6, "scope": "tenant"}
```

The same walk over the tenant's shared facts (a separate call):

```json
{"op": "graph_recall", "seed_ids": ["2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f"], "hops": 2, "scope": "tenant"}
```
