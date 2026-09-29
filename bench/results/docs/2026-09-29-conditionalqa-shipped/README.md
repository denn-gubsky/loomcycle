# RFC DM phase M — the shipped header and rerank on ConditionalQA (2026-09-29)

Pre-registration: `bench/docs/measure/PREREG.md`. It was committed before any ingestion
into the measured stores. Amendment 1 (a rerank timeout on a shared GPU) was committed
before any score was computed. Harness: `bench/docs/measure/`.

- **Runtime:** everything ran through the **built feature**, loomcycle `main` @ 653a6497: the index-only header, `Document op=search` and the `memory_rerank` step.
- **Store:** an isolated server with a throwaway Postgres + pgvector store.
- **Models:** `bge-m3` embeddings; `qwen3.8:latest` (thinking off) as reranker and reader, on the Spark's Ollama.
- **Corpus:** 652 gov.uk guidance pages and 2,515 questions. The query is the question's scenario followed by the question.

## Verdict

**The header and the rerank both hold on a third corpus, through the shipped code, and
answers improve rather than drop.**

| hypothesis | result |
|---|---|
| **H1** `header` > `plain`, R@5, 2,515 q | **HOLDS**: 0.8604 vs 0.7678 (+9.3pp), discordant 279 / 46, p = 7.9e-42 |
| **H2** `header_rr` > `header`, R@5 | **HOLDS**: 0.9332 vs 0.8604 (+7.3pp), discordant 202 / 19, p = 8.5e-40 |
| **H3** `header` F1 not below `plain` (margin 0.02), 800 q | **HOLDS**: +0.0425, 95% CI [+0.0214, +0.0634]. The header is not merely non-inferior; it improves answers |
| **H4** `header_rr` F1 > `header` | **HOLDS**: +0.0466, 95% CI [+0.0262, +0.0670] |

Per the pre-registered decision: the header's default ON is confirmed on a second
corpus through the built feature, and the rerank is documented as measured through the
built feature.

## Arms

| arm | R@1 | R@3 | R@5 | R@10 | MRR@10 | EM (800 q) | F1 (800 q) |
|---|---|---|---|---|---|---|---|
| `plain` | 0.4891 | 0.6990 | 0.7678 | 0.8350 | 0.6078 | 0.4996 | 0.5515 |
| `header` | 0.5877 | 0.7984 | 0.8604 | 0.9153 | 0.7040 | 0.5321 | 0.5940 |
| `header_rr` | 0.8115 | 0.9157 | 0.9332 | 0.9439 | 0.8647 | 0.5667 | 0.6405 |
| oracle (gold sections) | | | | | | 0.5900 | 0.6651 |

- **Reading the table:** recall is over READABLE sections (a bodyless heading is skipped in rank order).
- **Answers:** EM/F1 are the mean over three reader passes. The per-pass F1 values are:
  - `plain`: 0.546 / 0.551 / 0.558
  - `header`: 0.591 / 0.595 / 0.595
  - `header_rr`: 0.638 / 0.642 / 0.642
- **Answer effect sizes:** EM differences are +0.0325 (header − plain, CI [+0.012, +0.053]) and +0.0346 (rerank − header, CI [+0.014, +0.055]).
- **Against the ceiling:** with the rerank on, the reader gets within 0.025 F1 of the oracle.

## Instrument checks

| # | check | result |
|---|---|---|
| 1 | each imported tree matches the prepared sections, both stores | pass (652 / 652) |
| 2 | plain titles carry no letter | pass |
| 3 | `plain` R@5 > 10 × chance (0.00074) | pass (0.768) |
| 4 | rerank applied on ≥ 95% of `header_rr` searches | pass (100%, after Amendment 1) |
| 5 | oracle F1 above `plain` F1 | pass (0.665 vs 0.552) |
| 6 | every (arm, pass, question) answered | pass (8,000 / 8,000) |

**Amendment 1.** From about question 1,200 of `header_rr`, every rerank timed out at the
30 s default (243 of 1,453). Another workload on the shared Spark was swapping models and
evicting `qwen3.8`. The timeout was raised to 120 s and the timed-out searches were
re-run. A second sweep re-ran 25 that timed out again. The final arm has all 2,515
searches reranked. The rows reranked before the change are unchanged, since a longer
timeout only affects a call that would otherwise have timed out.

## Exploratory (post hoc, not tested) — where the header's gain comes from

| gold section's own heading | n | plain R@5 | header R@5 | discordant (header / plain) | p |
|---|---|---|---|---|---|
| used on ≥ 20 pages ("Overview", "Eligibility", …) | 782 | 0.799 | 0.841 | 55 / 22 | 2.2e-4 |
| rarer | 1,733 | 0.754 | 0.869 | 224 / 24 | 7.4e-42 |

The right **page** is among the top 5 for 0.910 of questions in `plain` and 0.932 in
`header`: +2.2pp.
- **Mostly within-page:** the header's gain comes mostly from picking the right **section within the page**, not from finding the page.
- **Distinctive headings gain most:** the gain is largest where the heading is distinctive (the most frequent among header-only wins: "If you lose your case", "Proof of identity", "If you cannot go to your assessment"). There the heading's own words match what the reader asks.
- **Generic headings gain less:** the generic, repeated headings the header was expected to disambiguate gain too, but less.

## Caveats

- These are `qwen3.8` numbers. `qwen3.6` (the NQ and PolicyQA model) CUDA-faulted on the
  Spark beside another workload's `gpt-oss`. The effects are comparable across corpora;
  the absolute values are not.
- The reader sees excerpts rendered identically in every arm, from the corpus. The answer
  effects therefore measure retrieval, not the reader seeing header text; the shipped
  header is index-only.
- ConditionalQA answers are often conditional ("yes, if…"). The metric scores the answer
  string only, not its conditions.

## Files

- `summary.json` — `analyze.py` output (the pre-registered analysis, unchanged).
- `results/*.jsonl.gz` — each arm's ranking per question.
- `answers/*.jsonl.gz` — every reader answer (oracle 1 pass, each arm 3 passes).
- `explore.py` — the post hoc breakdown above.

## Reproduce

```sh
cd bench/docs/measure
python3 prepare.py --src <ConditionalQA v1_0 dir> --out <probe>
cp reader_subset.json <probe>/
# start loomcycle with loomcycle.yaml (throwaway Postgres + pgvector; Ollama with bge-m3 + qwen3.8)
python3 ingest.py --probe <probe> --store header && python3 ingest.py --probe <probe> --store plain
for arm in plain header header_rr; do python3 search.py --probe <probe> --arm $arm; done
python3 answer.py --probe <probe> --arm oracle --pass-no 1
for p in 1 2 3; do for arm in plain header header_rr; do
  python3 answer.py --probe <probe> --arm $arm --pass-no $p; done; done
python3 analyze.py --probe <probe>
```
