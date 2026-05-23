---
name: sqlite-vec
description: SQLite-side Vector Memory via the sqlite-vec extension — opt-in build with `-tags=sqlite_vec` + CGO.
---
Loomcycle v0.10.2 ships the **build mechanism** for SQLite Vector
Memory via the `sqlite-vec` extension. The default build is unchanged
(pure-Go, modernc.org/sqlite, no CGO — Vector Memory refuses with
`vector_unsupported` on SQLite); operators wanting vectors on SQLite
build with `-tags=sqlite_vec` which swaps to the CGO mattn driver +
loads the sqlite-vec extension at every connection-open.

**STATUS in v0.10.2:** the build mechanism IS in place; the actual
`MemoryEmbed*` method implementations are STUBBED pending v0.10.3.
This means operators building with `-tags=sqlite_vec` today get:

- A working CGO binary that loads the sqlite-vec extension at boot
- A clear "build tag active" log line at startup confirming the
  extension loaded
- `SupportsVectors()` still returns `false` — the Memory tool will
  still refuse vector ops, with the same `vector_unsupported` error
  the default build returns

The build-tag opt-in is the architectural commitment. The full vec0
virtual-table schema design lands in v0.10.3 once we've benchmarked
the dimension-partitioning tradeoffs against real workloads.

For SQLite operators who NEED vector Memory TODAY, use Postgres +
pgvector instead — that path has been production-ready since v0.9.0.

## Why the default build doesn't ship vectors

`modernc.org/sqlite` (the loomcycle default SQLite driver) is pure
Go — no CGO, no C-extension loading. It can't load sqlite-vec's
shared library. The pure-Go posture keeps the loomcycle binary at
~30 MB statically-linked + portable across operating systems without
a C toolchain — exactly what the README highlights as the deploy
story.

Switching to `mattn/go-sqlite3` (CGO) unlocks extension loading but
costs the portability:

- The binary becomes platform-specific (must compile on the target
  arch with a C toolchain).
- Cross-compilation requires Zig as CC or a target-specific GCC.
- Release tarballs would need to double (default + sqlite_vec
  variants per platform — 4 → 8 SKUs).

The build-tag opt-in lets operators choose: default stays portable;
operators who want vectors on SQLite take the CGO hit explicitly.

## Why use SQLite + sqlite-vec instead of Postgres + pgvector

Both back-ends are first-class. Pick SQLite + sqlite-vec when:

- You're already running a single-file SQLite deployment (small
  team, ~thousands of memory rows, no multi-replica HA need).
- You don't want to operate a Postgres cluster.
- You're embedding loomcycle into a desktop app or edge deployment
  where SQLite is the natural fit.

Pick Postgres + pgvector when:

- You're at JobEmber scale (tens of thousands of memory rows,
  multiple concurrent operators, multi-replica HA on the v0.10.x
  roadmap).
- You already have a Postgres cluster for the rest of your stack.
- You want the `vector(N)` per-row dimension flexibility (pgvector
  supports variable dim per row natively; sqlite-vec's vec0
  partitions by fixed dim per virtual table).

## Build with sqlite-vec

```sh
# macOS: install via Homebrew
brew install sqlite-vec
export LOOMCYCLE_SQLITE_VEC_PATH=$(brew --prefix sqlite-vec)/lib/vec0

# Linux (Debian/Ubuntu)
apt install libsqlite3-mod-vec  # check distro for exact package name
export LOOMCYCLE_SQLITE_VEC_PATH=/usr/lib/sqlite3/vec0

# Then build loomcycle with the tag
CGO_ENABLED=1 go install -tags=sqlite_vec github.com/denn-gubsky/loomcycle/cmd/loomcycle@v0.10.2

# Run as usual — the build will load the extension at every Open()
loomcycle --config loomcycle.yaml
```

You'll see this boot log confirming the tag is active:

```
sqlite: sqlite_vec build active — extension path=/opt/homebrew/opt/sqlite-vec/lib/vec0 (MemoryEmbed* implementation lands in v0.10.3; SupportsVectors() still false until then)
```

If `LOOMCYCLE_SQLITE_VEC_PATH` is empty, the binary refuses to start
with a clear error:

```
sqlite_vec build requires LOOMCYCLE_SQLITE_VEC_PATH pointing at the sqlite-vec extension shared library...
```

A `sqlite_vec`-tagged binary without the path is always a
misconfiguration; failing loud at boot is the right call.

## Verifying the build

You can confirm the tag took effect without configuring an embedder
by running `loomcycle` against an empty config and checking the log
for the `sqlite_vec build active` line. The extension loads on every
connection-open, so the line fires within a second of process start.

`--version` doesn't currently report the build tag set — track via
the boot log.

## Roadmap to v0.10.3

The v0.10.3 follow-up wires the actual MemoryEmbed* methods against
sqlite-vec's `vec0` virtual-table API. The design tradeoff:

- **One vec0 table per dimension** (`memory_embeddings_1024`,
  `memory_embeddings_3072`, etc.) — clean modeling, more migrations,
  harder to add new embedder models post-deployment.
- **One vec0 table with auxiliary columns** (`vec0(embedding
  float[1024], +scope_id text, +key text)` and store the dim per row
  in a sidecar `memory_embedding_meta` table) — single table, but
  mixed-dim queries need application-side filtering.

The decision lands in v0.10.3 with a benchmark workload to inform.

## Related topics

- `vector-memory` — the cross-backend Vector Memory architecture.
- `voyage-embedder` — embedder choice. Voyage works regardless of
  whether you build with the sqlite_vec tag; the tag only affects
  where the embeddings are STORED, not how they're computed.
