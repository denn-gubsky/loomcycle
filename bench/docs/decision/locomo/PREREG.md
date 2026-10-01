# Decision-model reranking of MEMORY recall on LoCoMo (pre-registration)

Written and committed **before** any pool is fetched or any rerank is run. Any later change
is a dated amendment, committed before the run it affects.

## Question

**No rerank touches memory recall today.** The shipped rerank refuses any search that
cannot return document chunks (`not_a_document_search`), because it was measured on
documents.

On ConditionalQA a decision model (`nimble`) matched the shipped listwise rerank on
documents (`../PREREG.md`, `bench/results/docs/2026-09-30-decision-rerank/`), and RFC DQ
proposes it for documents.

This probe asks the question RFC DQ left open (its decision 4): **does reranking help
memory recall at all, and does a decision model do it?**

## Corpus and pools

- **LoCoMo** (`snap-research/locomo`, CC BY-NC: fetched at run time, never committed), all
  10 conversations, ingested by the LoCoMo harness (`bench/cmd/locomo -mode=ingest -scope user`).
  - One memory row per turn, keyed by its dia_id, in user scope `locomo-<sample_id>`.
  - The row text is `[<session date>] <speaker>: <text>`.
  - 5,882 rows, embedded with `bge-m3`.
- **Questions:** the harness's own `-mode=convert` output. Categories 1–4, evidence-less
  questions dropped by the harness: **1,535 questions**.
  - Ground truth is each question's evidence turn ids, so no judge is involved.
  - The category is joined from the dataset by (conversation, question).
- **Pools:** each question's pool is `POST /v1/_memory/search`, top_k 20, no rerank.

## Arms (every model on the Spark's Ollama 0.35.0)

| arm | model | method |
|---|---|---|
| `none` | — | the pool order |
| `qwen_list` | qwen3.8 | the SHIPPED listwise rerank, run by `bench/cmd/rerankpool`: `reranker.Build` and `memory.RerankTexts`, the measured prompt and repair, unchanged; phase M's settings |
| `nimble_choice` | `nimble` | one `choice` call per question, as in the ConditionalQA probe |
| `nimble_point` | `nimble` | one `noul` call per (question, turn), as in the ConditionalQA probe |
| `tev1_point` | `tev1:4b` | as `nimble_point` |

- **Candidate text:** the stored turn text, cut to 1,200 characters (no turn is that long).
- **Calls:** the same calls, wording and tie rule as the ConditionalQA probe
  (`../rerank.py`).

## Metrics

Per question:
- recall@k = the share of its evidence turns in the top k;
- hit@k = any evidence turn in the top k;
- the reciprocal rank of the first evidence turn within the 20.

**Primary metric: recall@5.** LoCoMo's weak categories (multi-hop, open-domain) have
several evidence turns, and recall says whether all of them reach the reader, not just
one.

## Hypotheses

**Co-primary** (1,535 questions; paired bootstrap of per-question recall@5 differences,
10,000 resamples, seed 1; Bonferroni over two, so 97.5% CIs):
- **H1** — `nimble_choice` beats `none` on recall@5: the CI lower bound is above 0.
- **H2** — `nimble_point` beats `none` on recall@5. Same test.

**Secondary** (reported whatever they show):
- **S1:** `qwen_list` beats `none`. Does any rerank help memory?
- **S2:** `tev1_point` beats `none`.
- **S3 / S4:** each `nimble` arm is not worse than `qwen_list` by more than 0.03.
- **Also:** recall@1/@10, hit@1/@5 and MRR@20; every metric by category; p50/p95 latency.

## Instrument checks (each can fail; a failed check voids what depends on it)

1. **The corpus and pools exist:** 1,535 questions, with ≥ 99% of pools holding 20
   candidates. Voids all.
2. **The `none` arm reproduces the harness's published baseline:** recall@10 0.6675 (same
   corpus, same embedder), within ±0.01. Otherwise the pools are not that baseline's.
   Voids all.
3. **`qwen_list` reports `reranked: true` on ≥ 95% of questions.** Voids S1, S3, S4.
4. **No decision call is left failed.** An aborted arm is void.

## Decision

- **H1 or H2 holds:** memory rerank with a decision model gets its own RFC (RFC DQ decision 4).
  - The better arm, its latency, and the categories it helps go into the RFC's case.
- **Neither holds, but S1 does:** reranking helps memory and a decision model does not. A
  listwise memory rerank is considered separately.
- **Neither holds, and S1 fails:** reranking memory recall is recorded as not helping on
  LoCoMo. The document-only scope of the shipped rerank stands, now measured.
