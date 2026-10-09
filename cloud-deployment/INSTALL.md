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
  sudo chown -R 65532:65532 data work retention-exports && \
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
- Adjust presets / retention to taste. The Ollama context size and GPU offload
  are in `config/loomcycle.yaml`, not here.

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
OLLAMA_API_KEY=                              # optional: hosted Ollama cloud (the cloud-kimi alias)
BRAVE_API_KEY=                               # optional paid search fallback
```

Notes:
- **`LOOMCYCLE_ADMIN_TOKEN` = `LOOMCYCLE_AUTH_TOKEN`** (the super-admin bearer). Do
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
   `kind` warning. Settings → Routing shows which Gemini model each pattern alias
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

## Routine upgrades

The image versions and the loomboard ref are variables in `docker-compose.yaml`:

| Variable | Sets | Default in the compose file |
|---|---|---|
| `LOOMCYCLE_VERSION` | the two `loomcycle-browser` tags (runtime and migration) | the release this directory was written for |
| `LOOMCYCLE_SIDECAR_VERSION` | `loomcycle-builder-docker` and the sandbox session image | the last minor release |
| `LOOMBOARD_REF` | the loomboard release tag the hosted board is built from | `v0.3.1` |

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
- for loomcycle, the `loomcycle-browser` image of that version must be published;
  until it is, the script answers "not published yet" and the next run tries again.

So the key deploys a published release and nothing else. The deploy account can run
Docker, which is root on the host: whoever can push a release tag to either
repository decides what runs here.

**What a loomcycle deploy does:**

1. dumps both databases and copies the compose file, the config and `versions.env`
   to `~/work/loomcycle-cloud-deploy/backups/` (the last 10 are kept);
2. copies this directory and `cloud-web/` from the release over the stack's files,
   when the release carries this script. `.env.insecure`, `.env.secure`,
   `versions.env`, the data directories and files the repository does not hold are
   left alone. **`config/loomcycle.yaml` and `docker-compose.yaml` on the host are
   overwritten**, so change them in the repository, not on the host;
3. writes the new versions, pulls, and runs `up -d --build` (the migration runs
   first, as on any `up`);
4. waits up to three minutes for `/healthz` to report the new version. If it does
   not, the compose file, the config and the versions are put back and the stack is
   started again, and the job fails. **The databases are not restored
   automatically**: a migration that already ran stays applied, and the dumps from
   step 1 are there for a manual restore.

A loomboard deploy rebuilds the `loomboard` image from the tag and restarts that one
service, and puts the previous tag back if the new one does not answer.

**Setup, once:**

1. On the host, install the script and create `versions.env` with what is running:
   ```bash
   mkdir -p ~/work/loomcycle-cloud-deploy/bin
   install -m 755 deploy/loomcycle-cloud-deploy ~/work/loomcycle-cloud-deploy/bin/
   printf 'LOOMCYCLE_VERSION=1.108.1\nLOOMCYCLE_SIDECAR_VERSION=1.108.0\nLOOMBOARD_REF=v0.3.1\n' > versions.env
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
| `app.loomcycle.cloud` → 502 / 1033 | The tunnel route must target `http://tailscale:8787` (not `loomcycle:…`); confirm `cloudflared` is `running` and the hostname exists in the dashboard. |
| landing status strip grey / config "not published" | `LOOMCYCLE_PUBLIC_CONFIG=1` must be set (it is by default); confirm the landing can reach `tailscale:8787` (`make logs S=landing`). |
| `landing` exits at boot: `FATAL: CF_ACCESS_TEAM_DOMAIN + CF_ACCESS_AUD required` | Set both in `.env.insecure` from the Access app (Phase 4), or `MINT_DEV_ALLOW_UNVERIFIED=1` for local testing only. `FATAL: LOOMCYCLE_ADMIN_TOKEN required` → set it in `.env.secure`. |
| **Create token** → `not signed in` (401) | The Access cookie isn't present — the `/api` Access app is missing/misconfigured, or you didn't complete the Google login. Verify the app protects `loomcycle.cloud` path `api` and `CF_ACCESS_AUD` matches. |
| **Create token** → `502 upstream …` | The landing reached loomcycle but the mint failed: `LOOMCYCLE_ADMIN_TOKEN` isn't a valid admin bearer (must equal `LOOMCYCLE_AUTH_TOKEN`), or the runtime is down. Check `make logs S=landing`. |
| Ollama unreachable from loomcycle | Use the **raw tailnet IP** in `OLLAMA_BASE_URL` (MagicDNS is unavailable in the shared netns); confirm `tailscale status` shows the peer. |
| `tailscale` unhealthy | Bad/expired `TS_AUTHKEY`, or `/dev/net/tun` missing; check `make logs S=tailscale`. |
| Bashbox fallback commands (`git`/`go`/…) "not found" | The plain `denngubsky/loomcycle` image is minimal — either switch the `loomcycle` + `loomcycle-migrate` images to `denngubsky/loomcycle-toolbox:<same-tag>`, or rely on the `sandbox` preset (its session image has the full toolchain). |
