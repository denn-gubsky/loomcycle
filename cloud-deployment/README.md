# loomcycle — cloud deployment (self-contained Docker Compose)

A single Docker Compose stack that runs **everything** loomcycle needs and exposes
it publicly through Cloudflare — built for `cloud-home.local` (Ubuntu 25.04) but
portable to any Docker host. Unlike `deploy/truenas/` (which depends on external
TrueNAS apps for Postgres/SearXNG/Ollama), this bundles Postgres 18 + pgvector,
SearXNG, the code-exec sandbox, a Tailscale egress sidecar, a **Node landing
server** (static site + self-serve token minting), and a Cloudflare tunnel — one
`make up`.

> **Step-by-step setup lives in [`INSTALL.md`](INSTALL.md).** This file is the map.

## Architecture

```
Internet ─▶ Cloudflare edge ─▶ cloudflared ─┬─ app.loomcycle.cloud ─▶ tailscale:8787  (loomcycle UI/API)
              (+ Access on         │         └─ loomcycle.cloud      ─▶ landing:8080    (Node → cloud-web/ + /api/mint)
               loomcycle.cloud/api)│
compose network `loomnet`:
  postgres(pg18+pgvector)   loomcycle-migrate(one-shot)   builder-sidecar(:9000)
  searxng(:8080)            landing(node:8080)            cloudflared
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
| `landing` | Node (`./landing`, built) | Serves `cloud-web/`, proxies the public reads `/healthz` + `/v1/config` (inner links), and mints `substrate:tenant` tokens behind Cloudflare Access. |
| `cloudflared` | `cloudflare/cloudflared` | Outbound tunnel; routes managed in the Cloudflare dashboard (token method). |

### The landing server + self-serve minting

`landing/server.js` (Node) does three things:

1. **Serves the static marketing site** (`cloud-web/`, bind-mounted at `/srv`, with
   HTTP range so the videos stream).
2. **Proxies the runtime's public reads as same-origin "inner links"** — a
   hardcoded **allowlist** (`GET /healthz` + `GET /v1/config`, the only reads the
   page makes) forwarded to `http://tailscale:8787`. It is deliberately an
   allowlist forwarding fixed upstream paths — **not** a blanket `/v1/*` proxy with
   a `/v1/_*` denylist, which a `..`/`%5f` bypass could slip past — so the
   operator-admin plane is never reachable on the apex. The bearer-gated app API
   stays on `app.loomcycle.cloud`.
3. **Mints `substrate:tenant` tokens** for a Cloudflare-Access-authenticated
   visitor. `POST /api/mint` verifies the Access JWT (`Cf-Access-Jwt-Assertion`
   against the team JWKS + AUD), derives a stable `(tenant, subject)` from the
   verified email, and calls `POST /v1/_operatortokendef` with the **admin token
   held server-side** (`LOOMCYCLE_ADMIN_TOKEN`). The browser never sees the admin
   token and **cannot choose its own op/scopes/tenant** — the proxy forces
   `op=create` + `MINT_SCOPES` (`substrate:tenant,runs:create`). The once-shown
   token is returned, vaulted client-side (IndexedDB AES-GCM), and handed to
   `app.loomcycle.cloud` over an origin-pinned `postMessage`.

> **Why a Node backend and not a Caddy header-inject:** a raw reverse-proxy that
> injects the admin bearer onto `/v1/_operatortokendef` would forward the client's
> body verbatim — a visitor could POST `{scopes:["substrate:admin"]}` and mint an
> admin token. The backend constrains the request; that's the whole point.

### The Tailscale netns wiring (important)

`loomcycle` uses `network_mode: service:tailscale` so it can reach a local Ollama
over the tailnet. Consequences baked into this compose:

- **loomcycle has no compose DNS name of its own** — everything that dials it uses
  **`http://tailscale:8787`** (the cloudflared route for `app.loomcycle.cloud`, and
  the landing's `/v1` + `/healthz` proxy target).
- loomcycle still reaches `postgres:5432`, `searxng:8080`, `builder-sidecar:9000`
  by name (tailscale is a normal member of `loomnet`).
- loomcycle reaches Ollama by **raw tailnet IP** (`OLLAMA_BASE_URL=http://100.x:11434`),
  not MagicDNS. `TS_ACCEPT_DNS=false` keeps the netns DNS usable for service names.
- loomcycle can't declare `ports:`; a local-debug `127.0.0.1:8787` (optional) goes
  on the **tailscale** service.

## Config model

Two env files on the host:

- **`.env.insecure`** — non-secret operational settings (copy from
  [`.env.insecure.example`](.env.insecure.example); safe to read/commit).
- **`.env.secure`** — all secrets (`chmod 600`, **never committed**). There is no
  committed `.env.secure.example`; the full secret set is documented in
  [`INSTALL.md`](INSTALL.md) (repo policy: no `.env.secure*` file in git).

The `Makefile` passes **both** to `docker compose --env-file` so `${VAR}`
interpolation resolves. **Secrets are scoped**: `loomcycle`/`migrate` get only
their own secrets via explicit `${VAR}`; each sidecar reads just its one secret.
SearXNG is wired in `config/loomcycle.yaml` (not env).

## Security posture (read before exposing publicly)

- **The mint route is gated by Cloudflare Access** (Google). The landing verifies
  the Access JWT (never a bare header) and forces `substrate:tenant` (never admin).
- **The landing holds `LOOMCYCLE_ADMIN_TOKEN`** (= your `LOOMCYCLE_AUTH_TOKEN`) —
  server-side only, no host port, reachable only via the tunnel. Treat it as the
  most sensitive component after loomcycle itself.
- **loomcycle stays bearer-authed** (`LOOMCYCLE_AUTH_TOKEN`); the marketing landing
  is public but its `/api/*` is Access-gated. Consider Access over
  `app.loomcycle.cloud` too for defense-in-depth.
- **`LOOMCYCLE_HTTP_HOST_ALLOWLIST=*`** lets agents fetch any host — permissive on
  a public box. Tighten for production.
- **The builder-sidecar has effective host-root via the Docker socket** — the one
  privileged component by design; in-network, bearer-authed, no host port.
- Real secrets are authored **on the host only**; the repo ships no secret file.

## Quick start

```bash
# on cloud-home.local, from the deploy directory (e.g. /home/denn/work/loomcycle-cloud)
cp .env.insecure.example .env.insecure     # edit: OLLAMA_BASE_URL, CF_ACCESS_*, …
# create .env.secure from the template in INSTALL.md, then:  chmod 600 .env.secure
mkdir -p data work pgdata ts-state web retention-exports
sudo chown -R 65532:65532 data work retention-exports
cp -r /path/to/loomcycle/cloud-web/* web/  # the landing static files
make up && make ps
```

Full walkthrough (Tailscale key, Cloudflare tunnel + **Access** + DNS, the
`.env.secure` template, verification): **[`INSTALL.md`](INSTALL.md)**.
