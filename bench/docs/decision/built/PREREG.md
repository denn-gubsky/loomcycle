# RFC DQ P3 — the shipped decision reranker, measured through the built feature

Written and committed **before** the store is rebuilt or any search is run. Any later
change is a dated amendment, committed before the run it affects.

## Question

The decision probe (`bench/docs/decision/PREREG.md`, #1544) measured `nimble` reranking
ConditionalQA pools from a Python client. RFC DQ P2 (#1619) shipped it as
`memory.reranker.kind: decision`. P3 asks whether the **built feature** delivers what the
probe measured: the same ranking quality and the same speed, through `Document op=search`
and an agent's `memory_rerank`.

## Setup

- **Corpus:** ConditionalQA. Phase M's prepared data (`bench/docs/measure/prepare.py`):
  652 gov.uk pages, the header store rebuilt by `bench/docs/measure/ingest.py`, all
  **2,515 questions**.
- **Runtime:** loomcycle `main` after #1619 merges. Config in `loomcycle.yaml`:
  - embedder `bge-m3` and reranker `nimble`, both on the Spark's Ollama;
  - `kind: decision`, `timeout_ms: 120000`;
  - the per-agent rerank at its shipped defaults (20 candidates × 1,200 characters).
- **Arms**, both `Document op=search` with limit 10 in the same store:
  - `none` — an agent without `memory_rerank`;
  - `decision` — the same search by an agent with `memory_rerank: {enabled: true}`.
- **Run order:** the two arms of a question run back to back (`run.py`), so their time
  difference is the rerank under the same load.
- **Recall** counts readable sections (as in phase M and the probe).

## Hypotheses (from RFC DQ P3, fixed before the run)

- **H1 — quality:** the built `decision` arm's R@5 is **≥ 0.914**, which is the probe's
  0.944 minus 0.03.
  - Point estimate over all 2,515 questions.
  - The Wilson 95% interval is reported beside it.
- **H2 — speed:** the median per-question rerank time (`decision` wall time minus `none`
  wall time) is **≤ 1,830 ms**, which is the probe's 1,464 ms p50 plus 25%, on the same host.

Reported, not tested:
- `decision` vs `none` on R@5 and R@1 (exact McNemar);
- R@3/10;
- p50/p95 times per arm;
- the rerank reasons.

## Instrument checks (each can fail; a failed check voids what depends on it)

1. **The rerank applies:** `reranked: true` on ≥ 95% of the `decision` searches. Voids H1
   and H2.
2. **The rebuilt store reproduces phase M:** the `none` arm's R@5 is within ±0.02 of
   phase M's `header` arm (0.8604). This is the same corpus, embedder and search; a rebuilt
   index reorders some pools, which is why the tolerance is not zero. Voids H1, which is
   compared with a number measured on another build of the store.
3. **Every question is answered by both arms:** `complete`. Voids H1 and H2.

## Decision

- **H1 and H2 hold:** the built feature delivers the probe's result. RFC DQ is done, and
  the Documents guide documents `kind: decision` with these numbers.
- **H1 fails:** the built feature ranks worse than the probe. Find the difference (the
  candidate text, truncation, the call) before recommending it.
- **H2 fails:** the built feature is slower than the probe. Find where the time goes
  before the guide quotes a speed.
