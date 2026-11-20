---
name: File/copy
description: "File op=copy — copy a file or a folder between the run's inbox, your volumes and your workspaces, without the bytes passing through you."
---
`copy` copies a file or a whole folder from one place to another: the inbox,
one of your volumes, or a workspace this run opened. The bytes stream through
the runtime; you see only where they went.

| From | To | Use |
|---|---|---|
| the inbox or a volume | a workspace | hand a file to a script |
| a workspace | a read-write volume, or the inbox | keep a result |
| a workspace | another workspace of this run | split work between two environments |

The inbox and a volume can also be copied into each other; `keep` is the
older form of inbox to volume.

- A `to` ending in `/`, or naming a folder that exists, gets the source's
  name inside it. Otherwise `to` is the new name.
- `overwrite` is off by default: a copy onto an existing file or folder is
  refused. With `overwrite: true` a file is replaced at once, never left half
  written, and a folder is merged into file by file. The inbox never
  replaces a file: a name already taken there gets a numbered variant.
- A folder is copied whole or not at all. It is listed first, and refused
  before anything is written when it holds more files or bytes than the
  operator allows (`workspace.copy.max_files`, `workspace.copy.max_bytes`),
  when a file in it is over the per-file limit, or when the inbox has not
  room for it. A copy into a workspace over its disk quota fails and leaves
  nothing. Links and special files in a folder are not copied; the answer
  names them in `skipped`.
- Two workspaces on the same worker are copied by that worker
  (`same_worker: true`); otherwise the bytes pass through the runtime.

Grants: a workspace needs your `workspace` grant (`read` to copy from it,
`write` to copy into it); a volume needs Read to copy from and Write to copy
into a read-write one; the inbox is the run's own. The operator's hooks
judge the final path, and for a folder every file's own source and target
path too: one file a hook refuses refuses the whole copy, before anything
is written.

## Arguments

- `from` — the file or folder to copy: `workspace:<name>:<path>`,
  `volume:<name>:<path>`, or an inbox reference.
- `to` — where to copy it, in the same forms.
- `overwrite` — replace an existing file, or merge into an existing folder.
  Default false.

## Returns

A file: `{from, ref, size, sha256, media_type}`. A folder: `{from, to,
files, bytes, skipped}`. A copy the worker did: `{from, to, files, bytes,
same_worker: true}`.

## Errors

- `this run has no workspace named …`: open it first; only the run that
  opened a workspace reaches it.
- `… already exists`: pass `overwrite: true`, or choose another name.
- `copy_limit: …` names the bound the folder passes; nothing was copied.
- `workspace_over_quota: …`: the workspace's disk quota; remove files first.

## Examples

Hand a downloaded spreadsheet to a script:

```json
{"op": "copy", "from": "volume:inbox:documents/sales.xlsx", "to": "workspace:build:in/"}
```

```json result
{"from": "volume:inbox:documents/sales.xlsx", "ref": "workspace:build:in/sales.xlsx", "size": 48211,
 "sha256": "3f9a1c2b4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8"}
```

Keep the script's output folder in a volume:

```json
{"op": "copy", "from": "workspace:build:out", "to": "volume:reports:2026/q3/"}
```

```json result
{"from": "workspace:build:out", "to": "volume:reports:2026/q3/out", "files": 4, "bytes": 18230}
```
