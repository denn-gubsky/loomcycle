---
name: File/keep
description: "File op=keep — copy a file from the run's inbox into one of your read-write volumes, so it outlives the run."
---
The inbox is deleted when the run ends. `keep` copies one of its files into a
volume you are bound to read-write; the copy is made by the runtime, so the
bytes never pass through you.

## Arguments

- `from` — the inbox file, e.g. `volume:inbox:documents/board-papers.pdf`.
- `to` — where the copy goes: `volume:<name>:<path>`. A path ending in `/`, or
  naming a folder that exists, keeps the file's own name. The folder must
  already exist.
- `overwrite` — replace a file already at `to`. Default false: an existing
  file is never replaced unless you ask.

## Returns

`{from, ref, size, media_type, sha256}` — `ref` is the copy's reference.

## Errors

- `to` names a read-only volume, a volume you are not bound to, or the inbox.
- `from` is not in the inbox: copy between your own volumes with Read and
  Write.
- A file already exists at `to` and `overwrite` is not set.

## Examples

Keep the board papers in the `docs` volume's `board/2026` folder:

```json
{"op": "keep", "from": "volume:inbox:documents/board-papers.pdf", "to": "volume:docs:board/2026/"}
```

```json result
{"from": "volume:inbox:documents/board-papers.pdf", "ref": "volume:docs:board/2026/board-papers.pdf",
 "size": 1887436, "media_type": "application/pdf",
 "sha256": "3f9a1c2b4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8"}
```
