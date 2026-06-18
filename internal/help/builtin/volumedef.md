---
name: volumedef
description: the VolumeDef tool — provision, inspect, and remove CONFINED dynamic filesystem volumes at runtime (RFC AH Phase 2a).
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
  (even in one tenant) each get their OWN `work` with no collision. The
  whole spawn tree resolves it (sub-agents inherit it via the narrow-only
  rule); it is **auto-purged when the top-level run completes** (a singleton
  sweeper backstops crashes). Requires an active run; refused if the name
  collides with a static volume or already exists in this run. There is no
  `delete`/`purge` for ephemeral volumes — lifetime is the run.
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
`volume_def_scopes` in the agent yaml — `any` (create/delete/purge any
name) or `named:<volume>` (a single name). Without a grant,
create/delete/purge are refused; `get`/`list` are tenant-scoped reads.

## Binding to a dynamic volume

After `create`, an agent binds to the volume by name exactly like a static
one — `volumes: [repo-a]` in its AgentDef. Run-start resolves the name
(static first, then your tenant's dynamic volumes, then shared) and the
file/exec tools confine to its root. Spawn confinement is unchanged: a
sub-agent's volumes are the narrow-only intersection with the parent's.
