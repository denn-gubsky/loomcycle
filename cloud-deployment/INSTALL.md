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
  mkdir -p data config work dynamic pgdata ts-state searxng web retention-exports pinchtab-data && \
  sudo chown -R 65532:65532 data work dynamic retention-exports && \
  sudo chown -R 1000:1000 pinchtab-data"
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
   | `app.loomcycle.cloud` | `http://loomcycle-edge:8787` |
   | `loomcycle.cloud`     | `http://landing:8080` |

   > ⚠️ **The app hostname points at `loomcycle-edge:8787`, NOT `loomcycle:8787`.**
   > loomcycle shares the tailscale netns and has no DNS name of its own;
   > `loomcycle-edge` is the tailscale container's alias on the compose `edge`
   > network, which only cloudflared shares. Going through it is what lets
   > loomcycle trust `CF-Connecting-IP` from cloudflared alone. Older installs
   > used `http://tailscale:8787`: that still works, but every sign-in then
   > counts against one shared throttle bucket.

3. Cloudflare auto-creates the proxied DNS records for the tunnel.

4. **Cloudflare Access — gate `/api` (required: the website's sign-in and signup).** In shared-tenant mode the Access policy decides who may request access at all.
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
- Adjust presets / retention to taste. The Ollama context size and GPU offload
  are in `config/loomcycle.yaml`, not here.

Create **`.env.secure`** (there is no committed template — repo policy forbids a
`.env.secure*` file in git). Fill every `REPLACE_ME`, then `chmod 600 .env.secure`:

```bash
# ── loomcycle core ──
LOOMCYCLE_AUTH_TOKEN=REPLACE_ME              # openssl rand -hex 32
LOOMCYCLE_OPERATOR_TOKEN_PEPPER=REPLACE_ME   # openssl rand -hex 32
# LOOMCYCLE_ADMIN_TOKEN=SAME_AS_AUTH_TOKEN   # per-person mode only (LANDING_TENANT empty): the landing mints with it
LOOMCYCLE_AUTH_MANAGE_TOKEN=REPLACE_ME       # the landing's auth:manage token — see "Enabling accounts" below
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
OLLAMA_API_KEY=                              # optional: hosted Ollama cloud (the cloud-kimi alias)
BRAVE_API_KEY=                               # optional paid search fallback
```

Notes:
- **`LOOMCYCLE_ADMIN_TOKEN`** is needed only if you run the landing in per-person
  mode (`LANDING_TENANT=` empty), where it mints tokens. The default shared-tenant
  mode never mints, so leave it out and the landing holds no admin credential.
  When you do set it, it is `LOOMCYCLE_AUTH_TOKEN` (the super-admin bearer). Do
  NOT mint a dedicated `substrate:admin` token for it — that disables the
  `LOOMCYCLE_AUTH_TOKEN` login (no-lockout gate).
- The DSN host is **`postgres`** (the compose service) — don't change it.
- **Headless browser (dev/exec):** the `pinchtab` sidecar gives the deterministic
  `dev/exec` agent `mcp__browser__*` tools (driven via the envelope's optional
  `browser` steps). PinchTab's MCP is stdio-only, so the runtime uses the published
  `denngubsky/loomcycle-browser` image, which carries the pinchtab MCP client
  binary. Nothing is built locally for the runtime any more. The sidecar is internal-only (no published port), and PinchTab keeps
  browsing **local-only** until you widen its domain allowlist (IDPI) — to test
  external sites or your own deployment, follow the pinchtab security guide.
- **Serve-and-test (browse a server the session runs):** the compose ships a
  dedicated `loom-dev` bridge network with pinchtab attached and
  `SANDBOX_EXPOSE_NETWORK=loom-dev` on the builder-sidecar. A `dev/exec` envelope
  with `expose:<alias>` then attaches its session there, reachable from the browser
  at `http://<alias>:<port>`. This needs the **runtime AND sidecar at ≥ 1.43.0** (the
  expose seam) — the images pin to `1.108.0`; the sidecar is a separate,
  multi-arch image (`docker pull` a `linux/amd64` tag on TrueNAS, not a hand-built
  single-arch one). Add each alias to PinchTab's IDPI allowlist first
  (`pinchtab config set security.allowedDomains "127.0.0.1,localhost,::1,<alias>"`,
  then restart the sidecar) — see [`../docs/SANDBOX.md`](../docs/SANDBOX.md).
- **PinchTab allowlist — persistence & declarative option:** `pinchtab config set`
  writes to `/data/.config/pinchtab/config.json`, which is the `./pinchtab-data`
  bind mount, so **the allowlist already survives restarts and reboots** (the "restart
  to apply" only reloads the running server). To manage it declaratively instead —
  version-controlled, no manual `config set` on a fresh volume — copy
  `pinchtab-config.json.example` to `pinchtab-config.json`, then uncomment
  `PINCHTAB_CONFIG` + the read-only config mount in the `pinchtab` service. There is
  **no env var for `allowedDomains`** (pinchtab exposes only `PINCHTAB_CONFIG`,
  `PINCHTAB_TOKEN`, `PINCHTAB_RATE_LIMIT_MAX`); the token stays in the env var while
  the mounted file carries only the non-secret allowlist.

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
6. **Request-access flow** (the self-serve path, shared-tenant mode): open
   `https://loomcycle.cloud`, click **Sign in with Google** → Cloudflare Access →
   Google → back to the landing signed in; **Request access** → the app's invite page →
   choose a password → back on `loomcycle.cloud/?welcome=1`. Signing in now shows the
   awaiting-approval page until a `loomcycle-dev` operator approves the new user in the
   app's **Users** console. (A `curl` to `/api/signup` WITHOUT an Access cookie must
   return `401`; `/api/mint` and `/api/team` return `404` in this mode.)
   In per-person mode (`LANDING_TENANT=` empty) the old flow applies instead: **Create
   token** mints a `substrate:tenant` token for the derived `t_…` tenant.
7. **Functional**: run a `chat/medium` agent that uses WebSearch (proves SearXNG),
   a `dev/exec` run (proves the builder sidecar via `mcp__sandbox__*` — e.g.
   `{"commands":["uname -a"]}`), and a Document op (proves SQL Memory + pgvector).

---

## Enabling accounts (password gate; optional until phase 5)

### Shared tenant (the default since 2026-10-31)

Everyone who signs up on the website joins one tenant, `loomcycle-dev`
(`LANDING_TENANT`, set by compose). A visitor who passed Cloudflare Access gets an
invite into it with their e-mail as the login, chooses a password on the app, and
then waits as a **pending** user until an operator approves them. Two things decide
who gets in: the Access policy (who may ask at all) and the operator's approval
(who may use the workspace). There is no website team console and no token vault
in this mode; operators manage people in the app's **Users** console, and API
tokens are made in the app (`/ui` → Tokens).

1. **Runtime** — the release after v1.129.0, which has the password gate's
   pending-approval change (`new_user.status: pending` on a password gate).
   Deploy that runtime **with or before** this repo's `config/loomcycle.yaml`:
   an older runtime ignores the `status` key without a word and would admit
   every sign-up unapproved. The landing guards this too: shared-mode signup
   answers `503 approval_unavailable` while the runtime's `/healthz` version is
   below `LANDING_MIN_RUNTIME_VERSION` (compose default `1.129.1`; set it in
   `.env.insecure` if the release number differs).
2. **The landing's `auth:manage` token** — mint it as in step 2 below and put it in
   `.env.secure` as `LOOMCYCLE_AUTH_MANAGE_TOKEN`. Without it **Request access**
   answers "Accounts are not enabled".
3. **Drop `LOOMCYCLE_ADMIN_TOKEN` from `.env.secure`** (or leave it; shared mode
   never reads it). The landing then holds only the `auth:manage` token.
4. **Existing `loomcycle-dev` users** keep their subjects. Give each a password from
   the app (an operator's own invite or reset), not through the website: if one of
   them presses **Request access** while their subject is their e-mail, the landing
   answers "This address already has an account here" instead of making a
   duplicate account.
5. `make up`, then the manual pass in Phase 7 step 6 above.

Limits in this mode (landing memory): 10 sign-ups an hour per source address and
per e-mail, 3 a day per e-mail, 20 new invites a day for the whole tenant (an
invite that only replaces the visitor's own earlier one is not counted), and 50
live website invites at a time.

To go back to per-person tenants, set `LANDING_TENANT=` (empty) in
`.env.insecure`, put `LOOMCYCLE_ADMIN_TOKEN` back, and set the gate's
`manager_may_recover_owners: true` again (the owner signup attaches a password to
the owner, which needs it). The rest of this section describes that mode.

### Per-person tenants (`LANDING_TENANT` empty)

The landing can move sign-in from minted tokens to loomcycle's password gate
([`AUTH-INTEGRATION.md`](AUTH-INTEGRATION.md)): **Create your workspace** sends a
visitor to `app.loomcycle.cloud/ui/invite#…` to choose a password, and the owner
gets a **Team** view to invite, reset, disable and remove people. It stays off —
the site shows the token vault exactly as before — until
`LOOMCYCLE_AUTH_MANAGE_TOKEN` is set. Turn it on only after the runtime has the
password gate (plan **phase 0 + 1**):

1. **Runtime** — a loomcycle release with RFC FA P0 + P2, and the phase-1 config
   in `config/loomcycle.yaml`: a `kind: password` gate named as `LANDING_AUTH_GATE`
   (default `cloud`), `manager_may_recover_owners: true` (owner "Reset my
   password" and the legacy-owner migration need it), and
   **`allowed_return_to` listing `LANDING_SIGNUP_RETURN_TO` verbatim** — the match is
   exact, query string included (default `https://loomcycle.cloud/?welcome=1`).
   `LOOMCYCLE_PUBLIC_URL=https://app.loomcycle.cloud` is already set and must
   equal `LANDING_APP_URL`; the landing refuses an invite link that points
   anywhere else.
2. **Mint the landing's token** with the admin bearer (it must be minted alone;
   `tenant_id`/`subject` are required but mean nothing for this scope):

   ```bash
   curl -s -X POST https://app.loomcycle.cloud/v1/_operatortokendef \
     -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" -H 'Content-Type: application/json' \
     -d '{"op":"create","name":"cloud-landing","tenant_id":"ops","subject":"cloud-landing","scopes":["auth:manage"]}'
   ```

   Put the returned `token` into `.env.secure` as `LOOMCYCLE_AUTH_MANAGE_TOKEN=…`
   (by hand; don't paste it anywhere else). Keep `LOOMCYCLE_ADMIN_TOKEN` for now —
   `/api/mint` stays until phase 5.
3. **Non-secret settings** in `.env.insecure` (defaults shown in
   `.env.insecure.example`): `LANDING_AUTH_GATE`, `LANDING_APP_URL`,
   `LANDING_SIGNUP_RETURN_TO`. Compose passes them to the landing as `AUTH_GATE`,
   `APP_URL` and `SIGNUP_RETURN_TO`; the prefix keeps them apart from the
   runtime, which loads the same `.env.insecure`.
4. `make up` (recreates `landing`), then check:
   `curl -s https://loomcycle.cloud/api/whoami` with your Access session reports
   `"auth_gate":true`; without a session it is `401`. Then the manual pass: a fresh
   Google account → **Create your workspace** → choose a password → back on
   `loomcycle.cloud/?welcome=1` → **Manage team** → invite, redeem in a private
   window, reset, disable.

To test before deploying, run the landing suite and its smoke against a local
runtime: `cd landing && npm ci && npm test`, then
`SMOKE_RUNTIME_URL=http://127.0.0.1:8787 SMOKE_MANAGE_TOKEN=… node smoke.mjs`
(the runtime's `LOOMCYCLE_PUBLIC_URL` must be that same URL).

To turn accounts off again, remove the token from `.env.secure` and `make up`.

---

## Telegram bridge (optional)

The `telegram-bridge` service is a Telegram bot for one loomcycle agent
(`denngubsky/loomcycle-bridge-telegram`, the `@loomcycle/bridge-telegram`
package). Each person who messages the bot is their own loomcycle user in one
tenant; a group is a user of its own. Telegram posts each message to the bridge
through the tunnel; the bridge talks to the runtime inside `loomnet`. The full
behaviour (groups, trust, buttons, limits) is in
[`packages/bridge-telegram/README.md`](../packages/bridge-telegram/README.md).

It is **off by default** (a compose profile), so a release deploy never starts
it half-configured.

1. **The bot.** In Telegram, open @BotFather, send `/newbot` and keep the token
   it gives you. Leave privacy mode on and do not make the bot a group admin.
   Optionally `/setcommands` with `new`, `stop` and `help`.
2. **Your Telegram id.** Message @userinfobot (or any id bot) for your numeric
   user id, for the allow-list. A refused chat is also logged by the bridge as
   `tg:<chat id>`.
3. **The bridge's loomcycle token.** Sign in to `https://app.loomcycle.cloud/ui`
   as the admin, open **Settings → Tokens** and generate a token named
   `telegram-bridge` with scope `substrate:tenant` in the tenant the Telegram
   users will live in (for example `telegram`). It is shown once. The bridge uses it only to register
   each Telegram user and mint them a short-lived token limited to the one agent.
   Make sure that tenant's user tier reaches a provider this stack runs (here the
   local Ollama).
4. **Tunnel route.** On the tunnel from Phase 4, add a Public Hostname:

   | Hostname | Path | Service |
   |---|---|---|
   | `telegram.loomcycle.cloud` | `^/telegram/webhook$` | `http://telegram-bridge:8443` |

   Do **not** put Cloudflare Access on it: Telegram cannot sign in, and every
   call is authenticated by the webhook secret instead (a wrong one gets `401`).
   If Bot Fight Mode or a WAF rule challenges Telegram's calls, add a skip rule
   for Telegram's webhook ranges `149.154.160.0/20` and `91.108.4.0/22`.
5. **Secrets** — append to `.env.secure`:

   ```bash
   TELEGRAM_BOT_TOKEN=REPLACE_ME                 # from @BotFather
   TELEGRAM_BRIDGE_WEBHOOK_SECRET=REPLACE_ME     # openssl rand -hex 32
   TELEGRAM_BRIDGE_LOOMCYCLE_TOKEN=REPLACE_ME    # the token from step 3
   ```

6. **Settings** — in `.env.insecure` (see the Telegram block of
   `.env.insecure.example`): `COMPOSE_PROFILES=telegram`,
   `TELEGRAM_BRIDGE_ALLOWED_USERS=<your id>`, and `TELEGRAM_BRIDGE_AGENT` if not
   `chat/medium`. The image version is `BRIDGE_TELEGRAM_VERSION` (default in the
   compose file); release deploys do not change it.
7. **Start and verify.**

   ```bash
   make up                       # or: docker compose … up -d telegram-bridge
   make logs S=telegram-bridge   # expect "telegram webhook set" then "telegram-bridge listening"
   ```

   The bridge calls Telegram's `setWebhook` at every start with
   `TELEGRAM_BRIDGE_PUBLIC_URL/telegram/webhook` and the secret. Then message the
   bot in Telegram: the reply streams in as the agent writes it.

The bridge keeps which loomcycle session each chat is in under the
`telegram-bridge-data` volume, so a restart continues every conversation.

---

## Upgrading an existing deployment to 1.108.0

The steps below take a stack on an older release to 1.108.0. The numbered list is
the order; the notes after it are what changed underneath you.

1. **Back up.** Dump both databases and keep the old files:
   ```bash
   docker compose exec postgres pg_dump -U loomcycle loomcycle        > loomcycle.sql
   docker compose exec postgres pg_dump -U loomcycle loomcycle_sqlmem > loomcycle_sqlmem.sql
   cp docker-compose.yaml docker-compose.yaml.bak && cp -r config config.bak
   ```
   Move `config.bak` OUT of the deploy directory's `config/` tree: every `*.yaml`
   under `/config` is loaded.
2. **Check the Ollama host** before anything else. It must run Ollama **0.35 or
   later** (decision models) and have pulled every model `config/loomcycle.yaml`
   names:
   ```bash
   docker compose exec tailscale wget -qO- http://100.x.x.x:11434/api/version
   docker compose exec tailscale wget -qO- http://100.x.x.x:11434/api/tags
   ```
   Needed: `nimble`, `clef`, `bge-m3`, and the chat models behind `local-small`,
   `local-medium`, `local-high` and `local-dense`. A model that is not listed is
   skipped as a tier candidate without an error, and the run goes to a paid provider.
3. **Sync the files** from the repo: `docker-compose.yaml` and
   `config/loomcycle.yaml`. Keep your own `OLLAMA_BASE_URL`, hostnames and Access
   settings. In `.env.insecure`:
   - remove `LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX` and `LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU`
     (the YAML sets them now, and the YAML wins);
   - remove `LOOMCYCLE_SCHEDULER_FIRE_TIMEOUT_SECONDS` if present (no longer read);
   - set `SANDBOX_IMAGE` to the `1.108.0` tag (the compose file sets the value the
     sidecar uses; this line only keeps the env file in step).
   In `.env.secure`, add `GEMINI_API_KEY` if you want the Gemini candidates, and
   `OLLAMA_API_KEY` for the hosted-Ollama alias. A provider with no key is skipped.
   If `pgdata` is still mounted at `/var/lib/postgresql/data`, leave your working
   mount as it is; the compose file now mounts the parent, which is what pg18 needs
   on a fresh install.
4. **Pull, then migrate with the runtime stopped.** The migrations up to 0097
   include an index on `runs` that is built inside its transaction and holds writes
   to that table while it runs.
   ```bash
   make pull
   docker compose --env-file .env.insecure --env-file .env.secure stop loomcycle
   make migrate            # exits 0
   make up
   ```
   The runtime image is pulled now, not built: `loomcycle.Dockerfile` is gone.
5. **Verify.** `curl -s https://app.loomcycle.cloud/healthz` reports `1.108.0`.
   In the runtime log, the resolve probe lists the Ollama models, and there is no
   `kind` warning. The Web UI's Routing page shows which Gemini model each pattern alias
   resolved to. `GET /v1/_decide/models` lists `decide` and `decide-deep`.
6. **Backfill embeddings** if this deployment had no embedder before. Rows written
   earlier have no vector and are not found by semantic search until
   `POST /v1/_memory/reembed?tenant=<id>` has run for each tenant (admin bearer; it
   refuses an admin call that names no tenant).

What changed, for this stack:

- **Models and tiers come from `config/loomcycle.yaml`**, the same set the TrueNAS
  deployment uses. Before, this stack ran on the presets' defaults.
- **Memory is switched on end to end**: an embedder (`bge-m3`), a decision-model
  reranker (`nimble`), and the hourly `memory-consolidation` schedule. Until now
  what agents added to memory was queued and never consolidated.
- **Decision models** are available (`decide`, `decide-deep`), and the two chat
  agents hold the `Decision` tool.
- **The hook registry is gone.** Migration 0083 drops the `hooks` table and
  `/v1/hooks` no longer exists. Anything that registered a hook at startup must
  attach it to the agent definition instead.
- **Tokens are held to their scopes over MCP.** The landing mints
  `substrate:tenant,runs:create`, which covers everything in a tenant. A token you
  minted by hand with `runs:create` alone can no longer read runs.
- **Minting needs an explicit scope list**; an omitted one is refused.
- **Unset scope grants no longer deny.** An agent holding `Memory` with no
  `memory_scopes` reaches the calling user's data (and the tenant's, for a
  non-isolated member). The `chat/*` agents write the tenant's shared memory.
- **The tailnet range counts as private** for `WebFetch` / `HTTP`. An agent that
  must fetch from a tailnet host needs `LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST`.
  The Ollama provider and SearXNG are not affected.

---

## Upgrading to 1.122.0: access control and the run inbox

1.122.0 adds access control (rule lists, groups and bindings, written per tenant)
and keeps the files tools return in a run inbox under a `dynamic_root` volume. The
compose file and `config/loomcycle.yaml` in this directory carry both, so the
automatic deploy picks them up. One step is by hand, once, on the host, **before**
the first deploy that carries them: create the `dynamic` directory with the
runtime's owner. Docker would otherwise create it as root, and the runtime (uid
65532) could not write to it; every returned file would come back as
`[file not kept: …]`.

```bash
ssh -t "$HOST" "cd '$DEPLOY' && mkdir -p dynamic && sudo chown 65532:65532 dynamic"
```

- **Mode.** The compose file sets `LOOMCYCLE_ACL` to `shadow`: today's scope gates
  still decide, and every place the access rules would answer differently is
  logged as `authz: acl_shadow_diverged …` and audited. Every token class has a
  built-in list that reproduces its reach, so the log should stay quiet. To
  switch, put `LOOMCYCLE_ACL=enforce` (or `compat`) in the host's `.env.insecure`
  — it overrides the compose default — and recreate `loomcycle`.
- **Who writes rules.** Each Google sign-in gets its own tenant and a tenant
  operator token, so each user manages the lists for their own tenant in
  **Settings → Access**. A binding names one tenant, so `config/loomcycle.yaml`
  cannot bind the tenants the landing page creates; limits for every tenant stay
  with the token scopes and env settings.
- Schedules and webhooks created before 1.122.0 record no author, so they fire
  unnarrowed until re-saved. Snapshots do not carry lists, groups or bindings.

## Upgrading to Web UI sessions and the password gate

The first release with RFC FA P0 + P2 (Web UI sessions, the password gate,
`auth:manage`) is carried here by `config/loomcycle.yaml` (the `auth:` block)
and the compose file (`LOOMCYCLE_PUBLIC_URL`). The automatic deploy applies
both, so do these host steps **before** that release is deployed. The plan
behind them is [`AUTH-INTEGRATION.md`](AUTH-INTEGRATION.md), phase 1.

1. **`LOOMCYCLE_SECRET_KEY` must be set** in `.env.secure`. The password gate
   derives its pepper and the TOTP secret key from it, and **refuses to load
   without it**: the runtime would crash-loop on `a password gate needs
   LOOMCYCLE_SECRET_KEY`. Check that the running server already uses one
   (this prints a count, not the key):

   ```bash
   ssh "$HOST" "cd '$DEPLOY' && docker compose logs loomcycle 2>&1 | grep -c 'pepper derived from LOOMCYCLE_SECRET_KEY'"
   ```

   A count above 0 means it is set; nothing to do. If it is 0, the key is
   being added for the first time. Operator tokens minted before then are
   stored unpeppered and stop authenticating until they are rewrapped. So
   generate a key (`openssl rand -base64 32`), add `LOOMCYCLE_SECRET_KEY=…`
   to `.env.secure`, recreate `loomcycle`, then review and rewrap:

   ```bash
   C="docker compose --env-file .env.insecure --env-file .env.secure exec loomcycle loomcycle"
   $C tokens rewrap                                  # lists the unpeppered rows and a digest; writes nothing
   $C tokens rewrap --def-id <id> --def-id <id>      # rewrap the ones you minted (or --all-listed <digest>)
   ```

   Keep the key: losing it ends every session and makes every password,
   TOTP secret and stored credential unreadable.
2. **`LOOMCYCLE_PUBLIC_URL`** comes from the compose file
   (`https://app.loomcycle.cloud`). Leave the Cloudflare tunnel's Host
   header as it is (don't set `httpHostHeader`): loomcycle treats a request
   whose Host matches this URL as HTTPS and issues the `__Host-lc_session`
   cookie.
3. **Point the tunnel at the edge network.** The compose file adds an `edge`
   network that only cloudflared (fixed address `10.253.253.2`) and the
   runtime join, and the config trusts `CF-Connecting-IP` from that address
   alone. In the Cloudflare dashboard, change the `app.loomcycle.cloud`
   route's service from `http://tailscale:8787` to
   `http://loomcycle-edge:8787` after the deploy that adds the network. Until
   you do, the runtime sees every sign-in as coming from cloudflared's
   `loomnet` address: logins still work, but one attacker's failures slow
   everyone's (never spoofable either way). Make the change right after the
   deploy: no one has a password yet at that point (accounts start with the
   landing's signup, phases 2–3), so the window costs nothing if it's short.
   If `10.253.253.0/29` collides with a network on the host, change the
   subnet, cloudflared's `.2`, tailscale's `.3`, the `ip_range` and
   `auth.trusted_proxies` together.
4. **After the deploy, enable the landing's accounts** when you're ready:
   mint its `auth:manage` token and set its settings as in
   [Enabling accounts](#enabling-accounts-password-gate-optional-until-phase-5).
   Until then the site keeps the token vault.
5. **Check:** `https://app.loomcycle.cloud/ui/login` offers both the token
   box and "loomcycle.cloud account". Existing tokens keep working: the
   mode stays `token` until plan phase 6.

## Routine upgrades

The image versions are variables in `docker-compose.yaml`:

| Variable | Sets | Default in the compose file |
|---|---|---|
| `LOOMCYCLE_VERSION` | the two `loomcycle-browser` tags (runtime and migration) | the release this directory was written for |
| `LOOMCYCLE_SIDECAR_VERSION` | `loomcycle-builder-docker` and the sandbox session image | the last minor release |
| `LOOMBOARD_VERSION` | the `ghcr.io/denn-gubsky/loomboard` tag of the hosted board (`X.Y.Z`, no `v`) | the loomboard release this directory was written for |

Put them in a `versions.env` file next to the compose file; the Makefile passes it
when it exists. Then `make pull && make up`. If the new version bumped the schema,
`loomcycle-migrate` re-runs automatically on the next `up`.

- A **minor** release (`x.y.0`): move `LOOMCYCLE_VERSION` and
  `LOOMCYCLE_SIDECAR_VERSION` together.
- A **patch** release (`x.y.1`): move only `LOOMCYCLE_VERSION`. The sidecar and the
  session image are published on minor releases only.

Read the release's notes in `REVISIONS.md` first; a jump across several minors
needs the steps in the section above.

## Automatic deploys

`deploy/loomcycle-cloud-deploy` deploys one release and checks the result. A
scheduled CI job calls it over SSH with a key that can do nothing else.

```
GitHub: tag vX.Y.Z --> images published
                              ^
Gitea runner, every 10 min:   | reads the newest release tags (public, no token)
   ssh deploy-key@host "deploy loomcycle X.Y.Z" / "deploy loomboard vX.Y.Z"
                              |
host: loomcycle-cloud-deploy --+--> backup, pull, up, check /healthz
```

Why a poll from Gitea: the host is reachable from the Gitea runner and not from
GitHub's runners, and a poll needs no credential on GitHub.

**What the key can do.** Its `authorized_keys` entry forces the script, and the
script treats what the caller sends as a request:

- the only requests are `deploy loomcycle <X.Y.Z>`, `deploy loomboard <vX.Y.Z>` and
  `status`;
- the tag must exist in the project's GitHub repository;
- it must not be older than what is deployed;
- the release's image must be published (for loomcycle `loomcycle-browser`, for
  loomboard `ghcr.io/denn-gubsky/loomboard`); until it is, the script answers "not
  published yet" and the next run tries again.

So the key deploys a published release and nothing else. The deploy account can run
Docker, which is root on the host: whoever can push a release tag to either
repository decides what runs here.

**What a loomcycle deploy does:**

1. dumps both databases and copies the compose file, the config and `versions.env`
   to `~/work/loomcycle-cloud-deploy/backups/` (the last 10 are kept, failed
   attempts included);
2. copies this directory and `cloud-web/` from the release over the stack's files,
   when the release carries this script. `.env.insecure`, `.env.secure`,
   `versions.env`, the data directories and files the repository does not hold are
   left alone. **`config/loomcycle.yaml` and `docker-compose.yaml` on the host are
   overwritten**, so change them in the repository, not on the host.
   `searxng/` is the exception: the SearXNG container owns that directory once it
   has started, so the deploy leaves it alone — change `searxng/settings.yml` on
   the host. If the copy fails part-way, the files from step 1 are put back and
   the job fails;
3. writes the new versions, pulls, and runs `up -d --build` (the migration runs
   first, as on any `up`);
4. waits up to three minutes for `/healthz` to report the new version. If it does
   not, it rolls back and the job fails:
   - the runtime is stopped;
   - **if the database schema moved** (`schema_migrations` differs from what it was
     at step 1 — the new version's migration ran), the runtime's database is
     restored from the step-1 dump into a fresh database that then takes its name.
     The database the failed version left is kept as `loomcycle_failed_<stamp>`
     for a look at what went wrong; drop it when done. An old runtime cannot run
     on a newer schema, so without this the rollback would leave the stack down.
     Writes made between the dump and the failure are lost with it. The SQL
     Memory database is not restored: no migration changes it;
   - the compose file, the config and the versions are put back, the stack is
     started again, and the job waits for the old version to report healthy.
   If the database restore itself fails, the runtime is left stopped and the job
   says so; restore by hand from the step-1 dump.

A loomboard deploy pulls the release's image and restarts that one service, and
puts the previous version back if the new one does not answer. Nothing is built
on the host: loomboard publishes the image on each release (its
`docker/README.md`). The image comes from GHCR, whose package must be public;
the board's settings are environment variables of the `loomboard` service.

**Setup, once:**

1. On the host, install the script and create `versions.env` with what is running:
   ```bash
   mkdir -p ~/work/loomcycle-cloud-deploy/bin
   install -m 755 deploy/loomcycle-cloud-deploy ~/work/loomcycle-cloud-deploy/bin/
   printf 'LOOMCYCLE_VERSION=1.108.1\nLOOMCYCLE_SIDECAR_VERSION=1.108.0\nLOOMBOARD_VERSION=0.4.0\n' > versions.env
   ~/work/loomcycle-cloud-deploy/bin/loomcycle-cloud-deploy status
   ```
2. A key pair for CI (`ssh-keygen -t ed25519 -N "" -f ~/work/loomcycle-cloud-deploy/ci_deploy`).
   Its public half goes into `~/.ssh/authorized_keys` as one line:
   ```
   command="/home/<user>/work/loomcycle-cloud-deploy/bin/loomcycle-cloud-deploy",restrict ssh-ed25519 AAAA... gitea-loomcycle-cloud-deploy
   ```
3. On the Gitea server, a repository holding `deploy/gitea-deploy.yml` as
   `.gitea/workflows/deploy.yml`, with Actions enabled. In its Settings → Actions:
   - secret **`DEPLOY_SSH_KEY`**: the private key (then delete it from the host);
   - variables **`DEPLOY_HOST`** (an address the runner reaches), **`DEPLOY_USER`**
     and **`DEPLOY_KNOWN_HOSTS`** (`ssh-keyscan -t ed25519 <host>`, after checking
     the fingerprint against the host's own key).
4. Run the workflow once by hand. With nothing new it reports "already deployed"
   twice.

Without the secret and `DEPLOY_HOST` the job reports "not configured" and passes.
When the script changes, install it again (step 1): the host's copy is not updated
by a deploy.

By hand on the host, the same requests work as arguments:
`loomcycle-cloud-deploy deploy loomcycle 1.109.0`. Logs are in
`~/work/loomcycle-cloud-deploy/logs/`.

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
| runtime restarts in a loop, log `a password gate needs LOOMCYCLE_SECRET_KEY` (or `…LOOMCYCLE_PUBLIC_URL`) | The `auth:` block's password gate needs both. Set the missing one (see "Upgrading to Web UI sessions"), or comment out the `cloud:` gate in `config/loomcycle.yaml` to boot without it. |
| Web UI sign-in on `app.loomcycle.cloud` sets `lc_session` (not `__Host-lc_session`) and the log warns about plain HTTP; in strict mode it fails with `insecure_transport` | The tunnel rewrites the Host header, so loomcycle can't tell the request came through HTTPS. Remove `httpHostHeader` from the tunnel's route. |
| `app.loomcycle.cloud` → 502 / 1033 | The tunnel route must target `http://loomcycle-edge:8787` (or the older `http://tailscale:8787`; never `loomcycle:…`); confirm `cloudflared` is `running`, attached to the `edge` network, and the hostname exists in the dashboard. |
| landing status strip grey / config "not published" | `LOOMCYCLE_PUBLIC_CONFIG=1` must be set (it is by default); confirm the landing can reach `tailscale:8787` (`make logs S=landing`). |
| `landing` exits at boot: `FATAL: CF_ACCESS_TEAM_DOMAIN + CF_ACCESS_AUD required` | Set both in `.env.insecure` from the Access app (Phase 4), or `MINT_DEV_ALLOW_UNVERIFIED=1` for local testing only. `FATAL: LOOMCYCLE_ADMIN_TOKEN is required` → `LANDING_TENANT` is empty (per-person mode, which mints): set the token in `.env.secure`, or set `LANDING_TENANT` back. `FATAL: LANDING_TENANT must be …` → it is not a tenant id (letters, digits, `-`, `_`). |
| **Request access** → `Sign-ups are paused until the runtime is updated` (`503 approval_unavailable`); landing log `signup refused: runtime version … below LANDING_MIN_RUNTIME_VERSION` | The runtime is older than the first release that holds sign-ups for approval, or reports no release version (`devel`/`unknown` from a local build). Upgrade the runtime; lower `LANDING_MIN_RUNTIME_VERSION` only if that release really has the pending status. |
| **Request access** → `Too many requests for access right now` (`429 signup_limit`) | The shared tenant reached its 20 new invites for the day or 50 live website invites. Approve or remove pending users and revoke stale invites in the app, or wait a day. |
| **Create token** → `not signed in` (401) | The Access cookie isn't present — the `/api` Access app is missing/misconfigured, or you didn't complete the Google login. Verify the app protects `loomcycle.cloud` path `api` and `CF_ACCESS_AUD` matches. |
| **Create token** → `502 upstream …` | The landing reached loomcycle but the mint failed: `LOOMCYCLE_ADMIN_TOKEN` isn't a valid admin bearer (must equal `LOOMCYCLE_AUTH_TOKEN`), or the runtime is down. Check `make logs S=landing`. |
| **Create your workspace** → `Accounts are not enabled` (`503 auth_gate_not_configured`) | `LOOMCYCLE_AUTH_MANAGE_TOKEN` isn't reaching the landing — set it in `.env.secure` and `make up`. |
| **Create your workspace** → error, `make logs S=landing` shows `account upstream signup: 400 invalid_return_to` | `LANDING_SIGNUP_RETURN_TO` is not in the gate's `allowed_return_to` **verbatim** (exact match, query string included). |
| landing logs `account upstream …: 404 gate_not_found` | `LANDING_AUTH_GATE` doesn't name a `kind: password` gate in `config/loomcycle.yaml`, or the runtime predates the password gate. |
| landing logs `account upstream …: 401 …` or `403 insufficient_scope` | `LOOMCYCLE_AUTH_MANAGE_TOKEN` is not a live `auth:manage` token (retired, or minted with another scope). |
| site shows `link_mismatch` | The runtime's `LOOMCYCLE_PUBLIC_URL` differs from `LANDING_APP_URL`; make them equal. |
| **Reset my password** → "recovery for owners is turned off" | The gate lacks `manager_may_recover_owners: true`; the owner changes the password in the app (`/ui/account`) instead. |
| Ollama unreachable from loomcycle | Use the **raw tailnet IP** in `OLLAMA_BASE_URL` (MagicDNS is unavailable in the shared netns); confirm `tailscale status` shows the peer. |
| `tailscale` unhealthy | Bad/expired `TS_AUTHKEY`, or `/dev/net/tun` missing; check `make logs S=tailscale`. |
| A tool's file shows as `[file not kept: …]` | No `dynamic_root` volume, or the runtime cannot write it: `./dynamic` must exist and be owned by 65532 (see *Upgrading to 1.122.0*). |
| `403 {"code":"acl_denied", …}` on a request that worked before | `LOOMCYCLE_ACL=enforce` and no rule allows it, or a deny bound to the caller. The body names the rules; **Settings → Access → Simulator** shows every rule considered. Set `shadow` while you fix the list. |
| `503 acl_unavailable` | The caller's rules could not be read (a Postgres fault). Under `enforce` the request is refused rather than admitted. |
| Bashbox fallback commands (`git`/`go`/…) "not found" | The plain `denngubsky/loomcycle` image is minimal — either switch the `loomcycle` + `loomcycle-migrate` images to `denngubsky/loomcycle-toolbox:<same-tag>`, or rely on the `sandbox` preset (its session image has the full toolchain). |
