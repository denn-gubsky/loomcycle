# exp6.8 — self-evolving agents on LOCAL models (gemma4:max population)

The [exp6](../exp6-self-evolving-agents/) genetic algorithm, rerun on **local ollama models** to see
how local models behave over a long, multi-generation, auto-compacting agentic run. A population of
**`gemma4:max`** solver agents (the only locally-loaded model) mutate their own system prompt across
generations; the meta-agents — the **breeder** (GA controller) and **advisor** (task author + judge)
— run on **cloud sonnet**. No snapshot.

> **Result up front (this is the point of the example): the GA *completes* but the population does
> NOT converge.** It runs all 5 generations cleanly — lineage + promote intact, genes drift toward
> the rubric optimum (creativity 4.8→9.0), best output 0.82→0.87 — but the **mean stays flat** because
> **~35% of variant-slots produce no usable score** (gemma4:max silently skips the self-report ~20% of
> the time and hallucinates ~15%). **The limiting factor for local agentic evolution is the small
> model's per-run reliability, not the loomcycle substrate — which performed flawlessly.**

## What it demonstrates
- The full self-evolution substrate works the same with local models: `AgentDef.fork`/`promote` +
  lineage, `Agent.parallel_spawn`, `Evaluation`, `Memory`, and **per-agent `sampling.temperature`
  reaching the ollama model**.
- The **v0.37 local-model robustness** (heartbeat ticker + compaction tail-cap + 300s local timeouts)
  keeping long, slow local runs alive through many generations.
- A real, honest **negative-ish finding**: where small local models hit their ceiling as autonomous
  multi-step agents.

## Models — why this split
| Role | Model | Where |
|---|---|---|
| `exp6-solver` ×4 (the evolving population) | **gemma4:max** | LOCAL — the only locally-loaded model |
| `exp6-advisor` (grounded judge) | **claude-sonnet-4-6** | cloud (OAuth or API key) |
| `exp6-breeder` (GA controller) | **claude-sonnet-4-6** | cloud (OAuth or API key) |

A **local model as the breeder does not work** — `qwen3.6:max` mis-formatted the nested
`Agent.parallel_spawn` argument (passed the `spawns` array as a JSON *string*) and terminated its
turn early; the 80-step GA orchestration is beyond a local small model. Keeping **both** meta-agents
on cloud also means ollama never swaps two large models (qwen↔gemma) in VRAM mid-loop — a swap that
was corrupting context and dropping solver self-reports.

## The temperature cap
gemma4:max hallucinates above ~0.8 temperature. The creativity gene maps to a **capped** real
temperature `round(min(creativity/10, 0.7), 2)`, so every variant stays in the grounded band (≤0.7,
below the cliff) and produces scoreable answers. (The earlier uncapped 0.0–1.0 run hallucinated too
much to converge.) The advisor rewards **vivid + decisive + factually GROUNDED** — so hallucinated
specifics score low.

## Prerequisites
- **loomcycle ≥ v0.37.0** on PATH (or `LOOMCYCLE_BIN`) — the heartbeat ticker + compaction tail-cap
  are what keep long local runs from false-timeout deaths. Check `loomcycle --version`.
- **An ollama host** with your solver model — set `OLLAMA_BASE_URL` in `.env.local`. This example
  was tuned on `gemma4:max`; swap the tag in `loomcycle.yaml` for your local model.
- **A cloud sonnet provider for the meta-agents** — Anthropic OAuth (`loomcycle anthropic login`) or
  an `ANTHROPIC_API_KEY` (then change the two meta agents in `loomcycle.yaml` to `provider: anthropic`).
- **`python3`** (the driver + verifier are stdlib-only).

⚠️ Not fully self-contained: needs an ollama host + a cloud key. (The point is the local *population*,
not zero external deps.)

## Run + drive
Terminal 1 — start the server:
```bash
cd examples/exp6.8-local-evolution
./run.sh        # first launch copies .env.local.example → .env.local; set OLLAMA_BASE_URL + a sonnet provider, re-run
```
Watch the boot line `resolve probe: ollama-local reachable (... models)` and your sonnet provider
`reachable`.

Terminal 2 — drive the evolution (one breeder run per generation; the agents do all the
mutation/scoring/forking — the script just steps generations):
```bash
cd examples/exp6.8-local-evolution
MAX_GEN=5 THRESHOLD=0.90 ./work/exp6_run.sh evolve
```
Expect it to **complete 5 generations** with a flat/declining mean and a best around 0.85–0.90, e.g.:
```
gen | mean | max  | mean creativity
 0  | 0.75 | 0.82 | 4.8
 4  | 0.54 | 0.87 | 9.0   ← winner promoted; stopped at MAX_GEN
```
The recurring `0.0` scores are gemma4 solvers that dropped their self-report or hallucinated — the
finding, live.

## Verify (independent re-derivation)
```bash
./work/exp6_run.sh verify
```
Reads the `gen:*` ledger from the store and checks improvement (will flag REVIEW — the mean doesn't
climb), lineage integrity (PASS), and promotion (winner == active `exp6-solver`).

## Files
| File | Purpose |
|---|---|
| `loomcycle.yaml` | routing + the 3 agents (gemma4 solver / sonnet advisor + breeder), capped-temp breeder, compaction tuned for slow local prefill |
| `run.sh` | launcher — sets `OLLAMA_BASE_URL`, the 16K global window, 300s local timeouts, OAuth on |
| `work/exp6_run.sh` | thin generation-stepper driver (`evolve` / `verify`) — reused from exp6, model-agnostic |
| `work/exp6_verify.py` | independent re-derivation (improvement + lineage + gene-drift) |
| `.env.local.example` | secret/env template (bearer + ollama host + sonnet provider) |
| `loomcurl.sh` | token-safe REST helper |

## Notes / how you'd push past the wall
- **Per-model `num_ctx` is global-only** (`LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX`) — you can't give gemma4 a
  small window and another local model a larger one in the same instance. (RFC candidate.)
- **To actually converge on local models:** add an N-of-M retry per solver (re-spawn on a dropped
  self-report), or use a larger/more-reliable local solver model (at the cost of reintroducing the
  VRAM model-swap question). The infra is ready; the model is the variable.
