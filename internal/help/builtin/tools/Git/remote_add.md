---
name: Git/remote_add
description: "Git op=remote_add — add a named remote with an https URL; whether you may reach it is checked when it is used."
---
`remote_add` adds a remote to the repository's configuration, with git's
default fetch refspec (`refs/heads/*` into `refs/remotes/<remote>/*`). Needs
a read-write volume.

Only an `https://` URL with no user, password, query or fragment is accepted.
Whether its host is one the operator lists is not checked here but on every
`fetch`, `pull` and `push` that uses it.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `remote` (required) — the remote's name, such as `upstream`.
- `url` (required) — the remote's `https://` URL.

## Returns

`{op: "remote_add", ok: true, head, files}` — `head` and `files` as `status`
reports them. `remote_list` shows the new remote.

## Errors

- `already_exists` — a remote of that name exists; use `remote_set_url` to
  change its URL.
- `invalid_argument` — an invalid remote name, or the URL is not an
  acceptable `https://` URL.
- `volume_refused` — the volume is read-only.

## Examples

```json
{"op": "remote_add", "repo": "service", "remote": "upstream", "url": "https://github.com/acme/service.git"}
```
