---
name: File/checkout
description: "File op=checkout — write a set of Documents into a workspace folder as Markdown files laid out as the Path tree is, for a script to change and File op=commit to bring back."
---
`checkout` writes Documents into a new folder of a workspace this run
opened, one Markdown file per document, so a script (or you, with the
Workspace file ops) can change many of them at once. `commit` brings the
folder back. A bulk edit of a knowledge base becomes a script and a dry run.

- **Which documents.** Every document you may read named under a Path
  folder (`under_path`), or the documents you name by id (`documents`). A
  document you may not read is not written, and one you named but may not
  read is listed in `left_out` as if it did not exist.
- **Where.** Each document is written at its Path name relative to
  `under_path` (or to `/` for `documents`), as `<name>.md`:
  `/kb/policies/travel` checked out under `/kb` is `policies/travel.md`. A
  document named more than once is written once, at its first name.
- **What.** The round-trip form `Document op=export_md` gives, with each
  part's id, type and status in a `<!-- loom: … -->` line under its heading
  and the relations in a `<!-- loom-edges: … -->` block at the end. Only
  the parts in force are written: a part a later edition closed is not.
- **The manifest.** `.loom/checkout.json` in the folder lists each file's
  document id, the revision the document was taken at (a fingerprint of its
  text) and the sha256 of the file as written. `commit` compares against it.
- **All or nothing.** The folder appears whole or not at all, and it must
  not exist yet.
- **Bounds.** At most `workspace.checkout.max_documents` documents (default
  500) and `workspace.checkout.max_bytes` bytes (default 256 MiB), both
  operator settings. What passes a bound is left out and listed with the
  bound named; so is a document with no Path name to write it at, and one
  whose file would be over the 32 MiB a commit reads back.

Grants: the Document tool, with the scope's grants as `export_md` needs
them; this tool; and your `workspace` grant `write`. The operator's
Document hooks judge each document as an `export_md` (`via: "File"`), and
the Workspace hooks each file's path: one refusal refuses the whole
checkout, before anything is written.

## Arguments

- `under_path` — the Path folder whose documents to take, e.g. `/kb`.
- `documents` — instead of `under_path`: the ids of the documents to take.
- `scope` — the document store: `agent`, `user` (default) or `tenant`.
- `to` — the new folder, `workspace:<name>:<folder>/`.

## Returns

`{to, checkout_id, scope, root, files, bytes, manifest, left_out}`; each
`left_out` entry is `{document_id, path?, reason}`.

## Errors

- `… already exists; nothing was written`: check out into a new folder.
- `pass exactly one of under_path and documents`.
- `refused by a Document hook` / `refused by a Workspace hook`: nothing was
  written.

## Examples

Take a knowledge base's policies out for a script:

```json
{"op": "checkout", "under_path": "/kb/policies", "scope": "tenant", "to": "workspace:work:policies/"}
```

```json result
{"to": "workspace:work:policies", "checkout_id": "5f0c9a21d4e3b687", "scope": "tenant", "root": "/kb/policies",
 "files": 42, "bytes": 318204, "manifest": "workspace:work:policies/.loom/checkout.json", "left_out": []}
```
