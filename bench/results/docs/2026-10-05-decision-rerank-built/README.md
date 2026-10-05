# RFC DQ P3 — the shipped decision reranker, measured through the built feature (2026-10-05)

- **Pre-registration:** `bench/docs/decision/built/PREREG.md`, committed before the store
  was rebuilt.
  - Amendment 1 voided the first run and added a stability gate; it was committed before
    the re-run.
- **Harness:** `bench/docs/decision/built/`.
- **Runtime:** loomcycle `main` after #1619 (`kind: decision`).
- **Corpus:** all 2,515 ConditionalQA questions, phase M's corpus.
- **Arms:** both are the shipped `Document op=search` (limit 10), one without and one with
  an agent's `memory_rerank`. The rerank is served by `nimble` on the Spark.

## Verdict

**The built feature delivers the probe's result.** RFC DQ is done.

| hypothesis | result |
|---|---|
| **H1** built `decision` R@5 ≥ 0.914 (the probe's 0.944 − 0.03) | **HOLDS**: 0.9328, Wilson 95% [0.9223, 0.9419] |
| **H2** median rerank time ≤ 1,830 ms (the probe's 1,464 ms + 25%) | **HOLDS**: 1,451 ms |

| arm | R@1 | R@3 | R@5 | R@10 | p50 / p95 search |
|---|---|---|---|---|---|
| `none` | 0.5877 | 0.7984 | 0.8604 | 0.9153 | 0.79 / 1.50 s |
| `decision` | **0.7702** | **0.9093** | **0.9328** | **0.9439** | 2.27 / 2.91 s |

- **Gain over no rerank:** R@5 +7.2 points (discordant 202 / 20, p = 5e-39) and R@1 +18.3
  points (608 / 149, p = 1e-66).
- **Rerank time** per question (`decision` − `none`): p50 1.45 s, p95 1.87 s.
- **Against phase M**, which used the same corpus and the same unreranked search
  (`none` 0.8604 there too): the shipped `qwen3.8` listwise rerank reached R@5 0.9332. The
  decision kind reaches 0.9328 with a 9B model.

## Instrument checks

| # | check | result |
|---|---|---|
| 1 | rerank applied on ≥ 95% | pass (2,515 / 2,515) |
| 2 | `none` R@5 within ±0.02 of phase M's 0.8604 | pass (0.8604) |
| 3 | every question answered by both arms | pass |
| gate | 50 reference searches twice ≥ 5 min apart, identical, ≥ 95% vs the `dd9b58c1` reference | pass (50/50, 50/50) |

## The void first run (Amendment 1)

The first run scored `none` R@5 0.7781 and `decision` 0.8461. Check 2 failed, which voids
H1. Its data is kept as `summary-run1-void.json` and `results/built-run1-void.jsonl.gz`.

**What was ruled out:**
- **Not the code.** On the same store, the Sept 30 binary and the shipped one return
  identical rankings (100/100), and a `git bisect` over the 98 commits between them found
  every commit identical.
- **Not the store's contents.** The index texts are complete with their headers, embedding
  is synchronous, there is no approximate vector index, the table was analyzed before the
  run, and the default ranking is pure semantic.

**What it was:**
- **Transient state during the run.** Only the first ~500 questions diverged from what the
  store returns afterwards; later ones matched.
- **The cause is not established.** The best remaining candidate is the query side: the
  shared Spark's `bge-m3` serving different query embeddings while another workload swapped
  models.
- **The lesson:** on a shared embedding host, gate a retrieval run on a repeated reference
  sample before it starts. The re-run did.

## Files

- `summary.json` — `analyze.py` output for the re-run.
- `summary-run1-void.json` — the void first run's.
- `results/*.jsonl.gz` — per question: both arms' rankings, the rerank report and the times.
