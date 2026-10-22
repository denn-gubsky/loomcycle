---
name: Git/fetch
description: "Git op=fetch — download new commits from a remote and update its remote-tracking branches; never moves your checked-out branch."
---
`fetch` downloads what a remote has that the repository lacks and updates the
remote-tracking branches (`origin/main` and so on). Your branches and files
do not change: follow it with `merge` (or use `pull`) to move your branch.
Needs a read-write volume.

It works only when your agent's `git_remote` allows `fetch`, and only to a
host the operator lists. The URL is the remote's, as `remote_list` shows it,
and is checked on every call; `url` is not accepted here.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `remote` — the remote's name (default `origin`).
- `credential` — `"$cred:<name>"` or `"$ghapp:<name>"` for a private
  repository; omit for a public one.
- `refspecs` — what to fetch. Default: the remote's configured refspecs. A
  bare branch name like `"release"` fetches that branch into
  `origin/release`. A full refspec is `[+]refs/...:refs/...`, with at most
  one `*` on each side. A refspec that would write your checked-out branch is
  refused.
- `tags` — also fetch every tag (by default only tags pointing at fetched
  commits come along).
- `prune` — delete remote-tracking branches the remote no longer has.
- `depth` — limit the history fetched to this many commits.

## Returns

`{op: "fetch", up_to_date, updated: [{name, old, new}]}`. Each entry in
`updated` is a ref that moved; an empty `old` is a ref created, an empty
`new` one deleted.

## Errors

- `invalid_argument` — no such remote, a refspec is invalid or writes the
  checked-out branch, or `url` was given.
- `non_fast_forward` — a ref the remote moved is not a fast-forward of the
  local one; prefix the refspec with `+` to overwrite it.
- `invalid_revision` — the remote has no ref the refspecs name.
- `git_remote_not_allowed`, `git_network_disabled`, `host_not_allowed`,
  `credential_url_not_allowed`, `credential_unresolvable`, `auth_failed`,
  `not_found`, `source_too_large`, `upload_too_large`, `timeout`,
  `fetch_failed` — as for `clone`.

## Examples

Fetch everything from `origin`:

```json
{"op": "fetch", "repo": "service", "credential": "$cred:github"}
```

```json result
{"op": "fetch", "up_to_date": false, "updated": [
  {"name": "refs/remotes/origin/main", "old": "9c1e4b7a2d3f5e6a8b0c1d2e3f4a5b6c7d8e9f01", "new": "5d4c3b2a1f0e9d8c7b6a5f4e3d2c1b0a9f8e7d6c"}]}
```

One branch, plus all tags, dropping branches deleted on the remote:

```json
{"op": "fetch", "repo": "service", "credential": "$cred:github", "refspecs": ["release"], "tags": true, "prune": true}
```
