# Deploying the builder sidecar — isolated code execution

A complete walkthrough for giving loomcycle agents a **safe, isolated, ephemeral
sandbox** to compile/test/run code in — Python / Go / Rust / C++ / Node — where
each session runs in its own throwaway container, `--network none`, `--read-only`,
resource-capped. loomcycle stays distroless and drives the sidecar over HTTP-MCP;
the sidecar owns all the container machinery.

> **Sidecar vs. toolbox image.** The [toolbox image](../../docs/TOOLBOX_IMAGE.md)
> runs code in loomcycle's OWN container (weak isolation, single-tenant). This
> sidecar runs each session in a **dedicated, isolated container** — the path for
> untrusted or multi-tenant code, and for running compiled binaries (`./a.out`
> works here; it can't via the toolbox's Bashbox fallback). You can run either or
> both. Concepts + tool reference: [`../../docs/SANDBOX.md`](../../docs/SANDBOX.md).

Targets **TrueNAS SCALE Electric Eel 24.10+** (Docker-based), but the shape is the
same on any Docker/compose host.

---

## Choose an engine model

The sidecar needs a container engine to launch session containers. Three ways:

| Model | How | Best for |
|---|---|---|
| **Host Docker socket** (recommended for TrueNAS) | The sidecar drives the **host's** Docker over a mounted socket; sessions are **sibling** containers on the host. No nested engine, no Sysbox, no `privileged`. Image: [`Dockerfile.docker`](Dockerfile.docker), `SANDBOX_PODMAN_BIN=docker`. | TrueNAS + any Docker host. Simplest + most reliable. |
| **Nested rootless podman** | The sidecar runs its OWN podman inside itself; fully self-contained (sessions never touch the host engine). Image: [`Dockerfile`](Dockerfile). Needs **Sysbox** (secure) or **`privileged: true`** (TrueNAS has no Sysbox → privileged). | Stronger self-containment; non-Docker hosts. |
| **Dedicated VM** | Run the sidecar (either model) in a TrueNAS **VM** with its own Docker/podman; reach it over the network. The engine privilege is VM-confined — the strongest isolation of the sidecar itself. | Hardening the sidecar's own blast radius. |

This guide leads with the **host Docker socket** model; the nested-podman variant is
in [§ Alternative: nested podman](#alternative-nested-rootless-podman).

> ⚠️ **The sidecar is the privileged component.** Mounting the Docker socket (or
> running `privileged`) lets it control the host engine — effectively host root.
> That is by design: the sidecar does the container orchestration loomcycle must
> not. A compromised loomcycle still only reaches the constrained `sandbox_*` API,
> never the socket. Keep the sidecar **in-network (no host port), bearer-authed,
> single-tenant / trusted**. For a real trust boundary, use the VM model.

---

## Prerequisites

- A running loomcycle on TrueNAS (see [`../truenas/INSTALL.md`](../truenas/INSTALL.md)).
- A machine with **Docker** to build + push the two images (your dev box, or
  `cloud-home`). TrueNAS itself doesn't build images.
- A container registry the TrueNAS host can pull from — Docker Hub (`denngubsky/…`)
  or a private registry.

---

## Phase 1 — Build + push the two images

Two images are involved: the **sidecar** (runs the MCP server + drives the engine)
and the **session** image (the toolchain each sandbox runs). From a checkout of
this repo on your Docker box:

```sh
VER=1.23.1     # match your loomcycle version, or any tag you like

# 1) the sidecar — host-Docker-socket variant (Dockerfile.docker)
docker build -t denngubsky/loomcycle-builder-docker:$VER \
  -f deploy/builder/Dockerfile.docker --build-arg VERSION=$VER deploy/builder
docker push denngubsky/loomcycle-builder-docker:$VER

# 2) the session toolchain image (python/go/rust/c++/node, uid 1000)
docker build -t denngubsky/loomcycle-sandbox-session:$VER deploy/builder/session
docker push denngubsky/loomcycle-sandbox-session:$VER
```

- Replace `denngubsky/…` with your own registry namespace if not using Docker Hub.
- The **session image is pulled by the host Docker** (it runs the siblings), so it
  must be reachable from TrueNAS. If your registry is private, `docker login` on the
  TrueNAS host once so it can pull.
- Building the session image for `linux/amd64` is enough for a TrueNAS/amd64 box
  (add `--platform linux/amd64` if you build on Apple Silicon).

---

## Phase 2 — A shared secret

The sidecar authenticates every MCP call with a bearer. Generate one:

```sh
openssl rand -hex 32     # → the sandbox shared secret
```

You'll set **`SANDBOX_AUTH_TOKEN`** to the same value in **both** the sidecar and
loomcycle — one env var name, shared by both services. Keep it in a root-only env file
on the TrueNAS host, e.g. a tiny sidecar file plus a line in loomcycle's existing
secrets file:

```sh
# sidecar-only secret (keep loomcycle's PG DSN / provider keys OUT of the sidecar)
printf 'SANDBOX_AUTH_TOKEN=%s\n' "$THE_SECRET" \
  > /mnt/tank/loomcycle/config/sandbox.secrets.env
chmod 600 /mnt/tank/loomcycle/config/sandbox.secrets.env

# and add the same value to loomcycle's secrets file (Phase 4 of the loomcycle install)
echo "SANDBOX_AUTH_TOKEN=$THE_SECRET" \
  >> /mnt/tank/loomcycle/config/loomcycle.secrets.env
```

(As with the loomcycle app, TrueNAS's *Install via YAML* can't resolve `${VAR}`, so
secrets ride in `env_file:` — an absolute host path read at container-create.)

---

## Phase 3 — Add the sidecar to your loomcycle app's compose

**On TrueNAS, two separate custom apps do NOT share a Docker network** — so loomcycle
couldn't reach a standalone sidecar app by name. Add the sidecar as **another service
in the same compose** as loomcycle (Apps → your loomcycle app → **Edit**), so they
share the app network and loomcycle can dial `http://builder-sidecar:9000`.

Add this service alongside `loomcycle` / `loomcycle-migrate`:

```yaml
  builder-sidecar:
    image: denngubsky/loomcycle-builder-docker:1.23.1   # your Phase-1 sidecar image
    restart: unless-stopped
    env_file:
      - /mnt/tank/loomcycle/config/sandbox.secrets.env   # SANDBOX_AUTH_TOKEN
    environment:
      SANDBOX_PODMAN_BIN: docker                          # drive the HOST Docker
      SANDBOX_IMAGE: denngubsky/loomcycle-sandbox-session:1.23.1   # host-pullable
      # resource ceilings (per session; agents can ask for less, never more)
      SANDBOX_MAX_MEM_MB: "2048"
      SANDBOX_MAX_CPUS: "2"
      SANDBOX_MAX_TMPFS_MB: "2048"
      SANDBOX_SESSION_IDLE_TTL: "15m"
      SANDBOX_SESSION_MAX_TTL: "1h"
      # SANDBOX_ALLOW_EGRESS: "1"        # allow network:"egress" sessions (default: no network)
      # SANDBOX_RUNTIME: runsc           # only if gVisor is installed on the HOST Docker
    volumes:
      # The host Docker socket — verify the path on your TrueNAS (usually this).
      - /var/run/docker.sock:/var/run/docker.sock
    # No ports: — only loomcycle reaches it in-network at http://builder-sidecar:9000.
```

Notes:
- **Service name matters.** The `sandbox` bundle points at `http://builder-sidecar:9000/mcp`,
  so keep the service named `builder-sidecar` (or override the URL — Phase 4).
- **Socket path.** `/var/run/docker.sock` is the usual TrueNAS location; confirm with
  `ls -l /var/run/docker.sock` on the host. If your TrueNAS exposes it elsewhere, mount
  that path to `/var/run/docker.sock` in the container.
- **No `user:` line.** The sidecar's docker client needs to read the socket (owned by
  root/`docker` group); run it as root inside its own container (the default). It only
  ever issues the constrained engine calls the sandbox tools make.

### Optional — durable workspaces (persistent `/work`)

For long-running iterative dev, enable **durable workspaces** so a checkout + build
cache survive container churn/restarts (P2a). Add to the `builder-sidecar` service:

```yaml
    environment:
      SANDBOX_WORKSPACE_ROOT: /mnt/tank/loomcycle/sandbox-workspaces
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      # Docker-socket model: mount the workspace root at the SAME host path so the
      # dir the sidecar creates is the dir the host engine bind-mounts into sessions.
      - /mnt/tank/loomcycle/sandbox-workspaces:/mnt/tank/loomcycle/sandbox-workspaces
```

Create + own the root (`mkdir -p …; chown -R 1000:1000 …` so the uid-1000 session
user can write — the sidecar also chowns per-workspace best-effort). Then an agent
opens `sandbox_open {workspace:"my-project"}` and reopens the same name to resume
warm. Full detail: [`README.md`](README.md#durable-workspaces-persistent-work).

---

## Phase 4 — Wire loomcycle to the sidecar

On the **loomcycle** service, two changes:

1. Add the **`sandbox`** bundle to your presets — it registers the `dev/sandbox`
   agent + skill and the `mcp_servers.sandbox` block that dials the sidecar:
   ```yaml
   LOOMCYCLE_PRESETS: "base,document-agent,chat,agent-teams,team-examples,sandbox"
   ```
2. Ensure `SANDBOX_AUTH_TOKEN` is set (Phase 2 added it to
   `loomcycle.secrets.env`, which the loomcycle service already reads via `env_file`).
   The bundle's MCP header is `Authorization: Bearer ${SANDBOX_AUTH_TOKEN}` — the
   same env var name the sidecar authenticates against, so both services share one
   secret under one name.

That's it — the bundle carries the `mcp_servers.sandbox` URL (`http://builder-sidecar:9000/mcp`).
If your sidecar service has a different name/port, override it in your config overlay:

```yaml
mcp_servers:
  sandbox:
    transport: http
    url: http://builder-sidecar:9000/mcp
    headers:
      Authorization: "Bearer ${SANDBOX_AUTH_TOKEN}"
```

Redeploy the app (Save). loomcycle waits on migrate, boots, and the MCP pool connects
to the sidecar (lazy-retries if the sidecar starts a moment later — non-fatal).

---

## Phase 5 — Verify

1. **Sidecar up:** in the app logs the `builder-sidecar` service prints
   `loomcycle-builder <ver> listening on :9000 (... runtime="" ...)`.
2. **Tools discovered:** the loomcycle logs show the `sandbox` MCP server connecting
   and registering `mcp__sandbox__*` (or check the Web UI → Integrations / Library).
3. **End-to-end:** start the **`dev/sandbox`** agent (Web UI `/run`, or `POST /v1/runs`)
   and ask it to *"open a sandbox, write a Go hello-world, build and run it, and show
   the output."* It should `sandbox_open` → `sandbox_write` → `sandbox_exec "go run ."`
   → return the output → `sandbox_close`.
4. **Isolation spot-check** (on the TrueNAS host shell, during a run):
   ```sh
   docker ps --filter label=loomcycle.managed=1     # a loom-sbx-… sibling appears
   ```
   It shows `--network none`; after the run (or the TTL) it's gone. Inside a session,
   `sandbox_exec "curl https://example.com"` fails unless you set `SANDBOX_ALLOW_EGRESS=1`
   and open the session with `network:"egress"`.

---

## Alternative: nested rootless podman

Prefer the sidecar to run its OWN engine (sessions never touch the host Docker)? Use
the [`Dockerfile`](Dockerfile) image (podman/stable) instead, and give the service the
nesting capability. On TrueNAS (no Sysbox) that means `privileged: true`:

```sh
docker build -t denngubsky/loomcycle-builder:1.23.1 \
  --build-arg VERSION=1.23.1 deploy/builder && docker push denngubsky/loomcycle-builder:1.23.1
```

```yaml
  builder-sidecar:
    image: denngubsky/loomcycle-builder:1.23.1
    restart: unless-stopped
    privileged: true          # nested podman needs it on a host without Sysbox
    env_file:
      - /mnt/tank/loomcycle/config/sandbox.secrets.env
    environment:
      # SANDBOX_PODMAN_BIN defaults to "podman" — leave it unset here
      SANDBOX_IMAGE: denngubsky/loomcycle-sandbox-session:1.23.1   # pulled into the sidecar's OWN podman
      SANDBOX_MAX_MEM_MB: "2048"
      SANDBOX_MAX_CPUS: "2"
    # No docker socket, no host port.
```

Trade-offs vs. the socket model: no socket mount, sessions are fully inside the
sidecar — but `privileged: true` is a broad grant, and nested rootless podman can hit
storage-driver quirks on some kernels. If sessions fail to start, prefer the socket
model or the VM model. (Sysbox, where available, replaces `privileged: true` with a
secure nested runtime — `runtime: sysbox-runc` — but TrueNAS SCALE doesn't ship it.)

---

## Security posture

- **The sidecar is the one privileged component** (docker socket ≈ host root, or
  `privileged`). Keep it **in-network, no host port**, bearer-authed, and single-tenant
  / trusted. For a hard boundary around the sidecar itself, run it in a dedicated VM.
- **Every session container** launches `--network none` (egress only when both the
  operator enables it and the caller asks), `--read-only` rootfs, an in-memory
  `--tmpfs /work`, `--cap-drop=ALL`, `no-new-privileges`, a non-root user, and
  cpu/mem/pids caps.
- **Session ownership** is bound to the calling bearer's principal — a leaked
  `session_id` from another principal never resolves.
- **Cleanup** is automatic: idle/absolute TTL plus a boot sweep of any leftover
  `loomcycle.managed=1` containers.

---

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| Sidecar exits at start: `SANDBOX_IMAGE is required` / `SANDBOX_AUTH_TOKEN is required` | Set both — `SANDBOX_IMAGE` (env) and `SANDBOX_AUTH_TOKEN` (the `env_file`). For local dev only, `SANDBOX_ALLOW_ANON=1` skips auth (logs a warning). |
| `mcp__sandbox__*` tools never appear in loomcycle | (a) the sidecar isn't in the SAME compose/network → loomcycle can't resolve `builder-sidecar`; (b) token mismatch → the sidecar 401s (check `SANDBOX_AUTH_TOKEN` == `SANDBOX_AUTH_TOKEN`); (c) the `sandbox` preset isn't in `LOOMCYCLE_PRESETS`. |
| `sandbox_open` fails: `permission denied` on `/var/run/docker.sock` | The socket path is wrong or not mounted, or the container user can't read it — verify `ls -l /var/run/docker.sock` on the host and the `volumes:` mount; run the sidecar as root (no `user:` line). |
| `sandbox_open` fails: image pull / `no such image` | The HOST Docker can't pull `SANDBOX_IMAGE` — confirm you pushed the session image and, if the registry is private, `docker login` on the TrueNAS host. |
| Nested-podman sessions fail to start (podman model) | Nested rootless podman storage/kernel quirk — switch to the host-Docker-socket model, or run the sidecar in a VM. |
| `curl`/network fails inside a session | Expected — sessions are `--network none` by default. Set `SANDBOX_ALLOW_EGRESS=1` on the sidecar AND open the session with `network:"egress"`. |
| A leftover `loom-sbx-…` container lingers | The TTL sweeper reaps it (default 15m idle / 1h absolute); a sidecar restart also boot-sweeps `loomcycle.managed=1` containers. |

---

## Configuration reference

All sidecar knobs are in [`README.md`](README.md#configuration-environment) —
`SANDBOX_PODMAN_BIN` (podman|docker), `SANDBOX_IMAGE`, `SANDBOX_RUNTIME`,
`SANDBOX_ALLOW_EGRESS`, the `SANDBOX_MAX_*` ceilings, and the session TTLs.
