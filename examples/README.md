# loomcycle examples

Self-contained, runnable examples. Each lives in its own folder with everything
needed to start it standalone: a `loomcycle.yaml`, a `run.sh` launcher, a
`.env.local.example` secret template (empty values), and a comprehensive `README.md`
with reproduction + verification steps.

## Experiment examples (exp1–exp6)

These six mirror the loomcycle sandbox experiments in **static, self-contained**
form. Every one routes to **Anthropic OAuth (primary) → deepseek-v4-pro (fallback)**.

| Example | Primitive(s) | Self-contained? |
|---|---|---|
| [`exp1-tools-usage/`](exp1-tools-usage/) | Built-in tools (Read/Write/Edit/Bash/Web): a coding agent writes, runs, and verifies a program in a sandbox dir. | ✅ loomcycle + a provider only |
| [`exp2-interruption/`](exp2-interruption/) | Interruption (human-in-the-loop): an agent asks Yes/No, blocks, resolves over REST, branches. | ✅ loomcycle + a provider only |
| [`exp3-multiagent-loop/`](exp3-multiagent-loop/) | Channel + Memory + Evaluation + Context: a 3-agent, 5-hop refine/evaluate loop. | ✅ loomcycle + a provider only |
| [`exp4-gitea-telegram/`](exp4-gitea-telegram/) | Inbound webhooks + 3rd-party MCP (gitea-mcp) + Telegram: coder→PR→reviewer-merge→advisor→Telegram. | ⚠️ needs external Gitea + Telegram + the gitea-mcp binary (see its README) |
| [`exp5-scheduler-pipeline/`](exp5-scheduler-pipeline/) | Scheduler fan-out + `Context op=time` + `Channel.await` fan-in + `max_fires`: 5 RSS collectors → consolidator → Telegram, every 5 min, self-stops after 3 cycles. | ⚠️ needs Telegram + loomcycle ≥ v0.25.1 (see its README) |
| [`exp6-self-evolving-agents/`](exp6-self-evolving-agents/) | `AgentDef.fork`/`promote` + lineage + `Agent.parallel_spawn` + `Evaluation` + `Memory`: a meta-breeder runs a genetic algorithm over solver agents that mutate their own system prompt until one crosses a fitness threshold. | ✅ loomcycle + a provider only |

(The repo also ships `cluster/`, `observability/`, and `python-cli/` examples.)

## Quick start (any of exp1–exp3)
```bash
cd exp1-tools-usage
./run.sh            # first launch copies .env.local.example → .env.local; fill it in, re-run
```
Then follow that folder's `README.md` to drive + verify the run from a second terminal.

## Providers — OAuth primary, deepseek fallback
Each `loomcycle.yaml` lists both providers and the agents use `tier: middle`
(`claude-sonnet-4-6` over the OAuth provider, falling through to `deepseek-v4-pro`).
You need **at least one** of:
- **Anthropic OAuth (primary):** `loomcycle anthropic login` once, keep
  `LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED=1` (set by `run.sh`). Research/dev only.
- **DeepSeek (fallback):** put `DEEPSEEK_API_KEY` in `.env.local`. If OAuth isn't
  logged in / is excluded, runs use `deepseek-v4-pro` automatically.

## A note on `loomcycle validate` vs running
These examples are **tier-based** (so the OAuth→deepseek *fallback* works). `loomcycle
validate` resolves *explicit pins* but does **not** probe providers, so it reports
`no provider resolved` for a tier-based agent — that's a static-check limitation, **not
a config error**. Verify by **running**: `./run.sh` and watch the boot `resolve probe:`
lines — you should see `anthropic-oauth-dev reachable` and/or `deepseek reachable`, and
runs resolve live. (`loomcycle validate` is still useful to catch YAML/structural
errors; just expect the tier-resolution line.)

## Conventions
- `run.sh` is the entry point (sets the tool sandbox / webhooks / cap env, then launches
  `loomcycle --config ./loomcycle.yaml` from `./work`). Override the binary with
  `LOOMCYCLE_BIN`, the port with `LOOMCYCLE_LISTEN_ADDR`.
- Secrets live only in `.env.local` (gitignored); the committed template is
  `.env.local.example` (empty values). `run.sh` bootstraps `.env.local` on first run.
- `loomcurl.sh` is a token-safe REST helper (omits the bearer in dev open mode).
- State is SQLite under `./data` (gitignored); delete it for a clean slate.
