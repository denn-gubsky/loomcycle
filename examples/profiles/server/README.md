# Server profile

loomcycle as a shared installation: Postgres, traces to an OpenTelemetry
collector, sign-in gates, cloud and local models, and one tenant per team or
customer.

| File | What it holds |
|---|---|
| `loomcycle.yaml` | `presets: [server]`, the dynamic volume and the sign-in gates. Goes in `config/`. |
| `env.insecure.example` | Settings that are not secrets: the addresses, the collector. |
| `env.local.example` | Secret names with empty values. Five are required. |
| `docker-compose.yaml` | The runtime, a one-shot schema migration, and Postgres. |
| `postgres-init.sql` | Creates the second database, once. |

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

Fill in `env.local`: the token, the secret key, the Postgres password and the
two database URLs that carry it, and at least one provider key. In
`env.insecure`, set `LOOMCYCLE_PUBLIC_URL` to the address people will use, and
point `OTEL_EXPORTER_OTLP_ENDPOINT` at your collector or delete the two
`OTEL_` lines.

```sh
LOOMCYCLE_VERSION=<release> docker compose --env-file env.local up -d
docker compose --env-file env.local exec loomcycle /usr/local/bin/loomcycle doctor
```

`<release>` is the image tag to run. Compose stops with the name of any
required secret that is still empty. The runtime is published on
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

**Tools that reach outside.** Web access, the unsandboxed shell and the like
are commented lines at the end of `env.insecure`, each with what it loosens.
No preset turns one on.

## Traces and metrics

`OTEL_EXPORTER_OTLP_ENDPOINT` takes the standard OpenTelemetry variables; a
collector that needs credentials gets them from `OTEL_EXPORTER_OTLP_HEADERS`
in `env.local`. [`examples/observability`](../../observability/) has a
collector with Grafana and Tempo, and wiring for two hosted backends. The
preset's metrics sampler feeds the Web UI's metrics page; `/metrics` serves
Prometheus.

## Upgrading

Change `LOOMCYCLE_VERSION` and run `up -d` again. The `loomcycle-migrate`
service applies the new schema before the runtime starts.

## An external Postgres

Remove the `postgres` service and its `depends_on`, and point both URLs at
your server. It needs two databases, and the role in
`LOOMCYCLE_SQLMEM_PG_DSN` must sign in with a password and be allowed to
create roles (`ALTER ROLE … CREATEROLE`): SQL Memory makes one login role per
scope. [`docs/POSTGRES.md`](../../../docs/POSTGRES.md) has the details.
