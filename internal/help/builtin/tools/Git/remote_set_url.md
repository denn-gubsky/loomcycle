---
name: Git/remote_set_url
description: "Git op=remote_set_url — point an existing remote at a new https URL, replacing every URL it had."
---
`remote_set_url` changes where a remote points. Every URL the remote had,
push URLs included, is replaced by `url`. The remote's fetch refspecs are
kept; other settings of that remote (such as `tagopt` or `prune`) are
dropped. Its remote-tracking branches are not touched. Needs a read-write
volume.

Only an `https://` URL with no user, password, query or fragment is accepted.
Whether its host is one the operator lists is checked on every `fetch`,
`pull` and `push` that uses it.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `remote` (required) — the remote's name.
- `url` (required) — the new `https://` URL.

## Returns

`{op: "remote_set_url", ok: true, head, files}` — `head` and `files` as
`status` reports them. `remote_list` shows the new URL.

## Errors

- `invalid_argument` — no such remote (add it with `remote_add`), an invalid
  remote name, or the URL is not an acceptable `https://` URL.
- `volume_refused` — the volume is read-only.

## Examples

```json
{"op": "remote_set_url", "repo": "service", "remote": "origin", "url": "https://github.com/acme/service-renamed.git"}
```
