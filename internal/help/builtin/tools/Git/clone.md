---
name: Git/clone
description: "Git op=clone — copy a remote https repository into an empty or new volume directory, with a stored credential for a private one."
---
`clone` copies a remote repository into the directory named by `repo`, which
must be empty or not exist yet (it is created). If the clone fails, whatever
it wrote is removed, so no half-made repository is left behind. Needs a
read-write volume.

It works only when your agent's `git_remote` allows `fetch`, and only to a
host the operator lists. The URL must be `https://`, with no user, password,
query or fragment in it.

## Arguments

- `volume`, `repo` — where the clone goes (defaults: your default volume, its
  root).
- `url` (required) — the repository's `https://` URL.
- `credential` — `"$cred:<name>"` or `"$ghapp:<name>"` for a private
  repository; omit for a public one. The runtime resolves and sends it; you
  never see its value, and it is not written into the repository.
- `branch` — the remote branch to check out (default: the remote's default
  branch).
- `remote` — the name to give the remote (default `origin`).
- `depth` — a shallow clone with only this many commits of history.
- `single_branch` — fetch only `branch` (or the default branch).
- `no_checkout` — create the repository but leave the files out of the
  directory.

## Returns

`{op: "clone", head: {branch, hash, detached}}`.

## Errors

- `already_exists` — the directory is not empty.
- `invalid_argument` — the URL is not an acceptable `https://` URL, or a name
  or `depth` is invalid.
- `invalid_revision` — the remote has no such `branch`.
- `git_remote_not_allowed`, `git_network_disabled`, `host_not_allowed` — the
  network is closed to you, or to this host.
- `credential_url_not_allowed` — the credential's owner did not allow it for
  this URL. `credential_unresolvable` — it could not be resolved.
- `auth_failed`, `not_found` — the server refused the credential, or has no
  such repository (a private one without a credential can look like this).
- `source_too_large`, `upload_too_large`, `timeout`, `fetch_failed` — the
  repository passed the operator's limits, or the transfer failed. A shallow
  `depth` or `single_branch` makes a large repository smaller.

## Examples

A private repository, with a stored credential:

```json
{"op": "clone", "repo": "service", "url": "https://github.com/acme/service.git", "credential": "$cred:github"}
```

```json result
{"op": "clone", "head": {"branch": "main", "hash": "9c1e4b7a2d3f5e6a8b0c1d2e3f4a5b6c7d8e9f01", "detached": false}}
```

Only the latest commit of one branch of a public repository:

```json
{"op": "clone", "repo": "vendor/lib", "url": "https://github.com/acme/lib.git", "branch": "stable", "depth": 1, "single_branch": true}
```
