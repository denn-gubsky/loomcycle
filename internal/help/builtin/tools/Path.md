---
name: Path
description: "Path tool — a Unix-like directory tree over your Memory entries, Volumes and Documents. Browse and rename resources by human-readable path with resolve/ls/stat/mkdir/mv/rm."
---
The `Path` tool gives your Memory entries, Volumes and Documents a **Unix-like
directory tree**. Instead of remembering a Memory key, a Volume name or a
Document id, you name the thing `/prefs/voice` or `/docs/launch` and navigate
with familiar operations.

It is a **naming layer, not a store.** Each resource keeps its own id; a path
entry maps a name to it. Resolving a path tells you what is there. Reading or
changing the thing itself still goes through its own tool (Document, Memory,
Read/Write) with that tool's access rules, so a path never grants access you
did not already have.

## When to use it — and when not

- **Use Path** to see what exists (`ls /docs`), to find what a name points at
  (`resolve`), or to reorganise (`mv`, `rm`) without touching content.
- **Do not use Path** to read or write content. Resolve the path, then call the
  Document tool for a document, Memory for a memory entry, or Read/Write for a
  file on a volume.

## Operations

Every operation takes `op`; all but the root listing take `path`. Fetch one
operation's article, with examples, as `Path/<op>` — for example
`{"op":"help","topic":"Path/ls"}`.

| op | What it does | Required besides `op` |
|---|---|---|
| `resolve` | what this path names (kind + reference) | `path` |
| `ls` | list a directory, one level or recursively, pageable | `path` |
| `stat` | the entry's full record, with timestamps | `path` |
| `mkdir` | create an empty directory so it persists and lists | `path` |
| `mv` | rename or move; a directory moves with everything under it | `path`, `to` |
| `rm` | remove the NAME; the resource behind it survives | `path` |

## Scopes

Every path lives in one tree, chosen by `scope`:

- `agent` (the default) — this agent's own tree.
- `user` — the end-user's tree. Needs a user id on the run.
- `tenant` — one tree shared by every user and agent in the tenant.

The same path in two scopes is two different entries, and one call reads only
the scope it names. Trees are tenant-isolated: another tenant's paths are
invisible, and asking for one is an ordinary "no such path". For how scopes
work across every tool, read `{"op":"help","topic":"scopes"}`.

## Path rules

Absolute and slash-rooted (`/docs/launch`). Segments are `[a-zA-Z0-9._-]` —
no spaces, no inner slashes — and `..` is refused, so a path cannot climb out
of its tree. At most 64 segments and 1024 characters.

Directories are implicit: a document at `/a/b/c` makes `/a` and `/a/b` list
as directories without anyone creating them. `mkdir` is only needed to keep an
EMPTY directory.

## How a resource gets a path

Nothing is named automatically, except documents:

- **Document:** `create_document` and `import_md` always register a path
  (`/documents/<title>` when you pass none); `set_path` moves it.
- **Memory:** pass `path` to `Memory op=set` to name the entry; read it back
  with `Memory op=get` and the same `path`.
- **Volume:** `VolumeDef op=create` mounts the volume at `/vol/<name>` (or
  `mount_at`) in the tenant tree.

A resource with no path is still reachable by its own id. Paths are additive.
