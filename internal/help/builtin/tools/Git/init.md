---
name: Git/init
description: "Git op=init — create a new, empty repository in a volume directory, creating the directory if it is missing."
---
`init` creates a repository with no commits. The directory named by `repo` is
created when it does not exist. Needs a read-write volume.

## Arguments

- `volume` — the volume; omit for your default volume.
- `repo` — the directory, relative to the volume root; omit for the root.
- `initial_branch` — the first branch's name (default `main`).

## Returns

`{op: "init", repo, branch}`, where `branch` is the branch the first commit
will create. The repository has no commit yet: `add` files, then `commit`.

## Errors

- `already_exists` — the directory already has a `.git`.
- `invalid_argument` — `initial_branch` is not a valid branch name, or `repo`
  leaves the volume.
- `volume_refused` — the volume is read-only or not one you are bound to.

## Examples

A new repository in `notes`:

```json
{"op": "init", "repo": "notes"}
```

One whose first branch is `trunk`, on the volume `work`:

```json
{"op": "init", "volume": "work", "repo": "site", "initial_branch": "trunk"}
```
