# Reranking MEMORY recall on LoCoMo (2026-10-01)

- **Pre-registration:** `bench/docs/decision/locomo/PREREG.md`, committed before any pool
  was fetched. No amendments.
- **Harness:** `bench/docs/decision/locomo/` plus `bench/cmd/rerankpool`, which runs the
  SHIPPED listwise rerank (`reranker.Build` + `memory.RerankTexts`) over pre-fetched pools.
- **Setup:**
  - LoCoMo, all 10 conversations, ingested by `bench/cmd/locomo`: 5,882 turn rows, `bge-m3`.
  - 1,535 questions (categories 1–4). The evidence turn ids are the ground truth; no judge.
  - Each question's pool is the top 20 of `POST /v1/_memory/search`.
  - Every model ran on the Spark (Ollama 0.35.0).
- **Licence:** LoCoMo is CC BY-NC, so only rankings and scores are committed here, no
  question or turn text.

## Verdict

**Reranking memory recall lifts recall@5 from 0.588 to 0.74, about +15pp.** A decision
model does it as well as the shipped listwise rerank, at a third of the latency.

| hypothesis | result |
|---|---|
| **H1** `nimble_choice` > `none`, recall@5 | **HOLDS**: +0.152, 97.5% CI [+0.133, +0.172] |
| **H2** `nimble_point` > `none`, recall@5 | **HOLDS**: +0.114, CI [+0.093, +0.136] |
| S1 `qwen_list` > `none` | holds: +0.153, CI [+0.134, +0.174] |
| S2 `tev1_point` > `none` | holds: +0.133, CI [+0.113, +0.154] |
| S3 `nimble_choice` not below `qwen_list` by > 0.03 | holds: −0.0014, CI [−0.0073, +0.0048] |
| S4 `nimble_point` not below `qwen_list` by > 0.03 | fails: −0.039, CI [−0.051, −0.028] |

**Pre-registered decision:** H1 and H2 hold, so memory rerank with a decision model gets
its own RFC (RFC DQ's decision 4). S1 also holds: reranking helps memory whatever model
does it, so the shipped rerank's document-only scope is now measured as leaving +15pp on
the table.

## Arms (1,535 questions)

| arm | recall@1 | recall@5 | recall@10 | hit@1 | hit@5 | MRR@20 | p50 / p95 rerank |
|---|---|---|---|---|---|---|---|
| `none` (pool order) | 0.338 | 0.588 | 0.667 | 0.387 | 0.660 | 0.509 | — |
| `qwen_list` (qwen3.8, shipped code) | **0.615** | **0.741** | 0.752 | **0.714** | **0.813** | **0.759** | 3.02 / 3.53 s |
| `nimble_choice` | 0.588 | 0.739 | **0.754** | 0.680 | 0.809 | 0.737 | **0.94 / 1.08 s** |
| `nimble_point` | 0.504 | 0.702 | 0.737 | 0.584 | 0.775 | 0.668 | 4.81 / 5.56 s |
| `tev1_point` (4B) | 0.555 | 0.720 | 0.748 | 0.638 | 0.795 | 0.708 | 2.91 / 3.09 s |

- **Ceiling:** the pool's own recall@20 is **0.758**, the most any rerank of 20 can reach.
  The two best reranks bring 0.74 of it into the top 5, about **98% of the ceiling**.
- **Latency** is the rerank call only. It excludes the search, which is the same for every arm.
- **The shipped rerank's misses:** it reported `unparseable` on 8 of 1,535 questions (0.5%).
  A decision model cannot.

## By category (recall@5)

| category | n | none | qwen_list | nimble_choice | nimble_point | tev1_point |
|---|---|---|---|---|---|---|
| single-hop | 841 | 0.673 | 0.829 | **0.834** | 0.800 | 0.813 |
| temporal | 320 | 0.671 | 0.810 | **0.814** | 0.768 | 0.785 |
| multi-hop | 282 | 0.332 | **0.516** | 0.495 | 0.474 | 0.488 |
| open-domain | 92 | 0.297 | **0.387** | 0.365 | 0.268 | 0.359 |

**The weak categories gain most:** multi-hop +18pp with `qwen_list` and +16pp with
`nimble_choice`. They still trail single-hop by 30pp.
- **That gap is retrieval depth, not order.** The pool's own recall@20, the most a rerank
  can reach, is:
  - multi-hop 0.544;
  - open-domain 0.437;
  - temporal 0.820;
  - single-hop 0.841.
- **The rerank already reaches most of what the pool holds:** `qwen_list` gets 0.516 of
  multi-hop's 0.544. To close more of the gap, the evidence must reach the 20 candidates
  first (a deeper pool, or better first-stage retrieval).

## Instrument checks

| # | check | result |
|---|---|---|
| 1 | 1,535 questions, ≥ 99% pools of 20 | pass (1,535; 100%) |
| 2 | `none` reproduces the harness's recall@10 0.6675 (± 0.01) | pass (0.6668) |
| 3 | `qwen_list` reranked on ≥ 95% | pass (99.48%; 8 `unparseable`) |
| 4 | no decision call left failed | pass |

## Against the ConditionalQA document probe

| | documents (ConditionalQA, R@5) | memory (LoCoMo, recall@5) |
|---|---|---|
| rerank gain over none | +6.9pp (`qwen_list`) | +15.3pp |
| `nimble_choice` vs `qwen_list` | +0.9pp | −0.1pp |
| `nimble_choice` latency vs `qwen_list` | ~3× faster | ~3× faster |
| top-1 cost of `nimble_choice` | −3.6pp R@1 | −2.7pp recall@1 |

**On both corpora:** one `nimble` `choice` call matches the 27B listwise rerank at the
depth an agent reads, is about 3× faster, and gives up a little at rank 1.
- **Pointwise:** worse than `choice` on both corpora, and no faster.
- **`tev1:4b`:** helps memory (+13pp) but did not help documents. The budget is not the
  reason: both corpora's pointwise inputs (one passage of ≤ 1,200 characters) fit its
  ~2,000-token budget. Why it fails on government guidance but not on chat turns is not
  established here.

## Files

- `summary.json` — `analyze.py` output.
- `results/*.jsonl.gz` — each arm's per-question order, scores and latency (turn ids
  only, no text).
