---
name: File/stat
description: "File op=stat — the size, media type, sha256 and origin of one file, without its bytes."
---
`stat` describes one file. It always reads the bytes for their digest. For a
file the runtime kept from a tool result, and still unchanged, it returns what
was recorded when it was kept, including which tool returned it. For any
other file (one written since, or outside the inbox) it reports the type the
bytes show and the origin as `unknown`. A volume other than the inbox needs
the Read tool.

## Arguments

- `path` — the file: a reference such as `volume:inbox:documents/a.pdf`, or a
  bare path in the inbox.

## Returns

`{ref, path, media_type, size, sha256, origin, captured_at}` for a kept file;
`{ref, path, media_type, size, sha256, origin: "unknown", modified}` otherwise.

## Errors

- The inbox does not exist yet: nothing has been kept in this run.
- No file at the path: list the folder with `ls`.

## Examples

What the board papers an MCP tool returned are:

```json
{"op": "stat", "path": "volume:inbox:documents/board-papers.pdf"}
```

```json result
{"ref": "volume:inbox:documents/board-papers.pdf", "path": "documents/board-papers.pdf",
 "media_type": "application/pdf", "size": 1887436,
 "sha256": "3f9a1c2b4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8",
 "origin": "mcp:ms365/get-attachment", "captured_at": "2026-10-23T09:12:44Z"}
```
