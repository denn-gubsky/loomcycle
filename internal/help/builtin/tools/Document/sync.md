---
name: Document/sync
description: "Document op=sync — reconcile a bound document with its peer copy, pulling the peer's keyed chunks in (default) or pushing yours up."
---
`sync` copies changes between a local document and the peer document it was
bound to with `set_remote`. It only works when the operator has configured
that peer as a document source. Run `diff_remote` first to see what it will
change.

**Only keyed chunks travel** — chunks that carry a `natural_key` (written with
`upsert_chunk`). Chunks are matched across the two servers by that key, never
by id. Chunks made with `create_chunk` or `import_md` have no key and are
skipped (counted in `excluded_unkeyed`).

For each keyed chunk, the side being written to receives the other side's
title, type, status, body, tags and validity period (`valid_at` /
`invalid_at`; a peer on an older version has none, which reads as undated),
its place in the tree (under the chunk
with the same parent key, at the same position; with no keyed parent, under
the root), and the `link_chunks` links between keyed chunks. Nothing is
deleted: a chunk or link that exists only on the receiving side stays. An
overwritten body is kept in that chunk's `history`. Facts a judge marked
unsupported are not copied.

## Links and their evidence

A link travels with its `confidence`, `valid_at` / `invalid_at` and
`attributes`. Only asserted links travel: a proposal is one side's review
state, and a link a judge refuted or an operator rejected stays where it is.

- **Pull.** A pulled link is asserted here with method `sync`; the method it
  was made by on the peer is kept in `attributes.origin_method` (for a link the
  peer itself pulled, the method it was first made by).
- **Evidence** is matched by the cited chunk's `natural_key` and its quote is
  checked again against this side's text, as `link_chunks` checks it. A row
  that cannot be checked here is dropped and counted in `evidence_dropped`: a
  quote from an unkeyed chunk, a chat turn or another scope, a key this side
  does not hold or may not read, or a quote that does not occur in this side's
  text. Evidence already recorded is not added twice.
- **A link pulled before** is updated in place when the peer's copy changed:
  its `confidence`, `invalid_at` and `attributes` follow the peer, a value the
  peer cleared is cleared, and its evidence is kept (counted in
  `edges_updated`). `valid_at` is part of a link's identity, so a peer link
  with another `valid_at` arrives as a second link beside the first. Once an
  operator here accepts a pulled link, it keeps its envelope like a link
  written here, counted in `edges_envelope_kept`.
- **A link written here** (by you, an import or a rule) keeps its own
  confidence, validity and attributes; only evidence it lacks is added. A
  difference is counted in `edges_envelope_kept`.
- **Push** sends the same envelope and the evidence the peer can check: a
  quote from a keyed chunk of this document whose text the peer now holds.
  The peer checks every quote again. A pushed link arrives on the peer with
  the peer's own `manual` method, its method here in
  `attributes.origin_method`. The peer has no way to change an existing
  link's envelope, so a push adds only the evidence a peer link lacks and
  counts an envelope that differs in `edges_envelope_kept`.

## Arguments

- `document_id` (or `id`), or `path` — one is required: the LOCAL document, as
  bound with `set_remote`.
- `direction` — `pull` (default): the peer's chunks are written into your
  document. `push`: your chunks are written to the peer.
- `scope` — `user` (default), `agent` or `tenant`: the scope of the local
  document, and the scope read on the peer. See
  `{"op":"help","topic":"scopes"}`.

## Returns

`{direction, source, remote_ref, remote_document_id, local_document_id,
created, updated, unchanged, reparented, edges_added, edges_skipped_rejected,
excluded_unkeyed, excluded_withheld}` — counts of keyed chunks. `excluded_unkeyed`
counts the sending side's chunks that could not be carried.
`edges_skipped_rejected` counts the peer's links a pull left out because an
operator rejected that relation here; a sync never undoes a rejection.
Three more counts appear only when they are not zero: `edges_updated`,
`edges_envelope_kept` and `evidence_dropped` (see Links and their evidence).

## Errors

- `sync: this document is not bound to a remote (call set_remote first)`.
- `sync: unknown document source ...` — the source was removed from the
  configuration. Ask the operator.
- `sync: fetch remote document: ...` / `sync: remote document not found at ...`
  — the peer is unreachable, refused the call, or has no document at that
  path. Check `remote_ref`; a retry helps only for a network failure.
- `sync: fetch remote document: ... blocked: <host> has no public addresses` —
  the source was authored at runtime and its peer is on a private network the
  operator has not allowed; nothing was sent. A retry will not help: ask the
  operator to add that hostname to `LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST`.
- `sync: base_url host "<host>" is not in LOOMCYCLE_HTTP_HOST_ALLOWLIST or
  LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST ...` — the source was authored at
  runtime and the operator does not list its host; nothing was sent. A retry
  will not help: ask the operator to list the host, or to declare the source
  in the server config.
- `sync: fetch remote document: api_key_env "$cred:<name>": tenant "<tenant>"
  has no tenant-level credential of that name` — the source names a stored
  credential that the tenant owning the source does not hold; nothing was
  sent. Create it with `CredentialDef` (`scope: tenant`) in that tenant.
- `sync: direction must be "pull" (default) or "push" ...` — `up` and `down`
  belong to `reorder_chunk`.
- The sync is not all-or-nothing: if it fails part-way, what was already
  written stays. Running it again continues from where it stands.

## Examples

Pull the peer's latest keyed chunks into the local runbook:

```json
{"op": "sync", "path": "/docs/runbook"}
```

```json result
{"direction": "pull", "source": "hq-docs", "remote_ref": "/docs/runbook",
 "remote_document_id": "0d9c8b7a6f5e4d3c2b1a09f8e7d6c5b4", "local_document_id": "b4407b522dc11495d3de371311db17f0",
 "created": 2, "updated": 1, "unchanged": 14, "reparented": 2, "edges_added": 1, "edges_skipped_rejected": 0, "excluded_unkeyed": 3, "excluded_withheld": 0}
```

Push local edits up to the peer:

```json
{"op": "sync", "id": "b4407b522dc11495d3de371311db17f0", "direction": "push"}
```
