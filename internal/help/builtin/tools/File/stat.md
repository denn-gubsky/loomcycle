---
name: File/stat
description: "File op=stat — the size, media type, sha256 and origin of one file, without its bytes."
---
`stat` describes one file. For a file the runtime kept from a tool result it
returns what was recorded when it was kept, including which tool returned it.
For any other file it reads the bytes to compute the digest and type, and
reports the origin as `unknown`.

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
