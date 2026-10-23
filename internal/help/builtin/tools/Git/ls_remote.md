---
name: Git/ls_remote
description: "Git op=ls_remote — list a remote repository's branches and tags by URL, without a local repository."
---
`ls_remote` asks a remote which refs it has. It needs no repository and no
volume, so it is the way to check a URL, a credential or a branch name
before you `clone`.

It works only when your agent's `git_remote` allows `fetch`, and only to a
host the operator lists. The URL must be `https://`.

## Arguments

- `url` (required) — the repository's `https://` URL.
- `credential` — `"$cred:<name>"` or `"$ghapp:<name>"` for a private
  repository; omit for a public one.

## Returns

`{op: "ls_remote", refs: [{name, hash}]}`, sorted by name, with full ref
names (`refs/heads/main`, `refs/tags/v1.4.0`). `HEAD` carries the hash of the
branch it names. An empty repository has no refs.

## Errors

- `invalid_argument` — the URL is not an acceptable `https://` URL.
- `git_remote_not_allowed`, `git_network_disabled`, `host_not_allowed`,
  `credential_url_not_allowed`, `credential_unresolvable`, `auth_failed`,
  `not_found`, `timeout`, `fetch_failed` — as for `clone`.

## Examples

```json
{"op": "ls_remote", "url": "https://github.com/acme/service.git", "credential": "$cred:github"}
```

```json result
{"op": "ls_remote", "refs": [
  {"name": "HEAD", "hash": "9c1e4b7a2d3f5e6a8b0c1d2e3f4a5b6c7d8e9f01"},
  {"name": "refs/heads/main", "hash": "9c1e4b7a2d3f5e6a8b0c1d2e3f4a5b6c7d8e9f01"},
  {"name": "refs/tags/v1.4.0", "hash": "4b2d8e1f0a9c7b6d5e4f3a2b1c0d9e8f7a6b5c4d"}]}
```
