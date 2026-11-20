---
name: File/clear
description: "File op=clear — empty the run's inbox, or one folder of it, now rather than when the run ends; or empty a folder of a workspace."
---
`clear` deletes every file in the inbox, or in one folder of it. The inbox
itself stays, empty. Keep anything you still need with `keep` first.

With `path`, `clear` empties a folder of a workspace this run opened
(`workspace:<name>:` is its root) and keeps the folder. It needs the `write`
workspace grant.

## Arguments

- `prefix` — the folder of the inbox to empty, e.g. `images` or `documents`.
  Omit to empty the whole inbox.
- `path` — instead, a folder of a workspace to empty, e.g.
  `workspace:build:out`.

## Returns

`{cleared, files, bytes}` — how many files went, and their total size.

## Examples

Drop the images, keep the documents:

```json
{"op": "clear", "prefix": "images"}
```

```json result
{"cleared": "volume:inbox:images", "files": 3, "bytes": 412337}
```

Empty a workspace's output folder before a new pass:

```json
{"op": "clear", "path": "workspace:build:out"}
```

```json result
{"cleared": "workspace:build:out", "files": 4, "bytes": 18230}
```
