# Installing loomcycle on TrueNAS SCALE — a walkthrough

A complete, copy-paste install for **TrueNAS SCALE Electric Eel 24.10+** using
**Route A — the custom-app paste compose** (the validated path; see
[`README.md`](README.md) for the catalog-app route and
[`../../docs/TRUENAS.md`](../../docs/TRUENAS.md) for the reference runbook).

Replace `tank` with your pool name and the `CHANGE_ME` / `PASTE_…` placeholders
throughout.

> **Image prerequisite.** Pin **`denngubsky/loomcycle:1.6.0` or newer** — v1.6.0 is
> the first release with the embedded presets (RFC AQ) that this app's
> `LOOMCYCLE_PRESETS` relies on. Older tags (≤ v1.5.0) have no presets and will fail
> to boot with `no config found`.

---

## Phase 1 — Postgres (your existing PG16)

loomcycle bundles no database. On your Postgres:

```sql
CREATE ROLE loomcycle LOGIN PASSWORD 'CHANGE_ME_STRONG';
CREATE DATABASE loomcycle        OWNER loomcycle;
CREATE DATABASE loomcycle_sqlmem OWNER loomcycle;   -- only if you enable SQL Memory
```

Your DSNs (used below):
`postgres://loomcycle:CHANGE_ME_STRONG@PG_HOST:5432/loomcycle?sslmode=disable`
(and `…/loomcycle_sqlmem`). Confirm the TrueNAS apps network can reach `PG_HOST:5432`.

## Phase 2 — Datasets (owned by uid 65532)

The distroless image runs as **uid 65532** — the mapped datasets must be owned by
it. From the TrueNAS shell:

```sh
mkdir -p /mnt/tank/loomcycle/{data,config,work}
chown -R 65532:65532 /mnt/tank/loomcycle
```

- `data` → loomcycle's own state (SQL-Memory cache, snapshots)
- `config` → the thin overlay (`loomcycle.yaml`)
- `work` → an agent filesystem Volume (RFC AH); add more datasets + mounts as needed

## Phase 3 — The config overlay

The presets supply the provider matrix; this overlay supplies only the agent
Volumes. Create `/mnt/tank/loomcycle/config/loomcycle.yaml` (copy
[`loomcycle.overlay.example.yaml`](loomcycle.overlay.example.yaml)):

```yaml
volumes:
  work:
    path: /mnt/work       # the IN-CONTAINER mount point (Phase 5 maps the dataset here)
    mode: rw
    default: true
```

## Phase 4 — Secrets

```sh
openssl rand -hex 32   # → LOOMCYCLE_AUTH_TOKEN (Web UI + API bearer)
openssl rand -hex 32   # → LOOMCYCLE_OPERATOR_TOKEN_PEPPER (hardens minted token hashes)
```

Plus at least one provider key (e.g. `ANTHROPIC_API_KEY`).

## Phase 5 — Install the app

**Apps → Discover Apps → ⋮ → Install via YAML.** Name it `loomcycle`, paste this
(placeholders filled), Install:

```yaml
name: loomcycle
services:
  loomcycle-migrate:
    image: denngubsky/loomcycle:1.6.0
    user: "65532:65532"
    restart: "no"
    command: ["migrate", "up"]
    environment:
      LOOMCYCLE_STORAGE_BACKEND: postgres
      LOOMCYCLE_PG_DSN: postgres://loomcycle:CHANGE_ME_STRONG@PG_HOST:5432/loomcycle?sslmode=disable
      LOOMCYCLE_SQLMEM_PG_DSN: postgres://loomcycle:CHANGE_ME_STRONG@PG_HOST:5432/loomcycle_sqlmem?sslmode=disable
  loomcycle:
    image: denngubsky/loomcycle:1.6.0
    user: "65532:65532"
    restart: unless-stopped
    depends_on:
      loomcycle-migrate:
        condition: service_completed_successfully
    ports:
      - "8787:8787"
    environment:
      LOOMCYCLE_LISTEN_ADDR: "0.0.0.0:8787"
      LOOMCYCLE_PRESETS: "base,document-agent"   # base matrix + the Document Assistant
      LOOMCYCLE_CONFIG_DIR: /config
      LOOMCYCLE_STORAGE_BACKEND: postgres
      LOOMCYCLE_PG_DSN: postgres://loomcycle:CHANGE_ME_STRONG@PG_HOST:5432/loomcycle?sslmode=disable
      LOOMCYCLE_SQLMEM_PG_DSN: postgres://loomcycle:CHANGE_ME_STRONG@PG_HOST:5432/loomcycle_sqlmem?sslmode=disable
      LOOMCYCLE_SQLMEM_ENABLED: "1"              # document-agent needs it
      LOOMCYCLE_PG_AUTOMIGRATE: "0"              # the init service handles migrations
      LOOMCYCLE_DATA_DIR: /data
      LOOMCYCLE_AUTH_TOKEN: PASTE_TOKEN_FROM_PHASE_4
      LOOMCYCLE_OPERATOR_TOKEN_PEPPER: PASTE_PEPPER_FROM_PHASE_4
      ANTHROPIC_API_KEY: PASTE_PROVIDER_KEY
    volumes:
      - /mnt/tank/loomcycle/data:/data
      - /mnt/tank/loomcycle/config:/config:ro
      - /mnt/tank/loomcycle/work:/mnt/work
    healthcheck:
      test: ["CMD", "/usr/local/bin/loomcycle", "health"]
      interval: 30s
      timeout: 5s
      retries: 5
      start_period: 20s
```

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
| `EACCES` / permission denied on `/data` | Datasets not owned by 65532 — `chown -R 65532:65532 /mnt/tank/loomcycle`. |
| migrate `connection refused` | DSN host/port unreachable from the apps network, or the role/DB doesn't exist (Phase 1). |
| `doc-manager` idle | Needs `LOOMCYCLE_SQLMEM_ENABLED=1` + the `loomcycle_sqlmem` DSN + a `middle` tier (base provides one). |
| An agent has no file access | Its bound volume isn't mapped — check the overlay `volumes:` `path` equals the in-container mount path (Phase 3 ↔ Phase 5). |
