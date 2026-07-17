# Running loomcycle on TrueNAS SCALE

**Why a packaged app, not hand-rolled compose-over-SSH?** TrueNAS operators expect
to install from a form, edit it later in the UI, and let dataset snapshots back it
up — not SSH in to hand-edit YAML. What made that practical is **embedded presets**
(RFC AQ): the binary ships the provider/tier matrix and the built-in agents inside
itself, so the install form only collects the *thin, deployment-specific* overlay —
which presets, the secrets, your Postgres DSN, and which datasets the agents may
touch. This is the first entry in loomcycle's operator-cookbook of deployment
postures. **The thesis: on TrueNAS, loomcycle is install-the-app-fill-the-form, and
everything provider-shaped already lives in the image.**

Applies to **TrueNAS SCALE Electric Eel 24.10+** (the Docker-Compose app engine;
also 25.04 Fangtooth). The pre-24.10 Helm/k8s app format does not apply.

---

## What you provide vs. what the image provides

| The image (RFC AQ) | You (the form / overlay) |
|---|---|
| All providers + tiers (`LOOMCYCLE_PRESETS=base`), OAuth/local overlays, the `document-agent` Document Assistant | Which presets to enable |
| The runtime, Web UI, MCP server, all tools | The bearer token + provider API key(s) |
| Migrations (`loomcycle migrate up`) | A Postgres ≥ 14 connection (the app bundles **no** DB) |
| — | The ZFS datasets agents may read/write (RFC AH Volumes) |

Secrets are **TrueNAS-managed env**, never written to a YAML layer. loomcycle does
not source `.env.local` in a container — only real env + the mounted overlay.

---

## Prerequisites (on the TrueNAS host)

1. **Postgres ≥ 14** reachable from the apps network (your existing instance,
   DB-per-service — see [`POSTGRES.md`](POSTGRES.md)). Create the databases:
   ```sql
   CREATE DATABASE loomcycle;
   CREATE DATABASE loomcycle_sqlmem;   -- only if you enable SQL Memory
   -- CREATEROLE is required for SQL Memory: each scope gets its own login role,
   -- provisioned at runtime by the SQL-Memory DSN's role. Without it Documents
   -- fail with "permission denied to create role". Omit if you don't use SQL Memory.
   CREATE ROLE loomcycle LOGIN PASSWORD '…' CREATEROLE;
   GRANT ALL PRIVILEGES ON DATABASE loomcycle TO loomcycle;
   GRANT ALL PRIVILEGES ON DATABASE loomcycle_sqlmem TO loomcycle;
   ```
2. **Datasets**, owned by the distroless uid (the image is fixed at **65532**):
   ```sh
   mkdir -p /mnt/tank/loomcycle/{data,config,work}
   chown -R 65532:65532 /mnt/tank/loomcycle
   ```
   - `data` → loomcycle's own state (SQL-Memory cache, snapshots).
   - `config` → the thin overlay (`loomcycle.yaml`).
   - `work` (and any more) → agent filesystem Volumes (RFC AH).
3. The **config overlay**: copy
   [`deploy/truenas/loomcycle.overlay.example.yaml`](../deploy/truenas/loomcycle.overlay.example.yaml)
   to `/mnt/tank/loomcycle/config/loomcycle.yaml` and edit its `volumes:` block so
   each entry's `path` is the **in-container** mount point you'll map below.

---

## Two routes

| Route | How | When |
|---|---|---|
| **A — custom app** | Apps → Discover → ⋮ → *Install via YAML*, paste the compose | Fastest, works today; edit the compose directly. |
| **B — catalog app** | Install wizard from a published train | Formal — a form you re-open to edit; needs render-validation first. |

### Route A — custom app (paste compose)

1. Edit [`deploy/truenas/docker-compose.yaml`](../deploy/truenas/docker-compose.yaml):
   put your **secrets** (`LOOMCYCLE_AUTH_TOKEN`, the Postgres DSNs, provider keys) in
   the root-only `loomcycle.secrets.env` the compose's `env_file:` points at — never
   inline (TrueNAS's paste-YAML can't resolve `${VAR}`); set the non-secret env
   (`LOOMCYCLE_PRESETS`, `LOOMCYCLE_PUBLIC_URL`, the pool/host placeholders) inline;
   and point the host-path mounts at your datasets. **Pin the image tag** (`:latest`
   is for testing only) — and choose toolbox vs distroless per the image-choice note
   below and in [`INSTALL.md`](../deploy/truenas/INSTALL.md).
2. Apps → **Discover Apps** → the **⋮** menu → **Install via YAML**. Name it
   `loomcycle`, paste the file, install.
3. The `loomcycle-migrate` service runs `migrate up` once; the runtime waits for it,
   then serves on `:8787`. Watch the app logs for `loomcycle listening`.

### Route B — catalog app (wizard)

The catalog source is [`deploy/truenas/catalog/`](../deploy/truenas/catalog/)
(`app.yaml`, `questions.yaml`, `ix_values.yaml`, `templates/`). The install form
groups: **Providers & Presets** (multiselect), **Secrets**, **Storage (Postgres)**,
**Runtime Options**, **Network**, **Storage (Datasets)**, and **Advanced
configuration**.

The **Advanced configuration** group exposes every non-secret knob from the env
catalogue as a documented field (blank = the binary default). It is **generated**
from the embedded `.env.insecure.example`, so it never drifts — regenerate it after
changing the env catalogue:

```sh
loomcycle truenas-questions   # emits the env_options block; splice under `questions:`
```

The template wires it with one generic loop (`for key, val in values.env_options`),
so adding a knob to `.env.insecure.example` + regenerating is all it takes — no
per-knob template edit.

> **⚠️ Validate before publishing.** `templates/docker-compose.yaml` uses the TrueNAS
> Jinja2 + `ix-lib` render library, which the catalog CI vendors at build time (it is
> not committed). Render-test it on **your** TrueNAS version and ix-lib release
> before publishing to a train — confirm in particular the **healthcheck exec form**
> (distroless has no shell, so the test must be `["CMD","/usr/local/bin/loomcycle",
> "health"]`, not a shell string) and the storage model. Until then, use **Route A**.

To publish: add `deploy/truenas/catalog/` as `ix-dev/community/loomcycle/` in a
catalog/train git repo (the `truenas/apps` layout), let its CI vendor the library +
generate the `trains/` output, and point TrueNAS at the catalog.

---

## Postgres & migrations

- `LOOMCYCLE_STORAGE_BACKEND=postgres`, `LOOMCYCLE_PG_DSN=…` (store),
  `LOOMCYCLE_SQLMEM_PG_DSN=…` (SQL Memory, only if `LOOMCYCLE_SQLMEM_ENABLED=1`).
- **Migrations are decoupled from the image deploy.** Route A runs them in a one-shot
  `loomcycle-migrate` init service (`LOOMCYCLE_PG_AUTOMIGRATE=0` on the runtime).
  Route B exposes an **Auto-migrate on boot** toggle (`=1`, default on for a single
  operator) — or turn it off and run `loomcycle migrate up` yourself before deploy.
- TrueNAS dataset snapshots back up `data` + your agent datasets; that's orthogonal
  to loomcycle's own `/v1/_snapshots`.

## Web search (SearXNG sidecar)

For a free, self-hosted web-search provider, add a **SearXNG sidecar** on the same
compose network as loomcycle — reachable in-network at `http://searxng:8080`, no
host port. It's the same pattern on TrueNAS as anywhere else: a `searxng` service,
a `searxng/settings.yml` with three required knobs (`secret_key`, `limiter: false`,
`formats: [html, json]`), and `search_providers: { searxng: { base_url: … } }` +
`search_priority:` in the overlay. Full recipe + verification: **[`docs/SEARCH.md`](SEARCH.md)**
(the commented sidecar is in [`docker-compose.example.yaml`](../docker-compose.example.yaml)).

## Code execution (the toolbox image)

By default the app runs the **distroless** image — no shell, no compilers, so agents
can't run scripts or compile code. To let them, switch both services to the
**`denngubsky/loomcycle-toolbox`** image (same uid + mount paths — a drop-in swap; it
bakes in python / go / rust / c++ / node + git/gh/curl/jq/rsync/wget/unzip/sqlite3)
and enable a shell tool, then grant it to an agent via its `tools:`:

- **`LOOMCYCLE_BASH_ENABLED=1`** — the raw `Bash` tool (unsandboxed: the whole
  toolchain, and it can run a compiled `./binary`). Simplest for a trusted box.
- **`LOOMCYCLE_BASHBOX_ENABLED=1`** + `LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS=<drivers>`
  + **`LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_ENV=HOME`** — the mostly-sandboxed Bashbox,
  where only allowlisted host commands escape. `HOME` is **required** or `go`/`cargo`/
  `npm`/`pip` fail (they need it for their caches); don't allowlist `jq`/`awk` (native
  to Bashbox) or `bash`/`sh`/`env`; a bare `./binary` can't run via Bashbox (use
  `go run` / `cargo run`, or the raw `Bash` tool).

⚠️ Code runs in loomcycle's OWN container — **single-tenant / trusted only**. For
untrusted or multi-tenant code, keep the distroless image and run each session in an
isolated, ephemeral container via the **builder sidecar** ([`SANDBOX.md`](SANDBOX.md)).
Full tradeoff + tool reference: [`TOOLBOX_IMAGE.md`](TOOLBOX_IMAGE.md). The paste
compose in [`deploy/truenas/docker-compose.yaml`](../deploy/truenas/docker-compose.yaml)
ships this block ready to use (toolbox image); walkthrough in
[`INSTALL.md`](../deploy/truenas/INSTALL.md).

## Volumes → datasets (RFC AH)

Each agent Volume is **a host dataset mounted into the container** + **a `volumes:`
entry in the overlay** that names it. The mount's in-container path must equal the
overlay entry's `path`:

```
compose:  /mnt/tank/loomcycle/work : /mnt/work        ┐ same in-container path
overlay:  volumes: { work: { path: /mnt/work, mode: rw, default: true } } ┘
```

An agent bound to `work` (or any agent, if `work` is `default: true`) reads/writes
that dataset, confined by RFC AH. With no `default` and no binding, an agent has no
filesystem access. Adding a Volume = a new mount + a new overlay entry + redeploy.
(Route B turns these into repeating form rows — RFC AR Phase 2.) **All mapped
datasets must be `chown`ed to 65532:65532.**

## Presets & secrets

- **Presets** (`LOOMCYCLE_PRESETS`, comma-separated, ordered): `base` for the full
  matrix; add `oauth` or `local` to prepend a provider on top; add `document-agent`
  to register the Document Assistant (needs `LOOMCYCLE_SQLMEM_ENABLED=1` + a `middle`
  tier). Browse them with `loomcycle presets` or the Web UI **Settings → Presets**.
- **Mint tenant tokens from the Web UI** (no shell needed): sign in with the admin
  (root) token, click the gear → **Settings → Tokens** ([`operator-tokens`](../internal/help/builtin/operator-tokens.md)).

## Health & upgrade

- **Health:** the container `HEALTHCHECK` is `loomcycle health` (GETs the unauth
  `/healthz` — distroless has no `curl`).
- **Upgrade:** bump the image tag → new binary → refreshed embedded presets
  automatically; your form answers / overlay persist. Run `migrate up` (or leave
  auto-migrate on) if the new version bumped the Postgres schema.

## Ingress (out of scope)

loomcycle has no built-in TLS. The container binds `0.0.0.0:8787` on the apps
network; front it with your existing tunnel/proxy (the house posture is a Cloudflare
Tunnel — no exposed host ports). The form does not imply a public bind.

## Version pinning

Pin both the **loomcycle image tag** (a release with RFC AQ) and the **TrueNAS
version** you validated against (`app.yaml: annotations.min_scale_version`, set to
`24.10.2.2`). The `questions.yaml`/`app.yaml` schema is stable across Electric Eel →
Fangtooth; the gating field is `lib_version` (the ix-lib render API). Re-validate on
a major TrueNAS upgrade.
