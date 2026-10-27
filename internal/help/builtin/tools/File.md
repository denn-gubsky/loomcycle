---
name: File
description: "File tool — the files a run's tools returned, kept in the run's inbox volume: inspect them, list them, copy one into a durable volume, remove them; download a URL into it and upload a file to a URL."
---
A tool call is text, so a file cannot reach you as one: base64 is too long to
hold, too easy to corrupt, and unreadable anyway. When a tool returns a file —
an MCP server's attachment or image, a binary HTTP response — the runtime keeps
it in your run's **inbox** volume and gives you one line in its place:

```
[file: volume:inbox:documents/board-papers.pdf · application/pdf · 1.8 MiB · sha256:3f9a1c2b4d5e… · from mcp:ms365/get-attachment]
```

`volume:inbox:documents/board-papers.pdf` is the file's reference. The path is
relative to the volume's root, as every file tool's is: Read takes it as
`volume: "inbox"`, `path: "documents/board-papers.pdf"`, and Crypto's `from`
and `to` take the reference as written.

When a file could not be kept, the line says why instead
(`[file not kept: … ]`); its bytes are never handed to you either way.

## The inbox

- It is made the first time a file is kept, and holds `documents/`, `images/`
  and later other folders.
- It belongs to the whole run: sub-agents you start see it too.
- **It is deleted when the run ends.** Copy what must outlive the run into one
  of your own volumes with `keep`.
- It has a size limit for each file and for the run as a whole; `rm` and
  `clear` give space back.

## Operations

| op | What it does | Main arguments |
|---|---|---|
| `stat` | size, type, sha256 and where one file came from | `path` |
| `ls` | the files in the inbox, or in a folder of one of your volumes | `path`, `recursive` |
| `keep` | copy an inbox file into one of your read-write volumes | `from`, `to`, `overwrite` |
| `rm` | delete one inbox file | `path` |
| `clear` | empty the inbox, or one folder of it | `prefix` |
| `download` | GET a URL into the inbox, or into one of your volumes | `url`, `to`, `headers`, `max_bytes` |
| `upload` | send a file to a URL, whole or in ranges for an upload session | `from`, `url`, `method`, `ranged`, `chunk_bytes` |
| `extract` | turn a DOCX, XLSX, PPTX, PDF, HTML, CSV, text, VTT/SRT caption or image file into Markdown in the inbox, its pictures kept as files; a recording is transcribed | `path`, `images`, `max_rows`, `excerpt_chars` |
| `transcribe` | turn an audio or video recording into a timed Markdown transcript in the inbox with the speech-to-text model | `path`, `language`, `vocabulary`, `diarize`, `timestamps`, `dry_run` |
| `import` | build a Document from a Markdown file, the pictures `extract` kept attached as image chunks | `path`, `title`, `doc_path`, `scope`, `tags` |

A path is a reference: `volume:inbox:<path>`, `volume:<name>:<path>` for a
volume you are bound to, or `volume:<path>` for your default volume. A bare
path, with no `volume:`, names a file in the inbox.

The inbox needs nothing but this tool. Another volume is reached with your own
grants: `keep` writes only when you also hold Write, and `stat` or `ls` of a
volume other than the inbox needs Read; both tools' hooks apply. `download`
into a volume needs Write and `upload` from one needs Read in the same way;
both are HTTP requests, so they need the HTTP tool as well (its hooks judge
them) and reach only hosts on its allowlist.

Do not use this tool to read a file's text: Read a text file, and do not ask
for a file's base64.
