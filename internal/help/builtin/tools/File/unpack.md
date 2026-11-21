---
name: File/unpack
description: "File op=unpack — copy the files of a ZIP or RAR archive into the inbox (or a read-write volume folder), keeping its folders, so each can be extracted on its own."
---
`unpack` lists a ZIP or RAR archive the runtime kept (or one in a volume you
can Read) and copies its files into `volume:inbox:unpacked/<archive>/`, its
folders kept. The runtime does the copying; the bytes never pass through you.
Nothing is extracted: run `extract` on each file you want afterwards, under
its own bounds.

The archive is recognised by its bytes, not its name. ZIP and RAR (versions 4
and 5) are read; 7z, tar, gzip and the rest are refused.

## Arguments

- `path` — the archive, e.g. `volume:inbox:documents/library.zip`.
- `to` — unpack into an existing folder of one of your read-write volumes
  instead, e.g. `volume:books:library/`. This needs the Write tool, and
  Write's hooks are asked about every file written. For an archive in a
  workspace, `to` may also be a folder of a workspace (see below).
- `all` — write every regular file. Default false: only the formats `extract`
  reads (documents, books, DjVu, images, captions, text), plus archives inside
  the archive. A recording is not among them; `all` writes it.
- `dry_run` — list what would be written and skipped, and why, writing
  nothing.

## What is written, and what is not

- An archive inside the archive is written as a file and listed, never
  opened: unpack it in its turn.
- Each name is sanitised as an inbox name is (Cyrillic transliterated, other
  characters `-`), folder by folder, at most 8 levels deep.
- Skipped, with the reason: an absolute path, a name with `..`, a hidden file
  or folder (a name starting with `.`), a name with nothing left once
  sanitised, a symbolic link, a device or other non-file, an encrypted ZIP
  entry. Folder entries are only counted.
- Nothing is overwritten: a name already taken gets `-2`, `-3`, ….
- Each file written into the inbox gets a capture record whose origin is
  `unpack:<archive>#<entry>`, so `stat`, `extract` and `import` say where it
  came from.

## Bounds

Sizes an archive declares are never trusted; the bytes are counted as they
are inflated.

- At most 10,000 entries listed and 2,000 files written.
- Each file at most the inbox's per-file limit (64 MiB unless the operator set
  another); a file that declares more is skipped.
- 1 GiB inflated in all, and never past the inbox's free space.
- No entry may expand more than 100 times its compressed size past its first
  MiB; one that declares that is skipped.
- A solid RAR decompresses every entry in order, so a skipped entry costs its
  decompression, which counts against the same 1 GiB.

When a bound is reached while inflating, unpacking stops: what was written
stays and is reported, and `stopped` names the bound.

A RAR that is encrypted, has encrypted headers, or is one volume of several
is refused, as is a RAR whose dictionary is over 64 MiB.

## An archive in a workspace

`path` may name an archive in a workspace of this run
(`workspace:<name>:<path>`, the workspace grant `read`). It is unpacked on
that workspace's worker, in its file converter, under the same rules and
bounds; the archive never leaves the worker.

- With no `to`, or a volume folder as `to`, the files go to the inbox or the
  volume as above.
- With `to` a folder of a workspace (`workspace:<name>:<folder>`, the grant
  `write`), the archive goes in whole or not at all: the folder is made
  holding the files (`overwrite: true` merges them into one already there),
  a bound reached while inflating writes nothing, the workspace's disk quota
  counts every file, and the operator's `Workspace` hooks are asked about
  every file's final path before any is placed, one refusal refusing the
  unpack. Into a workspace on the same worker the files never leave it.
- An archive that crashes the converter is refused in that workspace for a
  while.

## Returns

`{archive, format, to, written: [{ref, name, size, media_type}], skipped:
[{name, reason}], counts: {listed, written, skipped, folders, bytes_written,
bytes_inflated}, stopped?, notes?}`. A dry run returns `would_write` in place
of `written`, each `ref` where the file would go before any `-2` numbering.
At most 500 skipped entries are named (`skipped_truncated` says more were);
the counts are complete.

## Errors

- The file is not a ZIP or RAR archive, or a RAR is encrypted or multi-volume.
- Every extraction slot is busy: try again in a minute.
- `to` is not a folder of a read-write volume you are bound to, or you do not
  hold Write; or, into a workspace, the folder exists (pass `overwrite`), the
  quota would be passed, a bound was reached, or a hook refused a file.
- The archive is outside the inbox and you do not hold Read.

## Examples

See what a library archive holds without writing anything:

```json
{"op": "unpack", "path": "volume:inbox:documents/library.zip", "dry_run": true}
```

Unpack it:

```json
{"op": "unpack", "path": "volume:inbox:documents/library.zip"}
```

```json result
{"archive": "volume:inbox:documents/library.zip", "format": "zip", "to": "volume:inbox:unpacked/library/",
 "written": [{"ref": "volume:inbox:unpacked/library/fiction/Kobzar.fb2", "name": "fiction/Кобзар.fb2", "size": 412331, "media_type": "application/x-fictionbook+xml"}],
 "skipped": [{"name": "setup.exe", "reason": "not a format extract reads (application/octet-stream); pass all: true to write every file"}],
 "counts": {"listed": 3, "written": 1, "skipped": 1, "folders": 1, "bytes_written": 412331, "bytes_inflated": 416427}}
```

Then extract a book with `{"op": "extract", "path": "volume:inbox:unpacked/library/fiction/Kobzar.fb2"}`.
