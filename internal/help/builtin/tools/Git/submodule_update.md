---
name: Git/submodule_update
description: "Git op=submodule_update — clone or fetch one submodule and check it out, detached, at the commit HEAD records for it."
---
`submodule_update` makes one submodule's directory hold its repository at the
commit HEAD records for it. An empty or missing directory is cloned into; an
existing submodule repository fetches the commit when it lacks it. The
checkout is detached. Nested submodules are not updated: the update is not
recursive. Needs a read-write volume.

It is a network operation: it works only when your agent's `git_remote`
allows `fetch`, and only to a host the operator lists. The URL is the one
`submodule_list` shows (from `.gitmodules`, a relative one resolved against
`origin`), checked on every call; `url` is not accepted here.

The submodule is a repository of its own inside its directory, with its own
`.git` directory. Run other ops on it by setting `repo` to its path.

## Arguments

- `volume`, `repo` — the superproject (defaults: your default volume, its
  root).
- `submodule` (required) — the submodule's name, as `submodule_list` gives
  it.
- `credential` — `"$cred:<name>"` or `"$ghapp:<name>"` for a private
  repository; omit for a public one.

## Returns

`{op: "submodule_update", submodule: {name, path, url, commit,
initialized}}` — as `submodule_list` reports it after the update.

## Errors

- `invalid_argument` — no submodule of that name; it has no URL that can be
  resolved; HEAD records no commit for it; its path passes through a symlink
  or a file, or holds a `.git` that is not a directory; or the URL is not an
  acceptable `https://` URL.
- `already_exists` — its directory holds files but no repository.
- `worktree_dirty` — the submodule has uncommitted changes; commit or
  `restore` them there first.
- `commit_not_found` — the remote does not have the recorded commit.
- `not_a_repository` — no `.git` directory at `repo`.
- `volume_refused` — the volume is read-only.
- `git_remote_not_allowed`, `git_network_disabled`, `host_not_allowed`,
  `credential_url_not_allowed`, `credential_unresolvable`, `auth_failed`,
  `not_found`, `source_too_large`, `upload_too_large`, `timeout`,
  `fetch_failed` — as for `clone`.

## Examples

```json
{"op": "submodule_update", "repo": "service", "submodule": "vendor/lib", "credential": "$cred:github"}
```

```json result
{"op": "submodule_update", "submodule": {"name": "vendor/lib", "path": "vendor/lib",
 "url": "https://github.com/acme/lib.git", "commit": "0e1d2c3b4a5f6e7d8c9b0a1f2e3d4c5b6a7f8e9d", "initialized": true}}
```
