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

1. **Get the images.** Prebuilt multi-arch images are published to Docker Hub on
   each release (`.github/workflows/publish-sandbox-images.yml`) — pull the tag that
   matches your loomcycle version (or `:latest`):

   ```bash
   docker pull denngubsky/loomcycle-builder-docker:latest   # host-Docker-socket sidecar
   # or the rootless-podman sidecar: denngubsky/loomcycle-builder:latest
   docker pull denngubsky/loomcycle-sandbox-session:latest  # per-session toolchain
   ```

   …or build them yourself from `deploy/builder/`:

   ```bash
   docker build -t denngubsky/loomcycle-builder:latest deploy/builder
   docker build -t localhost/loomcycle-sandbox-session:latest deploy/builder/session
   ```

2. **Set a shared secret.** Add `SANDBOX_AUTH_TOKEN=<openssl rand -hex 32>`
   to your `.env.local` — it authenticates loomcycle → sidecar. (It's referenced
   by *name* in the config header below; loomcycle allows `${LOOMCYCLE_*}`
   interpolation into an MCP header.)

3. **Deploy the sidecar** on loomcycle's compose network (uncomment the
   `builder-sidecar` block in `docker-compose.example.yaml`), passing the same
   secret as `SANDBOX_AUTH_TOKEN` and the session image as `SANDBOX_IMAGE`.
   Nested podman needs a capable host runtime — **Sysbox** (secure) is preferred;
   `privileged: true` is the fallback (e.g. on TrueNAS, which lacks Sysbox).

4. **Enable the bundles:** `LOOMCYCLE_PRESETS=base,sandbox,dev-exec`. `sandbox`
   registers the `sandbox` MCP server + the `dev/sandbox-usage` delegation skill;
   `dev-exec` registers the deterministic code-js **`dev/exec`** agent (needs
   `LOOMCYCLE_CODE_AGENTS_ENABLED=1`):

   ```yaml
   mcp_servers:
     sandbox:
       transport: http
       url: http://builder-sidecar:9000/mcp
       headers:
         Authorization: "Bearer ${SANDBOX_AUTH_TOKEN}"
   ```

   (Selecting the bundle supplies this block; re-declare `url` in your overlay if
   the sidecar isn't at `builder-sidecar:9000`.)

5. **Run it.** Spawn the `dev/exec` agent with a JSON command envelope (or grant
   `mcp__sandbox__*` to your own agent), e.g.
   `{"commands":["git clone https://github.com/OWNER/REPO .","go build ./... && go test ./..."]}`.
   It opens a session, writes any `files`, runs the `commands`, reads back any
   `read` artifacts, and closes — returning structured `results` for you to act on.

## Tools

| Tool | Purpose |
|---|---|
| `sandbox_open` | Create a session → `session_id`. Params (clamped to operator ceilings): `network` (`none`/`egress`), `tmpfs_mb`, `cpu`, `mem_mb`, `pids`, and `workspace` (a **durable** `/work` — see below). |
| `sandbox_exec` | Run a command in the session's `/work`; returns combined output + exit code. |
| `sandbox_write` / `sandbox_read` | Files in / artifacts out (relative to `/work`; `base64` for binary). |
| `sandbox_close` / `sandbox_list` | Destroy / enumerate your sessions. |

A session is one long-lived container — open once, run many commands across a
compile→test→fix loop (workspace + build cache persist), close when done.
Sessions also expire on an idle/absolute TTL, and orphans are reaped at sidecar
startup.

### Durable workspaces (persistent `/work`)

By default `/work` is tmpfs — it dies with the container, so a TTL/reap/restart
loses the checkout + build cache. For **long-running iterative dev**, set
`SANDBOX_WORKSPACE_ROOT` on the sidecar and open with a `workspace` name:
`sandbox_open {workspace:"my-project"}` bind-mounts a persistent host dir at
`/work`. The container becomes disposable — reopen the same `workspace` name (even
after a reap or a sidecar restart) to resume warm. The host dir is fenced as
`<root>/<principal>/<name>` (charset-gated name, per-principal subtree,
symlink-escape-checked — never a caller path). Docker-socket model: mount the
workspace root into the sidecar at the **same host path** so the dir the sidecar
creates is the dir the host engine bind-mounts. Full detail:
[`../deploy/builder/README.md`](../deploy/builder/README.md#durable-workspaces-persistent-work).

### Authenticated git / gh (private repos)

By default a session has no credentials, so only **public** repos clone. To do
real dev — clone/push a **private** repo, use `gh` — inject a GitHub token:

1. Enable env injection on the sidecar: `SANDBOX_ALLOW_ENV_INJECTION=1`.
2. Store the token as a per-tenant/user credential named **`sandbox_github`**
   (`credentialdef op=create scope=user name=sandbox_github value=<token>`) — a
   PAT / fine-grained token. For a **short-lived, repo-scoped** token instead,
   store a GitHub App config and reference it with `$ghapp:<name>` (see
   [CREDENTIALS.md](CREDENTIALS.md#minting-a-github-app-token----ghappname)).
3. The `sandbox` bundle already forwards it: `mcp_servers.sandbox.headers` carries
   `X-Loom-Sandbox-Env-Gh-Token: "$cred:sandbox_github"`, which loomcycle resolves
   **server-side** (never in the model) and the sidecar maps to `GH_TOKEN` in the
   session env. `dev/exec` runs `gh auth setup-git` at session open, so `git` over
   HTTPS uses it too.

Unresolved (no such credential, or injection disabled) → the header is dropped →
git runs unauthenticated, public repos still work. Because the token lives in the
session env with egress on, prefer a **short-lived, repo-scoped GitHub App token**
(`$ghapp:` — overlay the header value to `$ghapp:sandbox_github_app`) over a broad
PAT. Full mechanism + caps:
[`../deploy/builder/README.md`](../deploy/builder/README.md#env-injection-credentials-into-a-session).

### Headless browser (web-UI testing)

To give the agent a browser (`mcp__browser__*`) for ad-hoc web-UI testing, run the
runtime as the **`denngubsky/loomcycle-browser`** image (the standard loomcycle
image + the [PinchTab](https://github.com/pinchtab/pinchtab) MCP client, published
alongside every release) and add a **`pinchtab/pinchtab` server sidecar**. PinchTab's
MCP is stdio-only, so loomcycle spawns `pinchtab … mcp` as a stdio bridge to the
sidecar — Chromium lives only in the sidecar, not in loomcycle.

1. Run the sidecar **internal only** (no published port — its control plane is not
   for public exposure), with `PINCHTAB_TOKEN` set, `shm_size: 2gb`, and the usual
   hardening (`read_only`, tmpfs, `cap_drop: ALL`, `no-new-privileges`).
2. Use `denngubsky/loomcycle-browser:<version>` for the loomcycle runtime and give
   it the same `PINCHTAB_TOKEN` (the stdio child inherits loomcycle's env).
3. Overlay the config (a file in `LOOMCYCLE_CONFIG_DIR`) to DECLARE the browser
   server — the `dev/exec` agent already grants `mcp__browser__*` in its ceiling and
   drives it via the envelope's optional `browser` steps, so no agent override is
   needed:
   ```yaml
   mcp_servers:
     browser:
       transport: stdio
       command: /usr/local/bin/pinchtab
       args: ["--server", "http://pinchtab:9867", "mcp"]
   ```

Scope: reachable URLs (public sites / your deployment / preview URLs). PinchTab
keeps browsing local-only until you widen its domain allowlist (IDPI). A full worked
example is in [`../cloud-deployment/`](../cloud-deployment/).

## Delegating from other agents

An agent doesn't hold the `mcp__sandbox__*` tools directly (by design — tool ceilings
are per operator-vetted agent, and skills can't widen them). To run code, it
**delegates to `dev/exec`** via the `Agent` tool. `dev/exec` is a DETERMINISTIC
code-js executor (no LLM): you send it a JSON **command envelope** and it runs the
steps in an isolated container and returns STRUCTURED results — the judgment (read a
failure, decide the fix, re-send) stays with the caller. The `sandbox` bundle ships a
**`dev/sandbox-usage` skill** teaching exactly this, and the bundled **`chat/*` agents
get the `Agent` tool**, so with `LOOMCYCLE_PRESETS=…,chat,sandbox,dev-exec` a chat
agent can:

- **one-shot:** `Agent {op:"spawn", name:"dev/exec", prompt:"{\"commands\":[\"git clone https://github.com/OWNER/REPO .\",\"go build ./... && go test ./...\"]}"}` — read `results`, and if a command failed, spawn again with corrected `commands`/`files`; or
- **multi-step:** first envelope with `"keep_open": true` → capture `session_id` → pass it back on later envelopes (same warm container) → finish with `"close": true`.

To let your own agent delegate, grant it the `Agent` tool (and, optionally, put
`dev/sandbox-usage` in its `skills:`). To let an agent run the sandbox *itself* rather
than delegate, grant it the `mcp__sandbox__*` tools directly. (`dev/exec` needs the
`dev-exec` bundle + `LOOMCYCLE_CODE_AGENTS_ENABLED=1`.)

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

Shipped: a single shared bearer (single-tenant), TTL + explicit `sandbox_close`
cleanup, the `runc`/`runsc` runtimes, and **durable workspaces** (above). Planned
follow-ups: a keepalive + run-liveness-poll GC + `sandbox_close_run`, attested
per-tenant identity (multi-tenant isolation), and the Kata microVM tier.
