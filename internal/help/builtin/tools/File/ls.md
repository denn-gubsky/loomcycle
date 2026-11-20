---
name: File/ls
description: "File op=ls — list the files kept in the run's inbox, or the files in a folder of one of your volumes."
---
`ls` lists a folder. With no `path` it lists the root of the inbox, where the
runtime keeps the files your tools returned. Files still being written are not
listed.

## Arguments

- `path` — the folder: a reference such as `volume:inbox:documents`,
  `volume:docs:board` or `workspace:build:out` (`workspace:build:` is the
  workspace's root, and needs the `read` workspace grant); default the
  inbox's root.
- `recursive` — list the files in every subfolder too (folders themselves are
  then not listed).

## Returns

`{path, entries: [{ref, size, media_type, origin, dir}], truncated}`.
`media_type` and `origin` are given for files the runtime kept. At most 1000
entries are returned; `truncated` says there were more. Before anything has
been kept the inbox does not exist and `entries` is empty, with a `note`.
In a workspace, folders are listed too (`dir: true`), and a link or another
non-regular file carries `type` (`link` or `other`): `copy` skips those.

## Examples

Everything in the inbox:

```json
{"op": "ls", "recursive": true}
```

```json result
{"path": "volume:inbox:.", "entries": [
  {"ref": "volume:inbox:documents/board-papers.pdf", "size": 1887436, "media_type": "application/pdf", "origin": "mcp:ms365/get-attachment"},
  {"ref": "volume:inbox:images/get.png", "size": 48211, "media_type": "image/png", "origin": "mcp:ms365/get-photo"}],
 "truncated": false}
```
