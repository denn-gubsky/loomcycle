---
name: volumedef
description: the VolumeDef tool — provision, inspect, and remove CONFINED dynamic filesystem volumes at runtime.
---
The **VolumeDef** tool provisions *dynamic* filesystem volumes at runtime —
tenant-scoped, mutable, and confined inside an operator-blessed parent. It
complements the static `volumes:` config (operator-authored, may map
anywhere): a dynamic volume lets a tenant spin up a working tree per job
without an operator config change.

For the per-agent `volume` argument, ro/rw enforcement, and spawn
confinement, see the **volumes** topic. This topic is about *creating* the
dynamic volumes an agent then binds to.

## The security model — you never supply a path

`create` takes a **name + mode only**. The runtime DERIVES the path:

```
<dynamic_root>/<tenant-segment>/<name>
```

where `<dynamic_root>` is the static volume the operator marked
`dynamic_root: true`, and `<tenant-segment>` is your tenant id (or
`_shared` for the shared tenant). You cannot supply a host path anywhere —
so a dynamic volume can never escape the operator-blessed parent, and the
destructive `purge` can only ever delete a runtime-derived path inside it.

Names must match `^[a-z0-9][a-z0-9_-]{0,63}$` — lowercase alnum, `_`, `-`,
no slashes or dots (so a name can't inject a path component).

`create` also refuses when something already on disk would redirect it: if
the tenant-segment directory or the volume's own directory is a symlink,
the create fails instead of following it out of the dynamic root.

## Operations

- **create** `{name, mode}` — provision the volume: derive the path, make
  the directory (`0700`), persist the mapping. `mode` is `rw` (default) or
  `ro`. Idempotent: re-creating with the same mode is a no-op; a different
  mode updates the mapping. Refused if the name collides with a static
  `volumes:` entry (operator yaml is ground truth) or if no `dynamic_root`
  is configured.
- **create** `{name, mode, ephemeral: true}` — provision a **run-scoped
  ephemeral** volume instead of a tenant volume. The path is derived as
  `<dynamic_root>/_ephemeral/<root_run_id>/<name>`, so two concurrent runs
  (even in one tenant) each get their OWN `work` with no collision. **Every
  agent of the run can use it, with the mode it was created with**: a
  sub-agent reaches it by name whatever its own `volumes:` list says, and
  its own volume rules do not narrow it. To take write access away from
  everyone, use `set_mode`; an agent whose definition says
  `ephemeral_volumes: ro` gets every ephemeral volume read-only, and so do
  the agents it spawns. It is **auto-purged when the top-level run completes** (a
  singleton sweeper backstops crashes). A team walk started with `mode: "detach"` uses
  its starter's ephemeral volumes and keeps them until it ends, even past the
  top-level run. Requires an active run; refused if the name
  collides with a static volume or already exists in this run. There is no
  `delete`/`purge` for ephemeral volumes — lifetime is the run.
- **create** `{name, ephemeral: true, mode, source: {git: {...}}}` — make a
  run-scoped volume holding **one commit of a git repository**. The runtime
  fetches it; you never see the token, and neither does any agent.

  ```json
  {"op": "create", "name": "repo", "ephemeral": true, "mode": "ro",
   "source": {"git": {"url": "https://github.com/acme/app",
                      "commit": "<40-digit sha>",
                      "ref": "refs/heads/feature-x",
                      "credential": "$cred:repo-acme-app"}}}
  ```

  - `url` — https only, on a host the operator allows.
  - `commit` — the full 40-digit sha. The volume holds exactly this commit.
  - `ref` — optional: where to look for the commit when the server will not
    serve a commit by its sha.
  - `credential` — `$cred:<name>` or `$ghapp:<name>`. It must be a credential
    restricted (`allowed_urls`) to a URL covering this repository. Leave it
    out for a public repository. For a GitHub App the token is made for this
    repository alone, read-only; the App config must allow reading contents.
  - `max_bytes` — optional; may only lower the operator's limit.
  - `diff_base` — optional: also write a diff from another commit to this
    one. `"merge-base:default"` diffs against the point where this commit's
    history meets the repository's default branch (what a pull request
    shows); a 40-digit sha diffs against that commit. The diff goes to
    `_meta/diff.patch` (a unified diff, renames shown as a deletion and an
    addition) and `_meta/numstat.json` (each changed path with its status,
    lines added and deleted, and why a change has no body, when it has none).
  - `outputs` — optional, with `diff_base`: `{"diff": "_meta/<name>",
    "numstat": "_meta/<name>"}` to name the two files. Both must be plain
    file names directly under `_meta/`.
  - `mode` applies to the agents of the run once the volume exists: with
    `ro`, nobody can change what was fetched.

  The answer gives the commit, the number of files and bytes written, and how
  many paths were left out. It carries no file content. What was left out
  (links that leave the tree, submodules, files over the size limit) is
  listed in `_meta/omitted.json` in the volume; there is no `.git`.

  It needs `source:git` in `volume_def_scopes` besides the grant for the
  name. A failure is one word (`auth_failed`, `not_found`,
  `commit_not_found`, `merge_base_not_found`, `source_too_large`, `timeout`, `host_not_allowed`,
  `credential_url_not_allowed`, `credential_unresolvable`,
  `git_source_disabled`, `fetch_failed`) and leaves nothing behind: the name
  can be used again.
- **set_mode** `{name, mode: "ro"}` — make one of this run's **ephemeral**
  volumes read-only. It applies at once to every agent of the run, you
  included, and to sub-agents already running: their next `Write`, `Edit` or
  `Bash` on the volume is refused, while `Read`, `Grep` and `Glob` go on
  working. **It cannot be undone**: `mode: "rw"` is refused, and so is
  re-creating the volume. Use it after you have filled a volume and before
  you hand it to agents that must not change it. It is not for a persistent
  volume (re-create that with the mode you want), and it works only inside a
  run. Under `Bashbox` a write to a read-only volume appears to succeed
  inside one command and is not kept.
- **get** `{name}` / **list** — inspect your tenant's dynamic volumes.
  A volume owned by another tenant is reported as not-found.
- **delete** `{name}` — remove the mapping but **LEAVE the files on disk**.
  "Unmap" the volume; the directory survives so you can re-`create` to remap
  it (or hand the tree to another tool).
- **purge** `{name}` — remove the mapping **AND delete the directory tree**.
  This is the destructive op. It re-derives the path from
  `(dynamic_root, tenant, name)` (never trusting the stored path), resolves
  symlinks, and refuses unless the real path is strictly inside the dynamic
  root under your tenant segment — so it can only ever delete your own
  volume's directory.

## Capability gate

The tool is default-deny. The operator grants it per-agent via
`volume_def_scopes` in the agent yaml — `any` (create/delete/purge/set_mode
any name) or `named:<volume>` (a single name). `source:git` is a third value,
needed in addition to create a volume from a git source; `any` does not
include it. Without a grant,
create/delete/purge/set_mode are refused; `get`/`list` are tenant-scoped
reads.

## Binding to a dynamic volume

After `create`, an agent binds to the volume by name exactly like a static
one — `volumes: [repo-a]` in its AgentDef. Run-start resolves the name
(static first, then your tenant's dynamic volumes, then shared) and the
file/exec tools confine to its root. Spawn confinement is unchanged: a
sub-agent's volumes are the narrow-only intersection with the parent's.

## Snapshots

A snapshot carries each persistent dynamic volume's name and mode, never
its path or its files. Restoring re-creates the volume the way `create`
would on the restoring host: the path is derived under that host's
`dynamic_root` and an **empty** directory is made there. Copy the files
yourself if the new host needs them. Ephemeral volumes are not carried.
