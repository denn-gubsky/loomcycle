---
name: File
description: "File tool — the files a run's tools returned, kept in the run's inbox volume: inspect them, list them, copy files and folders between the inbox, your volumes and your workspaces, remove them; download a URL into it and upload a file to a URL."
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
| `stat` | size, type, sha256 and where one file came from (also in a workspace) | `path` |
| `ls` | the files in the inbox, or in a folder of one of your volumes or workspaces | `path`, `recursive` |
| `copy` | copy a file or a folder between the inbox, your volumes and your workspaces | `from`, `to`, `overwrite` |
| `keep` | copy an inbox file into one of your read-write volumes | `from`, `to`, `overwrite` |
| `rm` | delete one inbox file, or a file or folder of a workspace | `path` |
| `clear` | empty the inbox or one folder of it (`prefix`), or a folder of a workspace (`path`) | `prefix`, `path` |
| `download` | GET a URL into the inbox, or into one of your volumes | `url`, `to`, `headers`, `max_bytes` |
| `upload` | send a file to a URL, whole or in ranges for an upload session | `from`, `url`, `method`, `ranged`, `chunk_bytes` |
| `extract` | turn a DOCX, DOC, ODT, XLSX, PPTX, PDF, HTML, EPUB, FB2, MOBI, AZW3, CHM, RTF, DjVu, CSV, text, VTT/SRT caption or image file into Markdown in the inbox, its pictures kept as files; a recording is transcribed | `path`, `images`, `max_rows`, `excerpt_chars`, `pages`, `max_chars`, `structure` |
| `transcribe` | turn an audio or video recording into a timed Markdown transcript in the inbox with the speech-to-text model | `path`, `language`, `vocabulary`, `diarize`, `timestamps`, `dry_run` |
| `import` | build a Document from a Markdown file, the pictures `extract` kept attached as image chunks | `path`, `title`, `doc_path`, `scope`, `tags` |
| `unpack` | copy the files of a ZIP or RAR archive into `volume:inbox:unpacked/<archive>/` (or a volume folder), keeping its folders; by default only what `extract` reads and archives inside it | `path`, `to`, `all`, `dry_run` |

A path is a reference: `volume:inbox:<path>`, `volume:<name>:<path>` for a
volume you are bound to, `volume:<path>` for your default volume, or
`workspace:<name>:<path>` for a file or folder in a workspace this run opened
(`workspace:<name>:` is its root). A bare path, with no `volume:` or
`workspace:`, names a file in the inbox.

The inbox needs nothing but this tool. Another volume is reached with your own
grants: `keep` (and `unpack` with `to`) writes only when you also hold
Write, and `stat` or `ls` of a volume other than the inbox (or an `extract` or
`unpack` of a file there) needs Read; both tools' hooks apply. `download`
into a volume needs Write and `upload` from one needs Read in the same way;
both are HTTP requests, so they need the HTTP tool as well (its hooks judge
them) and reach only hosts on its allowlist.

## Workspaces

A workspace (the `Workspace` tool) is a directory on a workspace worker that
belongs to the run that opened it. `stat`, `ls`, `copy`, `rm` and `clear` take
a workspace reference; the bytes stream through the runtime and never through
you. The other ops do not take one yet: `copy` the file into the inbox first.

- A workspace reference needs your agent's `workspace` grant: `read` to read
  from a workspace, `write` to copy into it or remove from it. You need this
  tool; the `Workspace` tool and the Read and Write grants are not involved.
- The operator's `Workspace` hooks judge each use as a `Workspace` call
  `{op: read|write|remove, name, path, via: "File"}`, on the final path.
- Only the run that opened a workspace reaches it; any other run, and a
  call from outside a run, gets "not found" or a refusal.
- `rm` and `clear` never remove the workspace's root: release the workspace
  with `Workspace op=release`.

Do not use this tool to read a file's text: Read a text file, and do not ask
for a file's base64.

## From outside a run

An operator or a service reaches a run's inbox with the `file` MCP tool. It
takes the File tool's input plus `run_id`, and offers `stat`, `ls`,
`extract`, `import`, `keep`, `rm` and `clear` — not `download` or `upload`,
which need a run's HTTP allowlist. It works on any run the caller may read;
another tenant's run is not found. The writing ops (`extract`, `keep`, `rm`,
`clear`) need the run's owner or an admin, and a tenant operator counts as an
admin for the runs of its own tenant.

Over HTTP the same ops are `POST /v1/runs/{run_id}/file` (the tool's input
without `run_id`), and `GET /v1/runs/{run_id}/inbox/<path>` saves one file's
bytes, for anyone who may read the run. The download is always an attachment;
a type a browser would run (HTML, SVG, scripts) is sent as
`application/octet-stream`. `X-Loomcycle-SHA256` is the hash of the bytes
sent, and `X-Loomcycle-Origin` the tool that returned the file — `unknown`
when the file changed after it was kept.
