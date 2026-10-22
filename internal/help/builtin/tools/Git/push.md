---
name: Git/push
description: "Git op=push — publish branches or tags to a remote, or delete a remote branch; force only when asked."
---
`push` sends local commits to a remote and moves the remote's refs. Needs a
read-write volume.

It works only when your agent's `git_remote` allows `push`, and only to a
host on the operator's push list, which is separate from the fetch list. The
URL is the remote's, as `remote_list` shows it, and is checked on every
call. A `"$ghapp:<name>"` credential gets a token that may write; a
`"$cred:<name>"` one is used as stored.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `remote` — the remote's name (default `origin`).
- `credential` — `"$cred:<name>"` or `"$ghapp:<name>"`.
- `refspecs` — what to push. Default: the current branch, to the branch of
  the same name. A bare branch name like `"fix-retry"` pushes that branch to
  the same name. A full refspec is `[+]refs/...:refs/...`;
  `":refs/heads/<name>"` deletes that remote branch.
- `tags` — also push every tag.
- `force` — overwrite remote branches even when that discards commits there.
- `force_with_lease` — overwrite only if each remote branch is still where
  this repository last fetched it. Safer than `force`; not with `force`.

## Returns

`{op: "push", up_to_date, updated: [{name, old, new}]}`. Each entry in
`updated` is a remote ref that moved; an empty `old` is a ref created, an
empty `new` one deleted. `up_to_date` is `true` when the remote already had
everything.

## Errors

- `push_rejected` — the remote refused an update: not a fast-forward (fetch,
  then `merge` or `pull`), or a protected branch.
- `invalid_argument` — no such remote, an invalid refspec, HEAD is detached
  and `refspecs` is missing, or both `force` and `force_with_lease`.
- `git_push_disabled` — the operator has not enabled push.
- `git_remote_not_allowed`, `git_network_disabled`, `host_not_allowed`,
  `credential_url_not_allowed`, `credential_unresolvable`, `auth_failed`,
  `not_found`, `upload_too_large`, `timeout` — as for `clone`;
  `upload_too_large` means the push passed the operator's size limit.

## Examples

Push the current branch:

```json
{"op": "push", "repo": "service", "credential": "$cred:github"}
```

```json result
{"op": "push", "up_to_date": false, "updated": [
  {"name": "refs/heads/fix-retry", "old": "", "new": "7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b"}]}
```

Push a branch under another name on the remote:

```json
{"op": "push", "repo": "service", "credential": "$cred:github", "refspecs": ["refs/heads/fix-retry:refs/heads/review/fix-retry"]}
```

Replace a rewritten branch, only if nobody pushed to it since the last fetch:

```json
{"op": "push", "repo": "service", "credential": "$cred:github", "refspecs": ["fix-retry"], "force_with_lease": true}
```

Delete a remote branch:

```json
{"op": "push", "repo": "service", "credential": "$cred:github", "refspecs": [":refs/heads/old-experiment"]}
```
