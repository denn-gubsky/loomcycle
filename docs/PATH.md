# Path (RFC AL)

Path is a **Unix-like virtual filesystem** over the things loomcycle already
stores — Memory entries, Volume mounts, and Documents. It lets agents (and,
since v1.4.0, external callers) address those resources by human-readable
paths like `/docs/launch` instead of opaque ids, organize them into a tree,
and move/rename/list them — all scope-aware and tenant-isolated.

**Why a naming layer, not "just use the resource ids":** a Memory key, a
Volume name, and a Document UUID are three unrelated namespaces with no shared
structure — you can't `ls` them together, can't group "everything under
`/projects/acme`", and can't rename a thing without rewriting every reference.
Path borrows the Linux **inode/dirent split**: each resource keeps its
permanent id (the "inode"), and a `dirents` row maps `(parent_path, name) →
resource` (the "directory entry"). So a rename or move is a cheap dirent
update that never touches the resource, and one tree spans all three resource
kinds. The alternative — a bespoke path column on each resource table — can't
express directories, cross-kind listing, or atomic subtree moves, and
re-implements the same mapping three times.

A dirent is a **name, not an authority grant.** Resolving `/docs/launch` to a
Document id does not, by itself, let you read that Document — the resource's
own scope/tenant check still applies. So the risk Path introduces is
*integrity* (a wrong-name mapping), not *confidentiality*.

## Surface

One tool, `Path`, gated by per-agent `tools: [Path]`. Six ops:

| op        | what it does |
|-----------|--------------|
| `resolve` | path → the dirent + its `resource_ref` (the backing id). |
| `ls`      | list a directory's entries (`recursive:true` walks descendants; `kind_filter` narrows by kind). |
| `stat`    | one entry's metadata (name, kind, resource_ref). |
| `mkdir`   | materialize an empty `directory` dirent so an empty branch persists and lists. Idempotent (ok if the directory already exists, explicitly or implied by descendants); refuses to clobber a non-directory. Intermediate directories under a created resource stay implicit (S3-style) — `mkdir` is only needed for an *empty* folder. |
| `mv`      | re-parent / rename a dirent (a move into the path's own subtree is refused — it would orphan the tree). |
| `rm`      | remove a dirent (`recursive:true` is **required** to remove a path that has descendants). |

`scope` selects the tree: `agent` (this agent, the default), `user` (this
end-user — needs a `user_id` on the run), or `tenant` (shared across the
tenant). Trees are keyed by the authoritative tenant; in open mode the tenant
is the shared `""`.

**Path grammar** (enforced by `normalizePath`): slash-rooted and absolute;
segments match `[a-zA-Z0-9._-]+`; **no `..`** (rejected, not resolved — this is
what makes the tree tenant-safe); at most 64 segments / 1024 chars.

**Resource kinds** a dirent can point at: `directory` (implicit, or materialized
by `mkdir`), `document`, `volume_mount`, `memory_entry`.

## How resources get a name

Path itself never creates resources — each resource **opts in** to a name when
it's created:

- **Memory** — `Memory.set { ..., path: "/notes/today" }` registers a
  `memory_entry` dirent alongside the K/V write; `Memory.get { path: ... }`
  resolves it.
- **Volumes** — `VolumeDef.create { ..., mount_at: "/vol/repo" }` registers a
  `volume_mount` dirent (default `/vol/<name>` when omitted).
- **Documents** — `Document.create_document { ..., path: "/docs/launch" }`
  registers a `document` dirent (see `docs/DOCUMENTS.md`).

SQL Memory deliberately stays **out** of the tree — it's a per-scope database,
not a named resource.

## Off-run: every transport (v1.4.0)

Besides in-band use, Path is a first-class operation on all four wire
surfaces, so a human or a UI can manage the same namespace agents build —
without spawning a run:

| Transport | Surface |
|-----------|---------|
| HTTP      | `POST /v1/_path` (body = the op-discriminated tool input) |
| gRPC      | `rpc Path(SubstrateRequest) returns (SubstrateResponse)` |
| MCP       | the LoomCycle MCP meta-tool `path` |
| TS        | `client.path(input)` (`@loomcycle/client`) |
| Python    | `await client.path(input)` |

All four dispatch through a single `Connector.Path` method. **Scope and tenant
are resolved server-side from the authenticated principal — never read from the
wire.** The endpoints are tenant-confined (`ScopeTenant`; an operator's
`substrate:admin` also satisfies).

```bash
# ls the user-scope root
curl -sS -X POST localhost:8080/v1/_path \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -d '{"op":"ls","scope":"user","path":"/"}'
# → {"path":"/","entries":[{"name":"docs","kind":"directory",...}, ...]}
```

```ts
const { entries } = await client.path({ op: "ls", scope: "user", path: "/docs" });
await client.path({ op: "mv", scope: "user", path: "/docs/launch", to: "/archive/launch" });
```

## Guarantees & caveats

- **`mv` can't orphan a tree** — moving `/a` to `/a/b` (into its own subtree)
  is refused.
- **`rm` is dirent-only** — it removes the *name*, not the backing resource
  (the Memory entry / Volume / Document survives and can be re-named). The
  `resource_too` flag (cascade-delete the resource) is **not supported in v1**.
- **No per-agent `path_scopes` ACL yet** — in v1, `tools: [Path]`
  grants all three scopes. Because a dirent is a name and not an authority
  grant, the exposure is integrity, not confidentiality; a finer ACL is a
  follow-up.
- **Pre-existing volumes don't auto-mount** — `mount_at` registers a dirent at
  *create* time; lazy mounting of volumes created before Path shipped is a
  follow-up.

## Where it lives

`internal/tools/builtin/pathtool.go` (the tool) + `pathnorm.go` (grammar); the
`dirents` table + `Dirent*` methods on both the sqlite and postgres stores
(`internal/store`). The `path` help topic (`Context op=help topic=path`) is the
in-agent reference.

---

**One-sentence thesis:** Path is the thin naming layer that turns three opaque
id namespaces into one Unix-like tree agents and humans can navigate — a dirent
is a name, the resource keeps the authority, and `..` never resolves.
