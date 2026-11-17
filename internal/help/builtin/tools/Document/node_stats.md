---
name: Document/node_stats
description: "Document op=node_stats — report which taxonomy nodes (concepts and document classes) have stale statistics, or recompute them: member counts, and where the store keeps vectors, spread, exemplars, nearest siblings and distinctive terms. An operator's or a granted pass."
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

It always works on the tenant scope, whatever `scope` the call names. It may be
run by an operator, or by a code agent granted `statement_methods`, holding the
tenant scope, whose access-control rules let it read every document of the
tenant scope: a node's figures are drawn from every member.

## Arguments

- `mode` — `report` (the default): every node with its status and why, writing
  nothing. `compute`: recompute the stale and missing nodes and store them;
  rows of nodes no longer in force are removed.
- `scheme` — only this scheme's concepts (by name). Without it, every concept
  and every document class; only then are rows of removed nodes reported and
  removed.

## Returns

`{mode, scheme?, vectors, note?, embed_model?, min_members, nodes, orphans?,
fresh, stale, missing, computed, removed}`:

- `vectors` — whether the vector stage runs here; `note` says why it does not.
- `nodes` — `{node_id, kind (concept | class), name, tag?, members,
  members_with_vector?, status (fresh | stale | missing), reason?, computed?,
  error?}`. `members` is the current count over the whole scope.
  `members_with_vector` is, in a report, the stored row's figure; after a
  compute, the members a vector was found for. `error` says why a node's
  statistics could not be computed (its counts were still stored).
- `orphans` — rows whose node is no longer in force, as `kind:node_id`.

## Errors

- `node_stats: mode "<x>" is not report or compute`.
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
