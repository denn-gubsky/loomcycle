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

## Phase 1 — Lay down the deploy directory

Copy this `cloud-deployment/` directory to the host deploy path and create the
runtime sub-directories:

```bash
mkdir -p /home/denn/work/loomcycle-cloud
cp -r cloud-deployment/* cloud-deployment/.gitignore /home/denn/work/loomcycle-cloud/
cd /home/denn/work/loomcycle-cloud

mkdir -p data config work pgdata ts-state searxng web caddy-data caddy-config retention-exports
# loomcycle + its migrate run as uid:gid 65532; give them their writable dirs.
sudo chown -R 65532:65532 data work retention-exports
```

`config/loomcycle.yaml` (SearXNG wiring), `searxng/settings.yml`, `Caddyfile`, and
`postgres/initdb.d/00-init.sql` came with the copy — leave them in place.

---

## Phase 2 — Landing page

Copy the drafted static site into `web/` (Caddy serves it; the `functions/` dir is
unused because Caddy reproduces those two proxies):

```bash
cp -r /path/to/loomcycle/cloud-web/* /home/denn/work/loomcycle-cloud/web/
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
   | `loomcycle.cloud`     | `http://landing:80` |

   > ⚠️ **The app hostname points at `tailscale:8787`, NOT `loomcycle:8787`.**
   > loomcycle shares the tailscale netns and has no DNS name of its own.

3. Cloudflare auto-creates the proxied DNS records for the tunnel. (Optional but
   recommended: add a **Cloudflare Access** application over `app.loomcycle.cloud`.)

---

## Phase 5 — Env files

```bash
cp .env.insecure.example .env.insecure
cp .env.secure.example   .env.secure
chmod 600 .env.secure
```

Edit **`.env.insecure`** (non-secret):
- `OLLAMA_BASE_URL` → the Ollama tailnet **IP** (Phase 3).
- `LOOMCYCLE_PUBLIC_URL=https://app.loomcycle.cloud` (already set).
- Adjust presets / retention / GPU knobs to taste.

Edit **`.env.secure`** (secrets — fill every `REPLACE_ME`):
- Generate tokens: `openssl rand -hex 32` for `LOOMCYCLE_AUTH_TOKEN`,
  `LOOMCYCLE_OPERATOR_TOKEN_PEPPER`, `SANDBOX_AUTH_TOKEN`, `SEARXNG_SECRET`.
- `POSTGRES_PASSWORD` and the password embedded in **both**
  `LOOMCYCLE_PG_DSN` + `LOOMCYCLE_SQLMEM_PG_DSN` **must match**.
- `TS_AUTHKEY` (Phase 3), `CLOUDFLARE_TUNNEL_TOKEN` (Phase 4).
- At least one provider key; `BRAVE_API_KEY` only if you want a paid search fallback.

The DSN host is **`postgres`** (the compose service) — don't change it.

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
4. **In-network**: `docker compose exec landing wget -qO- http://tailscale:8787/healthz` → ok;
   SearXNG: `docker compose exec searxng wget -qO- http://localhost:8080/healthz` → ok.
5. **Public**: `curl -s https://app.loomcycle.cloud/healthz` and
   `curl -s https://loomcycle.cloud/api/health` (landing → live status strip). Then
   browse `https://app.loomcycle.cloud/ui` and sign in with `LOOMCYCLE_AUTH_TOKEN`.
6. **Functional**: run a `chat/medium` agent that uses WebSearch (proves SearXNG),
   a `dev/sandbox` run (proves the builder sidecar via `mcp__sandbox__*`), and a
   Document op (proves SQL Memory + pgvector).

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
| landing status strip grey / `/api/config` shows "not published" | `LOOMCYCLE_PUBLIC_CONFIG=1` must be set (it is by default); confirm Caddy can reach `tailscale:8787`. |
| Ollama unreachable from loomcycle | Use the **raw tailnet IP** in `OLLAMA_BASE_URL` (MagicDNS is unavailable in the shared netns); confirm `tailscale status` shows the peer. |
| `tailscale` unhealthy | Bad/expired `TS_AUTHKEY`, or `/dev/net/tun` missing; check `make logs S=tailscale`. |
| Bashbox fallback commands (`git`/`go`/…) "not found" | The plain `denngubsky/loomcycle` image is minimal — either switch the `loomcycle` + `loomcycle-migrate` images to `denngubsky/loomcycle-toolbox:<same-tag>`, or rely on the `sandbox` preset (its session image has the full toolchain). |
