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

## Amendment 1 — 2026-10-05 17:45, after the first run, before any re-run

**The first run is void** by instrument check 2. Its `none` arm scored R@5 **0.7781**
against phase M's 0.8604, outside the ±0.02 tolerance. What was looked at:
- the check values;
- the two arms' R@ figures in `summary.json`;
- the diagnosis below.

**Diagnosis:**

1. **The code is not the cause.** On the SAME store, the Sept 30 binary (`dd9b58c1`, the
   decision probe's) and the shipped binary (main after #1619) now return identical
   rankings for 100 of 100 reference questions. A `git bisect` over the 98 commits
   between them found every tested commit identical to that reference.
2. **The drift was transient, in the store's state during the run, not its contents.**
   The first run's `none` rankings, compared with what the same store returns now:
   - 5 of 16 sampled questions identical among the first 500 the run searched;
   - 18 of 21 in the next 500;
   - 20 of 20 thereafter.
   The early figure (≈ 0.78) is close to phase M's no-header arm (0.768).
3. **What it was is not established.** These were ruled out:
   - the index texts are correct and complete (13,072 chunks embedded, headers present);
   - embedding is synchronous at write;
   - there is no approximate vector index (the vector leg is an exact scan);
   - autovacuum analyzed the table before the run;
   - the default ranking is pure semantic.
   The remaining candidate is the query side: the Spark's `bge-m3`, shared with another
   workload that was swapping models, serving query embeddings that differed during the
   first part of the run. This cannot be verified after the fact.

**Change.** Both arms are re-run, in full, on the same store, which is now settled.
Before the re-run starts, a **stability gate** must pass:
- 50 reference questions are searched (`none`) twice, at least 5 minutes apart;
- both passes must be identical to each other, and ≥ 95% identical to the `dd9b58c1`
  reference;
- if the gate fails, the re-run waits, and the gate is repeated.

The first run's results are kept beside the re-run (`built-run1.jsonl`) but are not
scored. Nothing else changes: the hypotheses, thresholds and checks stand. Check 2 still
applies to the re-run.
