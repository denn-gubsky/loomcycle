---
name: Document/ontology_signals
description: "Document op=ontology_signals — what the tenant's corpus uses and the ontology does not declare: types on documents, chunks and facts that no class declares, relation kinds no relation declares, and statements of a declared kind marked ontology_valid:false, grouped by kind and the two ends' types. Counts over every scope of the tenant, sample ids from the tenant scope, never a title or a body. An operator's read; it writes nothing."
---
`ontology_signals` reports where the stored data and the tenant ontology
disagree. It is the material an ontology is built or extended from: each row is
a candidate for a new class, a new relation kind, or a wider domain or range.
It only reads. Proposing an entry is `propose_entity`.

It always works on the tenant, so pass `scope: "tenant"` or no scope; any other
scope is refused. It **writes nothing and provisions nothing**: no scope is
created or migrated, and reading a scope does not count as a use of it.

**Who may call it.** An operator only. The counts cover every user's and every
agent's scope of the tenant, so a member and an agent run are refused.

## Arguments

- `scope` — `tenant`, or omitted. Any other scope is refused.
- `limit` — the most groups listed per signal: default 20, at most 100.

## What counts as declared

The classes and relation kinds in force: the tenant's own (those of a confirmed
ontology), the standard seed, the reserved kinds the runtime writes (`about`,
`references`, …), and **retired** entries, which stay declared for what already
uses them. A relation's former name (`@renamed_from`) is declared too. The
types the document primitive and the runtime write themselves (`root`,
`section`, `image`, `mermaid`, `canvas`, `SourceStub`, `SyncReport`,
`Annotation`) are never listed. A class or kind that is only proposed is not
declared: it is listed, with `proposal` saying so.

A tenant with no ontology document, or an unconfirmed one, is read against the
seed alone.

## Answer

- `ontology` — `document_id`, `confirmed`, and `fingerprint` (the plan
  fingerprint of the ontology the signals were read against; empty with no
  document).
- `complete`, `scopes_counted`, `scopes_skipped` — the user and agent scopes
  read. The pass has a 20-second budget for them; a scope not reached, or one
  that could not be read, is skipped, `complete` is false, and every count is
  then a lower bound. The tenant scope is always read.
- `undeclared_types[]` — one row per type and population:
  `{type, population, n, tenant_scope, other_scopes, documents, sample, proposal}`.
  - `population` is `document` (the type of a document), `chunk` (a chunk that
    is not a fact) or `fact` (a chunk carrying fact metadata). A document's
    root chunk is counted as the document only, so the three do not overlap.
  - Spellings that differ only in case are one type; `type` is the most used.
  - `documents` is how many documents hold the rows.
  - `sample` is up to five rows of the tenant scope: `{chunk_id, document_id}`,
    or `{document_id}` for a document.
- `undeclared_kinds[]` —
  `{kind, n, by_status, tenant_scope, other_scopes, ends, sample, proposal}`.
  - `by_status` counts by statement status; a retired statement counts as
    `expired`.
  - `ends[]` is `{from, to, n}`: the types of the chunks the statements join,
    as stored (`""` for an untyped or missing chunk), at most 20, largest first.
  - `sample` is up to five statement ids of the tenant scope.
- `invalid_ends[]` — `{kind, from, to, n, domain, range, sample}`: the live
  statements (not retired, not rejected) of a **declared** kind that carry
  `ontology_valid: false`, grouped by kind and the two ends' types as stored,
  with the kind's declared `domain` and `range`. The mark does not record which
  rule was broken, so a statement that broke a characteristic or a qualifier is
  counted here as well.
- `proposal` — `""`, `"proposed"` or `"rejected"`: whether the ontology
  document already holds a proposal of that name. `propose_entity` refuses
  both, so a pass skips them.
- `truncated[]` — the lists that are not whole: `undeclared_types`,
  `undeclared_kinds`, `invalid_ends` (cut at `limit`, or read from a scope with
  more distinct values than the store returns in one read) and
  `undeclared_kinds.ends`.

Each list is ordered largest `n` first. `n` covers every scope read;
`tenant_scope` and `other_scopes` split it. Samples come from the tenant scope
only, so a row found only in user or agent scopes has an empty `sample`.

The answer holds ids and counts only. To read what a sample says, fetch it with
`get_chunk`, `get_document` or `get_edge` in the tenant scope.

## Examples

```json
{"op": "ontology_signals", "scope": "tenant"}
```

```json result
{"ontology": {"document_id": "…", "confirmed": true, "fingerprint": "…"},
 "complete": true, "scopes_counted": 12, "scopes_skipped": 0,
 "undeclared_types": [{"type": "publication", "population": "chunk", "n": 56,
   "tenant_scope": 56, "other_scopes": 0, "documents": 9,
   "sample": [{"chunk_id": "…", "document_id": "…"}], "proposal": ""}],
 "undeclared_kinds": [{"kind": "supervises", "n": 14, "by_status": {"proposed": 14},
   "tenant_scope": 2, "other_scopes": 12,
   "ends": [{"from": "person", "to": "person", "n": 13}, {"from": "", "to": "person", "n": 1}],
   "sample": ["…"], "proposal": "rejected"}],
 "invalid_ends": [{"kind": "depends_on", "from": "Plan", "to": "product", "n": 6,
   "domain": ["TechnicalDocument"], "range": ["TechnicalDocument"], "sample": ["…"]}],
 "truncated": []}
```
