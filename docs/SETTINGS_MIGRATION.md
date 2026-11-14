# Migrating to the reduced settings

This guide is for operators upgrading an existing loomcycle deployment to the release that reduces its settings. It tells you what to change before you start the new binary, and what the binary does when you change nothing.

## 1. What changed, and why

Before this release loomcycle read about 318 environment variables. Many of them duplicated a yaml key, many were switches that every deployment set to the same value, and many were limits nobody changed. The same setting could be given in two places, and two deployments could differ in ways neither operator could see.

This release gives each setting one place, called a *plane* in this guide:

- **The environment** keeps secrets (keys, tokens, database connection strings), the identity of the host and process (listen addresses, directories, public URLs, the replica id), and security gates (switches that open something dangerous and are off unless you set them).
- **The yaml config** keeps policy and tuning: retention, concurrency, provider options, timeouts, sampling. Policy is the same on every replica, can be reviewed, and can be carried by a config layer or a preset.
- **Nothing** keeps a value that no deployment needed to change. It is now a constant of the runtime.

Why this, and not a compatibility period in which the old variables keep working: a variable that is read "for now" keeps both planes alive, and the deployment that never migrates is the one that later differs silently. The removed variables are deleted with no compatibility read. In exchange, the runtime tells you about every removed variable you still set, and refuses to start when ignoring one would make your deployment looser or move its data.

A deployment that changes nothing gets one of three outcomes when it starts the new binary:

1. **It starts with advisories.** An *advisory* is a warning line that names a variable the runtime no longer reads and says what to do instead. Nothing else changed for that variable.
2. **It refuses to start.** A *refusal* is a start-up error that names each removed variable whose value asked for something this version does not apply: a tighter limit, a feature kept off, another data store, data kept or deleted differently, or a setting that moved to the yaml and is not there. You fix each one and start again.
3. **It starts and behaves differently.** This is the dangerous outcome. It happens where the old behaviour depended on a variable you did *not* set, so there is nothing for the runtime to warn about. [Section 4](#4-behaviour-changes-with-no-variable-to-warn-about) lists every such case. Read it before you upgrade.

## 2. Before you upgrade: a checklist

Do these in order.

### 2.1 Back up

Back up the store (the SQLite data directory, or both Postgres databases if you use SQL Memory on Postgres), the SQL Memory scope files if you use the SQLite tier, your yaml config files and your environment files. Two of the changes in this release decide which store is opened and whether data is deleted with age; a backup is what makes a mistake recoverable.

### 2.2 Collect the names of the variables you set

You need the list of `LOOMCYCLE_*`, `SANDBOX_*` and `OTEL_*` names your deployment sets. Collect **names only**. Never paste values into a ticket, a chat or a commit: several of these variables hold secrets.

From a shell that has the deployment's environment:

```sh
env | cut -d= -f1 | grep -E '^(LOOMCYCLE|SANDBOX|OTEL|OLLAMA)_' | sort
```

From an environment file (prints the names, not the values):

```sh
grep -oE '^[A-Z][A-Z0-9_]*=' /path/to/envfile | tr -d '=' | sort -u
```

From a running container:

```sh
docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' <container> \
  | cut -d= -f1 | grep -E '^(LOOMCYCLE|SANDBOX|OTEL|OLLAMA)_' | sort
```

From a running process on Linux:

```sh
tr '\0' '\n' < /proc/<pid>/environ | cut -d= -f1 | grep -E '^(LOOMCYCLE|SANDBOX|OTEL|OLLAMA)_' | sort
```

Compare the list with the [reference tables](#7-reference-tables). Every name in tables A to E is a variable to act on.

### 2.3 Ask the new binary, before the real start

Run the new binary's `validate` and `doctor` commands with the **same environment and the same config layers** as the server (the same `LOOMCYCLE_PRESETS`, `LOOMCYCLE_CONFIG_DIR` and config file). The refusal rules compare each removed variable with what your yaml gives, so a check against a different yaml gives a different answer.

```sh
loomcycle validate /path/to/loomcycle.yaml
loomcycle doctor --config /path/to/loomcycle.yaml
```

Both commands assemble their config the way the server does: the presets, the config directory, the named file with its section files, and the `auth.env` beside the last file. With no file named, `loomcycle validate` and `loomcycle doctor` load what a bare `loomcycle` would load in that environment, so a deployment that has presets and a config directory but no single `loomcycle.yaml` is checked as it boots ([5.13](#513-config-file-locations)).

What they do with the removed variables:

| Command | A removed variable that is only advised | A removed variable that is refused |
|---|---|---|
| `loomcycle validate [<config> ...]` | Prints an `Advisories : N` block on standard output, one `  - …` line per advisory, then `OK`. Exit code `0`. | Prints `loomcycle: error: config: retired environment variables are set to …` on standard error. Exit code `2`. Nothing else is printed. |
| `loomcycle doctor [--config <path> ...]` | Prints one `WARN` check labelled `Config advisory` per advisory. A warning does not change the exit code. | Prints a `FAIL` check labelled `Config parses` that carries the refusal text, then the summary. Exit code `1`. The later checks do not run. |

`validate` only loads the config. `doctor` also checks that the SQLite data directory is writable (or that a Postgres connection string is set) and that the listen address can be bound; an address already in use by your running server is a warning, not a failure. Neither command opens the store, so neither reports a problem that only appears when the store is opened. One of those is the refusal of an inferred Postgres backend beside an existing SQLite store file ([4.5](#45-a-postgres-connection-string-with-no-backend-named-now-opens-postgres)): `validate` prints `OK` and the server start, or any other command that opens the store, stops.

In a container deployment, run the same commands in the new image with the deployment's environment and config mount, for example by overriding the service's command for one run. Adapt this to your compose file:

```sh
docker compose run --rm --no-deps <service> validate /config/loomcycle.yaml
```

### 2.4 Fix each item

For each refusal and each advisory, find the variable in the reference tables, apply the "what to do" column, and remove the variable's line. Section 5 has worked examples by area. A refusal names every refused variable at once, so one pass is usually enough; run `validate` again until it prints `OK` with no advisories.

Then read section 4 and decide, for each behaviour change, whether it applies to you.

### 2.5 Start

Start the new binary. Read the first screen of the boot log: any remaining advisory is there, as a `WARN` line.

## 3. How the runtime tells you

### The advisory

One line per removed variable that is present in the environment, including one set to the empty string:

```
LOOMCYCLE_SCHEDULER_ENABLED is set but no longer read: the scheduler is always on; a schedule fires unless its definition is disabled
```

The text after the colon says what to do instead. An advisory changes nothing: the variable is ignored and the server starts.

### The refusal

One error for all refused variables. Its leading sentence is:

```
retired environment variables are set to something this version does not apply (a tighter bound, a feature kept off, another store, data kept or deleted differently, or a setting that moved to the yaml and is not there), and ignoring them would change this deployment; act on each, then remove its line:
```

It is followed by one `NAME: what to do` entry per refused variable, sorted by name and separated by ` | `. The server does not start.

A refusal is judged on the value. The same variable set to what the runtime now does anyway is advised, not refused.

### The rule for a setting that moved to the yaml

One rule covers every variable whose setting moved to a yaml key in this release (the rows of table A marked **Moved**):

- The boot is **refused** when the variable is set to a value other than the default **and** the yaml does not name the key. The refusal entry names the key.
- The variable is only **advised** when its value is the default, or when the yaml names the key, with any value.

What this means in practice:

- **It holds in both directions.** A timeout you raised is refused like one you lowered. Either is lost when the variable is ignored, and a raised one fails later, in production, as slow calls that time out.
- **The two values are not compared.** Naming the key is your answer. If the yaml names the key with another value than the variable had, the yaml's value applies and the variable is only advised. Write the value you want in the yaml; do not expect the runtime to tell you that it differs from the variable.
- **"The default" is what the runtime did when the variable was unset.** A value the old read ignored counts as the default: text where a number was expected, or zero where only a positive number was read. Table A says, per variable, which values are "other than the default".
- **A key counts as named in any loaded layer**: your config file, a file in the config directory, or a preset. A key written with an empty value (`max_age:` with nothing after it) is not named.
- **Removing the variable always clears the refusal.** You then run with the default.

A variable is judged only where it had an effect before the upgrade, so a stray line in a deployment that never used the feature does not stop the boot:

| Variables | Judged only when |
|---|---|
| The retention family modes and ages, `LOOMCYCLE_RETENTION_DEFS_KEEP_LAST_N`, `LOOMCYCLE_RETENTION_EXPORT_DIR`, `LOOMCYCLE_RETENTION_INTERVAL_MS` | `LOOMCYCLE_RETENTION_ENABLED=1` is also in the environment (the retention sweeper ran). |
| `LOOMCYCLE_MEMORY_TRACE_MAX_AGE_MS` | `LOOMCYCLE_MEMORY_TRACE_INDEX` is `1` or `true`. |
| `LOOMCYCLE_TENANT_TELEMETRY_ALLOWED_HOSTS` | `LOOMCYCLE_TENANT_TELEMETRY` is on. |
| `LOOMCYCLE_METRICS_COLLECT_SYSTEM`, `LOOMCYCLE_METRICS_SAMPLE_INTERVAL_MS` | `LOOMCYCLE_METRICS_ENABLED=1` is also in the environment (the sampler ran). |
| The two `LOOMCYCLE_OLLAMA_LOCAL_*_TIMEOUT_MS` variables | The config declares `ollama-local` (the default providers layer does) and `OLLAMA_BASE_URL` is not `disabled`. |
| The three `LOOMCYCLE_CODE_AGENTS_*` option variables | The config declares a `code-js` provider (the default providers layer does). |
| `LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS`, `LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS` | At least one provider this deployment can use lacks the option; see [5.3](#53-provider-options). |

Outside these conditions the variable is only advised.

Other removed variables keep rules of their own, listed in the tables: the feature switches, the fixed caps, the switches that decided which data is kept, the store and SQL Memory, the SQL Memory limits, the hooks lists, the two Ollama options and `LOOMCYCLE_RETENTION_ENABLED`.

### Where each appears

| Place | Advisory | Refusal |
|---|---|---|
| Server boot log (standard error) | A `WARN` line with the message `config: warning` and the advisory in its `detail` field. | An `ERROR` line with the message `config: load failed` and the refusal in its `err` field. The process exits. |
| `loomcycle validate` | The `Advisories` block; exit code `0`. | `loomcycle: error: config: …`; exit code `2`. |
| `loomcycle doctor` | `WARN` checks labelled `Config advisory`. | A `FAIL` check labelled `Config parses`; exit code `1`. |
| `loomcycle mcp --upstream <url>` (the thin client) | A `WARN` line `config: warning` on standard error, at start. | Never. The thin client loads no config, so it only advises. |

The thin client's environment is usually set by an editor plugin or an MCP client configuration, not by your server's environment files. Check it separately: it used to read `LOOMCYCLE_MCP_UPSTREAM_HEADER_TIMEOUT_MS` and `LOOMCYCLE_MCP_UPSTREAM_RECONNECT_ATTEMPTS`, both now fixed.

The sandbox builder sidecar is a separate program with its own rule: it refuses to start while any of its removed `SANDBOX_*` variables is set to a non-empty value. See [5.14](#514-sandbox-builder-sidecar).

### Values are never printed

Advisories and refusals name variables and never include a value. Several removed variables held secrets. You can paste an advisory or a refusal into a ticket.

There is one exception, and it carries no secret: the refusal of a retired hooks list ([5.10](#510-hooks-lists)) quotes the first entry the yaml list lacks, which is an owner name, a host name or an address range. An entry that is anything else, such as a URL pasted with credentials, is not quoted.

## 4. Behaviour changes with no variable to warn about

These are the cases the runtime cannot detect, because the old behaviour came from a variable you did not set, or because an advisory is all it can give. Each entry says who is affected, what happens, and what to do.

### 4.1 Features that are now always on

Seven features lost their switch and run on every server: the scheduler, the inbound webhook receiver, code agents (the `code-js` provider), code hooks, channel hooks, Bashbox, and the resume of a fan-out parent.

**Who is affected.** A deployment that never set the switch, so the feature was off. (A deployment that set the switch to anything other than `1` gets a refusal instead; see table C.)

**What happens.** Each feature is inert without a definition, but definitions created while the feature was off start working at the first boot:

- A schedule definition that was authored while the scheduler was off now fires on its schedule.
- A webhook definition now answers on its inbound route.
- A channel that declares `hooks:` now has them applied to every message. Before, the hooks were skipped.
- A hook definition whose body is `code-js` now runs.
- An agent definition with a `code_body` now runs, provided the config declares a `code-js` provider (the default providers layer does). Without one, creating or forking an agent definition that carries a `code_body` is refused with `a code_body cannot run here (no code-js provider is configured)`. A definition already stored is still loaded.
- An agent that names `Bashbox` in its `tools` now gets the Bashbox sandbox.
- A fan-out parent (an agent blocked in a parallel spawn) is now parked on pause and resumed with its children.

**What to do.** Before the upgrade, list your schedule, webhook, hook and agent definitions and your channel `hooks:` blocks, in every tenant. Disable or delete each one you do not want to start working. To keep code agents unavailable, run without the default providers layer (`LOOMCYCLE_NO_DEFAULT_PROVIDERS=1`) and declare your own `providers:` with no `code-js` entry.

### 4.2 Tenant-authored JavaScript is available to every tenant operator

**Who is affected.** A multi-tenant deployment that kept code agents or code hooks off.

**What happens.** Anyone who may author an agent or a hook definition in a tenant can now run JavaScript inside the loomcycle process. A body is bounded in time. It is **not bounded in memory**: a body that grows a value in a loop can exhaust the process that every tenant shares.

**What to do.** Read the "code-js bodies are bounded in time, not in memory" property in [`SECURITY.md`](../SECURITY.md). If you need memory isolation between tenants, run them in separate processes. To remove code agents, see 4.1; there is no switch for code hooks, so control who may author hook definitions.

### 4.3 Bashbox and agents registered over MCP

**Who is affected.** A deployment where agents are registered through the MCP `register_agent` tool and use Bashbox.

**What happens.** Bashbox is stripped from the tools of an agent registered over MCP, as `Bash`, `Write` and `Edit` already were, unless `LOOMCYCLE_MCP_ALLOW_PRIVILEGED_TOOLS=1` is set. Such an agent silently loses its shell.

**What to do.** Set `LOOMCYCLE_MCP_ALLOW_PRIVILEGED_TOOLS=1` only if you trust every caller that can register an agent with a shell and with any host command you allowed for the Bashbox fallback.

### 4.4 SQL Memory turns on for SQLite instances

**Who is affected.** A deployment on the SQLite store that never set `LOOMCYCLE_SQLMEM_ENABLED`.

**What happens.** SQL Memory (a per-scope SQL database the runtime hosts) is now on. Documents start working, an agent that holds the Memory tool can run SQL in the caller's own scope, and scope files are created under `<data dir>/sqlmem` (or `LOOMCYCLE_SQLMEM_ROOT`).

**What to do.** To keep it off, set in the yaml:

```yaml
storage:
  sqlmem_enabled: false
```

On a Postgres store nothing changes unless `LOOMCYCLE_SQLMEM_PG_DSN` (or `storage.sqlmem_pg_dsn`) is set, in which case SQL Memory is now on without the switch.

### 4.5 A Postgres connection string with no backend named now opens Postgres

**Who is affected.** A deployment that runs on SQLite while `LOOMCYCLE_PG_DSN` (or `storage.pg_dsn`) is set, for example through an environment file shared with another service, and that never named a backend.

**What happens.** The store backend is inferred: Postgres when a connection string is configured, SQLite otherwise. This deployment now opens Postgres, and its SQLite data appears to be gone. The data is still in the SQLite file.

**What to do.** Pin the backend in the yaml before you upgrade:

```yaml
storage:
  backend: sqlite
```

When Postgres is inferred and the data directory already holds a SQLite store file (`loomcycle.db`), the store is not opened. The server, and every other command that opens the store, stops with:

```
an existing SQLite store was found at <data dir>/loomcycle.db, and a Postgres DSN is configured with no storage.backend in the yaml, so the Postgres store would now be used and the SQLite data left behind. Set storage.backend in the yaml to say which: `sqlite` to keep using the existing store, or `postgres` to use the DSN (`loomcycle migrate sqlite-to-postgres` copies the data across first)
```

`validate` and `doctor` do not open the store and do not report this. Do not rely on the refusal as your only protection: it looks for the file in the data directory this start uses, so a start with another data directory finds nothing. Set the key.

Every start logs the choice: an `INFO` line `store: backend chosen` with `backend` and `source`, where `source` is `yaml` when `storage.backend` named it and `inferred` otherwise.

### 4.6 A2A is served when a server card is named

**Who is affected.** A deployment that sets `LOOMCYCLE_A2A_SERVER_CARD` and never set `LOOMCYCLE_A2A_ENABLED`.

**What happens.** Naming the card turns A2A on: the server surface is mounted and the outbound `a2a__*` tools are registered. If no card with that name exists, the boot stops.

**What to do.** Unset `LOOMCYCLE_A2A_SERVER_CARD` to keep A2A off. Cards declared in the yaml do not turn A2A on by themselves.

### 4.7 Content directories under the config directory are loaded

**Who is affected.** A deployment that sets `LOOMCYCLE_CONFIG_DIR`, whose config directory contains a `skills/`, `agents/` or `help/` subdirectory, and that leaves `LOOMCYCLE_SKILLS_ROOT`, `LOOMCYCLE_AGENTS_ROOT` or `LOOMCYCLE_HELP_ROOT` unset.

**What happens.** Each unset variable now resolves to that subdirectory when it exists. Skills, agent files and help topics found there are loaded, and the boot logs each directory it adopted this way. Skill bodies and agent files are trusted as operator-authored content. In an adopted `agents/` directory a `.md` file with no frontmatter is skipped and logged, because such a file is a prompt body or a README, not an agent definition; a directory named by `LOOMCYCLE_AGENTS_ROOT` loads every `.md` file, as before.

**What to do.** Check what those directories contain and who can write to them. To decline the default, set the variable to the empty string (for example `LOOMCYCLE_AGENTS_ROOT=`), or point it at an empty directory where an empty value cannot be entered.

### 4.8 Debug flags are folded into the log level

**Who is affected.** A deployment that set `LOOMCYCLE_AUTH_VERBOSE`, `LOOMCYCLE_CHANNEL_DEBUG` or `LOOMCYCLE_OLLAMA_DEBUG_THINK`.

**What happens.** You get an advisory, and the diagnostic lines disappear at the default log level. They are now debug-level lines: the reason for a rejected credential, each channel publish with its commit time, and each Ollama request's model, effort and `think` flag.

**What to do.** Set `LOOMCYCLE_LOG_LEVEL=debug` while you diagnose. It also shows every other debug line.

### 4.9 The legacy usage archiver is gone

**Who is affected.** A deployment that deleted old runs with `LOOMCYCLE_USAGE_RUN_RETENTION_MS` and `LOOMCYCLE_USAGE_RUN_RETENTION_MODE`.

**What happens.** The archiver no longer exists; the usage sweeper only rolls up usage rows. If the archiver was deleting (mode `prune` or `export+prune` with a positive age) and the yaml sets no `retention.chats.mode`, the boot is refused. In every other configuration you get an advisory and nothing is deleted.

**What to do.** Move to the chats family of the `retention:` block ([5.2](#52-retention)).

### 4.10 Retention needs `retention.enabled`

**Who is affected.** Every deployment that deletes data with age.

**What happens.** Retention is configured only in the yaml `retention:` block, and the sweeper runs only when `retention.enabled` is `true`. If you remove the old `LOOMCYCLE_RETENTION_*` lines without writing the block, retention stops and nothing says so. If you write a family `mode` and forget `enabled: true`, you get an advisory and nothing is deleted. If you leave the old variables in place beside `LOOMCYCLE_RETENTION_ENABLED=1`, the boot is refused until `retention.enabled` agrees with the switch and the yaml names the key of every variable that is set to something other than its default (a deleting mode, a positive age, a kept-version count other than 5, an export directory, an interval other than one hour).

**What to do.** Write the block first ([5.2](#52-retention)), then remove the variables.

### 4.11 A limit you raised, removed or switched off returns to its default

**Who is affected.** A deployment that set a removed variable to something *looser* than the value that now applies.

**What happens.** For a limit that became a fixed value, a refusal covers only the tightening direction. A looser value is advised and dropped. (A setting that moved to the yaml is different: it is refused in both directions until the yaml names its key, see [section 3](#the-rule-for-a-setting-that-moved-to-the-yaml). So are the settings that decide which data is kept, see below.) Cases to check in your list of names:

- **Channel payload cap.** `LOOMCYCLE_CHANNELS_MAX_VALUE_BYTES` raised above 64 KiB, or set to `0` for "no cap": a channel message over 64 KiB is now refused on every surface that publishes.
- **Memory value cap.** `LOOMCYCLE_MEMORY_MAX_VALUE_BYTES` raised or `0`: a Memory value over 64 KiB is refused.
- **Sweeps switched off with `0`** (expired Memory entries, expired channel messages, expired registered agents, old metrics samples): all of them now always run on a fixed cadence.
- **Expired token rows kept forever** with `LOOMCYCLE_EXPIRED_TOKEN_RETENTION_SECONDS=0`: an expired token's row is now deleted 7 days after it expired.
- **Tool calls forced serial** with `LOOMCYCLE_TOOL_PARALLELISM=1`: 8 tool calls of one turn now run at once.
- **SSE keepalive disabled** with `LOOMCYCLE_SSE_KEEPALIVE_MS=0`: a run stream sends a keepalive every 20 s.

**What to do.** For a fixed value there is nothing to set; check that your workload fits.

**Settings that decided which data is kept stop the boot instead.** Four removed variables changed what is deleted or created, so the old "off" (or another age) is refused, not dropped:

- `LOOMCYCLE_USAGE_SWEEPER=0`: the usage rollup always runs. Per-call usage rows older than `retention.usage_detail.max_age` (30 days unless set) are deleted and their daily totals are kept. To keep per-call rows longer, raise that key.
- `LOOMCYCLE_HEARTBEAT_SWEEPER=0`: the run heartbeat sweeper always runs, and a run whose heartbeat went stale is marked failed.
- `LOOMCYCLE_MEMORY_PROVISION_IDENTITY_DOCS` set to `0`, `false`, `no` or `off`: the tenant-root and user-root documents are always created when a principal is established.
- `LOOMCYCLE_RETENTION_CHATS_INTERNAL_MAX_AGE_MS` set to any age other than 3 days (`259200000`), shorter or longer: chats of internal agents are kept 3 days and then deleted, when chat retention is on. A value of `0` used to inherit the chat age, so it is refused unless `retention.chats.max_age` is `3d`. A value that is not a whole number is refused; a negative number was always ignored and is only advised.

None of the four has a setting that brings the old behaviour back. Any other value of the first three is only advised.

### 4.12 A moved setting you removed without writing the yaml

**Who is affected.** A deployment that deletes a moved variable's line (or refreshes a compose file that no longer passes it) without writing the yaml key. The rule of [section 3](#the-rule-for-a-setting-that-moved-to-the-yaml) protects a variable that is still set; it cannot see one that is gone.

**What happens.** The default applies and nothing is logged:

- **Process metrics sampler.** No samples are recorded and the activity page of the Web UI is empty.
- **Document projection.** A positive `LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL` used to switch the projection on as well as set its cadence. The projection now needs `memory.projection.enabled: true` beside `memory.projection.interval`. A variable that is still set is refused until the yaml names both keys; one that is gone leaves the projection off.
- **Provider timeouts, GPU offload, context window.** Slow calls time out at the defaults; local models run where Ollama puts them.
- **Retention.** See [4.10](#410-retention-needs-retentionenabled).
- **Everything else in table A.** The yaml default applies.

Two cases where a variable that is still set is only advised although the setting is gone:

- **`LOOMCYCLE_SQLMEM_GC_INTERVAL_MS`.** The SQL Memory collection runs at the yaml's cadence (`storage.sqlmem_gc_interval_ms`, one hour unset).
- **A variable outside the conditions of the table in section 3**, for example a retention family variable in an environment without `LOOMCYCLE_RETENTION_ENABLED=1`. It did nothing before.

**What to do.** Write the yaml key from table A before you remove the variable.

### 4.13 Two audio queue sizes became one

**Who is affected.** A deployment that sized the speech and transcription queues differently.

**What happens.** `LOOMCYCLE_AUDIO_MAX_INFLIGHT` (default `2`) sizes both. The two routes keep separate queues, each of that size. A route you held to *fewer* requests is refused at boot; a route you gave *more* gets the shared size with an advisory.

**What to do.** Set `LOOMCYCLE_AUDIO_MAX_INFLIGHT` to the size you want for both.

### 4.14 The `base` preset names newer models and routes to Gemini

**Who is affected.** Every deployment that selects `base`, by name or through `personal` or `server`, and does not restate the alias or tier in its own config.

**What happens.**

| Alias or list | Before | Now |
|---|---|---|
| `haiku` | `claude-haiku-4-5` | `claude-haiku-5-5` |
| `sonnet` | `claude-sonnet-4-6` | `claude-sonnet-5-5` |
| `opus` | `claude-opus-4-7` | `claude-opus-5-5` |
| `cloud-kimi` | `kimi-k2.6` | `kimi-k3` |
| `gemini-lite`, `gemini-flash`, `gemini-pro`, `gemini-pro-preview` | not defined | patterns `gemini-*-flash-lite`, `gemini-*-flash`, `gemini-*-pro`, `gemini-*-pro-preview` |
| `local-coder` | pattern `qwen3-coder*` | pattern `qwen3.8*`, as the `local` preset has it |
| `provider_priority`, `user_tiers.default` | no `gemini` | `gemini` after `deepseek` |
| `tiers.low`, `tiers.middle` | no Gemini candidate | `gemini-lite`, `gemini-flash` after the local candidate |
| `tiers.high` | no Gemini candidate | `gemini-pro`, then `gemini-pro-preview`, after `cloud-kimi` |

A provider with no key is skipped, so a deployment with no `GEMINI_API_KEY` routes as before apart from the model ids. In the high tier a released Gemini pro model is chosen before a preview. The `low`, `medium`, `high` and `free` user tiers are unchanged.

The `local` preset's `local-fast` and `local-chat` aliases were written as `model: gpt-oss*` and `model: qwen3.6*`. A pattern is read only under `model_pattern`, so both candidates were skipped; they are patterns now. With `local` selected, a pulled `gpt-oss` or `qwen3.6` model becomes the first candidate of the low and middle tiers.

**What to do.** To keep a model, restate its alias in your own config: `models: { sonnet: { provider: anthropic, model: claude-sonnet-4-6 } }`. `GET /v1/_routing` and the Web UI's routing page show what each tier resolves to.

### 4.15 Two chat agents have a new content signature

**Who is affected.** A deployment that verifies bundled agents by content signature (`AgentDef verify`) and selects the `chat` bundle.

**What happens.** `chat/medium` and `chat/local-small` no longer set `compaction.memory_flush`. In their context mode the key did nothing, and every load printed an advisory saying so. What the agents do is unchanged, but the key is part of an agent's signed content, so both signatures change. `chat/local` is unchanged.

**What to do.** Record the new signatures. To have these agents queue what they discard for consolidation, set `context.harvest_to_memory: true` on them in your own config.

### 4.16 The undrained memory queue is reported when it holds rows

**Who is affected.** A deployment with agents that hold the `Memory` tool and no enabled consolidation schedule.

**What happens.** The advisory "agents can enqueue with Memory op=add but no enabled scheduled run drains it" is no longer printed by every load. The server logs it at start, per memory scope, when that scope's queue holds rows. `loomcycle validate`, `loomcycle doctor` and a config reload no longer show it.

**What to do.** Nothing, unless you relied on `validate` to tell you. The start log is where to look.

### 4.17 `include:` and `presets:` are keys of the layer assembly

**Who is affected.** A config file that carries a top-level `include:` or `presets:` key that used to be ignored as unknown.

**What happens.** `include:` works only inside a preset built into the binary; in your own file it stops the load. `presets:` selects embedded presets and is read from one file (section 6); in any other config file it stops the load, naming the file.

**What to do.** Remove a stray `include:`. Keep `presets:` only in the primary file.

### 4.18 `loomcycle init` writes a profile's short config

**Who is affected.** Anyone who runs `loomcycle init` and expects the long, fully-commented config.

**What happens.** `loomcycle init` writes the `personal` profile's config: one active line, `presets: [personal]`, and commented blocks for what is yours. `loomcycle init --profile server` writes the server profile's. Neither carries the routing matrix or example agents; the preset supplies them.

**What to do.** `loomcycle init --full` writes the long reference config, as before.

## 5. Step by step, by area

Each area shows the old environment lines and what replaces them. Every key name is the real one. Values are examples.

### 5.1 Storage and SQL Memory

The store backend follows the connection string. SQL Memory follows the store.

Before:

```sh
LOOMCYCLE_STORAGE_BACKEND=postgres
LOOMCYCLE_PG_DSN=            # secret
LOOMCYCLE_SQLMEM_ENABLED=1
LOOMCYCLE_SQLMEM_PG_DSN=     # secret
LOOMCYCLE_SQLMEM_MAX_ROWS=5000
LOOMCYCLE_SQLMEM_STATEMENT_TIMEOUT_MS=10000
```

After, in the environment (secrets and host paths stay):

```sh
LOOMCYCLE_PG_DSN=            # secret; its presence selects Postgres
LOOMCYCLE_SQLMEM_PG_DSN=     # secret; its presence turns SQL Memory on with Postgres
```

After, in the yaml:

```yaml
storage:
  backend: postgres                    # optional: pins the backend whatever the environment carries
  sqlmem_max_rows: 5000
  sqlmem_statement_timeout_ms: 10000
```

Rules:

- With no `storage.backend`, the store is Postgres when `LOOMCYCLE_PG_DSN` or `storage.pg_dsn` is set, and SQLite otherwise. An explicit `storage.backend` always wins. A yaml `pg_dsn` that is still an unexpanded `${NAME}` reference does not count as a connection string, and neither does a `LOOMCYCLE_PG_DSN` that is empty or only whitespace.
- An inferred Postgres backend is refused when the data directory already holds a SQLite store file; see [4.5](#45-a-postgres-connection-string-with-no-backend-named-now-opens-postgres). The boot log says which backend was chosen and whether the yaml named it.
- `storage.sqlmem_enabled` has three states. Unset: on with SQLite; on with Postgres when the SQL Memory connection string is set. `false`: off on either store. `true` on Postgres with no SQL Memory connection string stops the boot.
- With a Postgres store and no `LOOMCYCLE_SQLMEM_PG_DSN`, SQL Memory is off and the server starts. The boot log carries a `WARN` line: `sqlmem: SQL Memory is off because LOOMCYCLE_SQLMEM_PG_DSN is not set; Documents need SQL Memory and are off with it. Set the DSN of a separate database to turn both on, or storage.sqlmem_enabled: false in the yaml to say this is intended`. With `storage.sqlmem_enabled: false` the line is not logged.
- `LOOMCYCLE_SQLMEM_ROOT`, `LOOMCYCLE_SQLMEM_QUOTA_BYTES`, `LOOMCYCLE_SQLMEM_AUDIT_MODE`, `LOOMCYCLE_SQLMEM_SCOPE_TTL_MS`, `LOOMCYCLE_SQLMEM_TOTAL_MAX_BYTES` and `LOOMCYCLE_SQLMEM_SHARED_SCHEMAS` are still read from the environment. Do not move them.
- `LOOMCYCLE_PG_AUTOMIGRATE` is read in both directions: `1` turns schema migration at boot on and `0` turns it off, whatever `storage.pg_automigrate` says. Before, only `1` was read, so a host that carried `LOOMCYCLE_PG_AUTOMIGRATE=0` beside a yaml `pg_automigrate: true` migrated at boot; now it does not, and the schema moves only when you run `loomcycle migrate up`. The value is `1` or `0`, with spaces around it ignored. Any other non-empty value stops the load (`LOOMCYCLE_PG_AUTOMIGRATE: the value is not 1 (migrate the schema at boot) or 0 (do not)`): a `false` written to keep migration off used to be skipped, which left a yaml `pg_automigrate: true` in force. Empty or unset leaves the yaml's value.
- Set `storage.backend: postgres` on every Postgres deployment, and `storage.backend: sqlite` on every SQLite deployment that has a connection string anywhere in its environment. It costs one line, and the store no longer depends on what the environment happens to carry.

### 5.2 Retention

Retention moved from sixteen variables (the switch, the interval, the export directory, five family modes, five family ages, the kept-version count and the usage-detail and trace ages; the ages in milliseconds) to one yaml block of sixteen keys with durations.

Before:

```sh
LOOMCYCLE_RETENTION_ENABLED=1
LOOMCYCLE_RETENTION_INTERVAL_MS=86400000
LOOMCYCLE_RETENTION_EXPORT_DIR=/data/retention-exports
LOOMCYCLE_RETENTION_CHATS_MODE=export+prune
LOOMCYCLE_RETENTION_CHATS_MAX_AGE_MS=7776000000
LOOMCYCLE_RETENTION_DEFS_MODE=prune
LOOMCYCLE_RETENTION_DEFS_MAX_AGE_MS=2592000000
LOOMCYCLE_RETENTION_DEFS_KEEP_LAST_N=5
LOOMCYCLE_USAGE_DETAIL_RETENTION_MS=2592000000
LOOMCYCLE_MEMORY_TRACE_MAX_AGE_MS=2592000000
```

After, the full block:

```yaml
retention:
  enabled: true                 # starts the sweeper; default false
  interval: 24h                 # how often the sweeper runs; default 1h
  export_dir: /data/retention-exports   # where export+prune writes what it deletes
  chats:                        # aged sessions with their runs and events; a pinned session is exempt
    mode: export+prune
    max_age: 90d
  defs:                         # old versions of definitions
    mode: prune
    max_age: 30d
    keep_last_n: 5              # newest versions always kept; default 5; 0 keeps none
  mem:                          # memory of fully retired agents
    mode: off
    max_age: 30d
  mem_content:                  # superseded memory content in live scopes; off or prune only (no export)
    mode: off
    max_age: 30d
  empty_dossier:                # subject documents with no facts and no references
    mode: off
    max_age: 30d
  usage_detail:
    max_age: 30d                # per-call usage rows are rolled up after this; always runs; default 30d
  trace:
    max_age: 30d                # how long an indexed conversation turn stays searchable; default 30d
```

Rules:

- **The switch.** Nothing is deleted unless `retention.enabled` is `true` **and** a family's `mode` is `prune` or `export+prune`. The default of `enabled` is `false`. A mode with `enabled` not true produces an advisory and deletes nothing.
- **Modes.** `off` (the default), `prune`, `export+prune`. Any other value stops the load. The old variables accepted any text and treated it as off.
- **`max_age` is required when a mode deletes.** A family whose mode is `prune` or `export+prune` with no `max_age` stops the load. To delete everything eligible whatever its age, write `max_age: 0s`. Before, a missing age meant exactly that, without saying so.
- **Durations.** `30d`, `12h`, `1d12h`, `90m`. A bare number other than `0` is refused, because nothing says which unit it meant. To convert: 86400000 ms is `1d`, 2592000000 ms is `30d`, 7776000000 ms is `90d`.
- **Export directory.** `export+prune` writes each item under `export_dir` and then deletes it, for `defs`, `chats`, `mem` and `empty_dossier`. One of those four in that mode with no `export_dir` deletes nothing. Under `chats`, the chats of the runtime's own maintenance agents and sessions marked no-trace are deleted without a copy.
- **`mem_content` has no export.** Its mode is `off` or `prune`. `retention.mem_content.mode: export+prune` stops the load: the sweeper would delete and write no copy, with or without `export_dir`. `LOOMCYCLE_RETENTION_MEM_CONTENT_MODE=export+prune` was accepted and did exactly what `prune` does, so write `prune`.
- **Internal chats.** Chats of the runtime's own maintenance agents are deleted after 3 days whenever the chats family is on. This age is fixed.
- **Not part of the block.** The audit stream's retention stays in the environment (`LOOMCYCLE_AUDIT_RETENTION_DAYS`), and so does the process metrics retention (`LOOMCYCLE_METRICS_RETENTION_DAYS`).

Order of work: write the block, run `validate`, then remove the variables. With `LOOMCYCLE_RETENTION_ENABLED=1` in the environment, a retention variable set to something other than its default refuses the boot until the yaml names its key; once every such key is named, the variables are only advised. The yaml's values are the ones that apply, whatever the variables say, so check each converted age. A retention variable in an environment without `LOOMCYCLE_RETENTION_ENABLED=1` is only advised: retention was not running.

### 5.3 Provider options

Stream timeouts, the Ollama options and the code-agent budgets are options of each provider.

Before (a deployment with keys for Anthropic, OpenAI and hosted Ollama, and a local Ollama):

```sh
ANTHROPIC_API_KEY=           # secret
OPENAI_API_KEY=              # secret
OLLAMA_API_KEY=              # secret
LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS=30000
LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS=60000
LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS=900000
LOOMCYCLE_OLLAMA_LOCAL_IDLE_TIMEOUT_MS=900000
LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU=99
LOOMCYCLE_OLLAMA_NUM_CTX=32768
LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS=60
LOOMCYCLE_CODE_AGENTS_MAX_WALL_SECONDS=3600
LOOMCYCLE_CODE_AGENTS_DETERMINISTIC=1
```

After (the three keys stay in the environment):

```yaml
providers:
  anthropic:
    options:
      header_timeout_ms: 30000   # time to the response headers; default 60000
      idle_timeout_ms: 60000     # longest silence inside a stream; default 90000
  openai:
    options:
      header_timeout_ms: 30000
      idle_timeout_ms: 60000
  ollama:
    options:
      header_timeout_ms: 30000   # hosted Ollama took the global timeouts too
      idle_timeout_ms: 60000
      num_ctx: 32768             # hosted Ollama; unset = the model's own window
  ollama-local:
    options:
      num_ctx: 32768             # context window of every local model
      num_gpu: 99                # layers offloaded to the GPU; unset = Ollama decides
      header_timeout_ms: 900000  # default 600000
      idle_timeout_ms: 900000    # default 600000
  code-js:
    options:
      deterministic: true        # default false
      run_timeout_seconds: 60    # active time; default 120
      max_wall_seconds: 3600     # lifetime, waits included; default 86400
```

With this yaml the "before" environment starts with advisories only, and you can remove the nine lines.

Rules:

- A `providers:` entry merges over the built-in provider field by field. You do not restate the driver or the key variable.
- **There is no global timeout key.** The two global variables applied to every provider except `ollama-local`. Each provider that needs another value than the default sets its own options.
- **A global timeout other than the default is refused until every provider you can use has the option.** This is the rule of [section 3](#the-rule-for-a-setting-that-moved-to-the-yaml), applied per provider: if `LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS` is a positive number other than `60000`, or `LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS` a positive number other than `90000`, the boot is refused while a provider this deployment can use has no `header_timeout_ms` (or `idle_timeout_ms`) option. The refusal lists those provider ids:

  ```
  LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS: set providers.<id>.options.idle_timeout_ms in the yaml for each provider that needs another value than 90000 (the anthropic-oauth-dev provider has no options: it always waits 90000); the variable was set to something other than the default and the option is missing on these providers: anthropic, openai (set it on each, with the value you want that provider to have, or remove the variable to take the default)
  ```

- **Which providers count.** A provider counts when all three hold:
  1. The config declares it under `providers:`. The default providers layer declares the built-in ones.
  2. It is registered: the variable named by its `api_key_env` is set, or it names no key variable (a keyless endpoint you declared yourself). A built-in provider you have no key for is not registered, is never called, and is not counted.
  3. It took the global timeouts. `ollama-local` had its own pair, and `code-js` and the mock provider make no HTTP call, so they are not counted.

  Set the option on each provider the refusal lists. The option counts when it is a positive number; a quoted number or a zero is skipped where the option is read, so it does not count.
- **`anthropic-oauth-dev` has fixed timeouts.** That provider is not declared under `providers:` and has no options. It used to take the global variables; it now always waits 60 s for the headers and 90 s inside a stream, and it is not counted by the rule.
- **The `ollama-local` pair and the code-agent options follow the same rule for one provider.** `LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS` or `LOOMCYCLE_OLLAMA_LOCAL_IDLE_TIMEOUT_MS` set to a positive number other than `600000` is refused until `providers.ollama-local.options` has the option; `LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS` other than `120`, `LOOMCYCLE_CODE_AGENTS_MAX_WALL_SECONDS` other than `86400` and `LOOMCYCLE_CODE_AGENTS_DETERMINISTIC=1` are refused until `providers.code-js.options` has theirs.
- **`LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX` is still a variable, and it now wins over the yaml.** Set to a positive number, it is the local context window even when `providers.ollama-local.options.num_ctx` is set, in your own file or in a preset. Before this release the yaml option won. When the yaml names the option with anything but the variable's number, the boot log carries a `config: warning` line that says so (`LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX and providers.ollama-local.options.num_ctx are both set and differ: the environment's value is the one used; remove one of them`). That includes a yaml `num_ctx: 0`: before this release the zero replaced the variable and the model's own window applied; now the variable's window does. Keep the one you mean. It is the only Ollama setting left in the environment besides the endpoint: the hosted window (`providers.ollama.options.num_ctx`), the GPU offload and the stream timeouts are yaml only.
- `OLLAMA_BASE_URL` and `LOOMCYCLE_CODE_AGENTS_ROOT` also stay in the environment.
- **`LOOMCYCLE_OLLAMA_NUM_CTX` and `LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU` are refused while the yaml does not name the option.** Set to a positive number, each stops the boot unless the yaml has `providers.ollama.options.num_ctx` or `providers.ollama-local.options.num_gpu`. Dropped silently, hosted requests would run in the model's own window and local models on whatever Ollama picks, often the CPU. Write the option with the variable's number, then remove the variable. When the yaml already names the option, with any value, the variable is only advised: a yaml option always replaced the variable, so that value was the one in use before the upgrade too.

### 5.4 Timeout scaling

Before:

```sh
LOOMCYCLE_TIMEOUT_SCALING=measure
LOOMCYCLE_TIMEOUT_SCALING_REFERENCE_TPS=100
LOOMCYCLE_TIMEOUT_SCALING_MAX_MULTIPLIER=8
```

After:

```yaml
timeout_scaling:
  mode: measure            # measure (default) | off
  reference:
    decode_tps: 100        # default 100
  max_multiplier: 8        # default 8
```

Each of the three follows the rule of [section 3](#the-rule-for-a-setting-that-moved-to-the-yaml). `measure` is the default, so `LOOMCYCLE_TIMEOUT_SCALING=off` is refused until the yaml names `timeout_scaling.mode`; the reference speed and the multiplier are refused when they are positive numbers other than `100` and `8` and the yaml does not name their keys.

### 5.5 Embedder and projection

Before:

```sh
LOOMCYCLE_MEMORY_EMBED_BATCH_SIZE=64
LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS=10000
LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL=1h
```

After:

```yaml
memory:
  embedder:
    batch_size: 64         # texts per call; default 100; 1 sends one text per call
    timeout_ms: 10000      # one embedder call; default 30000
  projection:
    enabled: true          # the variable used to imply this
    interval: 1h           # unset or "0" = no sweeper
```

These keys sit beside the `provider` and `model` you already have under `memory.embedder`. Each of the three follows the rule of [section 3](#the-rule-for-a-setting-that-moved-to-the-yaml): a batch size other than `100`, a timeout other than `30000` or a positive interval is refused until the yaml names its key. The old `0` values have no yaml form: a batch size of `0` (one text per call) is `batch_size: 1`, and a timeout of `0` (no timeout) cannot be expressed, so choose a timeout. The projection interval stood for two keys, because a positive value also turned the projection on: it is refused until the yaml names both `memory.projection.interval` and `memory.projection.enabled`.

### 5.6 Concurrency

Before:

```sh
LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER=4
LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER=8
LOOMCYCLE_PROVIDER_QUEUE_DEPTH=16
```

After:

```yaml
concurrency:
  max_concurrent_runs: 8
  max_queue_depth: 32
  max_concurrent_runs_per_user: 4   # default 0 = no per-user cap
  max_queued_runs_per_user: 8       # default 16; 0 refuses at once over the cap
  provider_queue_depth: 16          # default 0 = max_queue_depth
```

`max_concurrent_runs` and `max_queue_depth` were already yaml-only. Each of the three variables follows the rule of [section 3](#the-rule-for-a-setting-that-moved-to-the-yaml): a per-user cap above zero, a queue bound other than `16`, or a provider queue depth other than `max_queue_depth` is refused until the yaml names its key.

### 5.7 Telemetry hosts and sampler ratios

Tracing itself was already configured by the standard `OTEL_*` variables. If you still set the old `LOOMCYCLE_OTEL_*` names, replace them (table D). The per-tenant parts moved to the yaml.

Before:

```sh
LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS=acme=1,beta=0.1
LOOMCYCLE_TENANT_TELEMETRY_ALLOWED_HOSTS=otel.example.com,*.collector.example.com
```

After:

```yaml
observability:
  tracing:
    tenant_sampler_ratios:      # tenant -> ratio, 0 to 1
      acme: 1
      beta: 0.1
  tenant_telemetry:
    allowed_hosts:              # the only hosts a tenant's collector endpoint may name
      - otel.example.com
      - "*.collector.example.com"   # a suffix entry must be quoted
```

Rules:

- A retired ratio list with at least one entry is refused until the yaml names `observability.tracing.tenant_sampler_ratios`. The yaml map is then the whole map: the variable no longer adds to it or replaces a tenant's ratio in it, so copy every tenant you need.
- A retired host list with at least one host is refused until the yaml names `observability.tenant_telemetry.allowed_hosts`, while `LOOMCYCLE_TENANT_TELEMETRY` is on: with no list, a tenant's collector endpoint may name any public host. With tenant telemetry off the list did nothing and is only advised.
- `LOOMCYCLE_TENANT_TELEMETRY` (the switch), `LOOMCYCLE_TENANT_TELEMETRY_ALLOW_HTTP` and `LOOMCYCLE_TENANT_TELEMETRY_MAX_RATIO` stay in the environment.

### 5.8 Login origins

Before:

```sh
LOOMCYCLE_UI_LOGIN_ORIGINS=https://landing.example.com
```

After:

```yaml
auth:
  login_origins:
    - https://landing.example.com
```

A retired variable that lists at least one origin is refused until the yaml names `auth.login_origins`: without the list, that page's sign-in would stop working. The yaml list is then the whole list, so copy every origin.

The variable was one comma-separated value; the key is a list with **one origin per item**. An item that holds a comma or a space stops the load (`auth.login_origins: entry 2 holds a comma or a space; write one origin per list item`): copied whole from the variable, it would equal no origin a browser reports.

```yaml
auth:
  login_origins:                       # not: ["https://a.example.com,https://b.example.com"]
    - https://a.example.com
    - https://b.example.com
```

### 5.9 Process metrics sampler

Before:

```sh
LOOMCYCLE_METRICS_ENABLED=1
LOOMCYCLE_METRICS_COLLECT_SYSTEM=1
LOOMCYCLE_METRICS_SAMPLE_INTERVAL_MS=5000
```

After:

```yaml
metrics:
  enabled: true
  collect_system: true     # system-wide CPU and memory; Linux only
  sample_interval: 5s      # default 5s; at least 1s
```

`LOOMCYCLE_METRICS_ENABLED=1` is refused until the yaml names `metrics.enabled`: ignored, the sampler would stop and the activity page go empty. Beside `LOOMCYCLE_METRICS_ENABLED=1`, `LOOMCYCLE_METRICS_COLLECT_SYSTEM=1` and a sample interval other than `5000` are refused until the yaml names `metrics.collect_system` and `metrics.sample_interval`; without the switch they did nothing and are only advised. A `sample_interval` under `1s` stops the load; the variable used to raise it to one second silently. This block is not the Prometheus endpoint: `LOOMCYCLE_METRICS_LISTEN` and `LOOMCYCLE_METRICS_RETENTION_DAYS` stay in the environment.

### 5.10 Hooks lists

`LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS` and `LOOMCYCLE_HOOKS_PRIVATE_HOST_ALLOWLIST` are removed. Both used to append to the yaml list; the yaml list is now the whole list.

Before:

```sh
LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS=acme:url-gate
LOOMCYCLE_HOOKS_PRIVATE_HOST_ALLOWLIST=10.20.0.0/16
```

After:

```yaml
hooks:
  permit_host_widen:
    owners:
      - "acme:url-gate"      # [tenant:]name
  private_host_allowlist:
    - 10.20.0.0/16
```

A variable that names an entry the yaml list lacks refuses the boot: without the entry, that hook's `allow_hosts` would stop being honoured, or its callback to a private address would be refused. A variable whose entries are all in the yaml is only advised.

The refusal quotes the first entry the yaml lacks, so you do not have to compare the two lists by eye:

```
LOOMCYCLE_HOOKS_PRIVATE_HOST_ALLOWLIST: list each host or CIDR range under hooks.private_host_allowlist in the yaml; the yaml list lacks "10.20.0.0/16" (the first of this variable's entries it lacks). Entries are compared as written: a wider range or a shorter suffix in the yaml does not count for it. If the yaml already allows every address your hooks call, remove the variable
```

Two ways to clear it:

- **Copy the entry into the yaml**, start again, and repeat for the next entry the refusal names, until the variable is only advised. Then remove the variable.
- **Remove the variable**, if the yaml list already grants what your hooks need. This is the case when the yaml has a wider entry than the variable (`10.0.0.0/8` in the yaml, `10.20.0.0/16` in the variable): the rule compares entries as written and cannot tell that the wider one covers it, but the runtime reads only the yaml list, and that list covers the address.

How entries are compared:

- An owner entry is `[tenant:]name`. An entry with no tenant is the shared tenant, and spaces around either part do not count.
- A host entry matches whatever its case and with or without a trailing dot. It is compared as written: a yaml entry for a shorter suffix or a wider range does not stand in for it, so copy the entry itself.

### 5.11 Dead-link collection

The two dead-link variables became one.

Before:

```sh
LOOMCYCLE_DEADLINK_GC_MS=3600000
LOOMCYCLE_DEADLINK_GC_DRY_RUN=1
```

After:

```sh
LOOMCYCLE_DEADLINK_GC=dry-run     # off | dry-run | on
```

Rules:

- `LOOMCYCLE_DEADLINK_GC` is `on` when unset: the collection runs and deletes unreachable chunk references. `dry-run` counts and logs what it would collect and deletes nothing. `off` does not run it. Any other value stops the boot.
- The collection runs every hour. The interval can no longer be set, and an interval in `LOOMCYCLE_DEADLINK_GC_MS` is not read.
- `LOOMCYCLE_DEADLINK_GC_MS` set to zero or a negative number (the old "off") stops the boot unless `LOOMCYCLE_DEADLINK_GC=off`.
- `LOOMCYCLE_DEADLINK_GC_MS` set to more than one hour (`3600000`) stops the boot while the collection deletes, that is unless `LOOMCYCLE_DEADLINK_GC` is `off` or `dry-run`: rows would be deleted sooner than you had set. An interval of an hour or less, and a value that is not a whole number, are only advised.
- `LOOMCYCLE_DEADLINK_GC_DRY_RUN=1` stops the boot unless `LOOMCYCLE_DEADLINK_GC` is `dry-run` or `off`. Any other value of it is only advised.

This setting stays in the environment because it decides whether data is deleted.

### 5.12 Audio queue

Before:

```sh
LOOMCYCLE_SPEECH_MAX_INFLIGHT=1
LOOMCYCLE_TRANSCRIPTION_MAX_INFLIGHT=1
```

After:

```sh
LOOMCYCLE_AUDIO_MAX_INFLIGHT=1    # default 2; sizes the queue of both routes
```

### 5.13 Config file locations

Three things decide which yaml files are loaded, in this order, each later one winning: the embedded presets, every `*.yaml` in `LOOMCYCLE_CONFIG_DIR`, then each `--config` file. Which presets: the `--preset` flag, else `LOOMCYCLE_PRESETS`, else the `presets:` key of the primary config file (section 6).

`LOOMCYCLE_CONFIG_FILES`, the third way to name config files, is removed. Set and non-empty, it refuses the boot: the files it names would silently not be loaded. The refusal is raised before any yaml is read, so it is also what you see when that variable named your only config:

```
a retired environment variable names config files that are no longer layered; act on it, then remove its line: LOOMCYCLE_CONFIG_FILES: pass each file as --config <path> (repeatable, in the same order), or place the files in LOOMCYCLE_CONFIG_DIR
```

Set to the empty string it is only advised.

Before:

```sh
LOOMCYCLE_CONFIG_FILES=/etc/loomcycle/base.yaml:/etc/loomcycle/site.yaml
```

After, either pass each file:

```sh
loomcycle --config /etc/loomcycle/base.yaml --config /etc/loomcycle/site.yaml
```

or place the files in one directory and name it:

```sh
LOOMCYCLE_CONFIG_DIR=/etc/loomcycle
```

Files in the config directory are loaded in lexical order, so name them so that the file that must win sorts last. Remember that a config directory now also supplies `skills/`, `agents/` and `help/` (4.7). The variable's files were the base and `--config` files layered after them; the config directory keeps that order, since its files load before any `--config`.

**The command-line tools load what the server loads.** The server and the commands that read the config (`validate`, `doctor`, `agents list`, `migrate --config`, `tokens`, `audit`, the memory tools) now build the list of layers with one function. One command does not yet: `loomcycle hash agent --config <file>` still reads that single file, with no presets, config directory, section files or `auth.env`. What you will notice:

- **`loomcycle validate` with no path** validates the server's stack: the presets, the config directory, and the auto-discovered `loomcycle.yaml` if there is one. Before, a path was required and the usage text was printed. Its first line names every layer of the stack, base first.
- **`loomcycle doctor` finds a deployment made of presets and a config directory.** Before, it looked for a single `loomcycle.yaml` and failed its `Config found` check on such a deployment. The check now lists the layers of the stack.
- **Each of these commands loads the `auth.env` beside the last config file**, as the server does, and only for variables the environment does not already set. Before, only the server and `doctor` did. A command run by hand therefore sees the persisted token and settings the server sees. (`hash agent` does not, and the memory tools load it only beside a file you name.)
- **Several files are named the way the server names them.** `--config` may be repeated in `doctor`, `agents list`, `migrate` and the memory tools, and `validate` takes several paths; the files are layered in the order given, as the server layers its own `--config` flags. A server started with `--config base.yaml --config site.yaml` is checked with `loomcycle validate base.yaml site.yaml` and `loomcycle doctor --config base.yaml --config site.yaml`. `validate` and `doctor` print every layer they loaded, base first.
- **`loomcycle migrate --config` targets the database the server would open, which may differ from before.** The DSN is whatever the whole stack gives: a later `--config`, a section file's `storage:` block, a file in the config directory, `LOOMCYCLE_PG_DSN` in the environment or in the `auth.env` beside the last file. Before it acts, each of `migrate up`, `down` and `status` prints two lines on standard error, `migrate: config layers: …` and `migrate: Postgres DSN taken from: …` (a yaml layer's file name, `the environment (LOOMCYCLE_PG_DSN)`, `auth.env (<path>)` or `--dsn`). Neither line contains the DSN. Read them before you confirm a `migrate down --yes`.
- **`agents list` says which layers it loaded.** It reports what the server would run, so it keeps the server's auto-discovery, and prints one line on standard error, `agents list: config layers: …`. Standard output carries the listing only, so a script that reads the JSON is unaffected.
- **The memory measurement tools load only what you select.** `memory-calibrate`, `memory-eval-live` and `memory-eval-judge` layer the embedded presets, the config directory and the files named with `--config`. They do not load an auto-discovered `loomcycle.yaml` or the `auth.env` beside it: the eval tools score the shipped prompts, and a personal config must not replace the agent or supply keys unasked. Each prints the layers it loaded on standard error. If you ran `memory-calibrate` in a directory with a `loomcycle.yaml` and no `--config`, name the file now: `loomcycle memory-calibrate --config loomcycle.yaml`.
- **The offline tools still run when the environment is one the server refuses.** `tokens rewrap`, `audit verify --tenant`, `--operator` and `--all`, and `migrate up`, `down` and `status --config` are what you reach for when the server will not start. For those commands only, a removed variable that the server refuses, and a bad value in one of the strict variables of [section 8](#an-environment-variable-with-a-bad-value-now-stops-the-start), is printed on standard error as a `loomcycle: warning: …` line and the command goes on; it reads the variable as unset, with two exceptions that are read as off. A bad `LOOMCYCLE_DEADLINK_GC` is held at `off`: none of these commands runs that sweep. A bad `LOOMCYCLE_PG_AUTOMIGRATE` is read as `0`, not as unset: `tokens rewrap` and `audit verify` open the store with that setting, and unset would let a yaml `pg_automigrate: true` migrate the schema under a line you may have written to prevent it. With it read as `0` they do not migrate, and stop if the database is behind this binary's migrations; run `loomcycle migrate up` first. `migrate` prints the same warning and does what its verb says: it does not read the setting, which governs migration at boot only. The server, `validate`, `doctor` and `agents list` keep failing on the same environment, so the warning is not a sign that the deployment is fine. Nothing about the store is relaxed: `LOOMCYCLE_STORAGE_BACKEND` naming another backend, and a Postgres DSN beside an existing SQLite store with no `storage.backend`, stop these commands as they stop the server.
- **A file named explicitly must exist.** A path that does not exist is an error in every command, as at boot. Before, the command-line tools skipped a named file that was missing when presets or a config directory supplied other layers.
- `agents list`, `validate` and `doctor` therefore agree with the running server on which agents exist and which provider each resolves to, provided they run with the server's environment.

### 5.14 Sandbox builder sidecar

The sandbox builder sidecar is a separate program configured by `SANDBOX_*` variables. Ten of them are removed and their values are fixed.

| Removed variable | Fixed value |
|---|---|
| `SANDBOX_CONTAINER_USER` | Sessions run as `1000:1000`, the reference image's user. |
| `SANDBOX_MAX_ENV_VARS` | At most 32 injected environment variables. |
| `SANDBOX_MAX_ENV_VALUE_BYTES` | 64 KiB per injected value. |
| `SANDBOX_DEFAULT_TMPFS_MB` | Workspace tmpfs of 512 MB by default. |
| `SANDBOX_MAX_TMPFS_MB` | Workspace tmpfs of 2048 MB at most. |
| `SANDBOX_MAX_PIDS` | 512 PIDs per session. |
| `SANDBOX_MAX_OUTPUT_BYTES` | 1 MiB of output per exec. |
| `SANDBOX_SESSION_IDLE_TTL` | A session is reaped after 15 minutes idle. |
| `SANDBOX_SESSION_MAX_TTL` | A session is reaped after 1 hour in all. |
| `SANDBOX_GC_INTERVAL` | Sessions are checked every minute. |

The sidecar has no place to print an advisory that an operator would see. It therefore **refuses to start** while any of these ten is set to a non-empty value, whatever the value. The error names each one:

```
SANDBOX_MAX_PIDS, SANDBOX_SESSION_IDLE_TTL: no longer read, the value is fixed (see "Fixed, with no variable" in deploy/builder/README.md); remove each to start
```

Remove the lines. If you had lowered one of these limits, the fixed value is looser than what you ran; decide whether that is acceptable before you start.

These remain: `SANDBOX_LISTEN_ADDR`, `SANDBOX_AUTH_TOKEN`, `SANDBOX_ALLOW_ANON`, `SANDBOX_PODMAN_BIN`, `SANDBOX_IMAGE`, `SANDBOX_RUNTIME`, `SANDBOX_WORKSPACE_ROOT`, `SANDBOX_ALLOW_EGRESS`, `SANDBOX_EXPOSE_NETWORK`, `SANDBOX_ALLOW_ENV_INJECTION`, `SANDBOX_MAX_CPUS`, `SANDBOX_MAX_MEM_MB`, `SANDBOX_MAX_SESSIONS`.

### 5.15 Sign-in mode

`auth.mode` in the yaml and `LOOMCYCLE_AUTH_MODE` in the environment no longer conflict. The effective mode is the stricter of the two (`strict` over `token`; `token` when neither is set), so neither plane lowers the other. Before, a yaml value below the environment value stopped the load. There is nothing to migrate; if you relied on that error to catch a config file that lowers the mode, know that the file is now overruled instead of refused.

When both are set and differ, an advisory says which one decided. It is printed wherever advisories are: the boot log, `validate`, `doctor` and the result of a config reload.

```
auth.mode (token) and LOOMCYCLE_AUTH_MODE (strict) differ: the stricter of the two holds, so the sign-in mode is strict (decided by LOOMCYCLE_AUTH_MODE)
```

The boot log also carries the line `auth: auth.mode and LOOMCYCLE_AUTH_MODE differ; the stricter of the two holds mode=strict decided_by=LOOMCYCLE_AUTH_MODE`, and the summary of `loomcycle validate` always prints the mode in force and where it came from:

```
Auth mode        : strict (from LOOMCYCLE_AUTH_MODE)
```

The source is `from auth.mode`, `from LOOMCYCLE_AUTH_MODE` (only the environment asked for this mode) or `default` (neither plane set one).

This is the one setting where the environment does not simply win. A yaml written for `token` that comes up `strict` because of the environment must still satisfy strict mode, which needs at least one gate a person can sign in through in `auth.gates`; without one the load fails. An error raised because the mode is strict names the plane that made it strict, so that you look in the right place:

```
strict mode (from LOOMCYCLE_AUTH_MODE) needs at least one interactive gate in auth.gates (e.g. `paste: {kind: token}`), or nobody can sign in to the Web UI
```

With `auth.mode: strict` in the yaml the same error begins `auth.mode: strict needs …`.

## 6. Profiles

Two complete minimal examples. Secrets are shown as names with empty values: fill them in your secret store or secret environment file, never in the yaml.

The embedded presets `personal` and `server` are the recommended starting point; each replaces a preset list with one name. `personal` layers `base`, `local`, `chat` and `document-agent` and sets nothing itself: SQLite, SQL Memory on it and the loopback listener are already the defaults. `server` layers `base`, `chat`, `document-agent` and `memory` and turns on the metrics sampler (`metrics: {enabled: true, collect_system: true}`). Neither sets a volume, a sign-in gate, retention, a listen address or a secret. No preset can turn on a switch that exists only in the environment (section 7, table F).

Select one in your primary config file:

```yaml
presets: [personal]
```

or in the environment with `LOOMCYCLE_PRESETS=personal`, or with `--preset`. The order is `--preset`, then `LOOMCYCLE_PRESETS`, then the yaml key: when a flag or the variable is set, the file's list is not layered, and if the two would give different stacks an advisory at boot, in `loomcycle validate` and in `loomcycle doctor` names both. `presets:` is read from one file only: the last `--config`, else the auto-discovered `loomcycle.yaml`, else `loomcycle.yaml` in `LOOMCYCLE_CONFIG_DIR`. In any other config file it stops the load and names the file. A preset named in the file is trusted exactly as the file is: a preset is yaml, and can carry agents, MCP servers or sign-in clients.

A preset lists the ones it stands for under `include:`. They are layered first, in the order written, each once, and the preset's own keys last. A unit you also select by name before a preset that includes it is layered where the preset places it, so `local,personal` is the same stack as `personal`, and an advisory says so. `include:` works only inside a preset built into the binary. `loomcycle presets` lists what your binary carries and `loomcycle presets show <name>` prints one with the stack it expands to.

[`examples/profiles/`](../examples/profiles/) has a complete, minimal configuration for each profile (the server one with a compose file and Postgres). `loomcycle init` writes the personal profile's yaml and `loomcycle init --profile server` the server's; `loomcycle init --full` writes the long reference config, which names no preset.

### 6.1 Personal: SQLite, local or cloud models

Non-secret environment:

```sh
LOOMCYCLE_LISTEN_ADDR=127.0.0.1:8787
LOOMCYCLE_DATA_DIR=./data
OLLAMA_BASE_URL=http://localhost:11434
```

Secret environment (leave out what you do not use):

```sh
LOOMCYCLE_AUTH_TOKEN=
ANTHROPIC_API_KEY=
OPENAI_API_KEY=
```

Yaml:

```yaml
presets: [personal]

# The store is SQLite because no Postgres connection string is set.
# SQL Memory, and with it Documents, is on because the store is SQLite.

volumes:
  work:
    path: /home/me/loomcycle-work   # must already exist
    mode: rw
    default: true

providers:
  ollama-local:
    options:
      num_ctx: 32768
      # num_gpu: 99                 # only where Ollama falls back to the CPU

concurrency:
  max_concurrent_runs: 4
  max_queue_depth: 16
```

What you no longer set: `LOOMCYCLE_STORAGE_BACKEND`, `LOOMCYCLE_SQLMEM_ENABLED`, `LOOMCYCLE_SCHEDULER_ENABLED`, `LOOMCYCLE_WEBHOOKS_ENABLED`, `LOOMCYCLE_CODE_AGENTS_ENABLED`, `LOOMCYCLE_BASHBOX_ENABLED`.

### 6.2 Server: Postgres, multi-tenant

Non-secret environment:

```sh
# Process and host identity
LOOMCYCLE_LISTEN_ADDR=0.0.0.0:8787
LOOMCYCLE_PUBLIC_URL=https://loomcycle.example.com
LOOMCYCLE_DATA_DIR=/data
LOOMCYCLE_CONFIG_DIR=/config
LOOMCYCLE_PG_AUTOMIGRATE=1

# Security gates
LOOMCYCLE_ACL=enforce

# Tracing, by the standard variables
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector.example.com:4318
OTEL_SERVICE_NAME=loomcycle
OTEL_TRACES_SAMPLER=parentbased_traceidratio
OTEL_TRACES_SAMPLER_ARG=0.1
LOOMCYCLE_ENVIRONMENT=production

# Prometheus scrape listener
LOOMCYCLE_METRICS_LISTEN=127.0.0.1:9464
```

Secret environment:

```sh
LOOMCYCLE_PG_DSN=
LOOMCYCLE_SQLMEM_PG_DSN=
LOOMCYCLE_SECRET_KEY=
LOOMCYCLE_OPERATOR_TOKEN_PEPPER=
LOOMCYCLE_AUTH_TOKEN=
LOOMCYCLE_TOKEN_OPS=
LOOMCYCLE_TOKEN_ACME=
ANTHROPIC_API_KEY=
OPENAI_API_KEY=
```

Yaml (`/config/loomcycle.yaml`):

```yaml
presets: [server]

storage:
  backend: postgres            # pinned; the connection strings stay in the environment

concurrency:
  max_concurrent_runs: 32
  max_queue_depth: 128
  max_concurrent_runs_per_user: 4
  max_queued_runs_per_user: 16

auth:
  mode: token                  # or strict, with sign-in gates under auth.gates

principals:
  ops-admin:
    subject: ops
    scopes: [substrate:admin]
    token_env: LOOMCYCLE_TOKEN_OPS
  acme-operator:
    tenant: acme
    subject: operator
    scopes: [substrate:tenant]
    token_env: LOOMCYCLE_TOKEN_ACME

# metrics: the `server` preset turns the sampler on (enabled, collect_system).

retention:
  enabled: true
  interval: 24h
  export_dir: /data/retention-exports
  chats:
    mode: export+prune
    max_age: 90d
  defs:
    mode: prune
    max_age: 30d

observability:
  tracing:
    tenant_sampler_ratios:
      acme: 1

volumes:
  work:
    path: /mnt/work            # must already exist
    mode: rw
    default: true
```

Notes:

- The SQL Memory connection string names a separate database from the main store. Its Postgres role needs `CREATEROLE`.
- More than one principal requires `LOOMCYCLE_SECRET_KEY`.
- Tracing headers go in `OTEL_EXPORTER_OTLP_HEADERS` or in a file named by `LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE`.

### 6.3 Containers and compose

- Keep the same split: secrets and host identity in `environment:` or an `env_file`, everything else in a yaml file on a mounted config directory named by `LOOMCYCLE_CONFIG_DIR`.
- For a secret the server cannot run without, use the required form so that a missing value stops `docker compose up` with a message instead of starting a half-configured server:

  ```yaml
  environment:
    LOOMCYCLE_SECRET_KEY: ${LOOMCYCLE_SECRET_KEY:?set LOOMCYCLE_SECRET_KEY in the secret env file}
    LOOMCYCLE_PG_DSN: ${LOOMCYCLE_PG_DSN:?set LOOMCYCLE_PG_DSN in the secret env file}
  ```

- Remove the removed variables from the compose file itself, not only from the env files. A line such as `LOOMCYCLE_SCHEDULER_ENABLED: "1"` produces an advisory on every start; one that passes an empty value for a removed gate (`LOOMCYCLE_BASHBOX_ENABLED: ""`) is refused.
- A typical server deployment before this release set, in its environment, the backend, the SQL Memory switch, the scheduler, webhook, code-agent and Bashbox switches, the metrics sampler, the retention block and the login origin. After it, the environment keeps the connection strings, keys, addresses and gates, and the yaml gains a `metrics:` block, a `retention:` block and `auth.login_origins`.

### 6.4 The TrueNAS app

- The install wizard's fields are generated from the environment variables the runtime reads. The fields for every removed variable are gone: among them the storage backend, the scheduler, webhook, code-agent and Bashbox switches, the metrics sampler switches, the provider stream timeouts, the timeout-scaling values, the embedder batch size and timeout, the projection interval, the SQL Memory limits, the per-route audio queue sizes (replaced by one `LOOMCYCLE_AUDIO_MAX_INFLIGHT` field), the hosted Ollama context window and the local GPU offload.
- The SQL Memory toggle remains. It now works by passing the SQL Memory connection string only when the toggle is on.
- Put what the wizard no longer offers into the config overlay (the `loomcycle.yaml` on the config dataset, selected by `LOOMCYCLE_CONFIG_DIR`), next to your `volumes:` block:

  ```yaml
  metrics:
    enabled: true
    collect_system: true

  providers:
    ollama-local:
      options:
        num_gpu: 99              # delete to let Ollama decide
  ```

  Add a `retention:` block, `memory.projection`, provider timeouts or `storage: sqlmem_*` keys there if you used the corresponding fields.
- **Existing installs.** The compose and the wizard no longer pass `LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU`, `LOOMCYCLE_METRICS_ENABLED` or `LOOMCYCLE_METRICS_COLLECT_SYSTEM`. An overlay written for an older release has neither setting, so add the two blocks above to it **before** you start the new image. If you refresh the compose and keep the old overlay, the app starts with the sampler off and no forced GPU offload, and logs nothing about it. If you keep the old compose, the app does not start, and the boot log names the three variables and the yaml key each needs.
- The local context window stays `LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX` in the compose or the wizard.
- If you run the paste compose, or an older version of the catalog app, it may still pass removed variables. Update the compose from the current `deploy/truenas/docker-compose.yaml`, or update the app, before you move to the new image.

## 7. Reference tables

Every variable the runtime removed is in exactly one of tables A to E. The last column says when a variable that is still set stops the boot; in every other case it produces an advisory.

Shorthand used in the last column:

- **Never**: the variable is only advised, whatever its value.
- **Lower**: the variable is refused when it is set to a positive whole number below the value that now applies, or to a value that is not a whole number. Zero, a negative number and an empty value are only advised.
- **Moved**: the rule of [section 3](#the-rule-for-a-setting-that-moved-to-the-yaml). The variable is refused when it is set to a value other than the default while the yaml does not name the key in the second column. The cell says which values are other than the default, and under which condition the variable is judged at all. A value outside those, and any value once the yaml names the key, is only advised.

### A. Variables that moved to the yaml

SQL Memory limits (`storage:`). "Lower" compares with the yaml value, or with the default when the key is absent.

| Variable | Yaml key | Format and default | Stops the boot when |
|---|---|---|---|
| `LOOMCYCLE_SQLMEM_STATEMENT_TIMEOUT_MS` | `storage.sqlmem_statement_timeout_ms` | Milliseconds; `30000` | Lower |
| `LOOMCYCLE_SQLMEM_MAX_ROWS` | `storage.sqlmem_max_rows` | Rows; `10000` | Lower |
| `LOOMCYCLE_SQLMEM_MAX_RESULT_BYTES` | `storage.sqlmem_max_result_bytes` | Bytes; 8 MiB | Lower |
| `LOOMCYCLE_SQLMEM_TXN_TIMEOUT_MS` | `storage.sqlmem_txn_timeout_ms` | Milliseconds; `30000` | Lower |
| `LOOMCYCLE_SQLMEM_MAX_OPEN_TXNS` | `storage.sqlmem_max_open_txns` | Count; `64` | Lower |
| `LOOMCYCLE_SQLMEM_MAX_OPEN_TXNS_PER_TENANT` | `storage.sqlmem_max_open_txns_per_tenant` | Count; a quarter of `sqlmem_max_open_txns`, at least 1 | Lower |
| `LOOMCYCLE_SQLMEM_MAX_TXN_DEPTH` | `storage.sqlmem_max_txn_depth` | Savepoint depth; `16` | Lower |
| `LOOMCYCLE_SQLMEM_GC_INTERVAL_MS` | `storage.sqlmem_gc_interval_ms` | Milliseconds; one hour | Never |
| `LOOMCYCLE_SQLMEM_SNAPSHOT_MAX_SCOPE_BYTES` | `storage.sqlmem_snapshot_max_scope_bytes` | Bytes; `0` = no cap | Set to a positive number while the yaml key is absent or larger, or to a value that is not a whole number |

Retention (`retention:`). Ages are durations such as `30d`, not milliseconds. An age written as an empty string, or as a day count that is not a number, stops the load and names the key.

| Variable | Yaml key | Format and default | Stops the boot when |
|---|---|---|---|
| `LOOMCYCLE_RETENTION_ENABLED` | `retention.enabled` | `true` or `false`; `false` | Set to `1` while the yaml key is not `true`, or set to anything else (the empty value included) while the yaml key is `true` |
| `LOOMCYCLE_RETENTION_INTERVAL_MS` | `retention.interval` | Duration; `1h` | Moved: a positive number other than `3600000` (one hour). Judged only with `LOOMCYCLE_RETENTION_ENABLED=1` in the environment. |
| `LOOMCYCLE_RETENTION_EXPORT_DIR` | `retention.export_dir` | Directory path; none | Moved: any non-empty path. Judged only with `LOOMCYCLE_RETENTION_ENABLED=1` in the environment. |
| `LOOMCYCLE_RETENTION_DEFS_MODE` | `retention.defs.mode` | `off`, `prune`, `export+prune`; `off` | Moved: `prune` or `export+prune` (any other value was off). Judged only with `LOOMCYCLE_RETENTION_ENABLED=1` in the environment. |
| `LOOMCYCLE_RETENTION_DEFS_MAX_AGE_MS` | `retention.defs.max_age` | Duration; required when the mode deletes | Moved: a positive number of milliseconds. Judged only with `LOOMCYCLE_RETENTION_ENABLED=1` in the environment. |
| `LOOMCYCLE_RETENTION_DEFS_KEEP_LAST_N` | `retention.defs.keep_last_n` | Count; `5`; `0` keeps none | Moved: zero or a positive number other than `5`. Judged only with `LOOMCYCLE_RETENTION_ENABLED=1` in the environment. |
| `LOOMCYCLE_RETENTION_CHATS_MODE` | `retention.chats.mode` | As the defs mode | Moved: `prune` or `export+prune`. Judged only with `LOOMCYCLE_RETENTION_ENABLED=1` in the environment. |
| `LOOMCYCLE_RETENTION_CHATS_MAX_AGE_MS` | `retention.chats.max_age` | Duration; required when the mode deletes | Moved: a positive number of milliseconds. Judged only with `LOOMCYCLE_RETENTION_ENABLED=1` in the environment. |
| `LOOMCYCLE_RETENTION_MEM_MODE` | `retention.mem.mode` | As the defs mode | Moved: `prune` or `export+prune`. Judged only with `LOOMCYCLE_RETENTION_ENABLED=1` in the environment. |
| `LOOMCYCLE_RETENTION_MEM_MAX_AGE_MS` | `retention.mem.max_age` | Duration; required when the mode deletes | Moved: a positive number of milliseconds. Judged only with `LOOMCYCLE_RETENTION_ENABLED=1` in the environment. |
| `LOOMCYCLE_RETENTION_MEM_CONTENT_MODE` | `retention.mem_content.mode` | `off` or `prune`; `off`. `export+prune` stops the load: this family has no export | Moved: `prune` or `export+prune` (which did what `prune` does; write `prune`). Judged only with `LOOMCYCLE_RETENTION_ENABLED=1` in the environment. |
| `LOOMCYCLE_RETENTION_MEM_CONTENT_MAX_AGE_MS` | `retention.mem_content.max_age` | Duration; required when the mode deletes | Moved: a positive number of milliseconds. Judged only with `LOOMCYCLE_RETENTION_ENABLED=1` in the environment. |
| `LOOMCYCLE_RETENTION_EMPTY_DOSSIER_MODE` | `retention.empty_dossier.mode` | As the defs mode | Moved: `prune` or `export+prune`. Judged only with `LOOMCYCLE_RETENTION_ENABLED=1` in the environment. |
| `LOOMCYCLE_RETENTION_EMPTY_DOSSIER_MAX_AGE_MS` | `retention.empty_dossier.max_age` | Duration; required when the mode deletes | Moved: a positive number of milliseconds. Judged only with `LOOMCYCLE_RETENTION_ENABLED=1` in the environment. |
| `LOOMCYCLE_USAGE_DETAIL_RETENTION_MS` | `retention.usage_detail.max_age` | Duration; `30d` | Moved: a positive number other than `2592000000` (30 days). |
| `LOOMCYCLE_MEMORY_TRACE_MAX_AGE_MS` | `retention.trace.max_age` | Duration; `30d` | Moved: a positive number other than `2592000000` (30 days). Judged only while `LOOMCYCLE_MEMORY_TRACE_INDEX` is `1` or `true`. |
| `LOOMCYCLE_USAGE_RUN_RETENTION_MODE` | `retention.chats.mode` (the usage archiver is gone) | As the defs mode | Set to `prune` or `export+prune` while `LOOMCYCLE_USAGE_RUN_RETENTION_MS` is a positive number and the yaml sets no `retention.chats.mode` |
| `LOOMCYCLE_USAGE_RUN_RETENTION_MS` | `retention.chats.max_age`, with `retention.chats.mode` | Duration | Never by itself; it is part of the rule above |
| `LOOMCYCLE_USAGE_EXPORT_DIR` | `retention.export_dir` | Directory path | Never |

Provider options (`providers.<id>.options`).

| Variable | Yaml key | Format and default | Stops the boot when |
|---|---|---|---|
| `LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS` | `providers.<id>.options.header_timeout_ms`, on each provider | Milliseconds; `60000` | Moved, per provider: a positive number other than `60000`, while a provider this deployment can use has no such option (a positive number). The refusal lists those providers; [5.3](#53-provider-options) says which providers count. |
| `LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS` | `providers.<id>.options.idle_timeout_ms`, on each provider | Milliseconds; `90000` | Moved, per provider: a positive number other than `90000`, while a provider this deployment can use has no such option (a positive number). |
| `LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS` | `providers.ollama-local.options.header_timeout_ms` | Milliseconds; `600000` | Moved: a positive number other than `600000`. The option counts when it is a positive number. Judged only while the config declares `ollama-local` and `OLLAMA_BASE_URL` is not `disabled`. |
| `LOOMCYCLE_OLLAMA_LOCAL_IDLE_TIMEOUT_MS` | `providers.ollama-local.options.idle_timeout_ms` | Milliseconds; `600000` | Moved: a positive number other than `600000`. The option counts when it is a positive number. Judged only while the config declares `ollama-local` and `OLLAMA_BASE_URL` is not `disabled`. |
| `LOOMCYCLE_OLLAMA_NUM_CTX` | `providers.ollama.options.num_ctx` | Tokens; unset = the model's own window | Set to a positive number while the yaml does not name the option |
| `LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU` | `providers.ollama-local.options.num_gpu` | Layers; unset = Ollama decides | As above |
| `LOOMCYCLE_CODE_AGENTS_DETERMINISTIC` | `providers.code-js.options.deterministic` | `true` or `false`; `false` | Moved: `1`. Judged only while the config declares a `code-js` provider. |
| `LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS` | `providers.code-js.options.run_timeout_seconds` | Seconds; `120` | Moved: a positive number other than `120`. The option counts when it is a positive number. Judged only while the config declares a `code-js` provider. |
| `LOOMCYCLE_CODE_AGENTS_MAX_WALL_SECONDS` | `providers.code-js.options.max_wall_seconds` | Seconds; `86400` | Moved: a positive number other than `86400`. The option counts when it is a positive number. Judged only while the config declares a `code-js` provider. |

Timeout scaling, memory, concurrency, observability, sign-in, hooks, metrics.

| Variable | Yaml key | Format and default | Stops the boot when |
|---|---|---|---|
| `LOOMCYCLE_TIMEOUT_SCALING` | `timeout_scaling.mode` | `measure` or `off`; `measure` | Moved: any non-empty value other than `measure`, in any letter case (`off`, or a mode that does not exist). |
| `LOOMCYCLE_TIMEOUT_SCALING_REFERENCE_TPS` | `timeout_scaling.reference.decode_tps` | Tokens per second; `100` | Moved: a positive number other than `100`. |
| `LOOMCYCLE_TIMEOUT_SCALING_MAX_MULTIPLIER` | `timeout_scaling.max_multiplier` | Number; `8` | Moved: a positive number other than `8`. |
| `LOOMCYCLE_MEMORY_EMBED_BATCH_SIZE` | `memory.embedder.batch_size` | Texts per call; `100`; `1` sends one text per call | Moved: any whole number other than `100`, zero and negative numbers included (they meant one text per call). |
| `LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS` | `memory.embedder.timeout_ms` | Milliseconds; `30000` | Moved: any whole number other than `30000`, zero and negative numbers included (they meant no timeout). |
| `LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL` | `memory.projection.interval`, with `memory.projection.enabled: true` | Duration; unset or `0` = no sweeper | Moved: a positive duration, or a value that is not a duration. Both keys must be named: the interval alone would be a cadence for a projection left off. |
| `LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER` | `concurrency.max_concurrent_runs_per_user` | Count; `0` = no per-user cap | Moved: a positive number. |
| `LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER` | `concurrency.max_queued_runs_per_user` | Count; `16`; `0` refuses at once over the cap | Moved: zero or a positive number other than `16`. |
| `LOOMCYCLE_PROVIDER_QUEUE_DEPTH` | `concurrency.provider_queue_depth` | Count; `0` = `max_queue_depth` | Moved: a positive number other than `concurrency.max_queue_depth` (what an absent key inherits). |
| `LOOMCYCLE_OTEL_TENANT_SAMPLER_RATIOS` | `observability.tracing.tenant_sampler_ratios` | Map of tenant to a ratio from 0 to 1; none | Moved: at least one entry. |
| `LOOMCYCLE_TENANT_TELEMETRY_ALLOWED_HOSTS` | `observability.tenant_telemetry.allowed_hosts` | List of hosts, each exact or a quoted `"*.suffix"`; none = any public host | Moved: at least one host. Judged only while `LOOMCYCLE_TENANT_TELEMETRY` is on. |
| `LOOMCYCLE_UI_LOGIN_ORIGINS` | `auth.login_origins` | List, one origin per item; none = the receiver is off | Moved: at least one origin. |
| `LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS` | `hooks.permit_host_widen.owners` | List of `[tenant:]name`; empty | It names an entry the yaml list lacks |
| `LOOMCYCLE_HOOKS_PRIVATE_HOST_ALLOWLIST` | `hooks.private_host_allowlist` | List of hosts and CIDR ranges; empty | It names an entry the yaml list lacks |
| `LOOMCYCLE_METRICS_ENABLED` | `metrics.enabled` | `true` or `false`; `false` | Moved: `1`. |
| `LOOMCYCLE_METRICS_COLLECT_SYSTEM` | `metrics.collect_system` | `true` or `false`; `false` | Moved: `1`. Judged only with `LOOMCYCLE_METRICS_ENABLED=1` in the environment. |
| `LOOMCYCLE_METRICS_SAMPLE_INTERVAL_MS` | `metrics.sample_interval` | Duration; `5s`; at least `1s` | Moved: a positive number other than `5000` (a number under `1000` counted as `1000`). Judged only with `LOOMCYCLE_METRICS_ENABLED=1` in the environment. |

Config file locations.

| Variable | Yaml key | Format and default | Stops the boot when |
|---|---|---|---|
| `LOOMCYCLE_CONFIG_FILES` | None: pass each file as `--config`, or place it in `LOOMCYCLE_CONFIG_DIR` | — | Set and non-empty |

### B. Variables that became fixed values

Each fixed value is the default the variable had, so a deployment that never set the variable sees no change. "Lower" compares with the fixed value.

Definition and evaluation sizes.

| Variable | Fixed value | Stops the boot when |
|---|---|---|
| `LOOMCYCLE_AGENT_DEF_MAX_DEFINITION_BYTES` | An agent definition is capped at 128 KiB. | Lower |
| `LOOMCYCLE_AGENT_DEF_MAX_DESCRIPTION_BYTES` | An agent description is capped at 8 KiB. | Lower |
| `LOOMCYCLE_AGENT_DEF_MAX_CODE_BYTES` | An inline code body is capped at 256 KiB. | Lower |
| `LOOMCYCLE_TEAM_DEF_MAX_DEFINITION_BYTES` | A team definition is capped at 1 MiB. | Lower |
| `LOOMCYCLE_SKILL_DEF_MAX_BODY_BYTES` | A skill body is capped at 128 KiB. | Lower |
| `LOOMCYCLE_SKILL_DEF_MAX_DESCRIPTION_BYTES` | A skill description is capped at 8 KiB. | Lower |
| `LOOMCYCLE_EVALUATION_MAX_JUDGEMENT_BYTES` | An evaluation judgement is capped at 32 KiB. | Lower |
| `LOOMCYCLE_EVALUATION_MAX_RATIONALE_BYTES` | An evaluation rationale is capped at 8 KiB. | Lower |

Tool and data caps.

| Variable | Fixed value | Stops the boot when |
|---|---|---|
| `LOOMCYCLE_GIT_SOURCE_MAX_BYTES` | A clone's byte limit is 256 MiB. | Lower |
| `LOOMCYCLE_GIT_SOURCE_MAX_OBJECT_BYTES` | A clone's object limit is 64 MiB. | Lower |
| `LOOMCYCLE_GIT_SOURCE_MAX_FILE_BYTES` | A clone's per-file limit is 5 MiB. | Lower |
| `LOOMCYCLE_GIT_SOURCE_MAX_FILES` | A clone's file-count limit is 100000. | Lower |
| `LOOMCYCLE_GIT_SOURCE_TIMEOUT_SECONDS` | A clone's time limit is 120 s. | Lower |
| `LOOMCYCLE_GIT_SOURCE_MAX_HISTORY` | A clone's history limit is 500 commits. | Lower |
| `LOOMCYCLE_GIT_SOURCE_MAX_CONCURRENT` | One clone runs at a time per replica. | Never |
| `LOOMCYCLE_GIT_PUSH_MAX_BYTES` | One Git push may send at most 100 MiB. | Lower |
| `LOOMCYCLE_FILE_BASE64_MIN_BYTES` | A base64 string of 4 KiB or more is captured as a file. | Never |
| `LOOMCYCLE_CRYPTO_MAX_BYTES` | A Crypto data argument is capped at 1 MiB. | Lower |
| `LOOMCYCLE_CRYPTO_MAX_OPS_PER_RUN` | A run may make 100 Crypto calls. | Lower |
| `LOOMCYCLE_MAX_DOCUMENT_ASSET_BYTES` | A document image and the `/v1/_document` body are capped at 8 MiB. | Lower |
| `LOOMCYCLE_DOCUMENT_SEARCH_FILTER_MAX_IDS` | A filtered document search binds at most 50000 chunk ids. | Lower |
| `LOOMCYCLE_DOCUMENT_FEED_MAX_STREAMS` | A principal holds at most 4 document feed streams. | Lower |
| `LOOMCYCLE_DOCUMENT_EVIDENCE_MAX_PER_EDGE` | A knowledge-base relation keeps at most 32 supporting quotes. | Never |
| `LOOMCYCLE_MEMORY_MAX_VALUE_BYTES` | A Memory value is capped at 64 KiB. | Lower |
| `LOOMCYCLE_CHANNELS_MAX_VALUE_BYTES` | A channel message is capped at 64 KiB. | Lower |
| `LOOMCYCLE_CHANNELS_MAX_PENDING_DEFERRED` | At most 10000 deferred publishes hold a timer. | Lower |
| `LOOMCYCLE_CHANNEL_HOOKS_CONCURRENCY` | A replica's hook worker decides 16 messages at once. | Never |
| `LOOMCYCLE_CHANNEL_HOOKS_PER_CHANNEL` | A replica's hook worker decides 4 messages of one channel at once. | Never |
| `LOOMCYCLE_CHANNEL_HOOKS_MAX_WAIT` | A message waits 15 minutes on fail-closed hooks before it is dropped. | Never |
| `LOOMCYCLE_TOOL_PARALLELISM` | 8 tool calls of one turn run at once. | Never |
| `LOOMCYCLE_TENANT_TELEMETRY_QUEUE` | A tenant telemetry pipeline queues 512 spans. | Never |
| `LOOMCYCLE_TENANT_TELEMETRY_MAX_PIPELINES` | 256 tenant telemetry pipelines are open at once. | Never |

Agents, client tools, pause and cancel.

| Variable | Fixed value | Stops the boot when |
|---|---|---|
| `LOOMCYCLE_DYNAMIC_AGENT_DEFAULT_TTL_SECONDS` | A registered agent lives 24 h unless its request names `ttl_seconds`. | Set to any number below 86400 (zero and negative numbers included), or to a value that is not a whole number |
| `LOOMCYCLE_INTERACTIVE_CHILD_IDLE_TTL_MS` | A resident child is reaped after 30 minutes idle; an open's `idle_ttl_seconds` overrides it per child. | Lower |
| `LOOMCYCLE_RESIDENT_MAX_TURN_SECONDS` | A resident child's turn is cut at 2 h. | Lower |
| `LOOMCYCLE_AGENT_CHILD_MAX_TIMEOUT_MS` | A spawn's `timeout_ms` has no ceiling. | Set to any positive number (every ceiling was tighter than none), or to a value that is not a whole number |
| `LOOMCYCLE_AGENT_POLL_WAIT_CAP_MS` | One Agent poll blocks at most 60 s. | Lower |
| `LOOMCYCLE_CLIENT_TOOL_TIMEOUT_MS` | A client-tool call blocks at most 60 s. | Lower |
| `LOOMCYCLE_CLIENT_TOOL_MAX_BYTES` | A client-tool frame is capped at 1 MiB. | Lower |
| `LOOMCYCLE_CLIENT_TOOL_MAX_CONNS` | A principal holds at most 8 client-tool connections. | Lower |
| `LOOMCYCLE_CANCEL_ACK_TIMEOUT_MS` | A cross-replica cancel waits 5 s for its acknowledgement. | Never |
| `LOOMCYCLE_PAUSE_CACHE_TTL_MS` | The shared pause state is cached for 1 s. | Never |
| `LOOMCYCLE_PAUSE_DEFAULT_TIMEOUT_MS` | A pause waits 30 s unless the request names `timeout_ms`. | Never |
| `LOOMCYCLE_MCP_MAX_CONCURRENT_CALLS` | The stdio MCP server dispatches 16 tool calls at once. | Never |
| `LOOMCYCLE_MCP_UPSTREAM_HEADER_TIMEOUT_MS` | The thin client waits 60 s for the upstream's response headers. | Never |
| `LOOMCYCLE_MCP_UPSTREAM_RECONNECT_ATTEMPTS` | The thin client retries a transport error 5 times. | Never |

Background cadences and kept-for periods.

| Variable | Fixed value | Stops the boot when |
|---|---|---|
| `LOOMCYCLE_HEARTBEAT_SWEEP_INTERVAL_MS` | The run heartbeat sweep runs every 60 s. | Never |
| `LOOMCYCLE_USAGE_SWEEP_INTERVAL_MS` | The usage rollup runs every hour. | Never |
| `LOOMCYCLE_SESSION_LOCK_GC_INTERVAL_MS` | Session locks are collected every 5 minutes. | Never |
| `LOOMCYCLE_SESSION_LOCK_MAX_IDLE_MS` | A session lock idle for 10 minutes is collected. | Never |
| `LOOMCYCLE_METRICS_SWEEP_INTERVAL_MS` | Old metrics samples are deleted every 15 minutes; `LOOMCYCLE_METRICS_RETENTION_DAYS` sets how many are kept. | Never |
| `LOOMCYCLE_TEAM_SUBSCRIPTIONS_TICK_SECONDS` | Armed team subscriptions are swept every 15 s. | Never |
| `LOOMCYCLE_DYNAMIC_AGENT_SWEEP_INTERVAL_MS` | Expired registered agents are deleted every 15 minutes. | Never |
| `LOOMCYCLE_EPHEMERAL_VOLUME_SWEEP_MS` | Ephemeral volumes of ended runs are purged every 60 s. | Never |
| `LOOMCYCLE_MEMORY_SWEEP_MS` | Expired Memory entries are deleted every 15 minutes. | Never |
| `LOOMCYCLE_CHANNELS_SWEEP_MS` | Expired channel messages are deleted every 15 minutes. | Never |
| `LOOMCYCLE_RESOLVE_PROBE_INTERVAL_MS` | Providers are re-probed every 15 minutes. | Never |
| `LOOMCYCLE_SSE_KEEPALIVE_MS` | A run stream sends a keepalive every 20 s. | Never |
| `LOOMCYCLE_LIMITS_RELOAD_INTERVAL` | Token limit rows are re-read every minute. | Never |
| `LOOMCYCLE_CHANGE_SUB_INTERVAL_MS` | Change subscriptions are delivered every 5 s. | Never |
| `LOOMCYCLE_MEMORY_CHANGES_RETENTION_HOURS` | Change-feed rows are kept 24 h. | Never |
| `LOOMCYCLE_EXPIRED_TOKEN_RETENTION_SECONDS` | An expired token's row is deleted 7 days after it expired. | Never |
| `LOOMCYCLE_RETENTION_CHATS_INTERNAL_MAX_AGE_MS` | Chats of internal agents are deleted after 3 days when chat retention is on. | Set to a positive number other than `259200000` (3 days), to a value that is not a whole number, or to `0` while `retention.chats.max_age` is not `3d` |

### C. Feature switches removed

The feature is always on, or is decided by another setting.

| Variable | What decides now | Stops the boot when |
|---|---|---|
| `LOOMCYCLE_SCHEDULER_ENABLED` | The scheduler is always on. A schedule fires unless its definition is disabled. | Set to anything other than `1`, the empty value included |
| `LOOMCYCLE_WEBHOOKS_ENABLED` | The inbound webhook receiver is always mounted. To keep it unused, keep no webhook definitions. | As above |
| `LOOMCYCLE_CODE_AGENTS_ENABLED` | The `code-js` provider is registered whenever the config declares it; the default providers layer does. | As above |
| `LOOMCYCLE_CODE_HOOKS_ENABLED` | A hook body may always be `code-js`. | As above |
| `LOOMCYCLE_CHANNEL_HOOKS` | The channel-hook worker always runs. To keep it unused, declare no `hooks:` on any channel. | As above |
| `LOOMCYCLE_BASHBOX_ENABLED` | Bashbox is always registered. An agent gets it through its `tools` list. | As above |
| `LOOMCYCLE_RESUME_FANOUT` | A fan-out parent always parks on pause and is resumed with its children. This cannot be turned off. | As above |
| `LOOMCYCLE_HEARTBEAT_SWEEPER` | The run heartbeat sweeper always runs. | Set to `0` |
| `LOOMCYCLE_USAGE_SWEEPER` | The usage rollup always runs; `retention.usage_detail.max_age` sets how long per-call rows are kept. | Set to `0` |
| `LOOMCYCLE_MEMORY_PROVISION_IDENTITY_DOCS` | The tenant-root and user-root documents are always created when a principal is established. | Set to `0`, `false`, `no` or `off` |
| `LOOMCYCLE_A2A_ENABLED` | A2A is on when `LOOMCYCLE_A2A_SERVER_CARD` names the server card to serve. | Set to anything other than `1` while `LOOMCYCLE_A2A_SERVER_CARD` is set |
| `LOOMCYCLE_SQLMEM_ENABLED` | SQL Memory is on with a SQLite store, and with a Postgres store when `LOOMCYCLE_SQLMEM_PG_DSN` is set. `storage.sqlmem_enabled: false` turns it off. | What the variable used to give (on for exactly `1`, otherwise the yaml's answer, off by default) differs from what this version resolves |
| `LOOMCYCLE_STORAGE_BACKEND` | The store is Postgres when `LOOMCYCLE_PG_DSN` or `storage.pg_dsn` is set, and SQLite otherwise. `storage.backend` in the yaml pins it. | Set to a non-empty value that names another backend than the one this version opens |

### D. Variables replaced by another variable

| Variable | Use instead | Stops the boot when |
|---|---|---|
| `LOOMCYCLE_SPEECH_MAX_INFLIGHT` | `LOOMCYCLE_AUDIO_MAX_INFLIGHT`, which sizes the queue of both audio routes (default `2`). | Lower, compared with `LOOMCYCLE_AUDIO_MAX_INFLIGHT` or its default |
| `LOOMCYCLE_TRANSCRIPTION_MAX_INFLIGHT` | `LOOMCYCLE_AUDIO_MAX_INFLIGHT`. | As above |
| `LOOMCYCLE_DEADLINK_GC_MS` | `LOOMCYCLE_DEADLINK_GC` = `off`, `dry-run` or `on` (default `on`). The collection runs every hour; the interval is no longer read. | Set to zero or a negative number unless `LOOMCYCLE_DEADLINK_GC=off`; or set to more than `3600000` unless `LOOMCYCLE_DEADLINK_GC` is `off` or `dry-run` |
| `LOOMCYCLE_DEADLINK_GC_DRY_RUN` | `LOOMCYCLE_DEADLINK_GC=dry-run`. | Set to `1` unless `LOOMCYCLE_DEADLINK_GC` is `dry-run` or `off` |
| `LOOMCYCLE_AUTH_VERBOSE` | `LOOMCYCLE_LOG_LEVEL=debug`: a rejected credential is logged at debug level. | Never |
| `LOOMCYCLE_CHANNEL_DEBUG` | `LOOMCYCLE_LOG_LEVEL=debug`: each channel publish is logged at debug level. | Never |
| `LOOMCYCLE_OLLAMA_DEBUG_THINK` | `LOOMCYCLE_LOG_LEVEL=debug`: each Ollama request's model, effort and think flag are logged at debug level. | Never |
| `LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT` | Retired: use `OTEL_EXPORTER_OTLP_ENDPOINT`, with its scheme (`http://host:4318`). | Never |
| `LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS` | Retired: use `OTEL_EXPORTER_OTLP_HEADERS` or `LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE`. | Never |
| `LOOMCYCLE_OTEL_SERVICE_NAME` | Retired: use `OTEL_SERVICE_NAME`. | Never |
| `LOOMCYCLE_OTEL_TRACES_SAMPLER_RATIO` | Retired: use `OTEL_TRACES_SAMPLER=parentbased_traceidratio` with `OTEL_TRACES_SAMPLER_ARG`. | Never |

The four `LOOMCYCLE_OTEL_*` names were already unread before this release. They are listed here because the runtime now reports them.

### E. Variables that were already dead

These were not read by the previous release either. The runtime now reports them.

| Variable | What to do | Stops the boot when |
|---|---|---|
| `LOOMCYCLE_INTERRUPTION_DEFAULT_TIMEOUT_MS` | Set `interruption.default_timeout_ms` in the yaml. | Never |
| `LOOMCYCLE_INTERRUPTION_MAX_TIMEOUT_MS` | Set `interruption.max_timeout_ms` in the yaml. | Never |
| `LOOMCYCLE_INTERRUPTION_MAX_PENDING_PER_RUN` | Set `interruption.max_pending_per_run` in the yaml. | Never |
| `LOOMCYCLE_INTERRUPTION_HEARTBEAT_INTERVAL_MS` | Set `interruption.heartbeat_interval_ms` in the yaml. | Never |
| `LOOMCYCLE_SNAPSHOT_MAX_BYTES` | Pass `max_bytes` on the snapshot request. | Never |
| `LOOMCYCLE_READ_ROOT` | The filesystem jail is gone: declare a `volumes:` block and bind agents to it. | Set to any non-empty value |
| `LOOMCYCLE_WRITE_ROOT` | As above. | Set to any non-empty value |
| `LOOMCYCLE_BASH_CWD` | As above; Bash runs in the root of the agent's read-write volume. | Set to any non-empty value |

### F. Settings that stay in the environment

Do **not** move these to the yaml. The rule: the environment keeps what is secret, what belongs to this host or process, and what opens something dangerous.

**Secrets.** A secret must not sit in a file that is reviewed, layered or shared.

| Variables | What they are |
|---|---|
| `LOOMCYCLE_AUTH_TOKEN`, `LOOMCYCLE_OPERATOR_TOKEN_PEPPER`, the variables your `principals:` name in `token_env` | Bearers and the token pepper. |
| `LOOMCYCLE_SECRET_KEY`, `LOOMCYCLE_SECRET_KEY_PREVIOUS` | The key that encrypts stored credentials and signs the audit chain. |
| `LOOMCYCLE_PG_DSN`, `LOOMCYCLE_SQLMEM_PG_DSN` | Database connection strings. Their presence also selects the store and turns SQL Memory on. |
| `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `DEEPSEEK_API_KEY`, `GEMINI_API_KEY`, `OLLAMA_API_KEY`, `BRAVE_API_KEY` and other provider or tool keys | Provider and tool keys. |
| `OTEL_EXPORTER_OTLP_HEADERS`, `LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE` | Collector credentials. |
| The variables your webhook and schedule definitions name | Signing secrets and trigger credentials. |

**Process and host identity.** One yaml should serve hosts that listen on different addresses and keep their files in different places.

| Variables | What they are |
|---|---|
| `LOOMCYCLE_LISTEN_ADDR`, `LOOMCYCLE_GRPC_ADDR`, `LOOMCYCLE_METRICS_LISTEN` | Listeners. |
| `LOOMCYCLE_PUBLIC_URL`, `LOOMCYCLE_A2A_PUBLIC_BASE_URL`, `OLLAMA_BASE_URL` | URLs of this deployment and of its local model host. |
| `LOOMCYCLE_DATA_DIR`, `LOOMCYCLE_SQLMEM_ROOT`, `LOOMCYCLE_CONFIG_DIR`, `LOOMCYCLE_SKILLS_ROOT`, `LOOMCYCLE_AGENTS_ROOT`, `LOOMCYCLE_HELP_ROOT`, `LOOMCYCLE_CODE_AGENTS_ROOT`, `LOOMCYCLE_AUDIT_LOG_PATH`, `LOOMCYCLE_WEBUI_DIR`, `LOOMCYCLE_SQLITE_VEC_PATH` | Paths on this host. |
| `LOOMCYCLE_PRESETS`, `LOOMCYCLE_CONFIG_STRICT`, `LOOMCYCLE_NO_DEFAULT_PROVIDERS` | Which config is loaded. This cannot be said inside the config. |
| `LOOMCYCLE_REPLICA_ID`, `LOOMCYCLE_REPLICAS_STALE_AFTER_MS`, `LOOMCYCLE_REPLICAS_SWEEP_INTERVAL_MS`, `LOOMCYCLE_HEARTBEAT_STALE_MS` | Cluster membership of this process. |
| `LOOMCYCLE_PG_AUTOMIGRATE`, `LOOMCYCLE_PG_MAX_OPEN_CONNS`, `LOOMCYCLE_PG_MIN_IDLE_CONNS`, `LOOMCYCLE_PGVECTOR_ENABLED` | How this process uses its database. |
| `LOOMCYCLE_LOG_LEVEL`, `LOOMCYCLE_LOG_FORMAT`, `LOOMCYCLE_ENVIRONMENT`, the standard `OTEL_*` variables | Logging and tracing of this process. |
| `LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX`, `LOOMCYCLE_AUDIO_MAX_INFLIGHT`, `LOOMCYCLE_EXTRACT_CONCURRENCY`, `LOOMCYCLE_BASHBOX_OVERLAY_MAX_BYTES` | Sizes that follow this host's memory and GPU. |

**Security gates.** A gate is off unless you set it, and it has no yaml form, so that no config layer or preset can open it.

| Variables | What they open or restrict |
|---|---|
| `LOOMCYCLE_ALLOW_OPEN_MODE`, `LOOMCYCLE_AUTH_MODE`, `LOOMCYCLE_ACL`, `LOOMCYCLE_UI_INSECURE_COOKIES`, `LOOMCYCLE_OPERATOR_KEY_RESTRICTION`, `LOOMCYCLE_TOKEN_AUTHORITY_INHERIT`, `LOOMCYCLE_LIMITS_ENFORCE` | Authentication and authorization posture. |
| `LOOMCYCLE_BASH_ENABLED`, `LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS`, `LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_ENV`, `LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_CREDS` | Host command execution. |
| `LOOMCYCLE_MCP_ALLOW_PRIVILEGED_TOOLS`, `LOOMCYCLE_MCP_ALLOW_DYNAMIC_STDIO`, `LOOMCYCLE_MCP_ALLOW_PRIVATE_IPS` | What MCP callers and MCP servers may do. |
| `LOOMCYCLE_HTTP_HOST_ALLOWLIST`, `LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST`, `LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST_MODE`, `LOOMCYCLE_HTTP_CALLER_AUTHORITATIVE` | Outbound network policy. |
| `LOOMCYCLE_GIT_SOURCE_HOSTS`, `LOOMCYCLE_GIT_PUSH_HOSTS` | The Git network. Unset keeps it off. |
| `LOOMCYCLE_WEBHOOKS_ALLOW_UNAUTHENTICATED`, `LOOMCYCLE_WEBHOOKS_ENV_ALLOWLIST`, `LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST` | Trigger authentication and credentials. |
| `LOOMCYCLE_A2A_SERVER_CARD`, `LOOMCYCLE_A2A_TENANCY_ROUTING` | The A2A surface. |
| `LOOMCYCLE_TENANT_TELEMETRY`, `LOOMCYCLE_MEMORY_TRACE_INDEX`, `LOOMCYCLE_MEMORY_CHANGES_ENABLED`, `LOOMCYCLE_REDACT_SECRETS`, `LOOMCYCLE_AUDIT_RETENTION_DAYS` | Where data is copied, exported or kept. |
| `LOOMCYCLE_SQLMEM_SCOPE_TTL_MS`, `LOOMCYCLE_SQLMEM_TOTAL_MAX_BYTES`, `LOOMCYCLE_SQLMEM_QUOTA_BYTES`, `LOOMCYCLE_SQLMEM_AUDIT_MODE`, `LOOMCYCLE_SQLMEM_SHARED_SCHEMAS` | SQL Memory data keeping and visibility. |
| `LOOMCYCLE_MOCK_ENABLED`, `LOOMCYCLE_KMS_VAULT_ALLOW_HTTP` | Test and development shortcuts. |

This is not the complete list of variables that are still read. The complete list is the environment variable reference in [`CONFIGURATION.md`](CONFIGURATION.md) and the annotated `.env.insecure.example`. A variable that is not in tables A to E is still read and needs no action.

## 8. Troubleshooting

### Refusal messages you are likely to see

Each refusal entry has the form `NAME: what to do`. The most common ones:

| Entry begins with | Cause | Fix |
|---|---|---|
| `LOOMCYCLE_SCHEDULER_ENABLED: the scheduler is always on; to keep it unused …` (and the same for the other six gates) | The variable is set to something other than `1`, often `0` or an empty value passed by a compose file. | Remove the line. If you meant to keep the feature off, apply the text of the entry: keep no definitions of that kind. |
| `LOOMCYCLE_STORAGE_BACKEND: the store is Postgres when LOOMCYCLE_PG_DSN (or storage.pg_dsn) is set and SQLite otherwise, which is not the backend this variable names …` | The variable says `postgres` with no connection string, or `sqlite` with one. | Set `storage.backend` in the yaml to the backend you run, then remove the variable. |
| `LOOMCYCLE_SQLMEM_ENABLED: SQL Memory follows the store, which is not what this variable asked for …` | You kept SQL Memory off on SQLite, or asked for it on Postgres without the SQL Memory connection string. | To keep it off, set `storage.sqlmem_enabled: false`. To have it on with Postgres, set `LOOMCYCLE_SQLMEM_PG_DSN`. Then remove the variable. |
| `LOOMCYCLE_RETENTION_ENABLED: the retention sweeper runs when retention.enabled is true in the yaml …` | The variable and `retention.enabled` disagree. | Set `retention.enabled` to what you want, then remove the variable. |
| `LOOMCYCLE_RETENTION_CHATS_MODE: set retention.chats.mode in the yaml; the variable was set to something other than the default and the yaml does not name retention.chats.mode …` (and the other retention modes, ages, the kept-version count, the export directory and the interval) | Retention ran (`LOOMCYCLE_RETENTION_ENABLED=1`), the variable is set to something other than its default, and the yaml does not name the key. | Write the `retention:` block ([5.2](#52-retention)), naming each key the refusal lists, then remove the variables. |
| Any other entry that ends `… the variable was set to something other than the default and the yaml does not name <key> (name the key, with the value you want, or remove the variable to take the default)` | A setting that moved to the yaml ([section 3](#the-rule-for-a-setting-that-moved-to-the-yaml)): `LOOMCYCLE_METRICS_ENABLED`, the timeout-scaling, embedder, projection, concurrency, sampler-ratio, telemetry-host and login-origin variables, the `ollama-local` timeouts and the code-agent options. | Write the named key in the yaml with the value you want, or remove the variable to run with the default. |
| `LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS: set providers.<id>.options.header_timeout_ms in the yaml for each provider … the option is missing on these providers: …` (and `LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS`) | The variable is a positive number other than the default, raised or lowered, and at least one provider you can use has no such option. | Set the option on each provider the entry lists ([5.3](#53-provider-options)), or remove the variable and accept the default. A built-in provider you have no key for is not listed and needs nothing. |
| `LOOMCYCLE_A2A_ENABLED: A2A is served whenever LOOMCYCLE_A2A_SERVER_CARD is set, and it is …` | The switch is off while the card is named. | Unset `LOOMCYCLE_A2A_SERVER_CARD` to keep A2A off, or remove the switch to serve it. |
| `LOOMCYCLE_SPEECH_MAX_INFLIGHT: both audio queues are sized by LOOMCYCLE_AUDIO_MAX_INFLIGHT …` | The route was held to fewer requests than the shared size. | Set `LOOMCYCLE_AUDIO_MAX_INFLIGHT` to the size you want, then remove the variable. |
| `LOOMCYCLE_UI_LOGIN_ORIGINS: set auth.login_origins in the yaml, as a list …` | The variable lists an origin and the yaml has no `auth.login_origins`. | Write `auth.login_origins` with one origin per list item ([5.8](#58-login-origins)). |
| `LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS: list each [tenant:]owner under hooks.permit_host_widen.owners in the yaml; the yaml list lacks "<entry>" …` (and `LOOMCYCLE_HOOKS_PRIVATE_HOST_ALLOWLIST: list each host or CIDR range under hooks.private_host_allowlist in the yaml; the yaml list lacks "<entry>" …`) | The variable names an entry the yaml list lacks; the refusal quotes the first one. Entries are compared as written, so a wider yaml range or the same owner under another tenant does not count. | Copy the quoted entry into the yaml list and start again until no entry is named ([5.10](#510-hooks-lists)), then remove the variable. If the yaml already grants what your hooks need (a wider range, for example), just remove the variable. |
| `LOOMCYCLE_OLLAMA_NUM_CTX: requests to hosted Ollama no longer carry this context window …` (and `LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU: requests to local Ollama no longer carry this GPU offload …`) | The variable is a positive number and the yaml does not name the option. | Set `providers.ollama.options.num_ctx` (or `providers.ollama-local.options.num_gpu`) to the same number, then remove the variable. |
| `LOOMCYCLE_DEADLINK_GC_MS: the dead-link collection runs every hour and deletes unreachable chunk references unless LOOMCYCLE_DEADLINK_GC says otherwise …` (and `LOOMCYCLE_DEADLINK_GC_DRY_RUN: …`) | You had the collection off, counting only, or running less often than hourly, and `LOOMCYCLE_DEADLINK_GC` does not say so. | Set `LOOMCYCLE_DEADLINK_GC` to `off` or `dry-run` ([5.11](#511-dead-link-collection)), then remove the old variable. |
| `LOOMCYCLE_USAGE_SWEEPER: the usage rollup always runs …`, `LOOMCYCLE_HEARTBEAT_SWEEPER: the run heartbeat sweeper always runs …`, `LOOMCYCLE_MEMORY_PROVISION_IDENTITY_DOCS: the tenant-root and user-root documents are always created …`, `LOOMCYCLE_RETENTION_CHATS_INTERNAL_MAX_AGE_MS: chats of internal agents are kept 3 days and then deleted …` | The variable switched the work off, or set another age ([4.11](#411-a-limit-you-raised-removed-or-switched-off-returns-to-its-default)). | Nothing brings the old behaviour back. Decide whether the fixed behaviour is acceptable, then remove the variable. |
| `LOOMCYCLE_READ_ROOT: the filesystem jail is gone …` | A deployment that predates volumes. | Declare a `volumes:` block and bind agents to it, then remove the variable. |
| `LOOMCYCLE_CHANNELS_MAX_VALUE_BYTES: a channel message is capped at 64 KiB` (and other fixed caps) | You had lowered the cap below the fixed value, or the value is not a number. | The cap can no longer be lowered. Decide whether the fixed value is acceptable, then remove the variable. |

A refusal for a fixed cap has no setting that brings the old value back. If you lowered a cap for a reason that still holds, enforce it outside loomcycle (a proxy limit, a quota in the calling application) before you remove the line.

### The server started on an empty store

The server opened a different store than before. Stop it before anything is written that you would have to merge.

- **It opened SQLite, and you run Postgres.** The connection string did not reach the process: `LOOMCYCLE_PG_DSN` is unset or empty, or the yaml `storage.pg_dsn` is an unexpanded `${NAME}` reference. Before this release `LOOMCYCLE_STORAGE_BACKEND=postgres` with no connection string stopped the boot; if you removed that variable and the connection string is missing, the store is SQLite. Fix the connection string and set `storage.backend: postgres`, which makes a missing connection string an error again.
- **It opened Postgres, and you run SQLite.** A connection string is present in the environment. Set `storage.backend: sqlite`. The start refuses this by itself only when it finds `loomcycle.db` in the data directory it was given.
- **SQL Memory and Documents look empty on SQLite.** Check that `LOOMCYCLE_SQLMEM_ROOT` still points at your scope files. It is still read from the environment.

Run `loomcycle validate <config>` with the server's environment: its first lines print the storage backend it resolves. The server's boot log has the same answer in the `store: backend chosen` line.

### Retention stopped

Nothing is deleted unless `retention.enabled: true` is in the yaml and a family has a deleting mode with a `max_age`. Check, in this order:

1. The `retention:` block exists in a yaml file that is actually loaded (see 5.13).
2. `enabled: true` is set. A mode without it produces the advisory `retention.<family>.mode is … but retention.enabled is not true`.
3. The family's `mode` is `prune` or `export+prune`.
4. For `export+prune` on `defs`, `chats`, `mem` or `empty_dossier`, `export_dir` is set and writable. Without it a family in that mode deletes nothing. (`mem_content` takes `off` or `prune` only.)
5. You did not rely on the removed usage archiver (4.9).
6. The server was restarted after the edit. The `retention:` block is read at start; a config reload reports it as `restart_required` and changes no running sweeper. `GET /v1/_retention` describes the sweeper as it runs and carries `"pending_restart": true` while the yaml asks for something else.

### Documents say SQL Memory is not enabled

Documents are backed by SQL Memory.

- **Postgres store.** SQL Memory is on only when `LOOMCYCLE_SQLMEM_PG_DSN` (or `storage.sqlmem_pg_dsn`) is set. `LOOMCYCLE_SQLMEM_ENABLED=1` no longer does anything, and without the connection string the server starts with SQL Memory off instead of stopping, and logs the `WARN` line `sqlmem: SQL Memory is off because LOOMCYCLE_SQLMEM_PG_DSN is not set …`. Set the connection string; its Postgres role needs `CREATEROLE`.
- **Either store.** Check that the yaml does not say `storage.sqlmem_enabled: false`.
- **TrueNAS app.** Turn the SQL Memory toggle on and fill in its connection string; the toggle passes the connection string only when it is on.

### My local model's context got small

- **Hosted Ollama (`ollama` provider).** `LOOMCYCLE_OLLAMA_NUM_CTX` is no longer read. A positive value stops the boot while the yaml does not name `providers.ollama.options.num_ctx`, so a server that started either has the option or never had the variable; without the option requests carry no context window and the model uses its own. The option's value is the window, whatever the variable said.
- **Local Ollama (`ollama-local` provider).** `LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX` is still read, and it wins over a yaml `providers.ollama-local.options.num_ctx`. If the yaml asks for a larger window, look for the variable in the environment of the process (an environment file, the compose, the app wizard); the boot log has a `config: warning` line when the two differ. With the variable unset, the yaml option is the window; look for the key in every loaded layer.
- **The model became slow as well.** `LOOMCYCLE_OLLAMA_LOCAL_NUM_GPU` is no longer read (a positive value stops the boot while the yaml does not name the option). If the variable was removed without writing the option, Ollama may have fallen back to the CPU. Set `providers.ollama-local.options.num_gpu`.

Set `LOOMCYCLE_LOG_LEVEL=debug` to see each Ollama request's model, effort and think flag.

### An environment variable with a bad value now stops the start

The settings the boot code used to read for itself are now read with the rest of the environment, and with the dead-link mode and the automigrate switch they share one rule: a value that cannot be honoured fails the load, and the error names the variable and never its value. Before, some of these stopped the boot and some fell back to the default without a word. `loomcycle validate` and `doctor` report the same error.

| Variable | Accepted | Error |
|---|---|---|
| `LOOMCYCLE_MCP_SPAWN_RUN_TIMEOUT_MS` | A whole number, `0` or more; `0` or unset = no transport timeout | `LOOMCYCLE_MCP_SPAWN_RUN_TIMEOUT_MS: not a whole number`, or `… must be 0 or more` |
| `LOOMCYCLE_OPERATOR_TOKEN_ROTATION_GRACE_SECONDS` | A whole number, `0` or more; `0` or unset = the 24h default. There is no setting for no grace | As above |
| `LOOMCYCLE_AUTH_CACHE_TTL_SECONDS` | A whole number, `0` or more; unset = `30`; `0` turns the cache off | As above |
| `LOOMCYCLE_USER_TOKEN_MAX_TTL_SECONDS` | A whole number, `1` or more; unset = no cap. `0` is refused, so a cap written as zero is not read as none | `… not a whole number`, or `… must be 1 or more` |
| `LOOMCYCLE_RUN_RATE_PER_MINUTE` | A whole number from `1` to `60000`; unset = no default rate. Operators are never bound by it | As above |
| `LOOMCYCLE_RUN_RATE_BURST` | A whole number from `0` to `60000`; `0` or unset = the same number as the rate. Read only when the rate is set; alone it configures nothing and is not checked | `… not a whole number`, `… must be 0 or more`, or `LOOMCYCLE_RUN_RATE_PER_MINUTE / LOOMCYCLE_RUN_RATE_BURST: …` when the pair is not a valid limit |
| `LOOMCYCLE_LIMITS_ENFORCE` | `strict`, or unset | `LOOMCYCLE_LIMITS_ENFORCE: not a known value (strict, or unset)` |
| `LOOMCYCLE_GIT_SOURCE_HOSTS` | A comma-separated list of host names; unset = git-sourced volumes are off | `LOOMCYCLE_GIT_SOURCE_HOSTS: an entry is not a host name (write github.com, not a URL)` |
| `LOOMCYCLE_DEADLINK_GC` | `off`, `dry-run` or `on`; unset = `on` | `LOOMCYCLE_DEADLINK_GC must be off, dry-run or on (unset is on)` |
| `LOOMCYCLE_PG_AUTOMIGRATE` | `1` or `0`; unset = the yaml's `storage.pg_automigrate` ([5.1](#51-storage-and-sql-memory)) | `LOOMCYCLE_PG_AUTOMIGRATE: the value is not 1 (migrate the schema at boot) or 0 (do not)` |

At boot the error is the `err` field of an `ERROR` line with the message `config: load failed`, and the process exits. Fix the value or remove the line. A value that is empty or only whitespace is unset for every row of the table. That is a change for `LOOMCYCLE_USER_TOKEN_MAX_TTL_SECONDS` and `LOOMCYCLE_RUN_RATE_PER_MINUTE`, which used to stop the boot on a value of only spaces: such a line now sets no cap and no rate, as an empty one does. Variables not in this table keep the older convention: a value that does not parse leaves the default in place.

## 9. Status of this guide

This guide was checked against the code of the release, the presets `personal` and `server`, the `include:` key and the `presets:` key included: nothing described here is pending.

To re-check after any later change:

- **Tables A to E** must contain exactly the names in the runtime's table of removed variables, and every "stops the boot" cell must match a refusal rule. The comparison was re-run after the last change; re-run it after any change to either.
- **The count** of environment variables before the release (about 318) and after.
