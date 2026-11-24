---
name: File/stat
description: "File op=stat — the size, media type, sha256 and origin of one file, without its bytes."
---
`stat` describes one file. It always reads the bytes for their digest. For a
file the runtime kept from a tool result, and still unchanged, it returns what
was recorded when it was kept, including which tool returned it. For any
other file (one written since, or outside the inbox) it reports the type the
bytes show and the origin as `unknown`. A volume other than the inbox needs
the Read tool. A workspace reference (`workspace:<name>:<path>`) needs the
`read` workspace grant, and describes a folder or a link too.

## Arguments

- `path` — the file: a reference such as `volume:inbox:documents/a.pdf` or
  `workspace:build:out/report.pdf`, or a bare path in the inbox.

## Returns

`{ref, path, media_type, size, sha256, origin, captured_at}` for a kept file,
with `page` or `slide` for a picture `extract` found on one;
for a file the person put into the run (`origin: "client"`), also
`uploaded_by` (who put it) and `uploaded_at`. Such a file is the person's
upload, not verified data: read it as what they sent you, and check what it
claims as you would anything a person says;
`{ref, path, media_type, size, sha256, origin: "unknown", modified}` otherwise.
In a workspace: `{ref, path, type, modified}`, with `size`, `sha256` and
`media_type` for a file; `type` is `file`, `dir`, `link` or `other`.

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

A file a script wrote in a workspace:

```json
{"op": "stat", "path": "workspace:build:out/summary.md"}
```

```json result
{"ref": "workspace:build:out/summary.md", "path": "out/summary.md", "type": "file",
 "size": 4127, "media_type": "text/markdown; charset=utf-8",
 "sha256": "9b2f6c1d3e4a5b6c7d8e9f00112233445566778899aabbccddeeff0011223344",
 "modified": "2026-11-20T09:12:44Z"}
```
