---
name: Git/remote_list
description: "Git op=remote_list — the repository's configured remotes and their URLs."
---
`remote_list` lists the remotes the repository's configuration names, sorted
by name. `fetch`, `pull` and `push` dial a remote's URL as listed here, and
the runtime checks it against the operator's host list each time.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).

## Returns

`{remotes: [{name, url}]}`.

## Errors

- `not_a_repository` — no `.git` directory at `repo`.

## Examples

```json
{"op": "remote_list", "repo": "service"}
```

```json result
{"remotes": [{"name": "origin", "url": "https://github.com/acme/service.git"}]}
```
