---
name: Git/tag_create
description: "Git op=tag_create — tag a commit: a lightweight tag, or an annotated one with a message, optionally signed with a crypto key you never see."
---
`tag_create` tags the commit `rev` names. Without `message` it makes a
lightweight tag (a name for the commit); with one, an annotated tag that
records the message, the tagger and the time. The tagger is the identity the
operator configured; without one, your agent's name at an address under
`loomcycle.invalid`. Needs a read-write volume. To publish a tag, `push` with
`tags` or a `refs/tags/...` refspec.

An annotated tag can be signed with `sign_key`: the name of an `ed25519`
crypto key your crypto_scopes reach. The runtime signs with it in git's SSH
signature format; you never see the key. Each signature counts as one Crypto
operation of the run. A lightweight tag cannot be signed.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `tag` (required) — the tag's name, such as `v1.4.0`.
- `rev` — the commit to tag. Default HEAD.
- `message` — makes the tag annotated. Required to sign.
- `force` — replace a tag of the same name.
- `sign_key` — sign the tag with this crypto key.
- `sign_key_scope` — `agent`, `user` or `tenant`: the scope that holds
  `sign_key`, when the name is in more than one. Only with `sign_key`.

## Returns

`{op: "tag_create", tag: {name, hash, annotated, message?}, signed_by?}`.
`hash` is the tagged commit. `signed_by` is present on a signed tag: the key's
public half as an `ssh-ed25519 ...` line, which a reviewer adds to an
allowed-signers file to verify the tag.

## Errors

- `already_exists` — the tag exists; pass `force` to replace it.
- `invalid_argument` — an invalid tag name, a `message` with no text,
  `sign_key` without `message`, or `sign_key_scope` without `sign_key`.
- `invalid_revision` — `rev` names no commit.
- `sign_key_refused` — the key is not one your crypto_scopes reach, is not an
  `ed25519` key the runtime holds, or the run's Crypto budget is spent; the
  detail says which.
- `signing_unavailable` — this deployment cannot sign.
- `volume_refused` — the volume is read-only.

## Examples

A lightweight tag on HEAD:

```json
{"op": "tag_create", "repo": "service", "tag": "build-142"}
```

A signed release tag:

```json
{"op": "tag_create", "repo": "service", "tag": "v1.4.0", "message": "Release 1.4.0", "sign_key": "release-signing"}
```

```json result
{"op": "tag_create",
 "tag": {"name": "v1.4.0", "hash": "4b2d8e1f0a9c7b6d5e4f3a2b1c0d9e8f7a6b5c4d", "annotated": true, "message": "Release 1.4.0\n"},
 "signed_by": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGq4Xh3mR2k0bJz9vT1wQe7sYd5cLp8nUf6aKo3iBr2M"}
```

Move an existing tag to another commit, with a key held at the tenant scope:

```json
{"op": "tag_create", "repo": "service", "tag": "v1.4.0", "rev": "5d4c3b2a1f0e9d8c7b6a5f4e3d2c1b0a9f8e7d6c", "message": "Release 1.4.0", "force": true, "sign_key": "release-signing", "sign_key_scope": "tenant"}
```
