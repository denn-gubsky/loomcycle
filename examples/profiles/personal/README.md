# Personal profile

loomcycle for one person on one machine: local models, cloud models or both,
SQLite, the chat agents and the document agent. Three files, about ten lines
you have to read.

| File | What it holds |
|---|---|
| `env.insecure.example` | Settings that are not secrets. One line selects the `personal` preset. |
| `env.local.example` | Secret names with empty values. One provider key, or none. |
| `loomcycle.yaml` | Your own settings over the preset. Everything in it is commented out. |

## Start in five minutes

```sh
cp env.insecure.example env.insecure
cp env.local.example env.local && chmod 600 env.local
```

Give it a model, one of:

- **A cloud key.** Put it in `env.local` (`ANTHROPIC_API_KEY=…`, or the OpenAI,
  DeepSeek or Gemini line).
- **Local models.** Run [Ollama](https://ollama.com) and pull a model the
  preset's patterns match (`loomcycle presets show local` lists them). Leave
  `OLLAMA_BASE_URL` as it is.

With both, local models are tried first and the cloud is the fallback.

```sh
set -a; . ./env.insecure; . ./env.local; set +a
loomcycle doctor --config ./loomcycle.yaml     # which providers are usable
loomcycle --config ./loomcycle.yaml
```

Open <http://127.0.0.1:8787/ui> and start a chat with `chat/medium`.

`loomcycle init` writes this same `loomcycle.yaml` to `~/.config/loomcycle/`,
where `loomcycle` finds it without `--config`.

## When something has no model

- **No key and no Ollama.** The server starts. A run fails with
  `no provider available for requested tier`, and the boot log names each
  provider and why it was left out.
- **A cloud key and no Ollama.** The agents that route by tier use the cloud.
  `chat/local-small` is pinned to a local model and fails naming the model it
  wanted.
- **Ollama with other models than the patterns match.** The tier has no local
  candidate. Point an alias at what you have, in `loomcycle.yaml`:

  ```yaml
  models:
    local-chat: { provider: ollama-local, model: your-model:tag }
  ```

## Giving the agents more

The preset turns on nothing that reaches outside the runtime. Each of these is
a line you add yourself:

- **Files.** Uncomment `volumes:` in `loomcycle.yaml` and name a directory.
- **The web.** `LOOMCYCLE_HTTP_HOST_ALLOWLIST` in `env.insecure`, and
  `BRAVE_API_KEY` in `env.local` for search.
- **A shell.** `Bashbox` (a sandbox confined to the volume) needs only the
  volume. `LOOMCYCLE_BASH_ENABLED=1` adds `Bash`, which is not a sandbox.
- **Durable memory.** `LOOMCYCLE_PRESETS=personal,memory` and the
  `scheduled_runs` block in `loomcycle.yaml`.

## Postgres instead of SQLite

Set `LOOMCYCLE_PG_DSN` in `env.local` (`postgres://user:password@host:5432/loomcycle`);
a DSN is all it takes to select Postgres. Documents and SQL Memory need a
second database and a role that may create roles: set
`LOOMCYCLE_SQLMEM_PG_DSN` to it. Apply the schema once with
`loomcycle migrate up`, or set `LOOMCYCLE_PG_AUTOMIGRATE=1` to have each start
do it. [`docs/POSTGRES.md`](../../../docs/POSTGRES.md) has the details; the
[server profile](../server/) runs this way.
