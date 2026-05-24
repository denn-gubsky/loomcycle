---
name: getting-started
description: "First-run walkthrough — `loomcycle init` → set env vars → `loomcycle doctor` → run the server."
---
The shortest path from a fresh `loomcycle` install to a working server
is three commands:

```sh
loomcycle init       # writes ~/.config/loomcycle/loomcycle.yaml + README.md
# then set required env vars in your shell rc (~/.zshrc / ~/.bashrc)
loomcycle doctor     # verifies config + provider keys + storage + listen address
loomcycle            # starts the server on 127.0.0.1:8787
```

## init

`loomcycle init` is non-destructive. By default it writes to
`~/.config/loomcycle/` (override with `--path /custom/dir`). If files
already exist there, it refuses unless you pass `--force`. The init
command writes only the yaml + the operator reference doc — never any
secrets.

When stdin is a TTY, init auto-enters a minimal 3-question wizard:

1. **Which provider's API key do you have?** (`anthropic` / `openai`
   / `deepseek` / `skip`)
2. **Env var to read the key from?** (defaults per provider —
   `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / `DEEPSEEK_API_KEY`)
3. **HTTP listen address?** (default `127.0.0.1:8787`)

Every other configuration knob stays as the commented sections in the
generated `loomcycle.yaml` — the operator edits them later if needed.
The wizard's final stdout block prints the env-var lines you need to
paste into your shell rc; never writes them to disk.

To skip the wizard in CI / Docker / scripted setups, pass
`--no-interactive`.

## Required environment

After `init`, set at least:

| Env var | Why |
|---|---|
| `LOOMCYCLE_AUTH_TOKEN` | Bearer token enforced on every `/v1/*` endpoint. Generate with `openssl rand -hex 32`. |
| `<PROVIDER>_API_KEY` | One per provider you'll route to. `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / `DEEPSEEK_API_KEY` / etc. |

Optional but commonly set:

- `LOOMCYCLE_DATA_DIR` — where the sqlite store lives (defaults to
  `~/.local/share/loomcycle`).
- `LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT` — enables distributed
  traces (see `help observability`).

See the full table in `~/.config/loomcycle/README.md` (the per-machine quickstart). For provider routing + tier conceptual deep-dive, see the repo's `docs/CONFIGURATION.md`.

## doctor

`loomcycle doctor` runs six checks in order and exits 0 only when no
check FAILed:

1. Config discoverable (via the same auto-discovery the server uses)
2. Config parses cleanly
3. `LOOMCYCLE_AUTH_TOKEN` set (WARN if absent — the server boots
   but every `/v1/*` request is allowed unauthenticated)
4. Per-configured provider: API-key env var set + `Provider.Probe()`
   succeeds (5s timeout, concurrent across providers)
5. Storage backend reachable (sqlite path writable; postgres `Open()`
   succeeds)
6. HTTP listen address bindable (test-bind-then-release)

Each check prints `[PASS]` / `[WARN]` / `[FAIL]` with a one-line
detail. Output is operator-friendly first; the exit code drives CI /
init scripts.

## Auto-discovery

When you run `loomcycle` with no `--config`, the binary walks:

1. `./loomcycle.yaml` (current directory)
2. `$XDG_CONFIG_HOME/loomcycle/loomcycle.yaml`
3. `~/.config/loomcycle/loomcycle.yaml`

and uses the first existing file. If none exist, it prints a
single-line hint pointing at `loomcycle init` and exits with code 1.

Passing `--config /any/path.yaml` bypasses auto-discovery — explicit
paths win.

## Related topics

- `llm-gateway` — direct LLM routing endpoint (v0.11.0).
- `fairness` — per-user concurrency quota policy.
- `observability` — OTEL setup once you're past first-run.
- `sqlite-vec` — opt into Vector Memory on SQLite via build tag.
