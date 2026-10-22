---
name: Git/checkout
description: "Git op=checkout — switch to a branch, create one and switch to it, or detach HEAD at a revision; refuses when local changes would be lost."
---
`checkout` moves HEAD and rewrites the tracked files to match:

- `branch` — switch to an existing local branch;
- `branch` + `create` — make a new branch at `rev` (default HEAD) and switch
  to it;
- `rev` alone — detach HEAD at that revision.

It is stricter than the git program: ANY uncommitted change to a tracked
file, staged or not, refuses the switch, as does an untracked file the target
would overwrite. Commit or `restore` first, or pass `force`. Needs a
read-write volume.

With `sparse`, the worktree holds only the tracked files under the listed
directories; every other tracked file is removed from disk but stays in the
index, so it is not reported as deleted and is still in the next commit. A
file at a left-out path is removed only when it matches what the index held;
anything else is someone's work and stays. The list is not remembered: a
later `checkout`, `reset` or `merge` writes any file it adds wherever it is,
so pass `sparse` again to keep the worktree narrow.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `branch` — the local branch to switch to, or to create.
- `rev` — the revision to detach at; with `create`, the new branch's start.
- `create` — create `branch` first.
- `force` — discard uncommitted changes to tracked files and switch anyway.
  Untracked files are kept unless the target has a file at the same path.
- `sparse` — check out only these directories (or files), relative to the
  repository. A directory matches by whole path components: `"src"` covers
  `src/...` but not `src2`.

## Returns

`{op: "checkout", ok: true, head, files}` — `head` and `files` as `status`
reports them after the switch.

## Errors

- `worktree_dirty` — local changes would be lost; the detail names the files.
- `invalid_revision` — the branch does not exist (to make it, pass `create`),
  or `rev` names no commit.
- `already_exists` — `create` with a branch that exists.
- `invalid_argument` — neither or both of `branch` and a detaching `rev`, an
  invalid branch name, or a `sparse` entry that is empty, absolute, has a
  `..` component or reaches into `.git`.
- `volume_refused` — the volume is read-only.

## Examples

Start a new branch from the current commit:

```json
{"op": "checkout", "repo": "service", "branch": "fix-retry", "create": true}
```

A local branch tracking the remote's `release` branch:

```json
{"op": "checkout", "repo": "service", "branch": "release", "create": true, "rev": "origin/release"}
```

Look at an old release, detached:

```json
{"op": "checkout", "repo": "service", "rev": "v1.3.0"}
```

Switch to `main` with only `docs` and `src/api` on disk:

```json
{"op": "checkout", "repo": "service", "branch": "main", "sparse": ["docs", "src/api"]}
```

Switch back, throwing away uncommitted edits:

```json
{"op": "checkout", "repo": "service", "branch": "main", "force": true}
```
