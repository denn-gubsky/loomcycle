# loomcycle-builder — sandbox sidecar

A reference **sidecar** that gives loomcycle agents a safe, isolated, ephemeral
place to run a real dev toolchain (Python / Go / Rust / C++ / Node) — compile,
test, run code — **without loomcycle leaving its distroless image or touching a
container engine**.

## Why a sidecar

loomcycle ships distroless (no shell, no compilers, no podman) and runs non-root
(uid 65532) — it *cannot* run podman itself (no subuid range, no `newuidmap`),
and mounting a host podman socket into the process that runs model-authored code
would be ≈ host root. So the container engine lives here, in a separate service,
and loomcycle drives it over the one distroless-safe channel it already speaks:
**MCP over HTTP**. The sidecar owns all podman + isolation + tmpfs complexity; a
compromised loomcycle can only call the constrained `sandbox_*` API, never craft
a privileged container.

```
agent (loomcycle · distroless)
   │  mcp__sandbox__sandbox_exec        (HTTP-MCP, bearer-authed)
   ▼
loomcycle-builder (this sidecar)  ──►  per-session container
   rootless podman + runsc/tmpfs        --network none --read-only
                                        --cap-drop=ALL --tmpfs /work
```

## Tools (MCP `mcp__sandbox__*`)

| Tool | Purpose |
|---|---|
| `sandbox_open` | Create a session container; returns a `session_id`. Params: `network` (none/egress), `tmpfs_mb`, `cpu`, `mem_mb`, `pids` (all clamped to operator ceilings). |
| `sandbox_exec` | Run a command in a session (`/work`); returns combined output + exit code. |
| `sandbox_write` / `sandbox_read` | Put files in / pull artifacts out (paths relative to `/work`; `base64` for binary). |
| `sandbox_close` | Destroy a session (idempotent). |
| `sandbox_touch` | Reset a session's idle timer — a keepalive to hold a container across a gap between commands. |
| `sandbox_close_run` | Close ALL your sessions for a given `root_run_id` (bulk teardown of a run tree). |
| `sandbox_list` | List your own sessions. |

Sessions are tagged with their owning loomcycle **run** via the attested
`X-Loom-Root-Run` header loomcycle forwards (`${run.root_run_id}`) — that's what
`sandbox_close_run` matches on. The tag is set from the request header, never a
tool argument, and it's principal-scoped (a caller only ever closes its own).

Sessions are long-lived — open once, run many commands (the workspace, deps, and
build cache persist across `sandbox_exec` calls), close when done. They also
expire on an idle / absolute TTL, and any leftover container is reaped at sidecar
startup (`loomcycle.managed=1` label).

## Build

```bash
# The sidecar image (podman-capable):
docker build -t denngubsky/loomcycle-builder:dev --build-arg VERSION=dev .

# The session image agents run in (pin by digest in prod):
docker build -t localhost/loomcycle-sandbox-session:latest ./session
```

## Deploy

**Full step-by-step (TrueNAS + any Docker host): [`INSTALL.md`](INSTALL.md).**

The sidecar needs a container engine to launch sessions — pick a model:

- **Host Docker socket** (recommended for TrueNAS / any Docker host) — build the
  [`Dockerfile.docker`](Dockerfile.docker) image, mount `/var/run/docker.sock`, and
  set `SANDBOX_PODMAN_BIN=docker`. Sessions are sibling containers on the host
  engine; no nested engine, no Sysbox, no `privileged`.
- **Nested rootless podman** — build the [`Dockerfile`](Dockerfile) image (self-
  contained: sessions live inside the sidecar). Needs `runtime: sysbox-runc`
  (secure) or `privileged: true` (fallback; TrueNAS has no Sysbox).

Run it on loomcycle's compose/app network, **no host port** — only loomcycle reaches
it in-network. On TrueNAS put it in the **same compose as loomcycle** (separate custom
apps don't share a network). Socket-model service:

```yaml
services:
  builder-sidecar:
    image: denngubsky/loomcycle-builder-docker:latest   # or loomcycle-builder for the podman model
    container_name: builder-sidecar
    restart: unless-stopped
    environment:
      SANDBOX_PODMAN_BIN: docker                         # drive the host Docker (socket model)
      SANDBOX_AUTH_TOKEN: ${LOOMCYCLE_SANDBOX_TOKEN}     # shared secret (see below)
      SANDBOX_IMAGE: denngubsky/loomcycle-sandbox-session:latest
      SANDBOX_MAX_MEM_MB: "2048"
      SANDBOX_MAX_CPUS: "2"
      SANDBOX_SESSION_IDLE_TTL: "15m"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock        # socket model only
    # No ports: — in-network only.
```

Then point loomcycle at it (in the operator config or by selecting the `sandbox`
bundle — see `docs/SANDBOX.md`):

```yaml
mcp_servers:
  sandbox:
    transport: http
    url: http://builder-sidecar:9000/mcp
    headers:
      Authorization: "Bearer ${run.user_bearer:-${LOOMCYCLE_SANDBOX_TOKEN}}"
```

The **shared secret** (`LOOMCYCLE_SANDBOX_TOKEN`) goes in loomcycle's env and the
sidecar's `SANDBOX_AUTH_TOKEN` — set both to the same value. Per-run bearers, when
present, take precedence (the `${run.user_bearer:-…}` fallback form).

## Configuration (environment)

| Var | Default | Purpose |
|---|---|---|
| `SANDBOX_LISTEN_ADDR` | `:9000` | Listen address. |
| `SANDBOX_AUTH_TOKEN` | — (required) | Shared bearer every MCP request must present. |
| `SANDBOX_ALLOW_ANON` | `0` | `1` = run unauthenticated (LOCAL DEV ONLY). |
| `SANDBOX_IMAGE` | — (required) | The session toolchain image. |
| `SANDBOX_RUNTIME` | `""` | OCI runtime: `""`\|`runc`\|`crun`\|`runsc`\|`kata`. |
| `SANDBOX_CONTAINER_USER` | `1000:1000` | In-container user (never root). |
| `SANDBOX_ALLOW_EGRESS` | `0` | `1` permits `network:"egress"` sessions. |
| `SANDBOX_WORKSPACE_ROOT` | (unset) | Absolute host dir enabling **durable workspaces** — a session opened with `workspace:<name>` bind-mounts `<root>/<principal>/<name>` at `/work` instead of tmpfs (see below). Unset = tmpfs-only. |
| `SANDBOX_DEFAULT_TMPFS_MB` / `SANDBOX_MAX_TMPFS_MB` | `512` / `2048` | Workspace tmpfs size + ceiling. |
| `SANDBOX_MAX_CPUS` / `SANDBOX_MAX_MEM_MB` / `SANDBOX_MAX_PIDS` | `2` / `2048` / `512` | Per-session resource ceilings. |
| `SANDBOX_SESSION_IDLE_TTL` / `SANDBOX_SESSION_MAX_TTL` | `15m` / `1h` | Idle + absolute session TTL. |
| `SANDBOX_GC_INTERVAL` | `1m` | GC tick. |
| `SANDBOX_MAX_SESSIONS` | `32` | Concurrent session cap. |
| `SANDBOX_MAX_OUTPUT_BYTES` | `1048576` | Per-exec output cap. |

## Durable workspaces (persistent `/work`)

By default `/work` is an in-memory tmpfs that vanishes when the session container
is closed or reaped — great for one-shot tasks, wrong for long iterative dev
(you'd re-clone + rebuild every time, and a TTL/restart loses everything). Set
`SANDBOX_WORKSPACE_ROOT` to enable **durable workspaces**: a session opened with
`workspace:<name>` bind-mounts a persistent host dir at `/work`, so the checkout
and the build cache (`GOCACHE`/`CARGO_HOME`/`npm` cache all redirect onto `/work`)
**survive container close, reap, and sidecar restart** — reopen the same
`workspace` name to resume warm. The container is cattle; the work persists.

- **Fenced, never caller-controlled.** The host dir is derived as
  `<SANDBOX_WORKSPACE_ROOT>/<principal>/<name>`: the name is charset-gated
  (`[a-z0-9_-]`, no `/`/`.`/`..`), the principal comes from the bearer (so one
  principal can't reach another's workspace), and the resolved path is asserted
  strictly inside the root (symlink-escape defence) — the same posture as
  loomcycle's VolumeDef fencing. Unset root → a `workspace:` request is refused.
- **Docker-socket model — path identity matters.** Sessions are siblings on the
  *host* engine, so the bind-mount source is a *host* path. Mount your workspace
  root into the sidecar **at the same path** it has on the host (e.g.
  `-v /mnt/tank/loomcycle/workspaces:/mnt/tank/loomcycle/workspaces`) and set
  `SANDBOX_WORKSPACE_ROOT` to it, so the dir the sidecar creates is the dir the
  host engine mounts.
- **Ownership.** The session runs as `SANDBOX_CONTAINER_USER` (uid 1000); the
  sidecar `chown`s each workspace to it (works in the socket model, where the
  sidecar runs as root). On a rootless nested sidecar, pre-`chown` the root to
  `1000:1000`.
- **Retention is yours.** A durable workspace persists until you remove it
  (`sandbox_close` only removes the *container*). Back it with ZFS snapshots on
  TrueNAS; prune stale workspaces out of band.

## Security posture

- **Bearer required** on every request (constant-time check); the sidecar
  refuses to start without a token unless `SANDBOX_ALLOW_ANON=1`.
- **Every session container** launches `--network none` (unless egress is both
  operator-enabled and requested), `--read-only`, `--cap-drop=ALL`,
  `--security-opt no-new-privileges`, non-root user, with `--pids-limit` /
  `--memory` / `--cpus` caps and an in-memory `--tmpfs /work` (nothing the agent
  writes touches disk).
- **No host shell injection**: podman is exec'd directly (no host shell); file
  paths ride in env vars and content via stdin.
- **Session ownership**: each session is bound to the caller's principal; a
  leaked/guessed `session_id` from another principal never resolves. In this
  release the principal is the shared bearer (single-tenant); the per-tenant
  attested-identity binding is the next phase.

## Scope (this release)

Single shared bearer, TTL + explicit close for cleanup, `runc`/`runsc` runtimes,
(P2a) **durable workspaces** (above), and (P2b) the **`sandbox_touch`** keepalive +
**`sandbox_close_run`** bulk-teardown with attested `X-Loom-Root-Run` session
tagging. Deferred: **auto-reap on run-end** (a run-liveness poll or a loomcycle
run-end push — the absolute TTL is the current backstop), attested per-tenant
identity (multi-tenant isolation via the forwarded `X-Loom-Tenant`), and the Kata
microVM tier.

## Tests

```bash
go test ./...        # arg-construction, MCP wire contract, auth, GC — no podman needed
```

End-to-end (needs a podman host): build both images, run the sidecar, and drive
`sandbox_open` → `sandbox_exec` → `sandbox_read` → `sandbox_close` via loomcycle.
