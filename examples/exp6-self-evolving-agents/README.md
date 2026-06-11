# exp6 — self-evolving agents (genetic lineage of prompt-mutating solvers)

A meta-agent runs a small **genetic algorithm** over a population of solver agents that
**mutate their own system prompt** to score better on a task — generation by generation,
until one crosses a fitness threshold. It exercises loomcycle's self-evolution substrate
(no new primitives): `AgentDef.fork`/`promote` (mutation + lineage), `Agent.parallel_spawn`
(the generation), `Evaluation` (the fitness function), `Memory` (the generation ledger).

```
            ./work/exp6_run.sh evolve   (thin stepper: one breeder run per generation)
                    │
              exp6-breeder   (GA controller — AgentDef + Agent + Evaluation + Memory)
   gen 0 ─ advisor authors the "trick" + rubric → Memory task:spec ;
           seed 4 solver variants via AgentDef.fork (sub-optimal genes)
   each generation g:
     1. SOLVE   Agent.parallel_spawn → 4× exp6-solver, pinned by def_id (each carries its genes
                in its own system_prompt); each self-reports run_id + answer to Memory.
     2. SCORE   exp6-advisor scores each run → Evaluation.submit {novelty,decisiveness,correctness}.
     3. SELECT  best = max score → gen:g:summary.
     4. STOP?   best ≥ THRESHOLD (or g == MAX_GEN-1) → AgentDef.promote(best) + result:summary.
     5. MUTATE  each survivor REFLECTS on its own score+feedback and PROPOSES its child's genes;
                the breeder applies it via AgentDef.fork(parent_def_id=survivor).  ← the "self" in self-evolving
```

## What it demonstrates

| Primitive | Role in the loop |
|---|---|
| **`AgentDef` fork / promote / get** | the *mutation* (a new `system_prompt` + `effort` + `sampling.temperature`) and *selection* (set the active def); `parent_def_id` forms a real **lineage tree** |
| **`Agent` parallel_spawn** | runs a *generation* — the population fanned out, collected as an AND-barrier; children pinned to distinct mutant `def_id`s |
| **`Evaluation` submit / aggregate** | the *fitness function* — a judge submits a numeric score + named dimensions; selection reads it back |
| **`Memory` (user scope)** | the shared *generation ledger* (genotype + score + lineage per variant), readable by every role agent and the external verifier |
| **`Context` self / lineage** | introspection — a solver reads its own `run_id`; the verifier walks the ancestry |

The three **genes** (integers 0–10, expressed as literal text in each solver's prompt, so they
are heritable material): **creativity** (literal ↔ bold/vivid), **courage** (hedging ↔ decisive),
**caution** (confident ↔ self-critical). `creativity` also drives **real runtime knobs**: the
`effort` tier and — on **loomcycle v0.28.0+** — the per-agent `sampling.temperature =
round(creativity/10, 2)` (0.0 focused … 1.0 wild), set on each forked variant via the AgentDef
`sampling` overlay and readable by the solver through `Context op=self`. So a gene changes the
model's actual sampling, not just prompt text; the temperature evolves with the lineage. (On a
pre-0.28.0 build the `sampling` overlay is ignored — the run still works on prompt + effort.)
The advisor's rubric rewards vivid + decisive + correct answers, so the optimum persona is
high-creativity / high-courage / low-caution — and the population evolves toward it.

Routing: **Anthropic OAuth (primary) → deepseek-v4-pro (fallback)** via `tier: middle`.

## Prerequisites
- **loomcycle** on PATH (or `LOOMCYCLE_BIN`). The `AgentDef`/`Evaluation` primitives are
  long-standing; **v0.28.0+** is recommended so the `sampling.temperature` gene actually tunes the
  model (older builds ignore it and fall back to prompt + effort — the run still completes).
- **A provider** — Anthropic OAuth (`loomcycle anthropic login`, kept enabled by `run.sh`) or
  `DEEPSEEK_API_KEY` in `.env.local`.
- **`python3`** (the driver + the independent verifier are stdlib-only).

This is the **most self-contained** example: no external services, no network egress, no MCP,
no scheduler, no webhooks.

## Run + drive

> **`loomcycle validate` note:** tier-based config (for the OAuth→deepseek fallback) — `validate`
> reports `no provider resolved` because it doesn't probe providers. Not a config bug; verify by
> **running** and watching the boot `resolve probe:` lines.

Terminal 1 — start the server:
```bash
cd examples/exp6-self-evolving-agents
./run.sh        # first launch copies .env.local.example → .env.local; add a provider, re-run
```

Terminal 2 — drive the evolution (one `exp6-breeder` run per generation; the agents do all the
mutation/scoring/forking — the script just steps generations and detects the stop condition):
```bash
cd examples/exp6-self-evolving-agents
./work/exp6_run.sh evolve     # ~3-4 min per generation; up to MAX_GEN=5
```
Expect a monotonic climb, e.g.:
```
  gen0: mean=0.50 max=0.72 genes(c,k,a)=(3.0,3.5,7.0)   ← seeds, far from optimum
  gen1: mean=0.67 max=0.82 genes(c,k,a)=(3.0,5.2,5.2)
  gen2: mean=0.76 max=0.87 genes(c,k,a)=(3.5,6.5,3.8)
  gen3: mean=0.83 max=0.90 genes(c,k,a)=(7.8,6.5,3.8)   ← crossed THRESHOLD → STOP
  RESULT: {"generations":4,"best_score":0.9,"winner_def_id":"def_…","stopped":"threshold"}
```
(Exact numbers vary run to run — it's an LLM-judged fitness landscape.)

## Verify (independent re-derivation — don't trust the breeder's report)

`evolve` runs this automatically at the end; re-run it any time with:
```bash
./work/exp6_run.sh verify
```
It reads the `gen:*` ledger straight from the store and checks:
- **Improvement** — `mean(last gen) ≥ mean(gen 0)` (PASS).
- **Gene drift** — the mean gene vector moves toward the optimum (creativity↑, courage↑, caution↓).
- **Lineage integrity** — every gen>0 variant's `parent_def_id` resolves to a real prior def (PASS).
- **Promotion** — the active `exp6-solver` def == the winner.

You can also inspect by hand:
```bash
BASE=http://127.0.0.1:8787
# the generation ledger:
./loomcurl.sh "$BASE/v1/_memory/scopes/user/exp6/keys?prefix=gen:&limit=200"
# the winning def + its lineage parent:
./loomcurl.sh -X POST "$BASE/v1/_agentdef" -H 'Content-Type: application/json' \
  -d '{"op":"get","def_id":"<winner_def_id from result:summary>"}'
```

## Tuning

The GA constants live in the **breeder's system prompt** in `loomcycle.yaml` (`POP`, `MAX_GEN`,
`THRESHOLD`, the seed gene vectors). Keep the driver's `THRESHOLD`/`MAX_GEN` env in sync if you
change them (the breeder decides the stop; the driver only mirrors it for its own loop check).
Seeds are deliberately **sub-optimal** so there's a gradient to climb — seeding near the optimum
makes a gen-0 variant win immediately (no evolution to watch).

## Teardown

Ctrl-C the server; delete `./data` for a clean slate (re-running `evolve` on a non-empty store
will skip seeding because gen-0 variants already exist).

## Files

| File | Purpose |
|---|---|
| `loomcycle.yaml` | routing + the 3 role agents (breeder / solver / advisor) with full prompts; the breeder's prompt encodes the GA loop |
| `run.sh` | launcher (OAuth on; no other enablement needed) |
| `.env.local.example` | secret template (empty) — bearer + provider only |
| `work/exp6_run.sh` | thin generation-stepper driver (`evolve` / `verify`) |
| `work/exp6_verify.py` | independent re-derivation from the store (improvement + lineage checks) |
| `loomcurl.sh` | token-safe loomcycle REST helper |

## A note on the *dynamic* twin

In the loomcycle sandbox, exp6 also has a **fully-dynamic** variant where the breeder itself is
authored at runtime via `POST /v1/_agentdef`. That surfaced **F40** — the AgentDef overlay didn't
round-trip `agent_def_scopes`, so a runtime-authored meta-agent couldn't fork — **fixed in
loomcycle v0.26.2 (#436)**. This example ships the **static** variant (breeder declared in yaml),
which runs on older builds too.
