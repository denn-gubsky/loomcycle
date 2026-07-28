# loomcycle — cloud deployment (self-contained Docker Compose)

A single Docker Compose stack that runs **everything** loomcycle needs and exposes
it publicly through Cloudflare — built for `cloud-home.local` (Ubuntu 25.04) but
portable to any Docker host. Unlike `deploy/truenas/` (which depends on external
TrueNAS apps for Postgres/SearXNG/Ollama), this bundles Postgres 18 + pgvector,
SearXNG, the code-exec sandbox, a Tailscale egress sidecar, a Caddy landing
server, and a Cloudflare tunnel — one `make up`.

> **Step-by-step setup lives in [`INSTALL.md`](INSTALL.md).** This file is the map.

## Architecture

```
Internet ─▶ Cloudflare edge ─▶ cloudflared ─┬─ app.loomcycle.cloud ─▶ tailscale:8787  (loomcycle UI/API)
                                             └─ loomcycle.cloud      ─▶ landing:80      (Caddy → cloud-web/)

compose network `loomnet`:
  postgres(pg18+pgvector)   loomcycle-migrate(one-shot)   builder-sidecar(:9000)
  searxng(:8080)            landing(caddy:80)             cloudflared
  tailscale ◀── loomcycle (network_mode: service:tailscale) ──▶ Ollama @ 100.x:11434 (tailnet)
```

### Services

| Service | Image | Role |
|---|---|---|
| `postgres` | `pgvector/pgvector:pg18` | Main store (`loomcycle`) + SQL-Memory aux (`loomcycle_sqlmem`). |
| `loomcycle-migrate` | `denngubsky/loomcycle:1.38.0` | One-shot `migrate up` on the main db, then exits. |
| `tailscale` | `tailscale/tailscale` | Kernel-mode egress to the tailnet; **owns the netns loomcycle shares**. |
| `builder-sidecar` | `denngubsky/loomcycle-builder-docker:1.25.2` | Sandboxed code exec (`mcp__sandbox__*`) via the host Docker socket. |
| `loomcycle` | `denngubsky/loomcycle:1.38.0` | The runtime. `network_mode: service:tailscale`. |
| `searxng` | `searxng/searxng` | Keyless search backend (wired via `config/loomcycle.yaml`). |
| `landing` | `caddy:2` | Serves `cloud-web/` + reproduces the page's `/api/health` + `/api/config` proxies. |
| `cloudflared` | `cloudflare/cloudflared` | Outbound tunnel; routes managed in the Cloudflare dashboard (token method). |

### The Tailscale netns wiring (important)

`loomcycle` uses `network_mode: service:tailscale` so it can reach a local Ollama
over the tailnet. Consequences baked into this compose:

- **loomcycle has no compose DNS name of its own** — everything that dials it uses
  **`http://tailscale:8787`** (the cloudflared dashboard route for
  `app.loomcycle.cloud`, and Caddy's `/api/*` proxy).
- loomcycle still reaches `postgres:5432`, `searxng:8080`, `builder-sidecar:9000`
  by name (tailscale is a normal member of `loomnet`).
- loomcycle reaches Ollama by **raw tailnet IP** (`OLLAMA_BASE_URL=http://100.x:11434`),
  not MagicDNS. `TS_ACCEPT_DNS=false` keeps the netns DNS usable for service names.
- loomcycle can't declare `ports:`; a local-debug `127.0.0.1:8787` (optional) goes
  on the **tailscale** service.

## Config model

Two env files on the host (see the `.example` templates):

- **`.env.insecure`** — non-secret operational settings (safe to read/commit).
- **`.env.secure`** — all secrets (`chmod 600`, never committed).

The `Makefile` passes **both** to `docker compose --env-file` so `${VAR}`
interpolation resolves; each sidecar reads only its own secret, while
loomcycle/migrate bulk-load both via `env_file:`. SearXNG is wired in
`config/loomcycle.yaml` (not env). See [`INSTALL.md`](INSTALL.md).

## Security posture (read before exposing publicly)

- **loomcycle stays bearer-authed** (`LOOMCYCLE_AUTH_TOKEN`); the apex landing is
  public. Consider a **Cloudflare Access** policy over `app.loomcycle.cloud` for
  defense-in-depth.
- **`LOOMCYCLE_HTTP_HOST_ALLOWLIST=*`** lets agents fetch any host — permissive on
  a public box. Tighten for production.
- **The builder-sidecar has effective host-root via the Docker socket** — it's the
  one privileged component by design. Kept in-network, bearer-authed, no host port.
- Real secrets are authored **on the host only**; this directory ships only
  `.example` templates.

## Quick start

```bash
# on cloud-home.local, from the deploy directory (e.g. /home/denn/work/loomcycle-cloud)
cp .env.insecure.example .env.insecure   # edit: OLLAMA_BASE_URL, PUBLIC_URL, …
cp .env.secure.example   .env.secure     # fill secrets; chmod 600 .env.secure
mkdir -p data work pgdata ts-state web retention-exports caddy-data caddy-config
sudo chown -R 65532:65532 data work retention-exports
cp -r /path/to/loomcycle/cloud-web/* web/    # the landing static files
make up && make ps
```

Full walkthrough (Tailscale key, Cloudflare tunnel + DNS, verification): **[`INSTALL.md`](INSTALL.md)**.
