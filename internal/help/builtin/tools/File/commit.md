---
name: File/commit
description: "File op=commit — bring a checkout's folder back: a dry run (the default) reports each file, and a real commit imports changed files as new editions and new files as new Documents, never deleting one."
---
`commit` compares the folder `File op=checkout` wrote with its manifest and
the store, and reports each file:

| State | Meaning | A real commit |
|---|---|---|
| `unchanged` | the file is as it was written | nothing |
| `changed` | the file was changed (`lines_added`, `lines_removed`) | a new edition of its document |
| `new` | a `.md` file the checkout did not write | a new document at the Path its place maps to |
| `missing` | the file is gone | nothing: a commit never deletes a document |
| `conflict` | the document changed in the store after the checkout, is no longer named at the file's path, or (for a new file) a document is already named there | nothing: refused and listed, the rest go through |
| `ignored` | not a `.md` file, a name that maps to no Path name, not UTF-8, or a new file in a checkout of named documents | nothing |

`dry_run` is **true unless you pass `dry_run: false`**: read the report
first, then commit.

- **A changed file** is imported as a new edition of its document
  (`Document import_md` with `reimport`): parts whose text did not change
  keep their ids and relations, changed parts are superseded (the old text
  stays in the document's history), parts the file no longer has are
  closed, nothing is deleted.
- **A new file** `docs/sub/name.md` in a checkout of `/kb` becomes a document
  at `/kb/sub/name`; its first `#` heading is its title.
- **What comes back is a script's text.** The `<!-- loom: … -->` lines and
  the relations block are dropped before the import: ids, types and
  relations come from the store, never from the file. The rest is
  neutralised as every import is (raw HTML is escaped), and a picture is
  attached only as a `data:` URL, never read from a volume or a workspace.
  Each new edition's provenance names the workspace, the run and the
  checkout, and the document is tagged `source/workspace` and `untrusted`.
- **The manifest chooses nothing.** The runtime keeps its own record of the
  checkout and commits by it: a manifest whose scope, root or files were
  changed is refused whole, and so is a checkout the runtime holds no
  record of (taken before a restart: check out again). A file is committed
  onto the document the store names at the file's Path, which must be the
  recorded one; a new file only to a Path no document is named at, under
  the checkout's `under_path` (a checkout of named `documents` takes no new
  file).
- **After a real commit** the manifest is updated for what was committed, so
  the next commit finds those files unchanged.

Grants: the Document tool, with the scope's write grants (for the tenant
scope, `tenant` in both `memory_scopes` and `sql_scopes`); this tool; your
`workspace` grant `read`, and `write` for a real commit (it updates the
manifest). The Workspace hooks judge every file read, and the Document
hooks every Document call (`via: "File"`): the exports and the path lookup
it compares with, and for each file it imports the `import_md`
(`{scope, document_id | path, reimport?}`), its `add_tags` and the
`export_md` that records it, all before the first write: one refusal
refuses the whole commit.

## Arguments

- `from` — the checkout's folder, `workspace:<name>:<folder>/`.
- `dry_run` — report only. Default true.

## Returns

`{from, checkout_id, scope, root, dry_run, counts, files}`: `counts` per
state, and each file as `{file, status, document_id?, path?, lines_added?,
lines_removed?, reason?}`. After a real commit a file imported is
`committed` (with `edition`, the reimport's report, or `new`) or `failed`
with the reason.

## Errors

- `no checkout manifest at …`: pass the folder a checkout wrote.
- `… is not a checkout manifest this runtime wrote`, `… no longer matches
  the runtime's record`, `this runtime holds no record of checkout …`:
  nothing was written; check out again.
- `refused by a Document hook` / `refused by a Workspace hook`: nothing was
  imported.

## Examples

See what a script changed:

```json
{"op": "commit", "from": "workspace:work:policies/"}
```

```json result
{"from": "workspace:work:policies", "checkout_id": "5f0c9a21d4e3b687", "scope": "tenant", "root": "/kb/policies",
 "dry_run": true, "counts": {"unchanged": 39, "changed": 2, "new": 1},
 "files": [{"file": "travel.md", "status": "changed", "document_id": "9b1e0c7a2f4d6e8091a3b5c7d9e1f203", "path": "/kb/policies/travel", "lines_added": 3, "lines_removed": 1}]}
```

Commit it:

```json
{"op": "commit", "from": "workspace:work:policies/", "dry_run": false}
```
