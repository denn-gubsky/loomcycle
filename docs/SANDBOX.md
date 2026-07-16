# Sandboxed code execution

There are two ways to let a loomcycle agent run code, and they trade off
convenience against isolation:

- The **`loomcycle-toolbox` image** ([docs/TOOLBOX_IMAGE.md](TOOLBOX_IMAGE.md))
  bakes a toolchain into loomcycle's own container. Simple, but code runs *as
  loomcycle* — single-tenant / trusted only.
- The **builder sidecar** (this doc) runs each code-execution session in a
  **separate, isolated, ephemeral container** — network-off, read-only rootfs,
  resource-capped, in-memory workspace. loomcycle stays distroless and never runs
  a container engine; it drives the sidecar over HTTP-MCP.

**Thesis:** use the toolbox image for a quick trusted setup; use the builder
sidecar when the code is untrusted or the deployment is multi-tenant.

## Why a sidecar (not loomcycle itself)

loomcycle ships distroless and non-root (uid 65532): it *cannot* run rootless
podman (no subuid range / `newuidmap`), and mounting a host podman socket into
the process that runs model-authored code would be ≈ host root. So the engine
lives in a sidecar, and loomcycle reaches it the one distroless-safe way it
reaches any external capability — MCP over HTTP. A compromised loomcycle can only
call the constrained `sandbox_*` API; it can never craft a privileged container.

```
agent (loomcycle · distroless)
   │  mcp__sandbox__sandbox_exec        (HTTP-MCP, bearer-authed, in-network)
   ▼
builder-sidecar                    ──►  per-session container
   rootless podman + tmpfs/runsc         --network none --read-only
                                         --cap-drop=ALL --tmpfs /work
```

## Quick start

1. **Build the images** (from `deploy/builder/`):

   ```bash
   docker build -t denngubsky/loomcycle-builder:latest deploy/builder
   docker build -t localhost/loomcycle-sandbox-session:latest deploy/builder/session
   ```

2. **Set a shared secret.** Add `LOOMCYCLE_SANDBOX_TOKEN=<openssl rand -hex 32>`
   to your `.env.local` — it authenticates loomcycle → sidecar. (It's referenced
   by *name* in the config header below; loomcycle allows `${LOOMCYCLE_*}`
   interpolation into an MCP header.)

3. **Deploy the sidecar** on loomcycle's compose network (uncomment the
   `builder-sidecar` block in `docker-compose.example.yaml`), passing the same
   secret as `SANDBOX_AUTH_TOKEN` and the session image as `SANDBOX_IMAGE`.
   Nested podman needs a capable host runtime — **Sysbox** (secure) is preferred;
   `privileged: true` is the fallback (e.g. on TrueNAS, which lacks Sysbox).

4. **Enable the bundle:** `LOOMCYCLE_PRESETS=base,sandbox`. That registers the
   `dev/sandbox` agent + skill and the `sandbox` MCP server:

   ```yaml
   mcp_servers:
     sandbox:
       transport: http
       url: http://builder-sidecar:9000/mcp
       headers:
         Authorization: "Bearer ${run.user_bearer:-${LOOMCYCLE_SANDBOX_TOKEN}}"
   ```

   (Selecting the bundle supplies this block; re-declare `url` in your overlay if
   the sidecar isn't at `builder-sidecar:9000`.)

5. **Run it.** Start the `dev/sandbox` agent (or grant `mcp__sandbox__*` to your
   own agent) and ask it to compile/test something. It opens a session, writes
   files, builds, tests, reads the artifact, and closes.

## Tools

| Tool | Purpose |
|---|---|
| `sandbox_open` | Create a session → `session_id`. Params (clamped to operator ceilings): `network` (`none`/`egress`), `tmpfs_mb`, `cpu`, `mem_mb`, `pids`. |
| `sandbox_exec` | Run a command in the session's `/work`; returns combined output + exit code. |
| `sandbox_write` / `sandbox_read` | Files in / artifacts out (relative to `/work`; `base64` for binary). |
| `sandbox_close` / `sandbox_list` | Destroy / enumerate your sessions. |

A session is one long-lived container — open once, run many commands across a
compile→test→fix loop (workspace + build cache persist), close when done.
Sessions also expire on an idle/absolute TTL, and orphans are reaped at sidecar
startup.

## Sidecar configuration

See the environment reference in
[`deploy/builder/README.md`](../deploy/builder/README.md#configuration-environment)
— `SANDBOX_AUTH_TOKEN` (required), `SANDBOX_IMAGE` (required), `SANDBOX_RUNTIME`
(`runc`/`crun`/`runsc`/`kata`), the `SANDBOX_MAX_*` ceilings, `SANDBOX_ALLOW_EGRESS`,
and the session TTLs.

## Isolation posture

Every session container: `--network none` (egress only when operator-enabled AND
requested), `--read-only` rootfs, in-memory `--tmpfs /work` (nothing written
touches disk), `--cap-drop=ALL`, `no-new-privileges`, non-root user, and
`--pids-limit`/`--memory`/`--cpus` caps. For code you truly don't trust, install
gVisor in the sidecar image and set `SANDBOX_RUNTIME=runsc` for a user-space
kernel boundary (Kata microVMs are a stronger, heavier option).

The sidecar is the one privileged component — keep it in-network (no host port),
bearer-authed, and behind Sysbox or an accepted `privileged` grant.

## Scope

This is the first phase: a single shared bearer (single-tenant), TTL + explicit
`sandbox_close` for cleanup, and the `runc`/`runsc` runtimes. Attested
per-tenant identity (multi-tenant isolation), run-liveness-poll GC, the Kata
tier, and a bind-mounted shared workspace are planned follow-ups.
