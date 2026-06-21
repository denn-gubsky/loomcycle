---
name: path
description: Path — a Unix-like filesystem (VFS) over your Memory, Volumes, and Documents. Address resources by human-readable, scoped, tenant-isolated paths with resolve/ls/stat/mkdir/mv/rm (RFC AL).
---
The `Path` tool gives your Memory entries, Volumes, and Documents a **Unix-like
directory tree** — so instead of remembering a Memory key or a Volume name, you
can say `/prefs/voice` or `/vol/repo-a` and navigate with familiar operations.

It is a **naming layer**, not a new store: each resource keeps its native id
(Memory key, Volume name, Document UUID); a *dirent* maps a path to it (Linux
inode/dirent separation). Resolving a path tells you what's there — using the
resource still goes through its own tool (Memory / Volume / Document) with its
own access rules. A path can't grant access you didn't already have.

## Scopes

Every path lives in a tree chosen by `scope`:
- `agent` (default) — your own tree.
- `user` — the end-user's tree (requires a `user_id` on the run).
- `tenant` — shared across the tenant.

Trees are **tenant-isolated**: you never see another tenant's paths (a
cross-tenant lookup is an opaque "no such path"). The same path string in two
scopes is two different entries.

## Ops

- `resolve path [scope]` — what's at this path? Returns `{kind, resource_ref,
  full_path}`, or an error if absent.
- `ls path [recursive] [kind_filter] [scope]` — list a directory. One level by
  default; `recursive:true` lists all descendants; `kind_filter` narrows to one
  kind (`document` / `volume_mount` / `memory_entry`).
- `stat path [scope]` — the dirent's full record.
- `mkdir path [scope]` — **no-op in v1**: directories are implicit (a leaf at
  `/a/b/c` implies `/a/` and `/a/b/`). Kept for forward-compat.
- `mv from to [scope]` — rename/relocate; the underlying resource is unchanged.
  Refuses if `to` already exists (no clobber). Moving a directory cascades to
  every descendant atomically.
- `rm path [recursive] [scope]` — remove the path entry. Refuses a path that has
  descendants unless `recursive:true` (Linux semantics). Removes the **name**
  only — the backing resource stays reachable by its native id. (`resource_too`
  is **not supported in v1**; delete the resource via its own tool.)

## Path rules

Absolute, slash-rooted (`/docs/launch`). Segments are
`[a-zA-Z0-9._-]` (no spaces, no inner slashes); `..` is **rejected** (a path
can't climb out of its tree). Max 64 segments, 1024 chars.

## Registering paths

Resources opt in to a path — nothing is auto-named:
- **Memory:** `Memory op=set … path:/prefs/voice` registers the entry; later
  `Memory op=get scope:… path:/prefs/voice` reads it by path.
- **Volume:** `VolumeDef op=create name:repo-a mode:rw [mount_at:/vol/repo-a]`
  mounts the volume in the tenant tree (default `/vol/<name>`).
- **Document** (when shipped): `Document op=create_document … path:/docs/plan`.

A resource created without a `path` / `mount_at` simply has no dirent and is
reached by its native id as before — the path layer is purely additive.
