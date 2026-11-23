---
name: Document/node_stats
description: "Document op=node_stats — report which taxonomy nodes (concepts and document classes) have stale statistics, or recompute them: member counts, and where the store keeps vectors, spread, exemplars, nearest siblings and distinctive terms; or list the misfits (documents nearer a sibling concept) and propose their placements; or rank concepts by recent growth and list quiet nodes; or list the groups that recur when a concept's members or a scheme's unplaced things are clustered again, and sibling concepts to merge, and propose them; or compare each document with each level's centroids and report the margin and its agreement with the live placements, and propose placements. An operator's or a granted pass."
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
  `split`: split, unplaced-grouping and merge candidates (below), writing
  nothing unless `propose`. `assign`: each document against each level's
  centroids (below), writing nothing unless `propose`.
- `scheme` — only this scheme's concepts (by name). Without it, every concept
  and every document class; only then are rows of removed nodes reported and
  removed.
- `propose` — mode `misfits`: file a placement proposal per misfit. Mode
  `split`: file each candidate as a proposal. Mode `assign`: file a placement
  proposal per eligible ranking.
- `limit` — with `propose`: the most proposals this call files (modes
  `misfits` and `assign`: default 20, at most 100; mode `split`: default 10, at
  most 50). Mode `assign` without `propose`: the documents per page (default
  50, at most 200).
- `margins` — mode `assign` only: the threshold per level, keyed by the level
  as a string, at most 1; a negative one turns the level off. Default
  `{"2": 0.03}`.
- `after`, `created_after` — mode `assign`: the page cursor, and the held-out
  bound (unix nanoseconds).
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

## Split

With `mode: split` the call clusters (k-means, k from 2 to 4) and reports three
kinds of candidate. It needs the vector stage: elsewhere `vectors` is false,
`note` says why, and nothing is listed.

- `splits` — each concept with at least 20 **direct** members that have a
  vector (documents placed on it that none of its narrower concepts holds:
  those are already arranged), whose clustering kept two groups or more. A group is listed only when it
  **recurs**: the members are clustered again four times, each time a random
  80% of them with another seed, and every run must find a group holding
  three quarters of the group's members it saw. `not_recurring` counts the
  groups that did not; they are never proposed. Each group would be a
  narrower concept of the node; the node keeps its id, its members and its
  rolled-up series.
- `unplaced` — per scheme, the things it projects that none of its concepts
  holds, those of periods whose unplaced share rose first (at most 2000),
  grouped the same way when there are at least 20. A thing whose similarity
  to its group's centre is below 0.5 stays unplaced, and so does every thing
  of a group that does not recur: `stays_unplaced` lists them. A group goes
  under the scheme's concept whose centroid is nearest, when at least 0.5
  alike (`parent_similarity`), else at the top of the scheme.
- `merges` — sibling concepts whose centroids are at least 0.95 alike: the one
  with fewer members (`node_id`) would be retired into the other
  (`into_id`). Each concept is retired by at most one candidate.

Each group carries a suggested `name` (its first distinctive terms, against
the other groups), its `terms`, `exemplars`, and its `documents` with titles
and briefs, nearest the centre first (at most 50). The thresholds are not
measured yet and depend on the embedding model. A call runs at most 100
clusterings within two minutes; past that it stops, sets `truncated` and says
so in `note` — pass `scheme` to cover the rest. Each concept is in at most one
merge, most alike first.

**No model checks a group here.** A group is meant to be proposed only when a model
grouping of the things' subtopic labels finds it too; `node_stats` makes no
model call, so `model_check` says so and each group lists the documents that
step reads. No shipped agent runs that check yet, so a group filed with
`propose` has been checked by no model; each filing says so, and an operator
rules on it.

It writes nothing unless `propose: true`. Then it files, in the answer's order
and at most `limit` per call, through `propose_entity` (every check of that op
applies, and every filing is inert until an operator accepts it): each group
as a proposed concept under its parent (`section: schemes`), its body saying
where it came from and that no model checked it; each merge as a retire
proposal of the smaller concept (`change: retire`), its body naming the
concept it merges into. The operator plans a retire proposal by its id before
accepting it. Moving documents under a new concept is never filed: they can be
placed only once it is in force. A concept of an `@origin adopted` scheme, or
one `@locked`, is `fixed` and never proposed for retiring; a local concept may
still be added under it. Filing is for an operator, or a run granted `rule` in
`statement_methods`, as for misfits.

It returns `{mode, scheme?, vectors, note?, embed_model?, min_members, runs,
recur_min, jaccard, far_similarity, merge_similarity, model_check, propose,
cap?, splits, unplaced, merges, proposed?, skipped?, members_skipped?,
issues?, truncated?}`: a split `{node_id, name, tag, scheme, members, direct,
clustered, spread, fixed?, k, groups, not_recurring}`; an unplaced grouping
`{scheme, scheme_id, things, clustered, truncated?, grouped, groups,
not_recurring, stays_unplaced, stays_truncated?}`; a group `{key, name, terms,
members, recurrence, runs, parent_id, parent, parent_similarity?, exemplars,
documents, documents_truncated?, proposal_id?, skipped?, detail?}`; a merge
`{node_id, name, tag, into_id, into, into_tag, similarity, members,
into_members, fixed?, proposal_id?, skipped?, detail?}`. `skipped` says why
nothing was filed: `exists` (an open proposal of that name under that parent),
`rejected`, `duplicate`, `fixed`, `cap` or `refused:<code>`.

## Assign

With `mode: assign` the call compares each document a scheme arranges (of the
classes its `@projects` names, or every document class) with the centroids of
one level's concepts at a time, and reads off the **nearest**, the **second**
and the **margin** between them (nearest less second). The unit is the whole
document: its document-level annotation vector. Level 1 compares the scheme's
top concepts. A lower level compares the children of a concept the document is
placed under live, or of the nearest concept one level up when that ranking
cleared its threshold. A comparison needs at least two concepts with a
centroid (three members with a vector each). A document that is a member of a
concept is compared with that concept's centroid without itself, so its own
pull does not make it agree. It needs the vector stage, as misfits does.

Without `propose` it writes nothing and answers a **report**: a page of
documents (`limit`, default 50, at most 200; continue with `after` set to
`next_cursor`), each with its live placements (the most specific concepts it
is placed under) and its rankings; and per level, over every document
compared, how often the nearest agrees with a live placement, in margin
buckets of 0.01 (`buckets`) and at or above each bucket's lower bound
(`at_least`). A ranking is judged (`agrees` present) only when the document is
placed under one of the compared concepts. `created_after` keeps only the
documents created after that instant: a held-out set, to check a threshold on
documents placed after it was chosen.

A level's **threshold** is set by `margins`, keyed by the level as a string:
the default is `{"2": 0.03}`, so level 1 has none. A ranking is `eligible`
when its margin is at least its level's threshold and the document is not
already placed under the nearest concept. With `propose: true` each eligible
ranking, largest margin first, is filed as a `subject` proposal to the nearest
concept, method `rule:centroid`, confidence the margin, the document's title as
evidence, at most `limit` per call (default 20, at most 100), never one any
reviewer rejected nor one already proposed or live. Who may file is as for
misfits. Every filing lands `proposed`: nothing here makes a placement live. A
tenant that has checked the margin may write a Review rule such as
`accept kind:subject method:rule:centroid when confidence>=0.03`, and only such
a rule rules on these proposals when they are filed.

It returns `{mode, scheme?, vectors, note?, embed_model?, min_members,
margins, propose, cap?, created_after?, documents, without_vector, levels,
page?, next_cursor?, proposals?, proposed?, skipped?, truncated?,
members_skipped?, issues?}`. `documents` counts the documents compared;
`without_vector` those arranged by a scheme with no annotation vector. A level
is `{level, threshold?, rankings, placed, agree, eligible, buckets, at_least}`,
each bucket `{from, to?, rankings, placed, agree, agreement?}`. A page entry
is `{document_id, title, placements, rankings}`; a ranking `{scheme, level,
parent?, compared, nearest, second, margin, agrees?, threshold?, eligible?}`,
`nearest` and `second` each `{node_id, node, tag, similarity}`. With `propose`,
`proposals` lists the eligible rankings (at most 200) with `document_id`,
`title` and the outcome: `statement_id` and `status`, or `skipped`
(`rejected`, `exists`, `cap`, `duplicate` or `refused:<code>`).

## Returns

`{mode, scheme?, vectors, note?, embed_model?, min_members, nodes, orphans?,
fresh, stale, missing, computed, removed, series, definitions?}`:

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
- `definitions` — `compute` where the vector stage runs: each concept's
  definition (its place in the scheme, name, meaning line and alternative
  labels) is embedded and kept, never returned, so `classify` can ask a large
  vocabulary's nearest concepts in one request. Only a definition whose text
  or embed model changed is embedded again. `{fresh, embedded, pending?,
  removed?, error?}`: `pending` counts the ones left for the next pass (past
  1000 a pass, or after an embedder `error`).

## Errors

- `node_stats: mode "<x>" is not report, compute, misfits, growth, split or assign`.
- `node_stats: propose is taken only with mode misfits, split or assign`.
- `node_stats: method_not_granted: …` — mode `misfits`, `split` or `assign`
  with `propose` from a run not granted `rule`.
- `node_stats: margins is taken only with mode assign`; `margins: "<x>" is not
  a level from 1 to 8`; `margins: level <n>'s margin must be at most 1`.
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

List the split and merge candidates of one scheme, writing nothing:

```json
{"op": "node_stats", "mode": "split", "scheme": "topics"}
```

```json result
{"mode": "split", "scheme": "topics", "vectors": true, "embed_model": "ollama/bge-m3", "min_members": 20, "runs": 5, "recur_min": 5, "jaccard": 0.75, "far_similarity": 0.5, "merge_similarity": 0.95, "model_check": "not run: node_stats makes no model call. A group is meant to be proposed only when a model grouping of the things' subtopic labels finds it too; each group lists the documents that step reads", "propose": false, "splits": [{"node_id": "7a2e3d4c5b6f4071829304a5b6c7d8e9", "name": "reporting", "tag": "topics/reporting", "scheme": "topics", "members": 24, "direct": 24, "clustered": 24, "spread": 0.41, "k": 2, "groups": [{"key": "3f2a1b0c9d8e7f6a", "name": "xbrl template taxonomy", "terms": ["xbrl", "template", "taxonomy"], "members": 12, "recurrence": 5, "runs": 5, "parent_id": "7a2e3d4c5b6f4071829304a5b6c7d8e9", "parent": "topics/reporting", "exemplars": [{"document_id": "0a1b2c3d4e5f60718293a4b5c6d7e8f9", "title": "XBRL filing templates", "similarity": 0.99}], "documents": [{"document_id": "0a1b2c3d4e5f60718293a4b5c6d7e8f9", "title": "XBRL filing templates", "brief": "The XBRL templates for the annual filing.", "similarity": 0.99}]}], "not_recurring": 0}], "unplaced": [], "merges": [{"node_id": "8b3f4e5d6c7a4182930415b6c7d8e9f0", "name": "auditing", "tag": "topics/auditing", "into_id": "9c4a5f6e7d8b4293a41526c7d8e9f0a1", "into": "audit", "into_tag": "topics/audit", "similarity": 0.97, "members": 3, "into_members": 4}]}
```


Check the level-2 margin on documents created since a date, writing nothing:

```json
{"op": "node_stats", "mode": "assign", "scheme": "topics", "created_after": 1795000000000000000, "limit": 1}
```

```json result
{"mode": "assign", "scheme": "topics", "vectors": true, "embed_model": "ollama/bge-m3", "min_members": 3, "margins": {"2": 0.03}, "propose": false, "created_after": 1795000000000000000, "documents": 2, "without_vector": 0, "levels": [{"level": 2, "threshold": 0.03, "rankings": 2, "placed": 2, "agree": 2, "eligible": 0, "buckets": [{"from": 0, "to": 0.01, "rankings": 0, "placed": 0, "agree": 0}, {"from": 0.01, "to": 0.02, "rankings": 0, "placed": 0, "agree": 0}, {"from": 0.02, "to": 0.03, "rankings": 0, "placed": 0, "agree": 0}, {"from": 0.03, "to": 0.04, "rankings": 1, "placed": 1, "agree": 1, "agreement": 1}, {"from": 0.04, "to": 0.05, "rankings": 1, "placed": 1, "agree": 1, "agreement": 1}, {"from": 0.05, "to": 0.06, "rankings": 0, "placed": 0, "agree": 0}, {"from": 0.06, "to": 0.07, "rankings": 0, "placed": 0, "agree": 0}, {"from": 0.07, "to": 0.08, "rankings": 0, "placed": 0, "agree": 0}, {"from": 0.08, "to": 0.09, "rankings": 0, "placed": 0, "agree": 0}, {"from": 0.09, "rankings": 0, "placed": 0, "agree": 0}], "at_least": [{"from": 0, "rankings": 2, "placed": 2, "agree": 2, "agreement": 1}, {"from": 0.01, "rankings": 2, "placed": 2, "agree": 2, "agreement": 1}, {"from": 0.02, "rankings": 2, "placed": 2, "agree": 2, "agreement": 1}, {"from": 0.03, "rankings": 2, "placed": 2, "agree": 2, "agreement": 1}, {"from": 0.04, "rankings": 1, "placed": 1, "agree": 1, "agreement": 1}, {"from": 0.05, "rankings": 0, "placed": 0, "agree": 0}, {"from": 0.06, "rankings": 0, "placed": 0, "agree": 0}, {"from": 0.07, "rankings": 0, "placed": 0, "agree": 0}, {"from": 0.08, "rankings": 0, "placed": 0, "agree": 0}, {"from": 0.09, "rankings": 0, "placed": 0, "agree": 0}]}], "page": [{"document_id": "0a1b2c3d4e5f60718293a4b5c6d7e8f9", "title": "Vendor exit plan", "placements": [{"node_id": "4d5e6f708192a3b4c5d6e7f8091a2b3c", "node": "outsourcing", "tag": "topics/risk-management/outsourcing"}], "rankings": [{"scheme": "topics", "level": 2, "parent": "topics/risk-management", "compared": 3, "nearest": {"node_id": "4d5e6f708192a3b4c5d6e7f8091a2b3c", "node": "outsourcing", "tag": "topics/risk-management/outsourcing", "similarity": 0.82}, "second": {"node_id": "5e6f708192a3b4c5d6e7f8091a2b3c4d", "node": "operational risk", "tag": "topics/risk-management/operational-risk", "similarity": 0.785}, "margin": 0.035, "agrees": true, "threshold": 0.03}]}], "next_cursor": "0a1b2c3d4e5f60718293a4b5c6d7e8f9"}
```
