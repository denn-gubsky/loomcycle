# Installing loomcycle on TrueNAS SCALE — a walkthrough

A complete, copy-paste install for **TrueNAS SCALE Electric Eel 24.10+** using
**Route A — the custom-app paste compose** (the validated path; see
[`README.md`](README.md) for the catalog-app route and
[`../../docs/TRUENAS.md`](../../docs/TRUENAS.md) for the reference runbook).

Replace `APPS2` with your pool name, `PG_HOST` with your Postgres host,
`TRUENAS_SCALE_HOST` with your TrueNAS host, and fill the `CHANGE_ME` / `PASTE_…`
placeholders throughout. **All secrets live in an external env file (Phase 4) —
never in the YAML you paste into the TrueNAS UI.**

> **Image prerequisite.** Pin **`denngubsky/loomcycle:1.6.0` or newer** — v1.6.0 is
> the first release with the embedded presets (RFC AQ) that this app's
> `LOOMCYCLE_PRESETS` relies on. Older tags (≤ v1.5.0) have no presets and will fail
> to boot with `no config found`.

---

## Phase 1 — Postgres (your existing instance, ≥ 14)

loomcycle bundles no database. On your Postgres:

```sql
-- CREATEROLE is REQUIRED if you enable SQL Memory (and therefore Documents):
-- the SQL-Memory Postgres tier isolates each scope in its own login role, so the
-- role behind LOOMCYCLE_SQLMEM_PG_DSN provisions per-scope roles at runtime.
-- Without it, every Document/SQL-Memory op fails with
-- "permission denied to create role (SQLSTATE 42501)". Harmless if you don't use
-- SQL Memory. See docs/SQL_MEMORY.md for the full provisioning model.
CREATE ROLE loomcycle LOGIN PASSWORD 'CHANGE_ME_STRONG' CREATEROLE;
CREATE DATABASE loomcycle        OWNER loomcycle;
CREATE DATABASE loomcycle_sqlmem OWNER loomcycle;   -- only if you enable SQL Memory
```

> Already installed without `CREATEROLE` and hitting "permission denied to create
> role"? Grant it in place (as a Postgres superuser), no reinstall needed:
> `ALTER ROLE loomcycle CREATEROLE;`

Your DSNs (used below):
`postgres://loomcycle:CHANGE_ME_STRONG@PG_HOST:5432/loomcycle?sslmode=disable`
(and `…/loomcycle_sqlmem`). Confirm the TrueNAS apps network can reach `PG_HOST:5432`.

## Phase 2 — Datasets (owned by uid 65532)

The distroless image runs as **uid 65532** — the mapped datasets must be owned by
it. From the TrueNAS shell:

```sh
mkdir -p /mnt/APPS2/loomcycle/{data,config,work}
chown -R 65532:65532 /mnt/APPS2/loomcycle
```

- `data` → loomcycle's own state (SQL-Memory cache, snapshots)
- `config` → the thin overlay (`loomcycle.yaml`) + the secrets env file (Phase 4)
- `work` → an agent filesystem Volume (RFC AH); add more datasets + mounts as needed

## Phase 3 — The config overlay

The presets supply the provider matrix; this overlay supplies only the agent
Volumes. Create `/mnt/APPS2/loomcycle/config/loomcycle.yaml` (copy
[`loomcycle.overlay.example.yaml`](loomcycle.overlay.example.yaml)):

```yaml
volumes:
  work:
    path: /mnt/work       # the IN-CONTAINER mount point (Phase 5 maps the dataset here)
    mode: rw
    default: true
```

## Phase 4 — Secrets & the secure env file

Keep **every** secret out of the pasted compose. First generate the two runtime
tokens:

```sh
openssl rand -hex 32   # → LOOMCYCLE_AUTH_TOKEN (Web UI + API bearer)
openssl rand -hex 32   # → LOOMCYCLE_OPERATOR_TOKEN_PEPPER (hardens minted token hashes)
```

Then write **all** secrets — the two tokens, the Postgres DSNs (they embed the DB
password), and every provider key — into a root-protected env file on the `config`
dataset. The compose `env_file:` directive (Phase 5) reads it and injects each
entry into the container as a real environment variable at deploy time, so nothing
secret ever appears in the YAML you paste into the TrueNAS UI.

> **Why a file, not `${VAR}` substitution?** Compose resolves `${VAR}` at *parse*
> time from a `.env` in the compose **project directory** — which *Install via YAML*
> generates and never exposes, so `${VAR}` would resolve to empty. `env_file:`
> instead reads an **absolute host path** at container-create time. That is the
> mechanism that actually works on a TrueNAS custom app.

From the TrueNAS shell:

```sh
cat > /mnt/APPS2/loomcycle/config/loomcycle.secrets.env <<'EOF'
# Runtime secrets — read by the Docker engine via compose `env_file:`.
# Format: raw KEY=value, one per line, NO quotes, NO `export`. `#` comments OK.
# Values are LITERAL — no ${VAR} expansion, and no referencing another line.
# Write the host + password out in full (e.g. @truenas.local, not @${PG_HOST}).
LOOMCYCLE_AUTH_TOKEN=PASTE_TOKEN_FROM_ABOVE
LOOMCYCLE_OPERATOR_TOKEN_PEPPER=PASTE_PEPPER_FROM_ABOVE
LOOMCYCLE_PG_DSN=postgres://loomcycle:CHANGE_ME_STRONG@PG_HOST:5432/loomcycle?sslmode=disable
LOOMCYCLE_SQLMEM_PG_DSN=postgres://loomcycle:CHANGE_ME_STRONG@PG_HOST:5432/loomcycle_sqlmem?sslmode=disable
ANTHROPIC_API_KEY=PASTE_PROVIDER_KEY
DEEPSEEK_API_KEY=PASTE_PROVIDER_KEY
GEMINI_API_KEY=PASTE_PROVIDER_KEY
OLLAMA_API_KEY=PASTE_PROVIDER_KEY
BRAVE_API_KEY=PASTE_BRAVE_API_KEY
EOF
chmod 600 /mnt/APPS2/loomcycle/config/loomcycle.secrets.env
```

Notes:

- **Values are literal — no `${VAR}` expansion.** An `env_file` is not a shell
  script: each line is a verbatim `KEY=value`, and `${PG_HOST}` / `${PG_PASSWORD}`
  would be passed through as that literal text (a broken DSN), not substituted —
  including references to another line in the same file. Write the host and
  password out in full. (`TRUENAS_SCALE_HOST` / `PG_HOST` in this guide are
  find-and-replace placeholders you fill in when authoring the file, not runtime
  variables.) If you want a single source of truth, expand it once at authoring
  time into literals — e.g. `TRUENAS_SCALE_HOST=truenas.local PG_PASSWORD=… \
  envsubst < template > loomcycle.secrets.env`.
- **Only the keys you use.** Delete any provider line you have no key for — an
  absent key just means that provider isn't wired.
- **Permissions:** the file is read by the host Docker engine (root), not by the
  uid-65532 container user, so `chmod 600` is both sufficient and the safest.
- **No clash with your overlay:** `LOOMCYCLE_CONFIG_DIR=/config` loads only
  `*.yaml`/`*.yml`, so this `.env` on the same dataset is ignored as config. It is
  still visible inside the container at `/config/loomcycle.secrets.env` (harmless —
  the process already holds these as env). To keep it out of the container
  entirely, put it on a sibling dataset you don't mount and point `env_file:` there.

## Phase 5 — Install the app

**Apps → Discover Apps → ⋮ → Install via YAML.** Name it `loomcycle`, paste this
(placeholders filled), Install:

```yaml
name: loomcycle
services:
  loomcycle-migrate:
    image: denngubsky/loomcycle:1.11.1
    user: "65532:65532"
    restart: "no"
    command: ["migrate", "up"]
    env_file:
      - /mnt/APPS2/loomcycle/config/loomcycle.secrets.env   # PG DSNs come from here
    environment:
      LOOMCYCLE_STORAGE_BACKEND: postgres
  loomcycle:
    image: denngubsky/loomcycle:1.11.1
    user: "65532:65532"
    restart: unless-stopped
    depends_on:
      loomcycle-migrate:
        condition: service_completed_successfully
    ports:
      - "8787:8787"
    env_file:
      - /mnt/APPS2/loomcycle/config/loomcycle.secrets.env   # tokens, DSNs, provider keys
    environment:
      # --- non-secret ("insecure") operational config — safe to keep inline ---
      LOOMCYCLE_LISTEN_ADDR: "0.0.0.0:8787"
      LOOMCYCLE_PUBLIC_URL: "http://TRUENAS_SCALE_HOST:8787"   # externally-reachable base URL (your tunnel/proxy host, or this for direct LAN); agents read it via `Context op=self` (≥ v1.6.7)
      LOOMCYCLE_PRESETS: "base,document-agent"   # base matrix + the Document Assistant
      LOOMCYCLE_CONFIG_DIR: /config
      LOOMCYCLE_STORAGE_BACKEND: postgres
      LOOMCYCLE_SQLMEM_ENABLED: "1"              # document-agent needs it
      LOOMCYCLE_PG_AUTOMIGRATE: "0"              # the init service handles migrations
      LOOMCYCLE_DATA_DIR: /data

      LOOMCYCLE_METRICS_ENABLED: "1"
      LOOMCYCLE_METRICS_COLLECT_SYSTEM: "1"
      LOOMCYCLE_METRICS_RETENTION_DAYS: "7"
      LOOMCYCLE_CODE_AGENTS_ENABLED: "1"
      LOOMCYCLE_WEBHOOKS_ENABLED: "1"
      LOOMCYCLE_SCHEDULER_ENABLED: "1"
      LOOMCYCLE_AUDIT_LOG_PATH: /data/audit.log   # a FILE, not the /data dir
      LOOMCYCLE_PG_MAX_OPEN_CONNS: "48"
      LOOMCYCLE_PG_MIN_IDLE_CONNS: "8"
      OLLAMA_BASE_URL: http://TRUENAS_SCALE_HOST:11434

      # --- shell tools (opt-in; grant per agent via allowed_tools) ---
      # Bashbox: in-process gbash sandbox (no OS process, rooted at the agent's
      # rw volume, no network). The safe default.
      LOOMCYCLE_BASHBOX_ENABLED: "1"
      # Bash: NOT a sandbox — reaches arbitrary files via absolute paths and the
      # network. Enabled here per request; only grant it to trusted agents.
      LOOMCYCLE_BASH_ENABLED: "1"
      # Host-command fallback: these named commands ESCAPE the gbash sandbox and
      # run on the real host (rw volume required). Keep the list tight.
      LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS: git,gh,curl,jq,rsync

      # --- HTTP / WebFetch host policy ---
      # No wildcard exists; this suffix-list allows whole TLDs (e.g. "com" ⇒ any
      # *.com). Broadly open to public hosts; private IPs stay hard-blocked.
      # CALLER_AUTHORITATIVE lets a run further narrow this per-call.
      LOOMCYCLE_HTTP_HOST_ALLOWLIST: com,org,net,io,ai,dev,gov,edu,co,me,app,xyz,uk,de
    volumes:
      - /mnt/APPS2/loomcycle/data:/data
      - /mnt/APPS2/loomcycle/config:/config:ro
      - /mnt/APPS2/loomcycle/work:/mnt/work
    healthcheck:
      test: ["CMD", "/usr/local/bin/loomcycle", "health"]
      interval: 30s
      timeout: 5s
      retries: 5
      start_period: 20s
```

> **Secrets vs. env precedence.** Both services pull secrets from the same
> `env_file`. If you ever set the same key in `environment:` too, the inline
> `environment:` value **wins** — so keep each secret in *only* the env file and
> never re-add an `LOOMCYCLE_AUTH_TOKEN:` / `*_API_KEY:` / `*_PG_DSN:` line here.

## Phase 6 — Verify

1. **App logs** (Apps → loomcycle → Logs): the `loomcycle-migrate` step runs and
   exits, then the runtime logs
   `config: layering 2 embedded preset(s)/bundle(s) as base: base, document-agent`
   and `loomcycle listening on 0.0.0.0:8787`.
2. **Health**: the app reports healthy (the `loomcycle health` check passes).
3. **Web UI**: browse `http://<truenas>:8787/ui`, sign in with `LOOMCYCLE_AUTH_TOKEN`
   → super-admin.
4. **Mint a tenant token** (no shell): the gear (top-right) → **Settings → Tokens** →
   Generate (name, tenant, `substrate:tenant`) → copy the once-shown secret.

## Phase 7 — Ingress (Cloudflare Tunnel)

Don't expose `8787` publicly. Point your existing `cloudflared` tunnel at the app's
internal address and gate it behind Cloudflare Access:

```yaml
# tunnel ingress:
- hostname: loomcycle.yourdomain
  service: http://<truenas-apps-ip>:8787
```

## Phase 8 — Upgrade

Bump the image tag → new binary → refreshed embedded presets automatically; your env
+ overlay persist. If the new version bumped the Postgres schema, the
`loomcycle-migrate` step re-runs `migrate up` on redeploy.

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `no config found` at boot | Image predates RFC AQ — pin `denngubsky/loomcycle:1.6.0` or newer. |
| `EACCES` / permission denied on `/data` | Datasets not owned by 65532 — `chown -R 65532:65532 /mnt/APPS2/loomcycle`. |
| migrate `connection refused` | DSN host/port unreachable from the apps network, or the role/DB doesn't exist (Phase 1). |
| App healthy but auth fails / no providers wired | The `env_file` didn't load — confirm the **absolute path** in `env_file:` is exact and the file is `chmod 600` (root-readable). Remember an `environment:` key overrides the same key in `env_file`, so don't re-add a secret inline. |
| migrate `LOOMCYCLE_PG_DSN ... required` | The `env_file:` line is missing from the `loomcycle-migrate` service (it needs the DSNs too), or the path is wrong. |
| Crash-loop ending `sqlmem audit: ... open /data: is a directory` | `LOOMCYCLE_AUDIT_LOG_PATH` must be a **file**, not the `/data` mount — set `/data/audit.log` (or remove the line; the audit log is optional). |
| Documents fail with `sqlmem: provision scope: ... permission denied to create role` | The SQL-Memory DSN's role lacks `CREATEROLE` (it provisions a per-scope login role). Grant it: `ALTER ROLE loomcycle CREATEROLE;` (Phase 1 now creates the role with it). |
| `doc-manager` idle | Needs `LOOMCYCLE_SQLMEM_ENABLED=1` + the `loomcycle_sqlmem` DSN + a `middle` tier (base provides one). |
| An agent has no file access | Its bound volume isn't mapped — check the overlay `volumes:` `path` equals the in-container mount path (Phase 3 ↔ Phase 5). |
