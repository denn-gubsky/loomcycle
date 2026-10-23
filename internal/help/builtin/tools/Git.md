---
name: Git
description: "Git tool — work on a git repository inside a volume without a shell: status, log, diff, commits, branches, and (when allowed) clone, fetch, pull and push with credentials you never see."
---
The `Git` tool works on a git repository that lives inside one of your
volumes. It runs git in the runtime itself, not as a program on the host, so
nothing in a repository's configuration (hooks, filters, helpers) ever runs,
and a credential you name is resolved and sent by the runtime: you never see
its value, and it is never written into the repository.

## Choosing the repository

- `volume` — the volume (omit for your default volume). Context op=self lists
  yours.
- `repo` — the repository's directory, relative to the volume root (omit for
  the root). `init` and `clone` create the directory when it is missing; a
  clone needs it empty.

Read operations work on a read-only volume. Everything that changes the
repository needs a read-write volume.

## Operations

| op | What it does | Main arguments |
|---|---|---|
| `status` | current branch, and every changed or untracked file | — |
| `log` | commits, newest first | `rev`, `paths` (first one filters), `max` (default 20, at most 200), `since`, `until`, `all` |
| `show` | one commit and its patch | `rev` (default HEAD), `max_bytes` |
| `diff` | a unified diff | none = unstaged changes; `staged` = staged vs HEAD; `from` = worktree vs a revision; `from` + `to` = two revisions; `paths`, `max_bytes` |
| `blame` | who last changed each line of a file | `paths` (one file), `rev`, `max` lines (default 500) |
| `grep` | search a commit's files (not your uncommitted changes) | `pattern`, `rev`, `paths`, `ignore_case`, `invert`, `max` matches (default 100) |
| `notes_list` | git notes on recent commits | `rev`, `max` |
| `submodule_list` | the submodules and the commits recorded for them | — |
| `branch_list`, `tag_list`, `remote_list` | what exists | — |
| `merge_base` | common ancestor | `from`, `to` |
| `rev_parse` | a revision's full hash | `rev` |
| `init` | a new repository | `initial_branch` (default main) |
| `add` | stage | `paths` (`.` = everything) |
| `rm` | delete and stage the deletion | `paths`, `cached` to keep the file |
| `mv` | rename a tracked file | `from`, `to` |
| `restore` | discard worktree changes, or unstage with `staged` | `paths` |
| `reset` | move the branch | `mode` soft, mixed (default) or hard; `rev`; `paths` (mixed only) |
| `commit` | record the staged changes | `message`; `all` stages tracked changes first; `amend`, `allow_empty` |
| `checkout` | switch branch, or detach at a revision | `branch`, or `rev`; `create` makes `branch` at `rev`; `force` discards local changes; `sparse` checks out only those directories |
| `branch_create`, `branch_delete` | | `branch`, `rev`; `force` deletes an unmerged branch |
| `tag_create`, `tag_delete` | a lightweight tag, or an annotated one with `message` | `tag`, `rev`, `message`, `force`, `sign_key` |
| `remote_add`, `remote_remove`, `remote_set_url` | manage remotes | `remote`, `url` (https only) |
| `merge` | fast-forward the current branch | `rev` |
| `clean` | remove untracked files | `dirs`, `dry_run` |
| `clone` | copy a repository into the volume | `url`, `credential`, `branch`, `depth`, `single_branch` |
| `fetch` | update remote-tracking branches | `remote` (default origin), `credential`, `refspecs`, `prune`, `tags` |
| `pull` | fetch and fast-forward the current branch | `remote`, `branch`, `credential` |
| `push` | publish | `remote`, `credential`, `refspecs` (default: the current branch), `force`, `force_with_lease`, `tags` |
| `ls_remote` | list a remote's refs without a repository | `url`, `credential` |
| `submodule_update` | fetch one submodule at its recorded commit | `submodule` (a name from submodule_list), `credential` |

Commits are made under the identity the operator configured, and each carries
a `Loomcycle-Run:` trailer naming the run, so a reviewer can trace it.

To sign a commit or an annotated tag, pass `sign_key`: the name of an
`ed25519` crypto key your crypto_scopes reach (`sign_key_scope` picks the
scope when the name is in more than one). The signature is git's SSH format;
the result's `signed_by` is the key's public half, which a reviewer lists in
an allowed-signers file to verify it. You never see the key. A signature
counts as one Crypto operation.

## The network

`clone`, `fetch`, `pull`, `ls_remote`, `submodule_update` and `push` work
only when your agent's `git_remote` allows them (`fetch` for all but push,
`push` for push) and only to hosts the operator lists; push has its own host
list. A submodule's URL comes from `.gitmodules` and is checked like any
remote, and a credential you name must be allowed for the submodule's own
URL, not the superproject's. Only `https://` URLs
are accepted. A `fetch`, `pull` or `push` uses the remote's URL as the
repository's configuration gives it, and the runtime checks that URL each
time.

Name a credential as `"$cred:<name>"` or `"$ghapp:<name>"`. It resolves only
when its owner restricted it to URLs that cover the repository, so a
credential can never be sent anywhere else. Omit it for a public repository.

## What is not available

Cherry-pick, rebase, revert, stash, and any merge or pull that is not a
fast-forward. When a pull or merge reports `non_fast_forward`, the branches
have diverged: fetch, inspect with `log` and `diff`, and decide how to
continue (for example, a new branch from the remote's tip). Read file contents
with the Read tool.

## Errors

Every failure is `{"error": "<class>", "detail": "..."}`. A network failure
carries only its class, never the server's own words. Classes include
`volume_refused` (no such volume, or a read-only one for a change),
`not_a_repository`, `invalid_argument`, `invalid_revision`,
`nothing_to_commit`, `worktree_dirty` (local changes would be lost; commit,
restore, or pass `force`), `non_fast_forward`, `push_rejected`,
`sign_key_refused`, `signing_unavailable`,
`git_remote_not_allowed`, `git_network_disabled`, `git_push_disabled`,
`host_not_allowed`, `credential_url_not_allowed`, `credential_unresolvable`,
`auth_failed`, `not_found`, `source_too_large` (the repository passed the operator's size limits), `upload_too_large`, `timeout` and `fetch_failed`.

## Examples

See what changed in the repository at `service` on your default volume:

```json
{"op": "status", "repo": "service"}
```

Stage everything and commit:

```json
{"op": "commit", "repo": "service", "message": "Fix the retry loop", "all": true}
```

Clone a private repository with a stored credential, then push a branch:

```json
{"op": "clone", "repo": "service", "url": "https://github.com/acme/service.git", "credential": "$cred:github"}
```

```json
{"op": "push", "repo": "service", "credential": "$cred:github", "refspecs": ["refs/heads/fix-retry:refs/heads/fix-retry"]}
```
