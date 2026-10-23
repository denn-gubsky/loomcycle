---
name: File/clear
description: "File op=clear — empty the run's inbox, or one folder of it, now rather than when the run ends."
---
`clear` deletes every file in the inbox, or in one folder of it. The inbox
itself stays, empty. Keep anything you still need with `keep` first.

## Arguments

- `prefix` — the folder to empty, e.g. `images` or `documents`. Omit to empty
  the whole inbox.

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
