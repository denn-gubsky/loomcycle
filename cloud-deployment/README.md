# loomcycle — cloud deployment (self-contained Docker Compose)

A single Docker Compose stack that runs **everything** loomcycle needs and exposes
it publicly through Cloudflare — built for `cloud-home.local` (Ubuntu 25.04) but
portable to any Docker host. Unlike `deploy/truenas/` (which depends on external
TrueNAS apps for Postgres/SearXNG/Ollama), this bundles Postgres 18 + pgvector,
SearXNG, the code-exec sandbox, a Tailscale egress sidecar, a **Node landing
server** (static site + self-serve token minting), and a Cloudflare tunnel — one
`make up`.

> **Step-by-step setup lives in [`INSTALL.md`](INSTALL.md).** This file is the map.
>
> **In progress:** sign-in moves from landing-minted tokens to loomcycle's password gate, managed by the landing through the `auth:manage` API. The landing and site side (phases 2–3) is built and switches on with `LOOMCYCLE_AUTH_MANAGE_TOKEN`; see [`AUTH-INTEGRATION.md`](AUTH-INTEGRATION.md).
>
> **Shared tenant (2026-10-31):** with `LANDING_TENANT` set (compose default `loomcycle-dev`) everyone signs up into that one tenant and waits for an operator's approval in the app; the website has no team console and mints nothing. See [`AUTH-INTEGRATION.md` → Shared tenant](AUTH-INTEGRATION.md#shared-tenant-2026-10-31).

## Architecture

```
Internet ─▶ Cloudflare edge ─▶ cloudflared ─┬─ app.loomcycle.cloud ─▶ loomcycle-edge:8787 (loomcycle UI/API, via the `edge` network)
              (+ Access on         │         ├─ loomcycle.cloud      ─▶ landing:8080    (Node → cloud-web/ + /api/mint)
               loomcycle.cloud/api)│         └─ telegram.loomcycle.cloud/telegram/webhook ─▶ telegram-bridge:8443 (optional)
compose network `loomnet`:
  postgres(pg18+pgvector)   loomcycle-migrate(one-shot)   builder-sidecar(:9000)
  searxng(:8080)            landing(node:8080)            cloudflared
  tailscale ◀── loomcycle (network_mode: service:tailscale) ──▶ Ollama @ 100.x:11434 (tailnet)
```

### Services

| Service | Image | Role |
|---|---|---|
| `postgres` | `pgvector/pgvector:pg18` | Main store (`loomcycle`) + SQL-Memory aux (`loomcycle_sqlmem`). |
| `loomcycle-migrate` | `denngubsky/loomcycle-browser:${LOOMCYCLE_VERSION}` | One-shot `migrate up` on the main db, then exits. Same image and tag as the runtime. |
| `tailscale` | `tailscale/tailscale` | Kernel-mode egress to the tailnet; **owns the netns loomcycle shares**. |
| `builder-sidecar` | `denngubsky/loomcycle-builder-docker:${LOOMCYCLE_SIDECAR_VERSION}` | Sandboxed code exec (`mcp__sandbox__*`) via the host Docker socket. |
| `loomcycle` | `denngubsky/loomcycle-browser:${LOOMCYCLE_VERSION}` | The runtime, with the PinchTab MCP client binary. `network_mode: service:tailscale`. |
| `searxng` | `searxng/searxng` | Keyless search backend (wired via `config/loomcycle.yaml`). |
| `landing` | Node (`./landing`, built) | Serves `cloud-web/`, proxies the public reads `/healthz` + `/v1/config` (inner links), mints `substrate:tenant` tokens behind Cloudflare Access, and — with `LOOMCYCLE_AUTH_MANAGE_TOKEN` — runs signup and team management on loomcycle's password gate. |
| `cloudflared` | `cloudflare/cloudflared` | Outbound tunnel; routes managed in the Cloudflare dashboard (token method). |
| `telegram-bridge` | `denngubsky/loomcycle-bridge-telegram:${BRIDGE_TELEGRAM_VERSION}` | **Optional** (compose profile `telegram`): a Telegram bot for one agent, its webhook at `telegram.loomcycle.cloud/telegram/webhook`. See INSTALL.md → "Telegram bridge". |

### The landing server + self-serve minting

`landing/server.js` (Node) does three things, and a fourth when enabled:

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

4. **Runs accounts on loomcycle's password gate** — only when
   `LOOMCYCLE_AUTH_MANAGE_TOKEN` (an `auth:manage` operator token) is set; without
   it these routes answer `503 auth_gate_not_configured` and the site keeps the
   vault. `POST /api/signup` sends a new visitor to an owner invite on
   `app.loomcycle.cloud/ui/invite#…` (they choose a password there; this site
   never sees it), and `/api/team/*` lets the owner invite members (a link they
   send themselves), issue reset links, disable/enable and remove them. The
   tenant comes only from the verified Access identity; a subject outside it is a
   `404`. Upstream paths are fixed, bodies are built field by field, errors reach
   the browser only as whitelisted codes, and no invite/reset link or code is
   ever logged. Limits: 10 signups/hour per source and per identity, 20
   invites/day and 50 pending per tenant. Tests: `cd landing && npm test` (a fake
   runtime) and `node smoke.mjs` (add `SMOKE_RUNTIME_URL` + `SMOKE_MANAGE_TOKEN`
   to drive a real runtime). Design and status: [`AUTH-INTEGRATION.md`](AUTH-INTEGRATION.md).

   **Shared-tenant mode** (`LANDING_TENANT` set): `POST /api/signup` invites the
   visitor into that one tenant at the gate's floor (no access mode or groups
   sent; loomcycle holds the new user pending until an operator approves it),
   `/api/team/*` and `/api/mint` (job 3) answer `404`, and the landing runs
   without `LOOMCYCLE_ADMIN_TOKEN`. Limits add 3 signups/day per e-mail, and the
   tenant's 20/day and 50 pending apply to signups. Signup refuses
   (`503 approval_unavailable`) while the runtime's `/healthz` version is below
   `LANDING_MIN_RUNTIME_VERSION`: an older runtime cannot hold sign-ups pending.

> **Why a Node backend and not a Caddy header-inject:** a raw reverse-proxy that
> injects the admin bearer onto `/v1/_operatortokendef` would forward the client's
> body verbatim — a visitor could POST `{scopes:["substrate:admin"]}` and mint an
> admin token. The backend constrains the request; that's the whole point.

### The Tailscale netns wiring (important)

`loomcycle` uses `network_mode: service:tailscale` so it can reach a local Ollama
over the tailnet. Consequences baked into this compose:

- **loomcycle has no compose DNS name of its own** — everything that dials it uses
  **`http://tailscale:8787`** (on `loomnet`; the cloudflared route for `app.loomcycle.cloud` uses the
  `edge`-only alias `http://loomcycle-edge:8787` so loomcycle can trust its `CF-Connecting-IP`; and
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
SearXNG is wired in `config/loomcycle.yaml` (not env), and so are the model
aliases, tiers, the memory embedder and reranker, and the decision models. That
file is kept in step with the TrueNAS deployment, which shares the Ollama host.

## Security posture (read before exposing publicly)

- **The mint route is gated by Cloudflare Access** (Google). The landing verifies
  the Access JWT (never a bare header) and forces `substrate:tenant` (never admin).
- **The landing holds `LOOMCYCLE_ADMIN_TOKEN`** (= your `LOOMCYCLE_AUTH_TOKEN`) —
  server-side only, no host port, reachable only via the tunnel. Treat it as the
  most sensitive component after loomcycle itself. Once accounts are on it also
  holds `LOOMCYCLE_AUTH_MANAGE_TOKEN`, which reaches user records in every tenant
  (and, with `manager_may_recover_owners`, can reset any owner's password); the
  admin token leaves the landing in phase 5. In shared-tenant mode the landing
  needs only the `auth:manage` token, and the gate has owner recovery off.
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
mkdir -p data work pgdata ts-state web retention-exports pinchtab-data
sudo chown -R 65532:65532 data work retention-exports
sudo chown -R 1000:1000 pinchtab-data    # the pinchtab container runs as uid 1000
cp -r /path/to/loomcycle/cloud-web/* web/  # the landing static files
make up && make ps
```

Full walkthrough (Tailscale key, Cloudflare tunnel + **Access** + DNS, the
`.env.secure` template, verification): **[`INSTALL.md`](INSTALL.md)**.
