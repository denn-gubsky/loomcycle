---
name: File/rm
description: "File op=rm — delete one file from the run's inbox and give its space back."
---
`rm` deletes one file from the inbox. The inbox has a size limit for the whole
run; removing a file you are done with makes room for the next one. Only inbox
files can be removed with this tool, and only when the run may change the
inbox (a sub-agent given the run's volumes read-only may not).

## Arguments

- `path` — the inbox file, e.g. `volume:inbox:documents/old.pdf`.

## Returns

`{removed, bytes}`.

## Examples

```json
{"op": "rm", "path": "volume:inbox:documents/old.pdf"}
```

```json result
{"removed": "volume:inbox:documents/old.pdf", "bytes": 52114}
```
