# RFC DM C3 — do derived search units beat the header? (pre-registration)

Written and committed **before** any ingestion, generation or search. Nothing below may
change after the first result is seen; any change is an amendment, dated and committed
before the run it affects.

## Question

RFC DM Design C adds optional derived search units — a description, claims and
questions a model writes about each chunk — on the intuition that they pay off where
readers ask in words the text does not use (policies, manuals, FAQs). On QASPER's
development papers they beat the header alone on answers (+0.03 F1, not significant,
never replicated) and lost to header + rerank. C3 measures units **on the kind of
document they are for**, through the **shipped feature** (C1 resolution + C2
generation), before anyone relies on them.

## Corpus

**PolicyQA test split** (MIT; github.com/wasiahmad/PolicyQA): 20 website privacy
policies (OPP-115), 500 segments, questions written by legal experts in their own
words. `prepare.py` builds:

- one Document per policy, in its **own user scope** (`pqa-NN`), named `/policies/pqa-NN`;
  each segment a section headed by its number (a letterless heading is dropped from
  the index header, so every chunk is indexed as `<site>` + its text — the header
  arm gains the document title and nothing invented);
- **2,643 (policy, question) pairs**. PolicyQA questions are generic and repeated, so a
  question means "in this policy, which segment answers it"; its gold is every segment
  of that policy it was asked of (median 1, mean 1.55). Chance R@5 ≈ 0.2.

## Arms (the built feature, `bench/docs/units/loomcycle.yaml`)

All arms run `Document op=search`, `limit: 10`, bare question as query, within the
question's policy scope. Embedder `bge-m3`; generator and reranker `qwen3.6:latest`
(`effort: low`), all on the TrueNAS Ollama host; Postgres + pgvector store (hybrid
vector + full-text).

| arm | units | rerank |
|---|---|---|
| `header` | off (`memory_units: false`) | off |
| `units` | on | off |
| `header_rr` | off | on (20 candidates × 1,200 chars) |
| `units_rr` | on | on |

Units are generated once, before any search, by `POST /v1/_document/derive_units`
over every policy (the `/policies` subtree is marked; all three kinds).

## Hypotheses (co-primary, α = 0.025 each)

- **H1** — `units` beats `header` on **R@5**, over all 2,643 pairs. Exact two-sided
  McNemar on paired hits; holds iff p < 0.025 **and** the units arm has more
  discordant wins.
- **H2** — `units_rr` beats `header_rr` on **R@5**, over the pre-drawn subset of
  **400 pairs** (`rr_subset.json`, `random.Random(20260928).sample`). Each rerank arm
  runs **three passes**; a pair is a hit when **≥ 2 of 3** passes hit. Same test.

Secondary (reported, not tested): R@1, R@10, MRR@10 per arm; the no-rerank arms on the
subset; which unit kinds found the gold hits; rerank applied rate; generation time and
units per chunk.

## Instrument checks (each can fail; a failed check voids the affected hypothesis)

1. Ingestion: every one of the 500 segments maps to a chunk (`ingest.py` aborts
   otherwise).
2. Generation: units exist for ≥ 95% of chunks with prose, with 0 failures left after
   the pass.
3. The `header` arm returns **no** `matched_unit` (units off really is off).
4. The `units` arm finds **≥ 1** gold hit through a unit (units reachable at all).
5. The `header` arm's R@5 is **more than twice chance** (the pipeline retrieves).
6. The rerank arms report `reranked: true` on ≥ 95% of searches.

## Decision

- H1 holds → units are documented as a measured benefit on policy-like text (R@5), with
  the effect size.
- H1 fails → the option stays documented as **"no measured benefit yet"** (RFC DM C3),
  with this result.
- H2 says whether units still add anything once the rerank is on; it does not change
  the H1 decision.

## Amendment 1 — 2026-09-28, before any result

The first generation pass (dry run and real) generated **nothing**: every policy came
back `documents_opted_in: 0`. Cause, a bug in the feature under test, not in the
probe: `import_md` names a document `/documents/<title>` and the probe's `set_path`
adds `/policies/pqa-NN`; the pass considered only the alphabetically first name, so no
imported document matched the marked `/policies` subtree. Fixed on the C2 branch
(`12f5022f`, with a regression test that fails without it). The server is rebuilt from
that commit; ingestion (unchanged by the fix) is kept; generation and every search run
after this amendment. Nothing else changes. No search had been run.
