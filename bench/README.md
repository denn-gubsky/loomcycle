# lc-bench — loomcycle model capability harness

A self-contained Go program that drives an externally-running loomcycle
instance over its HTTP MCP transport (`POST /v1/_mcp`) and grades
candidate models on three independent axes. The output is a capability
matrix that tells the operator which third-party models (DeepSeek,
Gemini, Ollama Cloud, self-hosted Ollama) are capable enough to slot
into jobs-search-agent's `user_tiers` overlay.

## Quick start

Prereqs:
- `loomcycle` running on `127.0.0.1:8787` (default; override with `--loomcycle`)
- `LOOMCYCLE_AUTH_TOKEN` env var set (the bench needs the operator bearer)
- Provider credentials in env: `DEEPSEEK_API_KEY`, `GEMINI_API_KEY`,
  `OLLAMA_API_KEY` (for Ollama Cloud), plus optional
  `LOOMCYCLE_BENCH_OLLAMA_DESKTOP_URL` (default
  `http://denn-desktop.local:11434`)
- `ANTHROPIC_API_KEY` for the judge model (set
  `LOOMCYCLE_BENCH_JUDGE_MODEL` to override the default
  `claude-sonnet-4-6`). Without an API key the semantic axis becomes
  pass-through.

Build + smoke (≈$1):

```sh
go build -o bin/lc-bench ./bench/cmd/lc-bench
./bin/lc-bench --quick --providers deepseek
```

Full sweep (≈$10–25, depending on which provider menus are large):

```sh
./bin/lc-bench --providers deepseek,gemini,ollama-cloud,ollama-desktop --user-tier bench --budget 25
```

`--user-tier bench` is the recommended pattern — see the [Recommended operator yaml](#wiring-denn-desktop-ollama-into-loomcycle) section. Without it, a first-turn provider failure (rate limit, content filter, transient 5xx) escalates through the resolver's fallback chain and the error you see in the matrix may be from the wrong provider entirely.

Output lands in `bench/results/<YYYY-MM-DD-HHMM>/`:
- `matrix.md` — human-readable verdict table
- `matrix.json` — machine-readable for tooling
- `matrix.csv` — spreadsheet drop-in
- `traces/<provider>-<model>/<case>.json` — full event stream per run

## What it does

For each requested provider, the harness:

1. **Discovers** models via `ListModels` on the existing provider
   driver in `internal/providers/<X>/`. Models matching `--models <regexp>`
   are kept; the rest are filtered out.
2. **Registers two dynamic agents per model** via the HTTP MCP
   `register_agent` tool — one `bench-low-<provider>-<model>` and one
   `bench-mid-<provider>-<model>`. Each agent gets a 7200 s TTL so
   abandoned ones reap themselves.
3. **Spawns runs** for every case in `bench/cases/<tier>/*.yaml`
   against the matching tier's dynamic agent. The runs use SSE
   notifications (`runEvents` capability opt-in at `initialize`) so
   tool-call events are captured, not just the final text.
4. **Grades** each (model, case) outcome on three axes:
   - **Structural** — regex match/anti-match + a lightweight
     JSON-schema validator (`internal/grader/structural.go`).
   - **Functional** — tool-call sequence check against the case's
     declared expectations (`internal/grader/functional.go`).
   - **Semantic** — judge model (Anthropic by default) rates the
     final text 0–100 against the case rubric
     (`internal/grader/semantic.go`).
5. **Aggregates** per-model verdicts and writes the matrix.

## Verdict legend

For each (provider, model) row:

| Verdict       | Condition |
|---------------|-----------|
| **CAPABLE**   | ≥80% structural pass AND ≥80% functional pass AND ≥0.70 average semantic score |
| **MARGINAL**  | Between FAIL and CAPABLE — operator decides per-tier |
| **FAIL**      | <50% pass on at least one axis |
| **INCONCLUSIVE** | At least one case failed at transport level (network, timeout) — re-run to verify |

The harness produces evidence; promoting a model into a `user_tier`
overlay remains a deliberate operator decision (per the
"no model pinning" rule in production).

## Flags

| Flag | Default | Purpose |
|---|---|---|
| `--loomcycle` | `http://127.0.0.1:8787` | base URL of the loomcycle instance |
| `--providers` | `deepseek,gemini,ollama-cloud,ollama-desktop` | comma-separated provider keys |
| `--models` | (empty) | regexp filter applied to discovered model lists |
| `--tier` | (empty) | limit to `low` or `middle` cases |
| `--budget` | `25.0` | USD cap; sweep halts when exceeded |
| `--quick` | `false` | smoke mode: 1 model/provider × 3 cases/tier |
| `--bench-root` | auto-detected | path to `bench/` (cases + agents) |
| `--out` | `bench/results/<ts>/` | output dir |
| `--case-timeout` | `4m` | per-case timeout |
| `--no-semantic` | `false` | skip judge calls (semantic axis = pass-through) |
| `--dry-run` | `false` | print plan without spawning runs |
| `--user-tier` | (empty) | loomcycle user_tier name. Use `bench` (configured with `fallback_on_error: false`) so first-turn failures stay as failures of the model under test rather than leaking errors from the resolver's fallback chain. |

## Wiring `denn-desktop` Ollama into loomcycle

Self-hosted models on `denn-desktop.local:11434` need an `ollama-desktop`
provider entry in `~/.config/loomcycle/loomcycle.yaml`. The harness
discovers the menu directly (no loomcycle round-trip) but the runs
themselves go through loomcycle's provider stack, which requires the
config:

```yaml
providers:
  ollama-desktop:
    type: ollama
    base_url: http://denn-desktop.local:11434
    # No api_key — local Ollama is unauthenticated.
```

Restart loomcycle after editing.

## Cases

16 cases total, 8 per tier. Each is a single YAML file at
`bench/cases/<tier>/<id>.yaml` with input prompt + three sets of
expectations. Cases mirror real production capability axes that have
bitten loomcycle in past incidents:

**Low tier (8)** — schema discipline, single-shot tool calls, short
multi-turn loops, batched ingest, schema-error recovery, scope
discipline, nested-JSON args, web search + JSON output.

**Middle tier (8)** — full MCP read/write cycle, faithful CV rewrite,
QA batch consistency, tool routing, long-context fidelity,
self-correction at scale, hallucination resistance, format switching.

See each `*.yaml` for the exact rubric.

## Trust + bias disclosure

The judge model (Anthropic `claude-sonnet-4-6` by default) will lean
toward Anthropic-style outputs when grading semantic axis. Rubrics are
written to target task quality, not style, but the bias cannot be
fully eliminated. The matrix should be read as "this model can or
cannot do the work in Anthropic's eyes", not as a neutral verdict.

A future v2 of the harness could rotate the judge across providers
(DeepSeek-as-judge, Gemini-as-judge, vote-and-average) to reduce this
bias. Out of scope for v1.

## Cost expectation

| Sweep | Approx cost | Approx duration |
|---|---|---|
| `--quick --provider deepseek` | $0.50 – $1.50 | 5–10 min |
| Full DeepSeek+Gemini | $5 – $12 | 30–60 min |
| Full + Ollama Cloud + desktop | $10 – $25 | 1–2 hours |

Local Ollama (`ollama-desktop`) is priced at $0/token in the harness
(self-hosted = no marginal $ cost). Cloud models use a coarse rate
card hand-curated for the May 2026 menu; see
`bench/internal/cost/cost.go`. The `--budget` cap is the hard
ceiling — once exceeded the sweep halts and emits a partial matrix.

## After the first sweep

1. Inspect `matrix.md` — look for any CAPABLE rows on cheap providers.
2. Drill into per-case `traces/<provider>-<model>/<case>.json` for
   the MARGINAL ones to understand WHY they fell short.
3. If a clear winner emerges, propose a `user_tiers` yaml change in
   a separate PR. The harness ships evidence; tier policy stays
   human-curated.
4. Update `~/.claude/projects/.../memory/project_local_model_selection_map.md`
   memory with the sweep date + verdict.

## Limitations + non-goals

- **Anthropic is the baseline; we do NOT test it** (would be
  redundant). Available behind `--include-anthropic` for verification
  only.
- **OpenAI** is unconfigured locally (no key); add to
  `internal/discover/discover.go` if needed.
- **Streaming events** rely on the HTTP MCP transport's SSE shape; a
  spawn_run that completes very fast may emit only the final frame.
  That's fine — the bench still grades the final text + any tool
  events captured.
- **The bench does NOT auto-promote models into tiers** — that's a
  policy decision, not a benchmarking outcome.
