---
name: Document/node_stats
description: "Document op=node_stats — report which taxonomy nodes (concepts and document classes) have stale statistics, or recompute them: member counts, and where the store keeps vectors, spread, exemplars, nearest siblings and distinctive terms; or list the misfits (documents nearer a sibling concept) and propose their placements; or rank concepts by recent growth and list quiet nodes. An operator's or a granted pass."
---
`node_stats` keeps statistics for each node of the tenant's taxonomy: every
concept of every scheme, and every document class. `describe_ontology` with
`with_stats: true` reads what it stored.

A node's **members** are whole documents. A concept's are the documents with a
live `subject` statement (or one of a narrower kind) to it or to a narrower
concept, each once — the documents `describe_ontology with_counts` counts and
`search concept:<tag>` draws from. A class's are the documents of that type and,
in a confirmed ontology, of its subclasses.

Where the store keeps vectors (Postgres with pgvector) and an embedder is
configured, a node with at least three members that have a document-level
annotation also gets:

- a centroid of their annotation vectors (stored, never returned) and its
  `spread`;
- `exemplars` — the three members nearest the centroid;
- `siblings` — the three nodes under the same parent nearest by centroid;
- `terms` — up to ten words frequent in the members' briefs and abstracts and
  rare in the siblings'.

Elsewhere, and below three members with a vector, a row keeps the counts only.

A row is **stale** when its node's definition (name, meaning line, place in the
scheme), its member set, a member's annotation or the embed model changed since
it was computed. An ontology edit to another node marks nothing stale.

A node's members are only the documents every member of the tenant may read: a
document the tenant's access-control rules keep from some member (a deny bound
to a group, a user, a token or the whole tenant) is in no node's figures — not
counted, not an exemplar, not in the terms, not in a series. The figures are
then served the same to every reader of the ontology. A member whose document
annotation is metadata-only (a catalogue record annotated without its content)
is counted and left out of the centroid, the exemplars and the terms. A row
stored before this rule is reported `stale` and computed again.

It always works on the tenant scope, whatever `scope` the call names. It may be
run by an operator, or by a code agent granted `statement_methods`, holding the
tenant scope, whose access-control rules let it read every document of the
tenant scope.

## Arguments

- `mode` — `report` (the default): every node with its status and why, writing
  nothing. `compute`: recompute the stale and missing nodes and store them;
  rows of nodes no longer in force are removed. `misfits`: the misplaced
  members (below), writing nothing unless `propose`. `growth`: growth, quiet
  nodes and unplaced periods from the stored series (below), writing nothing.
- `scheme` — only this scheme's concepts (by name). Without it, every concept
  and every document class; only then are rows of removed nodes reported and
  removed.
- `propose` — mode `misfits` only: file a placement proposal per misfit.
- `limit` — mode `misfits` with `propose`: the most proposals this call files
  (default 20, at most 100).
- `quiet_periods` — mode `growth` only: how many periods count as recent
  (default 6, at most 120).

## Misfits

With `mode: misfits` the call lists the **misfits**: member documents whose
annotation vector is nearer a sibling concept's centroid than their own
concept's (computed without them) by more than the margin, 0.05. Siblings are
the concepts under the same broader concept, or a scheme's top level;
document classes are not compared. It needs the vector stage: elsewhere
`vectors` is false and `note` says why, and nothing is listed.

It writes nothing unless `propose: true`. Then it files, largest margin
first, a `subject` proposal from the document to the nearer concept, method
`rule:centroid`, confidence the margin, the document's title as evidence,
through `propose_statement` (so the review policy routes it). At most `limit`
per call (default 20, at most 100). It never files a placement any reviewer
rejected for that document (a person, a judge or a review policy), nor one
already proposed or live. A proposal adds a topic: the document keeps the
placement it has. Nothing here changes the ontology.

Filing is for an operator, or a run granted `rule` in `statement_methods` —
whoever could stamp a `rule:` method itself; any other caller gets
`method_not_granted` for `propose` and may still read the misfits. Every
filing lands `proposed`: `@auto_assert` never asserts it, and only the
tenant's Review section can rule on it when it is filed.

It returns `{mode, scheme?, vectors, note?, embed_model?, margin,
min_members, propose, cap?, count, misfits, truncated?, proposed?,
skipped?, members_skipped?, issues?}`: `members_skipped` counts members whose
stored vector could not be compared (all zero, not finite, or of another
dimension), left out; `issues` names sibling groups the comparison failed on,
left out while the rest are answered. Each misfit is `{document_id, title, node_id, node,
nearest_node_id, nearest, own_similarity, nearest_similarity, margin,
statement_id?, status?, skipped?, detail?}` (at most 200 listed; `count` is
whole). `skipped` says why no proposal was filed: `rejected`, `exists`, `cap`,
`duplicate` (a second misfit of the same document toward the same concept) or
`refused:<code>`.

## Growth

With `mode: growth` the call reads the series `compute` stored (the rows
`measure` serves, so only documents every member of the tenant may read) and
writes nothing. Windows are counted in each node's own period (its scheme's
`@period`, else months), ending in the period that holds now; a member
dated after it counts in no window:

- `candidates` — the concepts with members dated in the last `quiet_periods`
  periods, ranked by `growth` (those members less the ones dated in the
  `quiet_periods` periods before), then by `recent`, then by size. These are
  where narrower concepts are most likely needed.
- `quiet` — concepts and document classes with members but none dated in the
  recent window, with `last_period` and `periods_since`. Reported only:
  nothing retires a node.
- `unplaced` — per scheme, the periods of its unplaced series, those whose
  unplaced share rose over the period before (`rising`) first, then the rest,
  most recent first. A rising period lists its unplaced `documents` (at most
  50): the things to group first.

A concept of an `@origin adopted` scheme, or one `@locked`, carries `fixed`
(`adopted` or `locked`): growth beneath it may add local concepts, never
rename or move it. A node with no stored series point is not judged;
`series_stale` and `series_missing` count the count and unplaced points that
are behind (including a stored point whose period no member is dated in any
more, which is not ranked from), and `note` says to run `compute` first. It
needs no vectors and reads none.

It returns `{mode, scheme?, quiet_periods, series_stale, series_missing,
note?, candidates, quiet, unplaced, truncated?}`: a candidate is `{node_id,
name, tag, scheme, period, members, recent, previous, growth, last_period,
fixed?}`; a quiet node `{node_id, kind, name, tag?, scheme?, period, members,
last_period, periods_since, fixed?}`; a scheme `{scheme, scheme_id, period,
periods}` with each period `{label, period_start, unplaced, basis, share,
previous_share?, rising, documents?, documents_truncated?}`. Each list holds at
most 200.

## Returns

`{mode, scheme?, vectors, note?, embed_model?, min_members, nodes, orphans?,
fresh, stale, missing, computed, removed, series}`:

- `vectors` — whether the vector stage runs here; `note` says why it does not.
- `nodes` — `{node_id, kind (concept | class), name, tag?, members,
  members_with_vector?, status (fresh | stale | missing), reason?, computed?,
  error?}`. `members` is the current count over the whole scope.
  `members_with_vector` is, in a report, the stored row's figure; after a
  compute, the members a vector was found for. `error` says why a node's
  statistics could not be computed (its counts were still stored).
- `orphans` — rows whose node is no longer in force, as `kind:node_id`.
- `series` — the taxonomy's series per node and period, which `measure`
  reads: `{shift?, fresh, stale, missing, computed, points?, truncated?}`.
  A point is stale when the documents it is computed from changed (a late
  member, a removed placement, a new date; for shift, a member's annotation or
  the embed model). `compute` appends a row for each stale or missing point
  and never changes a stored one; a period that lost every member gets an
  empty row. `points` lists the stale and missing ones (at most 200),
  `{measure, node_id, period, label, status, reason}`. `shift` says why shift
  is not kept here. Rows of a node no longer in force are kept.

## Errors

- `node_stats: mode "<x>" is not report, compute, misfits or growth`.
- `node_stats: propose is taken only with mode misfits`.
- `node_stats: quiet_periods is taken only with mode growth`; `quiet_periods must be 1 to 120`.
- `node_stats: permission: …` — the caller is not an operator or a granted
  agent, lacks the tenant scope, or may not read every document of it.
- `node_stats: no scheme "<x>" is in force`.

## Examples

See which nodes are out of date, writing nothing:

```json
{"op": "node_stats", "mode": "report"}
```

```json result
{"mode": "report", "vectors": true, "embed_model": "ollama/bge-m3", "min_members": 3, "nodes": [{"node_id": "6f1d2c3b4a5e4f60718293a4b5c6d7e8", "kind": "concept", "name": "Risk Management", "tag": "topics/risk-management", "members": 12, "members_with_vector": 11, "status": "stale", "reason": "members changed"}, {"node_id": "Guide", "kind": "class", "name": "Guide", "members": 30, "status": "missing", "reason": "never computed"}], "fresh": 0, "stale": 1, "missing": 1, "computed": 0, "removed": 0}
```

Recompute one scheme's concepts:

```json
{"op": "node_stats", "mode": "compute", "scheme": "topics"}
```

File placement proposals for at most five misfits of one scheme:

```json
{"op": "node_stats", "mode": "misfits", "scheme": "topics", "propose": true, "limit": 5}
```

```json result
{"mode": "misfits", "scheme": "topics", "vectors": true, "embed_model": "ollama/bge-m3", "margin": 0.05, "min_members": 3, "propose": true, "cap": 5, "count": 1, "misfits": [{"document_id": "0a1b2c3d4e5f60718293a4b5c6d7e8f9", "title": "Vendor exit plan", "node_id": "7a2e3d4c5b6f4071829304a5b6c7d8e9", "node": "reporting", "nearest_node_id": "6f1d2c3b4a5e4f60718293a4b5c6d7e8", "nearest": "Risk Management", "own_similarity": 0.21, "nearest_similarity": 0.93, "margin": 0.72, "statement_id": "9e8d7c6b5a4f40312e1d0c9b8a7f6e5d", "status": "proposed"}], "proposed": 1, "skipped": {}}
```

Rank one scheme's concepts by growth over the last three months:

```json
{"op": "node_stats", "mode": "growth", "scheme": "topics", "quiet_periods": 3}
```

```json result
{"mode": "growth", "scheme": "topics", "quiet_periods": 3, "series_stale": 0, "series_missing": 0, "candidates": [{"node_id": "6f1d2c3b4a5e4f60718293a4b5c6d7e8", "name": "Risk Management", "tag": "topics/risk-management", "scheme": "topics", "period": "month", "members": 12, "recent": 5, "previous": 1, "growth": 4, "last_period": "2026-11"}], "quiet": [{"node_id": "7a2e3d4c5b6f4071829304a5b6c7d8e9", "kind": "concept", "name": "reporting", "tag": "topics/reporting", "scheme": "topics", "period": "month", "members": 4, "last_period": "2026-03", "periods_since": 8}], "unplaced": [{"scheme": "topics", "scheme_id": "5e0c1b2a39484f5e6d7c8b9a0f1e2d3c", "period": "month", "periods": [{"label": "2026-10", "period_start": 1790812800000000000, "unplaced": 6, "basis": 9, "share": 0.6667, "previous_share": 0.25, "rising": true, "documents": ["0a1b2c3d4e5f60718293a4b5c6d7e8f9"]}]}]}
```
