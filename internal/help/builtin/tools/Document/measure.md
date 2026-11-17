---
name: Document/measure
description: "Document op=measure — the taxonomy's built-in series per node and period: count, share, shift, and per scheme unplaced; shown_at reads them as they were stored at a past moment, compare orders the concepts by how far their share moved."
---
`measure` reads the series `node_stats` keeps for the tenant's taxonomy. For
every concept, every document class and every period it stores:

- `count` — the node's members dated in the period, each document once (a
  broader concept counts its narrower concepts' members);
- `share` — a concept's members of the classes its scheme arranges
  (`@projects`; every document class without it) dated in the period, divided
  by every document of those classes dated in it (`basis`). Concepts only;
- `shift` — how far the period's members point away from all earlier members:
  1 − cos between the two centroids of their document annotation vectors. Kept
  only where the store keeps vectors (Postgres with pgvector) and an embedder
  is configured, and only when the period and the earlier periods each hold at
  least three members with a vector;
- `unplaced` — per scheme: the documents of its projected classes dated in the
  period that sit under none of its concepts, with the `basis` and the `share`.

**Dates.** A document counts under its root chunk's `valid_at`; else the
update time its import recorded (`source_updated_at`); else when it was stored.
Every point carries `fallback_share`, the share of its members dated only by
when they were stored: a corpus imported in one batch lands in one period, and
its series say nothing about the world.

**Periods** are months unless the scheme says `@period week` or `@period day`
(see `describe_ontology`). Weeks are ISO weeks (Monday to Sunday); every
period is in UTC. A document class keeps months.

**Nothing is overwritten.** A recomputed period whose inputs changed (a late
member, a removed placement, a member's annotation for `shift`) gets a new row
with a later `computed_at`; the earlier row stays. Read with `shown_at` to see
the series as it stood then. A node no longer in the ontology keeps its rows:
read them by its old chunk id.

It always reads the tenant scope (the call takes no `scope`) and needs
the tenant scope granted. A series is built from every member, so a caller
whose access-control rules limit its reading gets a node's series only when it
may read every document the series is built from (for `share` and `unplaced`,
every document of the period's `basis`), and only when the stored rows were
built from the documents there are now; otherwise the node carries
`withheld` and why.

## Arguments

- `measure` — required: `count`, `share`, `shift` or `unplaced`.
- `node` — one node: a concept by its chunk id or tag, or a document class by
  its name. Without it, every node the series is kept for. Not taken with
  `unplaced`.
- `scheme` — only this scheme's concepts (or, with `unplaced`, this scheme).
- `from`, `to` — only periods starting at or after `from` and before `to` (unix
  nanoseconds). With `compare` they are the range compared; `from` defaults to
  `compare.to`.
- `shown_at` — read the rows as they were stored at this moment (unix
  nanoseconds). Default: the latest.
- `compare` — `{from, to}`, the baseline range. With `measure: share`: sums each
  concept's members and basis over both ranges and orders the concepts by the
  log-ratio of the two shares (each smoothed by half a member, capped at ±3),
  largest move first. With `node`, that concept and those under it.

## Returns

`{measure, scheme?, from?, to?, shown_at?, note?, nodes, compare?}`:

- `nodes` — `{node_id, kind (concept | class | scheme), name, tag?, scheme?,
  period, retired?, points, withheld?}`; each point `{period_start (unix
  nanoseconds), label (2026-07, 2026-W29, 2026-07-21), value, members, basis?,
  share?, fallback_share, computed_at}`. For `count`, `value` is `members`; for
  `share`, `members` is the projected members it counts; for `shift`, `members`
  is the period's members with a vector.
- `note` — why a series is not kept here (`shift` without vectors).
- `compare` — `{range, baseline, nodes, withheld?}`; each node `{node_id, name,
  tag, scheme, count, basis, share?, baseline_count, baseline_basis,
  baseline_share?, log_ratio}`.

A period with no member is not listed. Nothing is listed until `node_stats`
`mode: compute` has run (or its sweeper, `memory.node_stats`).

## Errors

- `measure: measure is required` / `measure "<x>" is not a built-in series`.
- `measure: unplaced is kept per scheme, not per node`.
- `measure: compare orders nodes by share change; it takes measure share` and
  `compare needs from and to`.
- `measure: no node "<x>"` / `no scheme "<x>" is in force`.
- `measure: permission: …` — the caller lacks the tenant scope, or may not read
  the ontology document.

## Examples

A concept's monthly count:

```json
{"op": "measure", "measure": "count", "node": "topics/risk-management"}
```

```json result
{"measure": "count", "nodes": [{"node_id": "6f1d2c3b4a5e4f60718293a4b5c6d7e8", "kind": "concept", "name": "Risk Management", "tag": "topics/risk-management", "scheme": "topics", "period": "month", "points": [{"period_start": 1782864000000000000, "label": "2026-07", "value": 4, "members": 4, "fallback_share": 0.25, "computed_at": 1794950000000000000}, {"period_start": 1785542400000000000, "label": "2026-08", "value": 6, "members": 6, "fallback_share": 0, "computed_at": 1794950000000000000}]}]}
```

What grew and what shrank in August against July, in the topics scheme:

```json
{"op": "measure", "measure": "share", "scheme": "topics", "from": 1785542400000000000, "to": 1788220800000000000, "compare": {"from": 1782864000000000000, "to": 1785542400000000000}}
```
