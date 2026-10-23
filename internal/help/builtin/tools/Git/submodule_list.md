---
name: Git/submodule_list
description: "Git op=submodule_list — the submodules HEAD declares, the commit recorded for each, and whether it is checked out there."
---
`submodule_list` lists the submodules named in HEAD's `.gitmodules`, sorted
by name, with the commit HEAD records for each. It changes nothing and works
on a read-only volume. Fetch one with `submodule_update`.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).

## Returns

`{op: "submodule_list", submodules: [{name, path, url, commit,
initialized}]}`.

- `name` is what `submodule_update` takes.
- `url` is the one `.gitmodules` gives; a relative one (`../lib.git`) is
  resolved against the `origin` remote's URL. It is empty when that cannot be
  done, and such a submodule cannot be updated.
- `commit` is the commit HEAD records at `path`, empty if none.
- `initialized` is `true` when `path` holds a repository checked out at
  `commit`.

A repository with no `.gitmodules`, or no commit yet, returns an empty list.

## Errors

- `invalid_argument` — `.gitmodules` is over 1 MiB or does not parse.
- `not_a_repository` — no `.git` directory at `repo`.

## Examples

```json
{"op": "submodule_list", "repo": "service"}
```

```json result
{"op": "submodule_list", "submodules": [
  {"name": "vendor/lib", "path": "vendor/lib", "url": "https://github.com/acme/lib.git",
   "commit": "0e1d2c3b4a5f6e7d8c9b0a1f2e3d4c5b6a7f8e9d", "initialized": false}]}
```
