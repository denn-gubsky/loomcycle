# LoCoMo rating, rovemark protocol, fully local pipeline — 2026-09-24

**binary-J 0.706** (95% CI 0.683–0.730, bootstrap over questions) on all 1,535 scoreable
category 1–4 questions of the ten LoCoMo conversations. Partial credit 0.767 (our own
convention, not comparable to published tables). Every question graded; 102 answered
`NOT_FOUND`, scored wrong.

## Why this protocol, not the 250-question one

The published LoCoMo tables are not one protocol. Backboard scores 250 questions with a
generous binary rubric on gemini-2.5-pro; rovemark scores all 1,540 with gpt-4o-mini as
answerer and judge. They disagree by nine points on the same system (Zep 75.14 vs 66.0), so
a number means nothing without its protocol. This one targets rovemark because our loader
agrees with it exactly — categories 1–4 give 1,535 scoreable + 5 without usable evidence =
1,540 — and because its full-context baseline, 72.9, sits within a point of our own
raw-turns measurement (0.738).

## What ran, and what did not

| role | model | where |
|---|---|---|
| answerer (`locomo/orn-l2k24-sp`, `recall_attach_traces: true`) | ornith-1.5:35b | Spark, local |
| extractor, ontologist, memory judge, conflict judge | ornith-1.5:35b, `max_context_tokens: 131072` | Spark, local |
| embedder | bge-m3 | Spark, local |
| scribe, consolidator | code-js | in-process, no model |
| **judge (`locomo/judge`)** — the ONLY cloud call | deepseek-v4-flash | DeepSeek |

Configuration: `bench/synthesis/ingest.yaml` + `lme_gate_arms.yaml` + `locomo_rating.yaml`,
`LOOMCYCLE_PRESETS=base,memory`. Enforced at run time by a watchdog that polled `runs` and
`token_usage` every 20 s and would have stopped the server on any cloud call not made by the
judge; it never fired.

Two-phase: **A** (trace index ON) ingests, consolidates and dumps retrieval shape per
conversation, one scope each; **B** (trace index OFF, so no answering run writes its own
prompt into the index) answers with `-answer-only`. Trace-row count was checked unchanged
across B (5,945 before and after).

## Per category

| category | n | binary-J | partial |
|---|---|---|---|
| single-hop | 841 | 0.825 | 0.853 |
| temporal | 320 | 0.741 | 0.759 |
| multi-hop | 282 | 0.408 | 0.619 |
| open-domain | 92 | 0.413 | 0.467 |

Per conversation binary-J ranges 0.673 (conv-49) to 0.756 (conv-44); full table in
`summary-rating.json`.

## ⚠️ What this number is NOT comparable on

- **Different answerer and judge.** rovemark uses gpt-4o-mini for both. Ours answers with a
  local 35B model and grades with deepseek-v4-flash, whose leniency against gpt-4o-mini is
  uncalibrated. The judge is the larger error bar, and the CI above does not include it.
- **The judge truncates.** deepseek-v4-flash is a hybrid reasoning model; at `max_tokens:
  1000` it sometimes spent the whole budget thinking and returned nothing (22 of 1,535).
  `rejudge.py` re-graded exactly those — same judge, same prompt, budget 4000, then 8000 —
  and each such row keeps `verdict_orig` and `rejudged: true`. A verdict that fit in 1000 is
  untouched.

## Three failures the checkpoints caught

Each conversation was banked only after a completeness check, and every one of these would
otherwise have produced a plausible-looking number:

1. **The extractor silently ran in the paid cloud.** The memory bundle's tier-resolved agents
   fell through `tier:middle` from an unavailable local candidate to deepseek-v4-pro without a
   word (498 historical runs). Fixed by pinning every non-judge agent in `locomo_rating.yaml`.
2. **A fact count is not coverage.** conv-26 first passed `facts ≥ 50` with 8 of its 19 chats
   never extracted. The checkpoint now requires the consolidation queue drained and ≤2%
   extractor failures.
3. **ornith CUDA-faults when gpt-oss shares the GPU.** Sampling `/api/ps` every 30 s and
   joining it to each extractor call: 0 of 66 calls failed without gpt-oss loaded, 77 of 334
   with it. An earlier reading — that a small `num_ctx` caused it — was a confound (a larger
   window made Ollama evict gpt-oss). With the competing workload stopped, all ten stores built
   with zero extractor failures.

The failed attempts are recorded in `summary-checkpoints.json` alongside the banked ones.

## Spend

Judge (deepseek-v4-flash): 1,458 calls, 327,550 input + 186,083 output tokens — the whole
cloud bill of the run. Local ornith: 6,314 calls. No pricing table is configured, so
`token_usage.cost` is zero; the token counts are the record.

## Reproduce

`aggregate.py <rove-out>` pools the ten `B-conv-*/answer-report.json` into
`summary-rating.json`. The shell drivers (per-conversation phase A with the coverage
checkpoint, phase B with the grading-completeness checkpoint, the cost watchdog) lived in the
session scratchpad; their checks are described above.
