# Decision-model reranking on ConditionalQA (2026-09-30)

- **Pre-registration:** `bench/docs/decision/PREREG.md`, committed before any pool was fetched.
  - Amendment 1 moved the embedder to TrueNAS (the identical `bge-m3`).
  - Amendment 2 was forced by a failed instrument check: the baseline is rerun over the rebuilt pools, and every arm runs on the Spark.
  - Both amendments were committed before any rerank was scored.
- **Harness:** `bench/docs/decision/`.
- **Setup:**
  - 800 ConditionalQA questions (phase M's pre-drawn subset), each with a 20-candidate pool from the shipped `Document op=search`.
  - Five arms: the pool order, the shipped `qwen3.8` listwise rerank, and three Jev-style decision-model reranks over Ollama's `/v1/systemone`.
  - Every arm ran on the Spark (Ollama 0.35.0).

## Verdict

**`nimble` (9B) matches the shipped `qwen3.8` (27B) rerank on R@5, about 3× faster on the same host, but loses at R@1. Its probabilities are not calibrated enough to act as a "nothing here answers" gate.**

| hypothesis | result |
|---|---|
| **H1** `nimble_choice` not worse than `qwen_list` on R@5 by > 0.03 | **HOLDS**: 0.9437 vs 0.9350, diff +0.0088, 97.5% CI [−0.0013, +0.0200] |
| **H2** `nimble_point` not worse than `qwen_list` on R@5 by > 0.03 | **HOLDS**: 0.9413 vs 0.9350, diff +0.0063, 97.5% CI [−0.0063, +0.0200] |
| S1 `tev1_point` (4B), same test | fails: 0.8838, diff −0.0512, CI [−0.0737, −0.0300] |
| gate "usable" (AUC ≥ 0.85 and ECE ≤ 0.10) | **no** for both pointwise arms |

## Arms (800 questions)

| arm | R@1 | R@3 | R@5 | R@10 | MRR@10 | p50 / p95 latency | mean input tokens |
|---|---|---|---|---|---|---|---|
| `none` (pool order) | 0.5763 | 0.8075 | 0.8662 | 0.9263 | 0.7029 | — | — |
| `qwen_list` (qwen3.8, shipped) | **0.8125** | 0.9237 | 0.9350 | 0.9500 | **0.8682** | 4.98 / 6.10 s (with a 0.6 s search) | — |
| `nimble_choice` | 0.7762 | **0.9263** | **0.9437** | **0.9513** | 0.8498 | **1.46 / 1.89 s** | 2,648 |
| `nimble_point` | 0.7025 | 0.8938 | 0.9413 | 0.9500 | 0.8018 | 4.81 / 5.47 s | 6,944 (20 calls) |
| `tev1_point` | 0.6150 | 0.8287 | 0.8838 | 0.9263 | 0.7295 | 4.73 / 5.67 s | 6,184 (20 calls) |

**Against no rerank at all** (R@5, exact McNemar):
- `nimble_choice`: 67 / 5, p = 6e-15.
- `nimble_point`: 67 / 7, p = 2e-13.
- `tev1_point`: 63 / 49, p = 0.22, no better than the pool's own order.

**Latency** is wall time on the shared Spark.
- The baseline's figure includes the search (p50 0.60 s, measured separately on 30 queries), so its rerank alone is about 4.4 s.
- `nimble_choice` is **one call** per question. It never needed to cut the 1,200-character candidates to fit its 8,192-token prompt.
- The pointwise arms make 20 calls, 4 in flight, so they save nothing.

## Top-1 is where the decision model loses (post hoc, not tested)

| arm vs `qwen_list`, R@1 | discordant (arm / qwen) | exact McNemar p |
|---|---|---|
| `nimble_choice` | 32 / 61 | 0.0035 |
| `nimble_point` | 30 / 118 | 1.5e-13 |

- **R@1:** `nimble_choice` puts the right section **first** less often (0.776 vs 0.813).
- **R@3 to R@10:** it puts it in the top 3 to 10 as often as the shipped rerank, or slightly more.
- **Which matters:** for an agent that reads the top few results this is a wash. For a caller that trusts the first hit, it is a loss.
- **Why:** a `choice` answer is a softmax over "which ONE passage best answers". It separates the best candidate from the field, but the order below it is only as good as small probabilities are.

## Calibration and the gate (pointwise arms)

| arm | pairs | ECE | Brier | pair AUC | gate AUC (minimal pairs) |
|---|---|---|---|---|---|
| `nimble_point` | 16,000 | 0.188 | 0.140 | 0.890 | 0.755 |
| `tev1_point` | 16,000 | 0.214 | 0.139 | 0.839 | 0.699 |

**The probabilities are strongly overconfident.**
- Of the `nimble_point` pairs scored above 0.9 (mean 0.973), only 42% are gold sections.
- Scores between 0.4 and 0.9 are gold only 9–15% of the time.

**As a relevance ordering it is good (pair AUC 0.89); as a probability it is not.**

**The gate:**
- Question tested: the top score of a pool holding the gold vs the same pool with the gold removed.
- Result: the pool with the gold scores higher only 75% of the time.
- Reason: a pool without its gold still has a candidate the model is sure about.

**Label caveat:** the gold is ConditionalQA's evidence sections, and another section can
also answer. Some of the "overconfidence" is therefore true answers without the gold label,
and the gate numbers are pessimistic. They are still far from the pre-registered
thresholds.

## Instrument checks

| # | check | result |
|---|---|---|
| 1 | rebuilt pool top 10 = phase M's (≥ 98%) | **FAIL** (0.6875; 0.9237 as sets). Led to amendment 2 |
| 2 | phase M's rerank top 10 within the rebuilt pool (≥ 98%) | **FAIL** (0.975). Led to amendment 2 |
| 1b | the baseline reranked the same 20 candidates (≥ 98%) | pass (1.000) |
| 2b | the baseline reported `reranked: true` (≥ 95%) | pass (1.000) |
| 3 | pools of 20 | pass (1.000) |
| 4 | no decision call left failed | pass (0 failures on the Spark) |

**Cross-host agreement:** `nimble_point` on 205 questions run on both TrueNAS and the Spark
(same digest, same pools) gives the same top-1 for 99.0% of them. The largest probability
difference is 0.074.

## What this does and does not show

- **Ranking:** a 9B decision model with one call and no reply to parse ranks as well as a
  27B general LLM at the depth an agent reads, and it is about 3× faster.
  - It cannot fail as `unparseable`, and it cannot answer outside the candidate set.
  - The shipped rerank repairs its reply precisely because a general model can.
- **Top 1:** it is measurably worse at the very top.
- **The gate:** its probabilities do **not** support "nothing here answers". A gate still
  has no signal that works (see also the QPP probe).
- **Hardware:** on TrueNAS the same model ran at 11–13 s per `choice` call. The speed case
  depends on the host's prefill rate, not on the model family.
- **Scope:** one corpus, one subset of 800 questions.

## Files

- `summary.json` — `analyze.py` output.
- `results/*.jsonl.gz` — every arm's ranking, scores, latency and host per question,
  including the 205-question TrueNAS run of `nimble_point`.
