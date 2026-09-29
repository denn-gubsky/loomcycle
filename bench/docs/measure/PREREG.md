# RFC DM phase M — the shipped header and rerank on ConditionalQA (pre-registration)

Written and committed **before** any ingestion into the measured stores and before any
search. Nothing below may change after the first result is seen; any change is an
amendment, dated and committed before the run it affects. A mechanics smoke test (3 pages,
4 questions, a separate throwaway server and database, never scored) ran first and was
discarded.

## Question

RFC DM shipped two retrieval features, measured on QASPER and Natural Questions **as
text prepended to the chunk body**:
- an **index-only header**, `"<document title> — <heading path>"`, on by default;
- an opt-in **listwise rerank**.

Phase M measures the **built feature** instead of the probe's stand-in, on a corpus neither
was designed on. The questions are whether the header still earns its default, and
whether answers hold now that the reader no longer sees the header text.

## Corpus

**ConditionalQA v1.0** (github.com/haitian-sun/ConditionalQA; research use; `dev` + `train`,
since nothing is trained).
- 652 UK government guidance pages (gov.uk) with their real h1–h3 heading trees.
- Headings repeat heavily across pages: "Overview" 415 times, "Eligibility" 168, "How to apply" 131. This is the case the header exists for.

`prepare.py` builds:
- **One Document per page, twice.**
  - `header` keeps the real title and headings.
  - `plain` has the same text with the page and its headings **numbered**. The feature drops a title with no letter from the header (its own rule), so every plain chunk is indexed under its content alone.
- **13,724 sections**: section 0 is the page's text before its first heading, then one section per heading.
- **2,515 questions**, every answerable one whose evidence is found in the page. 105 are `not_answerable` and 3 have evidence not found.
  - A question's gold is every section holding its evidence (mean 1.9).
  - The **query** is the question's scenario followed by the question: the way the dataset poses it, and what a user describing their situation would send.

## Arms (`bench/docs/measure/loomcycle.yaml`, loomcycle `main` @ 653a6497)

| arm | store | rerank |
|---|---|---|
| `plain` | `cqa-plain` (no header) | off |
| `header` | `cqa-header` (the shipped header) | off |
| `header_rr` | `cqa-header` | on (the shipped defaults: 20 candidates × 1,200 chars) |

- **Store:** each store is ONE pooled user scope holding all 652 pages, so finding the page is part of every search. It is Postgres + pgvector, hybrid vector + full-text.
- **Search:** every arm runs `Document op=search` with limit 10.
- **Models:**
  - Embedder `bge-m3`, on the Spark's Ollama.
  - Reranker and reader **`qwen3.8:latest`**, thinking off, on the Spark.
  - `qwen3.6`, the NQ and PolicyQA model, CUDA-faults on every reader-size prompt while another workload keeps `gpt-oss` on the same GPU (6 of 6 failed; `qwen3.8` 8 of 8 ok). The operator chose `qwen3.8` (2026-09-29).
  - So these are `qwen3.8` numbers, not comparable in absolute terms to NQ or PolicyQA. Each corpus is its own study.

**Recall** counts READABLE sections only: a bodyless heading is skipped in rank order, as
the reader skips it. R@k means a gold section is among the first k readable hits.

**Reader:**
- Setup: `answer.py`, on a **pre-drawn subset of 800 questions** (`reader_subset.json`, `random.Random(20260929).sample`). The reader gets the arm's top 5 readable sections.
- Rendering: every excerpt is rendered the same way in every arm, **from the corpus**, as `<page title> > <heading path>` plus the text. The plain store's numbers exist only in its index; what an agent reads is the real page. So arms differ only in **which** sections were retrieved.
- Passes: three per arm (seeds 0, 11, 23; temperature 0). The per-question score is the mean over passes.
- Oracle: one pass on the gold sections (at most 5) gives the ceiling.
- Scoring: EM and SQuAD-style token F1, the max over the question's gold answers. No judge.

**The rerank arm runs one pass.** On PolicyQA, three rerank passes over 400 questions
differed on one question, so a second pass buys nothing but GPU time.

## Hypotheses

**Co-primary** (retrieval, all 2,515 questions; α = 0.025 each):

- **H1** — `header` beats `plain` on **R@5**. Exact two-sided McNemar on paired hits; holds
  iff p < 0.025 **and** `header` has more discordant wins.
- **H2** — `header_rr` beats `header` on **R@5**. Same test.

**Secondary** (answers, the 800-question subset; paired bootstrap, 10,000 resamples, seed 1):

- **H3** — answers do not drop with the header: `header` − `plain` mean F1 has a 95% CI
  lower bound **above −0.02** (non-inferiority, margin 0.02).
- **H4** — `header_rr` beats `header` on F1: the CI lower bound is above 0.

Reported, not tested: R@1/3/10 and MRR@10 per arm; EM differences with their CIs; the
oracle ceiling.

## Instrument checks (each can fail; a failed check voids the hypotheses that depend on it)

1. Ingestion: every page's imported tree matches prepare.py's sections, title by title,
   in both stores. `ingest.py` aborts otherwise. Voids all.
2. The plain store's titles carry no letter (so no plain chunk has a header). Voids H1, H3.
3. `plain` R@5 is more than **10× chance**. Chance is computed from the corpus as
   `min(1, 5·|gold| / readable sections)` per question, about 0.0007. This is a floor
   check that the pipeline retrieves at all, not a strength claim. Voids all.
4. The rerank reports `reranked: true` on ≥ 95% of `header_rr` searches. Voids H2, H4.
5. The oracle's F1 is above `plain`'s: the reader reads its excerpts. Voids H3, H4.
6. Every (arm, pass, question) in the reader subset has an answer: no reader call left
   failed. Voids H3, H4.

## Decision

- **H1 holds:** the header's default ON is confirmed on a second corpus, through the
  built feature.
- **H1 fails:** recorded as the header not transferring to this corpus. Whether the
  default changes is an operator decision this result informs; nothing changes
  automatically.
- **H2 holds:** the rerank is documented as measured through the built feature. **H2
  fails:** documented as corpus-dependent.
- **H3 fails:** the header's effect on answers is investigated before it is recommended
  further.
