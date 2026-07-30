# loomcycle cloud deployment — install runbook

Target: **Ubuntu 25.04** (`cloud-home.local`), deploy directory
`/home/denn/work/loomcycle-cloud/`. Everything runs in one Docker Compose stack;
loomcycle is exposed at `app.loomcycle.cloud` and the landing page at
`loomcycle.cloud`, both through a Cloudflare tunnel (no open host ports).

---

## Phase 0 — Host prerequisites

```bash
# Docker Engine + the compose plugin
sudo apt-get update && sudo apt-get install -y docker.io docker-compose-v2 make
sudo usermod -aG docker "$USER"     # log out/in so `docker` works without sudo
ls /dev/net/tun                      # must exist (kernel-mode tailscale needs it)
```

You also need, ahead of time:
- A **Tailscale** account with the local Ollama box already on the tailnet.
- A **Cloudflare** account with the `loomcycle.cloud` zone.
- At least one **LLM provider API key** (Anthropic/OpenAI/…), unless you route only
  to the local Ollama.

---

## Phase 1 — Copy the deploy directory to the host (rsync over SSH)

Run these from your **workstation** (where this repo is checked out) — nothing here
needs the repo present on the host. Set the target once:

```bash
HOST=denn@cloud-home.local            # your hosting PC (SSH access required)
DEPLOY=/home/denn/work/loomcycle-cloud
```

rsync `cloud-deployment/` to the host. The excludes keep local build + secret
artifacts off the wire: the landing image builds **on the host**, and the real
`.env.*` files are created on the host in Phase 5 (never shipped from your
workstation). There is **no `--delete`**, so a re-sync updates the config/app
files without ever touching the host's runtime data (`pgdata/`, `data/`, …) or its
env files:

```bash
rsync -avz \
  --exclude 'landing/node_modules' \
  --exclude '.env.secure' --exclude '.env.insecure' \
  cloud-deployment/ "$HOST:$DEPLOY/"
```

Then, on the host, create the runtime sub-directories + set ownership (`-t` gives
`sudo` a TTY for its prompt):

```bash
ssh -t "$HOST" "cd '$DEPLOY' && \
  mkdir -p data config work pgdata ts-state searxng web retention-exports pinchtab-data && \
  sudo chown -R 65532:65532 data work retention-exports"
```

`config/loomcycle.yaml` (SearXNG wiring), `searxng/settings.yml`, the `landing/`
Node app, and `postgres/initdb.d/00-init.sql` ride along in the sync — leave them
in place. The landing image builds on `make up` (`build: ./landing`). Re-run the
rsync anytime to push config/app changes; it never deletes the host's data or env.

---

## Phase 2 — Landing page

rsync the drafted static site into the host's `web/` (the Node landing serves it;
the `functions/` dir is unused because the landing reproduces `/api/*` + the `/v1`
inner-link proxy server-side). This also ships the hero/mascot videos, which are
**not in git** — they live on your workstation and reach the host via this sync:

```bash
# reuses $HOST / $DEPLOY from Phase 1
rsync -avz cloud-web/ "$HOST:$DEPLOY/web/"
```

---

## Phase 3 — Tailscale auth key

Tailscale **admin console → Settings → Keys → Generate auth key**: make it
**reusable** and (recommended) **tagged** (e.g. `tag:cloud`). Copy the
`tskey-auth-…` value into `TS_AUTHKEY` in `.env.secure` (Phase 5).

Find the local Ollama's tailnet IP after the stack is up (`make` step) — or from
any tailnet device: `tailscale status | grep -i ollama-host`. You'll put it in
`OLLAMA_BASE_URL` as a **raw IP** (e.g. `http://100.73.18.51:11434`).

---

## Phase 4 — Cloudflare tunnel + DNS

1. **Zero Trust → Networks → Tunnels → Create a tunnel** (type *Cloudflared*).
   Copy the **tunnel token** (`eyJ…`) → `CLOUDFLARE_TUNNEL_TOKEN` in `.env.secure`.
2. Add **two Public Hostnames** on that tunnel:

   | Hostname | Service |
   |---|---|
   | `app.loomcycle.cloud` | `http://tailscale:8787` |
   | `loomcycle.cloud`     | `http://landing:8080` |

   > ⚠️ **The app hostname points at `tailscale:8787`, NOT `loomcycle:8787`.**
   > loomcycle shares the tailscale netns and has no DNS name of its own.

3. Cloudflare auto-creates the proxied DNS records for the tunnel.

4. **Cloudflare Access — gate the mint route (required for self-serve minting).**
   Zero Trust → **Access → Applications → Add → Self-hosted**:
   - Application domain: `loomcycle.cloud`, **Path: `api`** (protects `/api/*` — the
     marketing page at `/` stays public).
   - Identity: add **Google** as the login method; add an Access **policy** (e.g.
     Allow — emails ending in your domain, or a specific allow-list).
   - After creating it, open the app's **Overview → Application Audience (AUD) Tag**
     and copy it → `CF_ACCESS_AUD` in `.env.insecure`. Set `CF_ACCESS_TEAM_DOMAIN`
     to your team hostname (e.g. `yourteam.cloudflareaccess.com`).
   - (Optional, recommended: a second Access app over `app.loomcycle.cloud` for
     defense-in-depth on the runtime UI.)

---

## Phase 5 — Env files

Phases 5–7 run **on the host** (Phases 3–4 were dashboard steps). SSH in first:

```bash
ssh "$HOST"                          # open a shell on the hosting PC, then:
cd /home/denn/work/loomcycle-cloud   # the $DEPLOY path, on the host
```

```bash
cp .env.insecure.example .env.insecure
chmod 600 .env.secure   # after you create it below
```

Edit **`.env.insecure`** (non-secret):
- `OLLAMA_BASE_URL` → the Ollama tailnet **IP** (Phase 3).
- `CF_ACCESS_TEAM_DOMAIN` + `CF_ACCESS_AUD` → from the Access app (Phase 4).
- `LOOMCYCLE_PUBLIC_URL=https://app.loomcycle.cloud` + `LANDING_ORIGIN=https://loomcycle.cloud`.
- Adjust presets / retention / GPU knobs to taste.

Create **`.env.secure`** (there is no committed template — repo policy forbids a
`.env.secure*` file in git). Fill every `REPLACE_ME`, then `chmod 600 .env.secure`:

```bash
# ── loomcycle core ──
LOOMCYCLE_AUTH_TOKEN=REPLACE_ME              # openssl rand -hex 32
LOOMCYCLE_OPERATOR_TOKEN_PEPPER=REPLACE_ME   # openssl rand -hex 32
LOOMCYCLE_ADMIN_TOKEN=SAME_AS_AUTH_TOKEN     # the landing mints with this; set = LOOMCYCLE_AUTH_TOKEN
# LOOMCYCLE_SECRET_KEY=REPLACE_ME            # optional (encrypted tenant credentials); openssl rand -base64 32
# ── Postgres (this password MUST equal the one in the two DSNs) ──
POSTGRES_PASSWORD=REPLACE_ME_STRONG
LOOMCYCLE_PG_DSN=postgres://loomcycle:REPLACE_ME_STRONG@postgres:5432/loomcycle?sslmode=disable
LOOMCYCLE_SQLMEM_PG_DSN=postgres://loomcycle:REPLACE_ME_STRONG@postgres:5432/loomcycle_sqlmem?sslmode=disable
# ── sidecars ──
SANDBOX_AUTH_TOKEN=REPLACE_ME                # openssl rand -hex 32 (shared with builder-sidecar)
TS_AUTHKEY=tskey-auth-REPLACE_ME             # Phase 3 (reusable, tagged)
CLOUDFLARE_TUNNEL_TOKEN=REPLACE_ME           # Phase 4
SEARXNG_SECRET=REPLACE_ME                    # openssl rand -hex 32
PINCHTAB_TOKEN=REPLACE_ME                    # openssl rand -hex 32 (dev/exec headless browser)
# ── provider keys — at least one your tiers route to ──
ANTHROPIC_API_KEY=
OPENAI_API_KEY=
GEMINI_API_KEY=
DEEPSEEK_API_KEY=
BRAVE_API_KEY=                               # optional paid search fallback
```

Notes:
- **`LOOMCYCLE_ADMIN_TOKEN` = `LOOMCYCLE_AUTH_TOKEN`** (the super-admin bearer). Do
  NOT mint a dedicated `substrate:admin` token for it — that disables the
  `LOOMCYCLE_AUTH_TOKEN` login (no-lockout gate).
- The DSN host is **`postgres`** (the compose service) — don't change it.
- **Headless browser (dev/exec):** the `pinchtab` sidecar gives the deterministic
  `dev/exec` agent `mcp__browser__*` tools (driven via the envelope's optional
  `browser` steps). PinchTab's MCP is stdio-only, so loomcycle uses a derived image
  (`loomcycle.Dockerfile`) that bakes in the pinchtab MCP client — `make up`
  therefore BUILDS the loomcycle image (first run is slower; needs Docker Hub
  access). The sidecar is internal-only (no published port), and PinchTab keeps
  browsing **local-only** until you widen its domain allowlist (IDPI) — to test
  external sites or your own deployment, follow the pinchtab security guide.

---

## Phase 6 — Bring it up

```bash
make config      # render + validate the merged compose (no secrets printed as values)
make up
make ps          # all services `running`/`healthy`; loomcycle-migrate `exited (0)`
```

`make` wraps `docker compose --env-file .env.insecure --env-file .env.secure …`.

---

## Phase 7 — Verify

1. **Migrate**: `make logs S=loomcycle-migrate` → runs and exits 0.
2. **Runtime**: `make logs S=loomcycle` → `listening on 0.0.0.0:8787`, presets
   layered, **no** `permission denied to create role` and **no** pgvector refusal.
3. **Tailnet**: `docker compose exec tailscale tailscale status` → connected;
   `docker compose exec tailscale wget -qO- http://100.x.x.x:11434/api/tags` → Ollama models.
4. **In-network**: `docker compose exec landing wget -qO- http://localhost:8080/_up` → `{"ok":true}`;
   the landing's inner-link proxy: `docker compose exec landing wget -qO- http://localhost:8080/v1/config` → the public config;
   SearXNG: `docker compose exec searxng wget -qO- http://localhost:8080/healthz` → ok.
5. **Public**: `curl -s https://app.loomcycle.cloud/healthz`; `curl -s https://loomcycle.cloud/healthz`
   and `https://loomcycle.cloud/v1/config` (the landing's inner-link proxy → live status strip
   on the page). Then browse `https://app.loomcycle.cloud/ui` and sign in with `LOOMCYCLE_AUTH_TOKEN`.
6. **Mint flow** (the self-serve path): open `https://loomcycle.cloud`, click **Sign in
   with Google** → Cloudflare Access → Google → back to the landing signed in; **Create
   token** → a real `substrate:tenant` token is shown once and vaulted. Confirm server-side:
   `curl -s -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" https://app.loomcycle.cloud/v1/_operatortokendef/names`
   lists the new token under its derived `t_…` tenant. (A `curl` to `/api/mint` WITHOUT an
   Access cookie must return `401`.)
7. **Functional**: run a `chat/medium` agent that uses WebSearch (proves SearXNG),
   a `dev/exec` run (proves the builder sidecar via `mcp__sandbox__*` — e.g.
   `{"commands":["uname -a"]}`), and a Document op (proves SQL Memory + pgvector).

---

## Upgrade

Bump the image tags in `docker-compose.yaml` (keep `loomcycle` + `loomcycle-migrate`
in lockstep), then `make pull && make up`. If the new version bumped the schema,
`loomcycle-migrate` re-runs automatically on the next `up`.

---

## Durable sandbox workspaces (advanced, optional)

The sandbox uses per-session tmpfs `/work` by default (nothing persists). Durable
**named** workspaces (`sandbox_open {workspace:…}`) require the sidecar's
`SANDBOX_WORKSPACE_ROOT` to be an **absolute host path bind-mounted into the
sidecar at the identical path** (the sidecar drives the host Docker engine, so the
path it creates must equal the path the host mounts). To enable, set in the
`builder-sidecar` service:

```yaml
environment:
  SANDBOX_WORKSPACE_ROOT: /home/denn/work/loomcycle-cloud/work
volumes:
  - ./work:/home/denn/work/loomcycle-cloud/work
```

---

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `loomcycle-migrate` exits 1, `connection refused` | Postgres not up yet, or the DSN password ≠ `POSTGRES_PASSWORD`. Check `make logs S=postgres`. |
| runtime logs `sqlmem: … permission denied to create role` | The DSN role lacks `CREATEROLE`. The init makes `loomcycle` a superuser (has it); if you swapped to a non-super role, `ALTER ROLE loomcycle CREATEROLE;`. |
| runtime refuses to start, pgvector missing | pgvector binaries absent — use the `pgvector/pgvector:pg18` image (it ships them); wipe `pgdata/` and re-init if the first migrate ran before pgvector existed. |
| `app.loomcycle.cloud` → 502 / 1033 | The tunnel route must target `http://tailscale:8787` (not `loomcycle:…`); confirm `cloudflared` is `running` and the hostname exists in the dashboard. |
| landing status strip grey / config "not published" | `LOOMCYCLE_PUBLIC_CONFIG=1` must be set (it is by default); confirm the landing can reach `tailscale:8787` (`make logs S=landing`). |
| `landing` exits at boot: `FATAL: CF_ACCESS_TEAM_DOMAIN + CF_ACCESS_AUD required` | Set both in `.env.insecure` from the Access app (Phase 4), or `MINT_DEV_ALLOW_UNVERIFIED=1` for local testing only. `FATAL: LOOMCYCLE_ADMIN_TOKEN required` → set it in `.env.secure`. |
| **Create token** → `not signed in` (401) | The Access cookie isn't present — the `/api` Access app is missing/misconfigured, or you didn't complete the Google login. Verify the app protects `loomcycle.cloud` path `api` and `CF_ACCESS_AUD` matches. |
| **Create token** → `502 upstream …` | The landing reached loomcycle but the mint failed: `LOOMCYCLE_ADMIN_TOKEN` isn't a valid admin bearer (must equal `LOOMCYCLE_AUTH_TOKEN`), or the runtime is down. Check `make logs S=landing`. |
| Ollama unreachable from loomcycle | Use the **raw tailnet IP** in `OLLAMA_BASE_URL` (MagicDNS is unavailable in the shared netns); confirm `tailscale status` shows the peer. |
| `tailscale` unhealthy | Bad/expired `TS_AUTHKEY`, or `/dev/net/tun` missing; check `make logs S=tailscale`. |
| Bashbox fallback commands (`git`/`go`/…) "not found" | The plain `denngubsky/loomcycle` image is minimal — either switch the `loomcycle` + `loomcycle-migrate` images to `denngubsky/loomcycle-toolbox:<same-tag>`, or rely on the `sandbox` preset (its session image has the full toolchain). |
