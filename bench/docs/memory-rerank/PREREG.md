# RFC DR P2 — does reranking memory recall improve answers? (pre-registration)

Written and committed **before** any store is built or any question is answered. Any
later change is a dated amendment, committed before the run it affects.

## Question

RFC DR P1 (#1630) lets the rerank reorder an agent's memory recall
(`memory.reranker.sources: [facts, notes]`). The evidence for it (#1571) was
retrieval-only and on raw turns: recall@5 0.588 → 0.74.

P2 measures what P1 did not:
- **(a)** recall over **consolidated facts**, not raw turns;
- **(b)** whether reranking that recall improves the **answers** an agent gives;
- **(c)** whether a deeper candidate pool (40 vs 20) lifts multi-hop, where 46% of the
  evidence sat outside the 20.

(a) and (b) are one experiment: the answerer's recall runs over the consolidated store.

## Setup — fully local (operator decisions, 2026-10-05)

- **Pipeline:** the LoCoMo rating pipeline (`bench/synthesis/ingest.yaml`,
  `lme_gate_arms.yaml`, `locomo_rating.yaml`, `LOOMCYCLE_PRESETS=base,memory`),
  layered with `measure.yaml`.
- **Models:** every model is **`qwen3.8:latest`** on the Spark, with thinking off and a
  32K window: the extractor, ontologist, memory judges, the answerer
  `locomo/orn-l2k24-sp` (with `recall_attach_traces`), and the **grading judge**.
- **No cloud call.** The A/B is valid because both arms share every model. The absolute
  numbers are **not** comparable with the 0.706 rating (different answerer and judge).
- **Reranker:** `kind: decision`, `nimble` on the Spark, `sources: [facts, notes]`.
- **Embedder:** `bge-m3` on the Spark.
- **Questions:** LoCoMo, all 10 conversations, `-sample-questions 40` per conversation
  (the harness's deterministic stratified draw), which gives **400 questions**,
  identical in both arms.
- **Order, per conversation (one resident at a time, as the harness requires):**
  - **A — build** (trace index ON): ingest as chats, consolidate (`-retrieval-dump`, so
    nothing is answered).
  - **B-off** (trace index OFF): `-answer-only`, the answerer WITHOUT `memory_rerank`.
  - **B-on** (trace index OFF): `-answer-only`, the same answerer WITH
    `memory_rerank: {enabled: true}` (`rerank-on.yaml`, the only difference).

## Hypotheses

**Primary — H1:** B-on's **binary-J** (judge says `correct`) beats B-off's over the 400
paired questions. The test is an exact two-sided McNemar on per-question correctness; it
holds iff p < 0.05 and B-on has more discordant wins.

**Reported, not tested:**
- partial credit (correct 1, partial 0.5);
- per-category binary-J;
- the `NOT_FOUND` rate per arm;
- answer latency p50 per arm;
- nimble rerank calls per answered question.

**(c), secondary — pool depth.** Retrieval only, on raw turns as in #1571, over the 282
multi-hop questions:
- **Arms:** the listwise rerank (`qwen3.8`, the shipped code via `bench/cmd/rerankpool`)
  shown the first 20 vs all 40 of a 40-row pool.
- **S1:** 40 beats 20 on multi-hop recall@5 (paired bootstrap 95% CI lower bound > 0).
- **Decision:** `candidates` stays 20 unless S1 holds with a gain ≥ 3 points.

## Instrument checks (each can fail; a failed check voids what depends on it)

1. **Coverage, per conversation (voids that conversation in H1):**
   - the build reports `consolidated in N pass(es)`;
   - it never reports "work still queued" or "SKIPPED";
   - ≤ 2% of `memory/extractor` runs failed.
2. **The arms differ only by the rerank (voids H1):**
   - B-off makes **no** nimble call;
   - B-on makes ≥ 1 nimble call for ≥ 80% of the questions where the answerer used
     recall at all.
3. **The judge reads (voids H1):** unparsed verdicts are ≤ 2% in each arm.
4. **Pairing (voids H1):** both arms graded the same 400 questions.

## Decision

- **H1 holds:** RFC DR documents memory reranking as improving answers, with the effect
  size. The guide recommends `sources: [facts, notes]` alongside `memory_rerank`.
- **H1 fails** (no significant difference): the retrieval gain is recorded as not
  reaching answers in this pipeline. The setting stays available and is documented with
  both results.
- **H1 reverses:** investigate before recommending.

## Amendment 1 (2026-10-05) — the (c) harness and its checks

Written after (b) started and before any (c) pool was fetched. It changes nothing in (b).

- **Harness:** `depth.sh` + `depth.py`. A fresh store (its own database) is built as
  #1571's was: `bench/cmd/locomo -mode=ingest -scope user`, with questions from
  `-mode=convert`.
  - Each multi-hop question's pool is `POST /v1/_memory/search` top_k **40**, no rerank.
  - The 20-arm is the **first 20 of that same pool**. The two arms rerank the same head
    and differ only by the 20 rows behind it.
  - Both arms run `bench/cmd/rerankpool` with #1571's `reranker.yaml` (qwen3.8 listwise,
    16K window).
- **Order:** (c) runs **after** (b) has finished. Both use qwen3.8 on the Spark, and
  sharing it would distort (b)'s latency.
- **S1's interval:** the paired bootstrap is two-sided 95% (10,000 resamples, seed 1)
  over the per-question difference in recall@5.
- **Checks (each voids S1):**
  5. **282 multi-hop questions** are pooled.
  6. **Rerank applied:** both arms report `reranked` on ≥ 95% of questions.
  7. **The store is #1571's:** the 20-arm's unreranked recall@5 is within ±0.02 of
     #1571's multi-hop `none`, 0.332.
