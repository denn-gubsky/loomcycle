# Server profile

loomcycle as a shared installation: Postgres, traces to an OpenTelemetry
collector, sign-in gates, cloud and local models, and one tenant per team or
customer.

| File | What it holds |
|---|---|
| `loomcycle.yaml` | `presets: [server]`, the dynamic volume and the sign-in gates. Goes in `config/`. |
| `env.insecure.example` | Settings that are not secrets: the addresses, the collector. |
| `env.local.example` | Secret names with empty values, and the release to run. Seven are required. |
| `docker-compose.yaml` | The runtime, a one-shot schema migration, and Postgres. |
| `postgres-init.sh` | Creates the runtime's database role and its two databases, once. |

The `server` preset supplies the rest: the provider matrix, the chat, document
and memory agents, and the metrics sampler. `loomcycle presets show server`
prints it. The yaml selects it with `presets: [server]`; that key is read from
`config/loomcycle.yaml` only, and `LOOMCYCLE_PRESETS`, when set, is used
instead.

## Start

```sh
cp env.insecure.example env.insecure
cp env.local.example env.local && chmod 600 env.local
mkdir -p config data dynamic && cp loomcycle.yaml config/
sudo chown -R 65532:65532 data dynamic        # the runtime's user in the image
```

Fill in `env.local`: the token, the secret key, the two Postgres passwords,
the two database URLs that carry the second one, at least one provider key,
and `LOOMCYCLE_VERSION` (the image tag to run). In `env.insecure`, set
`LOOMCYCLE_PUBLIC_URL` to the address people will use.

```sh
docker compose --env-file env.local up -d
docker compose --env-file env.local exec loomcycle /usr/local/bin/loomcycle doctor
```

Every `docker compose` command takes `--env-file env.local`: compose reads the
file each time, and stops with the name of any required value that is still
empty. The runtime is published on
`127.0.0.1:8787` only: put your reverse proxy or tunnel in front of it.

Sign in at `<LOOMCYCLE_PUBLIC_URL>/ui` by pasting `LOOMCYCLE_AUTH_TOKEN`.

## What you still decide

**Who signs in.** The yaml ships with the token gate alone. For people, pick
one interactive gate and uncomment it in `config/loomcycle.yaml`: `password`
(accounts loomcycle keeps; you send invite links) or `oidc` (your identity
provider). A chat bridge or a website adds one `kind: external` gate; the
bridge is its own service with its own settings, described in
[`packages/bridge-telegram`](../../../packages/bridge-telegram/).

**Tenants.** A tenant is a name carried by a token. Mint a token per person or
service with the operator's token:

```sh
docker compose --env-file env.local exec loomcycle \
  /usr/local/bin/loomcycle operator-token create --name alice --tenant example \
  --subject alice --scopes substrate:tenant,runs:create,runs:read
```

The token is printed once. Its holder sees that tenant's agents, runs,
documents and memory, and no other tenant's. People who sign in through a gate
get their tenant from the gate's `identity_map`. Do not mint a
`substrate:admin` token unless you mean it to replace `LOOMCYCLE_AUTH_TOKEN`:
the first one retires it.

**Models.** Every provider is wired; one with no key is skipped. To add local
models, set `OLLAMA_BASE_URL` in `env.insecure`; they are tried first.

**Retention.** Nothing is deleted with age until `retention:` in the yaml says
`enabled: true` and names a family with its `max_age`. The block is there,
commented out.

**Memory.** The `memory` agents are loaded, their hourly schedule is not
running: it calls a model for every user with new chats. Uncomment
`scheduled_runs` in the yaml to start it.

**Tools that reach outside.** The bundled chat agents list `Bash`, `Bashbox`,
`Write`, `Edit`, `HTTP`, `WebFetch`, `WebSearch` and `Agent` in their tools,
and any user of any tenant may run them. What those tools can do is decided by
the commented lines at the end of `env.insecure`, and each of those is one
switch for the whole server, not per tenant:

- `LOOMCYCLE_HTTP_HOST_ALLOWLIST` is a single list: the hosts every tenant's
  agents may fetch from. Name hosts; `*` lets any user's agent fetch any
  public URL. Private addresses stay blocked either way.
- `LOOMCYCLE_BASH_ENABLED=1` should stay off on a server with more than one
  tenant. It gives every user's chat agent an unsandboxed shell in the shared
  container once the agent has a writable volume. The shell starts in that
  volume and is not confined to it: it reaches other tenants' directories by
  absolute path, and the network. `Bashbox` is the shell for a shared server:
  it needs no switch, is confined to the agent's volume and has no network.
- `LOOMCYCLE_MCP_ALLOW_PRIVILEGED_TOOLS=1` lets an agent that an MCP client
  registers ask for `Bash`, `Bashbox`, `Write` and `Edit`. On a server those
  clients are remote and hold tenant tokens; leave it off.

No preset can turn any of these on: they exist only in the environment.

## Traces and metrics

Traces are off until you uncomment `OTEL_EXPORTER_OTLP_ENDPOINT` in
`env.insecure` and set it to your collector's address (the example value is a
placeholder). The other standard OpenTelemetry variables work too; a
collector that needs credentials gets them from `OTEL_EXPORTER_OTLP_HEADERS`
in `env.local`. [`examples/observability`](../../observability/) has a
collector with Grafana and Tempo, and wiring for two hosted backends. The
preset's metrics sampler feeds the Web UI's metrics page; `/metrics` serves
Prometheus.

## Upgrading

Change `LOOMCYCLE_VERSION` in `env.local` and run `up -d` again. The `loomcycle-migrate`
service applies the new schema before the runtime starts.

## The database role

The runtime does not connect as the Postgres superuser. `postgres-init.sh`
runs once, as the superuser, and creates the role `loomcycle`: not a
superuser, allowed to create roles, owner of the two databases. SQL Memory
needs the create-role right because it makes one login role per scope, each
confined to its own schema. `POSTGRES_PASSWORD` is the superuser's and is used
for that initialisation only; `LOOMCYCLE_DB_PASSWORD` is the runtime role's
and is the one in the two database URLs.

## An external Postgres

Remove the `postgres` service and its `depends_on`, and point both URLs at
your server. Run the SQL in `postgres-init.sh` there as a superuser: it needs
two databases, and the role in `LOOMCYCLE_SQLMEM_PG_DSN` must sign in with a
password and be allowed to create roles (`ALTER ROLE … CREATEROLE`). [`docs/POSTGRES.md`](../../../docs/POSTGRES.md) has the details.
