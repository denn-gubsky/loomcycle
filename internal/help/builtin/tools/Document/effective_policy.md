---
name: Document/effective_policy
description: "Document op=effective_policy — the retention and groups a confirmed ontology's concepts set on one document: the concepts it is under, the longest retention, every group they require, and what contributes nothing. Reported only; nothing is enforced."
---
`effective_policy` says what the tenant ontology's file-plan policy is for one
document. A concept in the ontology's Schemes section may declare
`@retention <period>` and `@acl <group>`; this op works out what those mean for
the document you name. It **enforces nothing**: no access is removed and nothing
is deleted by age because of the answer. It shows the policy before anything
acts on it.

How it is worked out:

- **The document's concepts.** In the tenant scope, the concepts the document's
  root has an accepted `subject` statement (or a narrower kind, such as
  `primary_topic`) or `classified_as` statement to. A proposed or rejected
  statement changes nothing. In a user or agent scope, where no statement
  reaches the ontology, the concepts whose tag (`<scheme>/<path>`) the root or
  the document carries.
- **One concept.** Its retention is its own `@retention`, else the nearest
  broader concept's. Its groups are every `@acl` from the top of the scheme
  down to it, so a narrower concept only adds groups.
- **Several concepts.** The retention is the longest of theirs; the groups are
  all of theirs, so a reader would have to be in every one.
- **What contributes nothing.** A draft ontology sets no policy. A concept that
  is only proposed, a period that does not parse and a group the tenant does
  not have are left out and listed in `issues`.

It asks read on the document, as `get_document` does, and shows the
ontology's concepts only if the caller may read the ontology document.

## Arguments

- `document_id` (or `id`, or `path`) — the document.
- `scope` — the scope the document is in (default user).

## Returns

`{document_id, scope, enforced, note, open_mode?, ontology_status, concepts,
retention?, retention_from?, groups, issues?}`:

- `enforced` — false. `note` says so in words.
- `open_mode` — true when no access-control rules apply to the caller, so groups
  would not limit access here even once enforced.
- `ontology_status` — `confirmed`, `draft`, or `""` when the tenant has no
  ontology.
- `concepts` — each `{concept, tag, chunk_id, via, effective_retention?,
  retention_from?, effective_groups?}`. `via` is the statement kind that puts the
  document under the concept, or `tag`. `retention_from` names the broader
  concept the retention comes from, when it is inherited.
- `retention` — the longest effective retention of the concepts, and
  `retention_from` the concept (by tag) it comes from. Absent when none sets one.
- `groups` — every group the concepts require; `[]` when none.
- `issues` — what contributes nothing to this document's policy, and a warning
  when no member of the tenant is in all of `groups`. That warning is checked
  only for a caller who may see who is in each of those groups; otherwise an
  issue says it was not checked.

## Errors

- A permission error when the caller's access-control rules do not let it read
  the document.
- `effective_policy: no such document` — the id is not a document of the scope.
- `effective_policy: the tenant ontology could not be read` is a storage
  failure; retrying is reasonable.

## Examples

```json
{"op": "effective_policy", "scope": "tenant", "document_id": "3f9a1c2b7d4e4f60a1b2c3d4e5f60718"}
```

```json result
{"document_id": "3f9a1c2b7d4e4f60a1b2c3d4e5f60718", "scope": "tenant", "enforced": false, "note": "Reported only: no access is removed and nothing is deleted by age because of this policy yet.", "ontology_status": "confirmed", "concepts": [{"concept": "Outsourcing", "tag": "file-plan/risk-management/outsourcing", "chunk_id": "7a1b2c3d4e5f40718293a4b5c6d7e8f9", "via": "subject", "effective_retention": "10y", "retention_from": "file-plan/risk-management", "effective_groups": ["compliance", "vendors"]}], "retention": "10y", "retention_from": "file-plan/risk-management/outsourcing", "groups": ["compliance", "vendors"]}
```
