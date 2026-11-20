---
name: File/rm
description: "File op=rm — delete one file from the run's inbox, or a file or folder of a workspace, and give its space back."
---
`rm` deletes one file from the inbox. The inbox has a size limit for the whole
run; removing a file you are done with makes room for the next one. Only inbox
files can be removed with this tool, and only when the run may change the
inbox (a sub-agent given the run's volumes read-only may not).

In a workspace this run opened, `rm` removes a file or a folder with what it
holds, and gives the space back to the workspace's disk quota. It needs the
`write` workspace grant, and it never removes the workspace's root: release
the workspace with `Workspace op=release`, or empty it with `clear`.

## Arguments

- `path` — the inbox file, e.g. `volume:inbox:documents/old.pdf`, or a file
  or folder of a workspace, e.g. `workspace:build:tmp`.

## Returns

`{removed, bytes}`; for a workspace `{removed, files, bytes}`.

## Examples

```json
{"op": "rm", "path": "volume:inbox:documents/old.pdf"}
```

```json result
{"removed": "volume:inbox:documents/old.pdf", "bytes": 52114}
```

```json
{"op": "rm", "path": "workspace:build:tmp"}
```

```json result
{"removed": "workspace:build:tmp", "files": 12, "bytes": 803311}
```
