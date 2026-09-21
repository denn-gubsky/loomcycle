# Does the SHAPE of a retrieved set say whether the question is answerable?

2026-09-21. A hypothesis from the prompt-injection literature: a foreign insertion
shows up as a break in token homogeneity, so perhaps a retrieved set that actually
answers the question is more **homogeneous** than one that merely matches it.

If it held, it would give the thing the LongMemEval `_abs` result says we need — a
**deterministic runtime gate**, no model decision anywhere (see `../2026-09-20-longmemeval/`).

This is stage 1: the cheapest test that can kill it.

## Design — paired, cross-store

Questions come from the two LoCoMo stores' own transcripts (parsed out of the judge
prompts, taking only the `Question:` line — the gold is neither needed nor touched).
Each question is searched against **both** conversation stores:

- against **its own** store → the answer IS there → **positive**
- against **the other** store → it is NOT → **negative**

The same question is therefore both a positive and a negative, so phrasing and length
cancel exactly rather than being controlled for. Retrieval only: the single model call
is the query embedding (ollama/bge-m3, dim 1024, Spark — the model that embedded the
stores). `top_k=24`, matching `recallTraceTopK`, so the probe sees exactly what the
grant attaches. **231 questions × 2 stores = 462 searches, 0 errors.**

## Result

**Search always returns a full set.** 462 of 462 searches returned 24 turns, every
cross-store one included. That is the premise of the problem, stated literally.

| signal | AUC | mean positive | mean negative |
|---|---|---|---|
| **top1 (raw cosine)** | **1.000** | 0.6591 | 0.4271 |
| mean_k | 0.994 | 0.5825 | 0.3906 |
| **std_k (homogeneity)** | **0.913** | 0.0298 | 0.0130 |
| **perplexity T=0.05** | **0.897** (inverted) | 18.86 | 22.83 |
| nqc | 0.717 | 0.0529 | 0.0335 |
| perplexity, raw | 0.282 | 23.92 | 23.99 |

The hypothesis is **right about the sign**: relevant sets are measurably tighter, and
lower perplexity does mean answerable.

## ⚠️ But three things matter more than those numbers

**1. AUC = 1.000 is a saturated instrument, not a success.** Cross-store negatives are
entirely off-topic — different people (Caroline/Melanie vs Jon/Gina) — so raw cosine
separates the classes with **no overlap at all**: positives [0.5017, 0.7960], negatives
[0.3267, 0.5446]. Nothing can be ranked against a metric that has already saturated.
This was flagged before the run; the magnitude was not.

**2. Homogeneity is largely a PROXY for the score, not independent evidence.**

- correlation with top1: perplexity **r = −0.614**, std_k **r = +0.423**
- narrow the band so the score is less decisive and homogeneity falls away much faster
  than the score does: std_k **0.913 → 0.697**, nqc **0.717 → 0.540** (chance),
  perplexity **0.897 → 0.655**, while top1 holds at **0.996**

Tight sets score high; that is most of why tightness looks predictive here.

**3. The true overlap band holds 2 positives against 12 negatives.** This corpus cannot
answer the real question, so nothing here should be read as a verdict.

## Implementation note worth keeping

**Perplexity over raw normalised cosines is dead** — 23.92 vs 23.99 against a ceiling of
24. Cosines occupy too narrow a range for entropy to resolve anything; it needs a
temperature (T=0.05 works, T=0.01 over-sharpens). Anyone implementing this will
otherwise bury the metric on its first run.

## Verdict: survives, not yet useful

The decisive test is the real `_abs` slice, where `top1` **cannot** saturate — `_abs`
questions are on-topic by construction, which is exactly what makes them hard. That is
also the regime where this data says homogeneity is weakest, so expectations should be
low. But it is not a refutation: "on-topic without an answer" is structurally different
from "off-topic with a lower score", and only the first is the real target.

## Files

- `probe.py` — paired cross-store retrieval dump
- `analyze.py` — signals + AUC (Mann-Whitney)
- `probe.yaml` — retrieval-only rig (embedder pinned to the store's own model)
- `summary-qpp.json` — all 462 measurements
