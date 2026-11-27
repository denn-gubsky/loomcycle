# Installing loomcycle on TrueNAS SCALE — a walkthrough

A complete, copy-paste install for **TrueNAS SCALE Electric Eel 24.10+** using
**Route A — the custom-app paste compose** (the validated path; see
[`README.md`](README.md) for the catalog-app route and
[`../../docs/TRUENAS.md`](../../docs/TRUENAS.md) for the reference runbook).

Replace `APPS2` with your pool name, `PG_HOST` with your Postgres host,
`TRUENAS_SCALE_HOST` with your TrueNAS host, `OLLAMA_HOST` with your Ollama host (if
you use local models), and fill the `CHANGE_ME` / `PASTE_…` placeholders throughout. **All secrets live in an external env file (Phase 4) —
never in the YAML you paste into the TrueNAS UI.**

> **Image.** This walkthrough uses the distroless **`denngubsky/loomcycle:1.153.0`**
> image: no shell and no toolchain. Agents do not run commands in the runtime's
> container; they run them in a workspace on a workspace worker (the `Workspace`
> tool). The [browser + workspace variant](#browser--workspace-variant) below adds a
> workspace worker on the container engine. The `Bash` and `Bashbox` tools, the
> `loomcycle-toolbox` image and the builder sidecar were removed in v1.172.0
> (`docs/MIGRATING-FROM-BASH.md`). Pin **≥ v1.6.0** — the first release with the
> embedded presets (RFC AQ) this app's `LOOMCYCLE_PRESETS` relies on; older tags fail
> to boot with `no config found`.
>
> **Registry.** Every loomcycle image is published to Docker Hub *and* to
> `ghcr.io/denn-gubsky/…` (same build, same digest). If a pull fails with
> `toomanyrequests`, swap the prefix — `ghcr.io/denn-gubsky/loomcycle:1.153.0`
> — everywhere in the YAML below. See "Docker Hub rate limits" in Troubleshooting;
> TrueNAS is unusually exposed to this because it re-checks every installed app for
> image updates on a timer, and each check spends from the same anonymous quota.

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

> The per-scope roles connect to `loomcycle_sqlmem` through `PUBLIC`'s `CONNECT`
> (the default). If your Postgres revokes `CONNECT` from `PUBLIC`, restore it on
> this database: `GRANT CONNECT ON DATABASE loomcycle_sqlmem TO PUBLIC;`

Your DSNs (used below):
`postgres://loomcycle:CHANGE_ME_STRONG@PG_HOST:5432/loomcycle?sslmode=disable`
(and `…/loomcycle_sqlmem`). Confirm the TrueNAS apps network can reach `PG_HOST:5432`.

## Phase 2 — Datasets (owned by uid 65532)

The distroless image runs as **uid 65532** — the mapped datasets must be owned by
it. From the TrueNAS shell:

```sh
mkdir -p /mnt/APPS2/loomcycle/{data,config,work,dynamic}
chown -R 65532:65532 /mnt/APPS2/loomcycle
```

- `data` → loomcycle's own state (SQL-Memory cache, snapshots)
- `config` → the thin overlay (`loomcycle.yaml`) + the secrets env file (Phase 4)
- `work` → an agent filesystem Volume (RFC AH); add more datasets + mounts as needed
- `dynamic` → the dynamic root: volumes the runtime makes — each run's `inbox`
  (files an MCP server, HTTP or WebFetch returns are kept there, not handed to
  the model as base64), ephemeral and VolumeDef volumes. Its host directory must
  be owned by uid 65532 too: the runtime creates `_ephemeral/r_<run>` under it at
  first use, and logs an error at boot when it cannot write there

## Phase 3 — The config overlay

The presets supply the provider matrix; this overlay supplies the agent
Volumes, the metrics sampler and the local GPU offload. Create
`/mnt/APPS2/loomcycle/config/loomcycle.yaml` (copy
[`loomcycle.overlay.example.yaml`](loomcycle.overlay.example.yaml)):

```yaml
volumes:
  work:
    path: /mnt/work       # the IN-CONTAINER mount point (Phase 5 maps the dataset here)
    mode: rw
    default: true
  dynamic:
    path: /mnt/dynamic    # runtime-made volumes, incl. each run's inbox
    mode: rw
    dynamic_root: true

metrics:                  # process metrics sampler (the Web UI's activity page)
  enabled: true
  collect_system: true

providers:
  ollama-local:
    options:
      num_gpu: 99         # force GPU offload; leave out to let Ollama decide

conversion:               # file conversion in the worker containers (Phase 5)
  mode: remote
  listen: 0.0.0.0:7788    # reachable only on the internal workers network
  insecure: true          # plain gRPC, acceptable because that network is internal
```

## Phase 4 — Secrets & the secure env file

Keep **every** secret out of the pasted compose. First generate the two runtime
tokens:

```sh
openssl rand -hex 32   # → LOOMCYCLE_AUTH_TOKEN (Web UI + API bearer)
openssl rand -hex 32   # → LOOMCYCLE_WORKER_TOKEN (the conversion workers present it)
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
LOOMCYCLE_WORKER_TOKEN=PASTE_WORKER_TOKEN_FROM_ABOVE
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

The conversion workers get a second file holding **only** the worker token, so
they never see the Postgres DSNs or your provider keys:

```sh
cat > /mnt/APPS2/loomcycle/config/loomcycle.worker.env <<'EOF'
LOOMCYCLE_WORKER_TOKEN=PASTE_WORKER_TOKEN_FROM_ABOVE
EOF
chmod 600 /mnt/APPS2/loomcycle/config/loomcycle.worker.env
```

The same value goes in both files: the runtime refuses a worker whose token
differs, and refuses to start the endpoint without one of at least 32 characters.

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
    image: denngubsky/loomcycle:1.153.0
    user: "65532:65532"
    restart: "no"
    command: ["migrate", "up"]
    env_file:
      - /mnt/APPS2/loomcycle/config/loomcycle.secrets.env   # PG DSNs come from here
  loomcycle:
    image: denngubsky/loomcycle:1.153.0
    user: "65532:65532"
    restart: unless-stopped
    depends_on:
      loomcycle-migrate:
        condition: service_completed_successfully
    ports:
      - "8787:8787"                 # never publish 7788 (the worker endpoint)
    networks: [default, workers]    # default: Postgres, Ollama, your proxy; workers: below
    env_file:
      - /mnt/APPS2/loomcycle/config/loomcycle.secrets.env   # tokens, DSNs, provider keys
    environment:
      # --- non-secret ("insecure") operational config — safe to keep inline ---
      LOOMCYCLE_LISTEN_ADDR: "0.0.0.0:8787"
      LOOMCYCLE_PUBLIC_URL: "http://TRUENAS_SCALE_HOST:8787"   # externally-reachable base URL (your tunnel/proxy host, or this for direct LAN); agents read it via `Context op=self` (≥ v1.6.7)
      LOOMCYCLE_PRESETS: "base,document-agent,chat,agent-teams,team-examples"   # matrix + built-in agents
      LOOMCYCLE_CONFIG_DIR: /config
      LOOMCYCLE_PG_AUTOMIGRATE: "0"              # the init service handles migrations
      LOOMCYCLE_DATA_DIR: /data

      # The metrics sampler is switched on in the config overlay (`metrics:`).
      LOOMCYCLE_METRICS_RETENTION_DAYS: "7"
      LOOMCYCLE_AUDIT_LOG_PATH: /data/audit.log   # a FILE, not the /data dir
      LOOMCYCLE_PG_MAX_OPEN_CONNS: "48"
      LOOMCYCLE_PG_MIN_IDLE_CONNS: "8"
      OLLAMA_BASE_URL: http://OLLAMA_HOST:11434
      LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX: "65535"     # pin the local model's context window
      # GPU offload (num_gpu) is in the config overlay: providers.ollama-local.options.

      # --- HTTP / WebFetch host policy ---
      # "*" allows any host — an SSRF surface, acceptable for a TRUSTED single-tenant
      # box behind a private network/tunnel (this deploy's posture). For multi-tenant,
      # NARROW it to a suffix list (e.g. com,org,net,io,ai,dev — allows whole TLDs).
      # Private IPs stay hard-blocked; a run can further narrow per-call.
      LOOMCYCLE_HTTP_HOST_ALLOWLIST: "*"

      # --- access control (see "Access control" below) ---
      # shadow: the scope gates still decide, and every place your rules would
      # answer differently is logged + audited (acl_shadow_diverged). Switch to
      # enforce once that is quiet. Read at startup: restart after a change.
      LOOMCYCLE_ACL: "shadow"
      # LOOMCYCLE_ACL_CACHE_TTL_SECONDS: "5"   # how long compiled rules are trusted (0 = every request)

      # --- files tools return (the run inbox, under the `dynamic` volume) ---
      # Defaults shown; uncomment to change.
      # LOOMCYCLE_FILE_MAX_BYTES: "67108864"          # one kept file (64 MiB)
      # LOOMCYCLE_FILE_INBOX_MAX_BYTES: "1073741824"  # a run tree's whole inbox (1 GiB)
      # LOOMCYCLE_MCP_MAX_FRAME_BYTES: "16777216"     # one MCP response (stdio line / HTTP body)
    volumes:
      - /mnt/APPS2/loomcycle/data:/data
      - /mnt/APPS2/loomcycle/config:/config:ro
      - /mnt/APPS2/loomcycle/work:/mnt/work
      - /mnt/APPS2/loomcycle/dynamic:/mnt/dynamic
    healthcheck:
      test: ["CMD", "/usr/local/bin/loomcycle", "health"]
      interval: 30s
      timeout: 5s
      retries: 5
      start_period: 20s
  conversion-worker:                # file conversion, out of the runtime (see "Conversion workers")
    image: denngubsky/loomcycle-worker:1.153.0   # same release as the runtime
    restart: unless-stopped
    depends_on:
      loomcycle:
        condition: service_started
    env_file:
      - /mnt/APPS2/loomcycle/config/loomcycle.worker.env    # ONLY LOOMCYCLE_WORKER_TOKEN
    environment:
      LOOMCYCLE_WORKER_DISPATCHER: loomcycle:7788
      LOOMCYCLE_WORKER_INSECURE: "1"
    deploy:
      replicas: 2                   # refused by your TrueNAS? copy the service as -1 and -2
    mem_limit: 1g
    read_only: true
    tmpfs:
      - /scratch:size=1g,mode=0700,uid=65532,gid=65532
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    networks: [workers]
networks:
  workers:
    internal: true                  # workers reach the runtime's 7788 and nothing else
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
4. **Mint a tenant token** (no shell): the left menu → **Access → Tokens** →
   Generate (name, tenant, `substrate:tenant`) → copy the once-shown secret.
5. **Conversion workers**: both are attached.
   ```sh
   curl -s -H "Authorization: Bearer <LOOMCYCLE_AUTH_TOKEN>" http://<truenas>:8787/metrics \
     | grep -E 'conversion_(workers|worker_respawns|jobs_total)'
   ```
   Expect `loomcycle_conversion_workers 2` and
   `loomcycle_conversion_worker_respawns_total 0` (in remote mode the runtime starts
   no workers of its own). After an agent reads a file,
   `loomcycle_conversion_jobs_total{kind="extract",outcome="ok"}` counts it.

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
`loomcycle-migrate` step re-runs `migrate up` on redeploy. Bump all three tags
together: `loomcycle-migrate`, `loomcycle` and `conversion-worker`.

An install made before the conversion workers (a compose with no `conversion-worker`
service) keeps converting inside the runtime. To move it onto workers, add the
worker token and `loomcycle.worker.env` (Phase 4), the `conversion:` block (Phase 3),
and the runtime's `networks:` line, the `conversion-worker` service and the `workers`
network (Phase 5), then redeploy and check Phase 6 step 5.

Operator tokens minted with no pepper (by a release before v1.119.0, or while
`LOOMCYCLE_SECRET_KEY` was unset) are refused until rewrapped, and the boot log names
them. In the TrueNAS shell, with the container from `docker ps`:

```sh
# 1. list the rows (writes nothing)
docker exec <loomcycle-container> /usr/local/bin/loomcycle tokens rewrap
# 2. rewrap the ones you minted (or: --all-listed <digest> from step 1)
docker exec <loomcycle-container> /usr/local/bin/loomcycle tokens rewrap --def-id <def_id>@<fingerprint> --def-id <def_id>@<fingerprint>
```

The container already carries the server's env and config, so the command sees the
same database and pepper. It is safe while the server runs. A row you never minted
was written into the database: retire it instead.

**Existing installs, upgrading to the release that moved settings to the yaml:**
the compose no longer passes `LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU`,
`LOOMCYCLE_METRICS_ENABLED` or `LOOMCYCLE_METRICS_COLLECT_SYSTEM`; the runtime
no longer reads them. Both settings are in the config overlay now, and an
overlay written for an older release has neither. Add this to
`/mnt/APPS2/loomcycle/config/loomcycle.yaml`, next to your `volumes:` block,
**before** you start the new image:

```yaml
metrics:
  enabled: true
  collect_system: true

providers:
  ollama-local:
    options:
      num_gpu: 99        # leave the key out to let Ollama decide
```

What happens if you skip it depends on the compose:

- **You refreshed the compose and kept your overlay.** The app starts. The
  metrics sampler is off (the Web UI's activity page stays empty) and local models
  run on whatever Ollama picks, often the CPU. Nothing is logged, because the
  variables are gone.
- **You kept the old compose.** The app does not start. The boot log names
  `LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU`, `LOOMCYCLE_METRICS_ENABLED` and
  `LOOMCYCLE_METRICS_COLLECT_SYSTEM` and the yaml key each one needs. Add the block above,
  then take the three lines out of the compose (or refresh it from
  [`docker-compose.yaml`](docker-compose.yaml)).

`LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX` stays in the compose. The full list of moved
settings is in [`docs/SETTINGS_MIGRATION.md`](../../docs/SETTINGS_MIGRATION.md).

**Upgrading to v1.122.0 or newer:** migration 0109 adds the access-control tables
(the `loomcycle-migrate` step applies it). The overlay needs the `dynamic` volume
and its dataset (Phase 2, Phase 3, Phase 5) or files tools return are refused; add
the dataset and its mount **before** the `dynamic:` overlay entry, since a volume
whose path is missing stops the boot. The volume name `inbox` is reserved. Access
control changes nothing until you set `LOOMCYCLE_ACL` or write a rule — see *Access control* below.

**The store-backed audit trail:** with any sign-in gate configured, strict mode, or
`LOOMCYCLE_ACL` set to `shadow` or `enforce`, the server records its audit trail in the
database (migration 0131) and needs `LOOMCYCLE_SECRET_KEY` in the secure env file
(Phase 4); without it the config is refused and the app does not start. On stop it
drains its audit queue after pausing runs: give the container a 30 s stop grace
(`stop_grace_period: 30s`) rather than Docker's default 10 s. Check the trail with
`docker exec <loomcycle-container> /usr/local/bin/loomcycle audit verify --all`.

**Upgrading to the release with RFC GD P3 (more than one principal):** the audit
trail is also required once more than one principal exists — the
`LOOMCYCLE_AUTH_TOKEN` plus a tenant token minted in the Web UI's Settings hub, a
`principals:` entry, or a second minted token. Such an install needs
`LOOMCYCLE_SECRET_KEY` (the catalog app's *Secret key* field, or the secure env
file of Phase 4) **before** you upgrade, or the app stops booting with an error naming
it. Generate it with `openssl rand -base64 32` and keep it. Token mints, user
approvals and access-control changes are then refused (`503 audit_unavailable`)
whenever the database cannot take their audit row; token retirements and user
disables still go ahead (watch `loomcycle_audit_unrecorded_total`).

## Access control — rule lists, groups and bindings

From v1.122.0 a tenant operator can say, per user or group, which agents, tools,
volumes, memory scopes and definitions a principal may use, with an explicit
**deny** that beats any allow. Token scopes stay the ceiling: a rule can narrow
what a token reaches, never widen it.

**1. Pick the mode** (`LOOMCYCLE_ACL`, set in Phase 5; restart after a change):

| Mode | Who decides | Use it |
|---|---|---|
| `compat` (default) | the scope gates, plus any deny you bind | nothing changes until you write a rule |
| `shadow` | the scope gates; every disagreement with the rules is logged and audited as `acl_shadow_diverged` | before `enforce`, for a few days of normal use |
| `enforce` | the rules: a question no rule allows is refused `403 acl_denied`; rules that cannot be read refuse with `503 acl_unavailable` | once `shadow` is quiet |

Every token class has a built-in list (`builtin:admin`, `builtin:tenant-operator`,
`builtin:member`, `builtin:isolated`, `builtin:run-plane`) that reproduces its reach,
so `enforce` with no rules written behaves like `compat`. The one difference: under
`enforce` an isolated user (`substrate:user`) may not list its tenant's users.

**2. Write rules** — either in the Web UI (the left menu's **Access** group: the
Groups, Policies and Bindings pages, and a Simulator that answers "may this user do this?"
with every rule it considered, against the saved rules or a draft), or declared in
the overlay `loomcycle.yaml` next to `volumes:`. Config rows are
read-only in the UI; a stored list with the same name shadows a config one.

```yaml
acls:
  members-no-shell:
    description: members may run agents but not shell commands or git pushes
    rules:
      - { effect: deny, actions: [invoke], resources: ["tool:Workspace"] }
      - { effect: deny, actions: [push],   resources: ["git:*"] }
      - { effect: deny, actions: [write],  resources: ["volume:work"] }   # work becomes read-only for them
groups:
  family:
    tenant: loomcycle-dev            # the tenant your tokens were minted in
    members: [alice, bob]            # token subjects
acl_bindings:
  - { acl: members-no-shell, group: family, tenant: loomcycle-dev }
  # also: user: <subject>, token: <token name>, or tenant_wide: true
```

A rule is `{effect, actions, resources, when}`. Resources look like `agent:chat/*`,
`tool:Workspace`, `volume:work`, `memory:tenant`, `document:tenant:/kb/**`,
`def:agent/*`, `team:*`; `when` can limit a rule by `transport`, `time`,
`interactive`, `tier`, `source_cidr` and a few more. The Policies tab's rule editor
offers every type and verb (the same catalog `POST /v1/_acldef` `op=catalog` returns). A malformed entry fails
`loomcycle validate` and the boot, naming the entry.

**3. What the rules reach.** Inside a run, a tool the caller may not `invoke` is
withheld from the model (reported once as a `capability_inert` event), a volume it
may not `write` is mounted read-only, and memory or history scopes it may not
`read` are dropped; sub-agents inherit the caller's rules. A paused, restored or
restarted run is narrowed again by the caller's **current** rules. A schedule or
webhook run is narrowed by its definition's author, an A2A run by the calling
peer's credential, and a team walk by whoever started it or the trigger's author.

**Upgrading from v1.121 or older:**

- Schedules and webhooks created before the upgrade record no author, so they fire
  unnarrowed until you re-save them (create again, fork, or edit their hook).
- A token limited to certain agents can no longer start team walks
  (`builtin:run-plane` denies `run team:*`, in `compat` too).
- Erasing a user (the Erasure page) now also revokes their member tokens and
  removes their group memberships and bindings. Config-declared memberships stay —
  remove them from the overlay.
- Snapshots don't carry lists, groups or bindings. After restoring on another
  instance, recreate them, or keep them in the overlay.

## Browser + workspace variant

The walkthrough above runs no commands for agents. To let an agent **build a
feature in a workspace, serve it, and look at it** — all in one run — paste
[`docker-compose.browser.yaml`](docker-compose.browser.yaml) in Phase 5 instead of the
inline compose. It runs the distroless **`loomcycle-browser`** image and adds:

- a **workspace worker** (`denngubsky/loomcycle-ws-worker`) on the **container
  engine**: each workspace's commands run in a container of their own (the
  `denngubsky/loomcycle-workspace-browser` session image: Python, Node, git and
  Chromium), with a read-only root, no capabilities and no network but the egress
  routes the runtime grants. The worker drives the host's Docker through its socket,
  so it is the one privileged component; the sessions hold no such right.
  Background: [`../../docs/WORKERS.md`](../../docs/WORKERS.md) "Workspace workers";
- a **`pinchtab`** headless browser for `mcp__browser__*` (browsing the web).

The `dev-workspace` preset supplies `dev/exec`, which runs a command envelope in a
workspace. An agent whose `workspace` grant holds `exec` and `connect` sees the page
its own server serves with `Workspace op=screenshot`; the browser runs inside the
workspace's session (`docs/WORKSPACES.md`, "Looking at a picture").

The deltas from the base install:

1. **Datasets (Phase 2)** — add `pinchtab-data`, owned by **uid 1000** (PinchTab is not
   the 65532 distroless user, and crash-loops on a root-owned `/data`), and the
   worker's two directories, owned by 65532, mode 0700:
   ```sh
   mkdir -p /mnt/APPS2/loomcycle/pinchtab-data /mnt/APPS2/loomcycle/workspaces /mnt/APPS2/loomcycle/wse
   chown -R 1000:1000 /mnt/APPS2/loomcycle/pinchtab-data
   chown 65532:65532 /mnt/APPS2/loomcycle/workspaces /mnt/APPS2/loomcycle/wse
   chmod 0700 /mnt/APPS2/loomcycle/workspaces /mnt/APPS2/loomcycle/wse
   ```
   The worker mounts both at their host paths, so the host's Docker finds what the
   worker names.
2. **Overlay (Phase 3)** — copy
   [`loomcycle.overlay.browser.example.yaml`](loomcycle.overlay.browser.example.yaml)
   (not the plain one) to `config/loomcycle.yaml`; it declares the `mcp_servers.browser`
   stdio bridge, the worker endpoint and `workspace: {mode: remote}`.
3. **Two more secret files (Phase 4)** — PinchTab must **not** see the PG DSN or
   provider keys, so its token lives in its own file:
   ```sh
   cat > /mnt/APPS2/loomcycle/config/browser.secrets.env <<'EOF'
   PINCHTAB_TOKEN=CHANGE_ME       # openssl rand -hex 32
   EOF
   chmod 600 /mnt/APPS2/loomcycle/config/browser.secrets.env
   ```
   loomcycle reads both `loomcycle.secrets.env` and `browser.secrets.env`; pinchtab
   reads only `browser.secrets.env`. The workspace worker's token is a minted
   operator token with the single scope `workers:attach`, so it can only be made once
   the app runs. Create the file empty now, then after the first start mint the token
   and restart the worker:
   ```sh
   install -m 600 /dev/null /mnt/APPS2/loomcycle/config/workspace.worker.env
   # after Phase 6, with the admin token in LOOMCYCLE_AUTH_TOKEN:
   loomcycle operator-token create --target http://TRUENAS_SCALE_HOST:8787 \
     --name ws1 --tenant <your tenant> --scopes workers:attach --worker-shared
   echo 'LOOMCYCLE_WORKER_TOKEN=<the token it printed>' > /mnt/APPS2/loomcycle/config/workspace.worker.env
   docker restart <workspace-worker-container>
   ```
   `--worker-shared` lets the one worker serve every tenant, which the container
   engine allows; drop it to serve `--tenant`'s runs only. The conversion workers'
   `LOOMCYCLE_WORKER_TOKEN` is a different value that never attaches a workspace
   worker, so keep the two files apart.
4. **Host Docker socket** — the worker bind-mounts `/var/run/docker.sock` (a
   privileged grant; keep the box trusted) and joins the socket's group: replace
   `DOCKER_GID` in the compose with `stat -c %g /var/run/docker.sock`. The session
   image is pulled by the **host** Docker on the first command (public — no login
   needed).
5. **PinchTab allowlist** — PinchTab browses **local-only** until you widen its IDPI
   allowlist; add the sites agents may browse, then restart it:
   ```sh
   docker exec <pinchtab-container> \
     pinchtab config set security.allowedDomains "127.0.0.1,localhost,::1,example.com"
   docker restart <pinchtab-container>
   ```
   The value persists in `pinchtab-data` (it survives reboots).

Verify end-to-end: `curl -s -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN"
http://TRUENAS_SCALE_HOST:8787/metrics | grep loomcycle_workspace_workers` shows 1,
then spawn `dev/exec` with `{"commands":["python3 --version"]}` — the reply carries
the command's output.

## Conversion workers

Every file conversion runs in the `conversion-worker` containers (RFC GO,
[docs/WORKERS.md](../../docs/WORKERS.md)): text extraction, OCR page rendering
(`pdftoppm`), voice-note conversion and spoken-reply encoding (`ffmpeg`), and image
re-encoding. One hostile or oversized file then exhausts a worker's 1 GiB, not the
app's memory: that worker dies and restarts, the runtime answers that one call
with an error and refuses the file for an hour, and every other run goes on. The
runtime's image needs no ffmpeg or poppler, so any of the runtime images works.

- **Isolation.** The workers are on the internal `workers` network with the runtime
  only: no internet, no access to the runtime's API, no dataset mounted. A file
  reaches a worker only as bytes over the worker endpoint (port 7788), which is
  never published.
- **Token.** `LOOMCYCLE_WORKER_TOKEN` in both env files (Phase 4), the same value.
- **Releases.** Keep the worker image on the runtime's release; bump both together
  in Phase 8.
- **More or bigger workers.** Raise `replicas`, or `mem_limit` for very large
  Office files. `LOOMCYCLE_WORKER_SLOTS` (default 1) runs more jobs per worker,
  but a crash then fails all of them.
- **Without workers.** Delete the overlay's `conversion:` block, the
  `conversion-worker` service, the `workers` network and the runtime's `networks:`
  line. Conversion then runs inside the runtime, and voice-note conversion and OCR
  page rendering need `ffmpeg` and `pdftoppm` in the runtime's own image.
- **Workers on another host.** Use TLS instead of the internal network; see
  docs/WORKERS.md (`conversion.tls`, `LOOMCYCLE_WORKER_TLS_CA`).

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `loomcycle_conversion_workers 0`, and the worker logs repeat a failed or refused attach | The token differs between `loomcycle.secrets.env` and `loomcycle.worker.env`, the overlay lacks `conversion: mode: remote`, or the runtime is not on the `workers` network. Fix it and redeploy the app. |
| The runtime crash-loops naming `conversion.listen` | The endpoint is set to the app's own port (8787), or off the loopback without `insecure: true` or `tls:`. Use `0.0.0.0:7788` with `insecure: true` on the internal network. |
| A file is refused because it crashed the converter | Converting it killed a worker (usually memory), so it is refused for an hour rather than take the next worker down. Raise the workers' `mem_limit` if the file is legitimate. |
| `no config found` at boot | Image predates RFC AQ — pin `denngubsky/loomcycle:1.6.0` or newer. |
| Crash-loop ending `auth: refusing to start in open mode on LOOMCYCLE_LISTEN_ADDR=0.0.0.0:8787` | No `LOOMCYCLE_AUTH_TOKEN` reached the container, so the server would serve every route unauthenticated on the LAN, and it refuses to. The `env_file` didn't load or lacks the token (Phase 4) — fix that; don't set `LOOMCYCLE_ALLOW_OPEN_MODE`. |
| (browser) pinchtab crash-loops `mkdir: can't create directory '/data/.config': Permission denied` | `pinchtab-data` dataset not owned by 1000 — `chown -R 1000:1000 /mnt/APPS2/loomcycle/pinchtab-data`. |
| (browser) `dev/exec` expose → `network loom-dev not found` | The sidecar image predates 1.43.0 (no expose seam), or nothing created `loom-dev` — use `docker-compose.browser.yaml` (declares the network + a multi-arch sidecar). |
| (browser) browser step → `navigation blocked by IDPI: Domain not in allowlist` | Add the alias to PinchTab's allowlist (step 5 above) and restart the sidecar. |
| (browser) browser step → `could not resolve navigation host` | The session isn't on `loom-dev` (missing `reachable at …` line) — the sidecar isn't honoring expose (old image) or pinchtab isn't on `loom-dev`. |
| `EACCES` / permission denied on `/data` | Datasets not owned by 65532 — `chown -R 65532:65532 /mnt/APPS2/loomcycle`. |
| migrate `connection refused` | DSN host/port unreachable from the apps network, or the role/DB doesn't exist (Phase 1). |
| App healthy but auth fails / no providers wired | The `env_file` didn't load — confirm the **absolute path** in `env_file:` is exact and the file is `chmod 600` (root-readable). Remember an `environment:` key overrides the same key in `env_file`, so don't re-add a secret inline. |
| migrate `LOOMCYCLE_PG_DSN ... required` | The `env_file:` line is missing from the `loomcycle-migrate` service (it needs the DSNs too), or the path is wrong. |
| Crash-loop ending `sqlmem audit: ... open /data: is a directory` | `LOOMCYCLE_AUDIT_LOG_PATH` must be a **file**, not the `/data` mount — set `/data/audit.log` (or remove the line; the audit log is optional). |
| Documents fail with `sqlmem: provision scope: ... permission denied to create role` | The SQL-Memory DSN's role lacks `CREATEROLE` (it provisions a per-scope login role). Grant it: `ALTER ROLE loomcycle CREATEROLE;` (Phase 1 now creates the role with it). |
| Documents fail with `permission denied for database "loomcycle_sqlmem"` (SQLSTATE 42501) | `PUBLIC` lacks `CONNECT` on the SQL-Memory database (a shared Postgres revoked it); the per-scope roles connect through it. Run `GRANT CONNECT ON DATABASE loomcycle_sqlmem TO PUBLIC;` |
| `doc/manager` idle | Needs the `loomcycle_sqlmem` DSN (`LOOMCYCLE_SQLMEM_PG_DSN`, which turns SQL Memory on) + a `middle` tier (base provides one). |
| An agent has no file access | Its bound volume isn't mapped — check the overlay `volumes:` `path` equals the in-container mount path (Phase 3 ↔ Phase 5). |
| Crash-loop ending `config: load failed err="volumes.dynamic: path \"/mnt/dynamic\" must already exist …"` | The overlay declares the `dynamic` volume but the app has no mount at `/mnt/dynamic` (an install made from a compose before v1.122.0). Create the dataset owned by 65532 (Phase 2) and add the mount (Phase 5: `…/dynamic:/mnt/dynamic`, or *Additional storage* in the wizard). To boot first, comment out the `dynamic:` block. |
| File downloads / kept tool results fail with `file not kept: mkdir "/mnt/dynamic/_ephemeral/r_…": permission denied` (the boot log also says the dynamic_root volume is not writable) | The `dynamic` dataset is not owned by the runtime uid. `chown -R 65532:65532 /mnt/APPS2/loomcycle/dynamic`. |
| A tool's file shows as `[file not kept: no dynamic_root volume is configured …]` | The overlay has no `dynamic_root: true` volume, or its mount is missing — add the `dynamic` entry (Phase 3) and its mount (Phase 5). |
| `403 {"code":"acl_denied", …}` on a request that worked before | A rule denies it, or (under `enforce`) no rule allows it. The body names the rules that denied; **Access → Simulator** shows every rule considered. Back to `shadow` while you fix the list. |
| `503 acl_unavailable` | The caller's rules could not be read (a database fault). Under `enforce` the request is refused rather than admitted; a paused run stays paused and a schedule tries again next tick. Check the Postgres connection. |
| Log lines `authz: acl_shadow_diverged …` | `shadow` mode found a request the rules would answer differently from the scope gates. Each line names the action and resource: adjust the list, or accept the change before `enforce`. |
| `toomanyrequests: You have reached your pull rate limit` | Docker Hub anonymous quota exhausted — see below. |

### Docker Hub rate limits

Docker Hub meters **anonymous** pulls per source IP, and over IPv6 the bucket is
keyed on the whole **/64 prefix** — so your TrueNAS box, your laptop and everything
else behind one connection drain a single shared quota. TrueNAS also re-checks every
installed app for image updates on a timer, and each check spends from it. Check
what's left, from the box:

```sh
TOKEN=$(curl -s "https://auth.docker.io/token?service=registry.docker.io&scope=repository:ratelimitpreview/test:pull" | jq -r .token)
curl -sI -H "Authorization: Bearer $TOKEN" \
  https://registry-1.docker.io/v2/ratelimitpreview/test/manifests/latest | grep -i ratelimit
```

Three fixes, in order of how well they hold:

1. **Pull from GHCR instead.** Every loomcycle image is mirrored to
   `ghcr.io/denn-gubsky/…` from the same build, so swapping the registry prefix
   changes nothing but where the bytes come from. Not metered like Docker Hub.
   Third-party images (`pinchtab/pinchtab`) still come from Docker Hub.
2. **Authenticate.** `sudo -i` then `docker login -u <user>` on the TrueNAS host, with
   a read-only access token — *not* `sudo docker login`, which can write the
   credential to the calling user's `~/.docker/config.json` while the app middleware
   reads root's. Authenticated pulls are metered per account, not per source IP.
   Verify: `cat /root/.docker/config.json` shows an `auths` entry.
3. **Pull less.** Add `pull_policy: missing` to each service so a restart reuses the
   image already on disk instead of re-fetching its manifest. Pinning by digest
   (`image: denngubsky/loomcycle@sha256:…`) also halves the requests a
   multi-arch tag costs, since it skips the manifest-list lookup.
