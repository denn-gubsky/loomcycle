---
name: TeamDef/verify
description: "TeamDef op=verify — is the active version the one you expect, and will it still run? Or, with an overlay, check an unsaved draft without writing it."
---
`verify` has two forms.

**The active version.** Pass `name`, and `content_sha256` to compare a hash
you hold. It answers whether that is the active version, and sweeps what the
version references but does not contain — a channel deleted, a member agent
retired — into `issues`, with `runnable`.

**A draft.** Pass `name` and an `overlay`: exactly what `create` or `fork`
would be sent. Nothing is written. It runs every check that save would, plus
the sweep, and returns all the problems at once, each with the JSON path of
the value at fault. It checks the draft as the save would — a fork when the
name has a version, else a create — unless `as` says which.

## Arguments

- `name` (required) — the team.
- `content_sha256` — the active-version form: the hash to compare.
- `overlay` — the draft form: the graph to check.
- `as` — the draft form: `create` or `fork`.
- `parent_def_id` — the draft form: check it as a fork of this version.
- `description` — the draft form: checked as a save would check it.

## Returns

`{name, deployed, matches, current_sha256, current_def_id, version, runnable,
issues?}`. The draft form adds `valid` (a save would be accepted),
`checked_as`, `content_sha256` (the draft's), `parent_def_id` for a fork, and
always `issues` — each `{kind, severity, detail, path?, state?, field?}` with
`severity` `refused`, `unrunnable` or `advisory`.

## Errors

- `verify: as, parent_def_id and description describe a draft — they need an
  overlay`.
- `verify: pass content_sha256 or overlay, not both ...`.

## Examples

Check a draft before saving it:

```json
{"op": "verify", "name": "pr-review", "overlay": {"max_iterations": 5}}
```

Is the deployed team the one you wrote?

```json
{"op": "verify", "name": "pr-review", "content_sha256": "3f9a0c1d2e4b5a6978c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2"}
```
