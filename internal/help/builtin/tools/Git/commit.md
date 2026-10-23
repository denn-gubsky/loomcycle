---
name: Git/commit
description: "Git op=commit — record the staged changes as a new commit under the operator's identity, with a trailer naming the run."
---
`commit` records what is staged as a new commit on the current branch (or a
detached HEAD). Needs a read-write volume.

The author and committer are the identity the operator configured; without
one, your agent's name at an address under `loomcycle.invalid`. You cannot
choose them. The message gets a `Loomcycle-Run: <run id>` trailer, so a
reviewer can trace the commit to the run that made it.

The commit can be signed with `sign_key`: the name of an `ed25519` crypto key
your crypto_scopes reach. The runtime signs with it in git's SSH signature
format; you never see the key. Each signature counts as one Crypto operation
of the run.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `message` (required) — the commit message.
- `all` — stage changes to files already tracked (edits and deletions) first.
  New files still need `add`.
- `amend` — replace the last commit with one holding what is staged now and
  this `message`. With nothing staged it only rewords the message.
- `allow_empty` — make the commit even though nothing is staged.
- `sign_key` — sign the commit with this crypto key.
- `sign_key_scope` — `agent`, `user` or `tenant`: the scope that holds
  `sign_key`, when the name is in more than one. Only with `sign_key`.

## Returns

`{op: "commit", hash, signed_by?}` — the new commit's full hash. `signed_by`
is present on a signed commit: the key's public half as an
`ssh-ed25519 ...` line, which a reviewer adds to an allowed-signers file to
verify the commit.

## Errors

- `nothing_to_commit` — nothing is staged. Stage with `add`, or pass `all`.
- `invalid_argument` — `message` is empty, `amend` in a repository with no
  commit yet, or `sign_key_scope` without `sign_key`.
- `sign_key_refused` — the key is not one your crypto_scopes reach, is not an
  `ed25519` key the runtime holds, or the run's Crypto budget is spent; the
  detail says which.
- `signing_unavailable` — this deployment cannot sign.
- `volume_refused` — the volume is read-only.

## Examples

Stage tracked changes and commit:

```json
{"op": "commit", "repo": "service", "message": "Fix the retry loop", "all": true}
```

```json result
{"op": "commit", "hash": "7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b"}
```

A signed commit, with the key held at the user scope:

```json
{"op": "commit", "repo": "service", "message": "Release 1.4.0", "sign_key": "release-signing", "sign_key_scope": "user"}
```

```json result
{"op": "commit", "hash": "5d4c3b2a1f0e9d8c7b6a5f4e3d2c1b0a9f8e7d6c",
 "signed_by": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGq4Xh3mR2k0bJz9vT1wQe7sYd5cLp8nUf6aKo3iBr2M"}
```

Reword the last commit:

```json
{"op": "commit", "repo": "service", "message": "Fix the retry loop's backoff", "amend": true}
```
