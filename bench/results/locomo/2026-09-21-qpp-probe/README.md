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

---

# Stage 2 — the real `_abs` slice. The hypothesis FAILS; the plain score survives.

2026-09-21. The 130-instance LongMemEval subset from `../2026-09-20-longmemeval/`
(100 answerable + **all 30 `_abs`** — 30 is the whole oracle corpus, not a sample),
re-ingested from scratch as chats with the trace index on, `consolidate-passes=0`.
**Retrieval only: no answerer, no judge.** The one model call is the query embedding.
130 of 130 measured, 0 errors, 0 empty indexes. Harness arm: `-retrieval-dump`.

## Result

| signal | AUC | p | mean answerable | mean `_abs` |
|---|---|---|---|---|
| **mean_k** | **0.786** | <0.0001 | 0.4999 | 0.4325 |
| **top1 (raw cosine)** | **0.783** | <0.0001 | 0.6507 | 0.5693 |
| std_k (homogeneity) | 0.672 | 0.0044 | 0.0741 | 0.0597 |
| **perplexity T=0.05** | **0.589** | **0.14** | 8.55 | 9.87 |
| perplexity raw | 0.576 | 0.21 | 18.85 | 20.65 |
| nqc | 0.557 | 0.34 | 0.1515 | 0.1393 |
| gap_top1_mean | 0.555 | 0.36 | 0.1508 | 0.1368 |
| perplexity T=0.01 | 0.542 | 0.49 | 1.90 | 1.79 |

Criterion was fixed before the run: **AUC ≥ 0.75 to be worth building on.** Only the two
plain score means clear it. With eight signals tested, Bonferroni is 0.05/8 = 0.00625 —
`mean_k`, `top1` and `std_k` survive it; nothing else comes close.

## ⚠️ The homogeneity hypothesis does not survive contact with `_abs`

**Perplexity is directionally right and statistically nothing**: answerable sets *are*
tighter (8.55 vs 9.87), but AUC 0.589 at **p = 0.14**. On LoCoMo's off-topic negatives
the same signal scored 0.897. The entire apparent power was the easy corpus.

**And conditional on the score it carries nothing at all.** Split at the median top1 and
re-test perplexity within each half:

| half | n (ans/`_abs`) | AUC (lower perplexity = answerable) |
|---|---|---|
| low top1 — the ambiguous region, where a second signal would matter | 40 / 25 | **0.394** |
| high top1 | 60 / 5 | 0.540 |

In the half where it would have to do the work, it is **inverted**. This is stage 1's
warning confirmed on the real task: homogeneity looked predictive because tight sets
also score high, not because tightness is independent evidence.

`std_k` is the one homogeneity variant that stays significant (0.672, p = 0.0044) — but
it is below the gate and blocks **1 of 30** `_abs` at an operating point that keeps 96%
of the answerable slice. That is not a lever.

## What DOES work, modestly: gate on the plain top-1 score

The single 95%-retention operating point undersells it — the curve is steep just below
it. Estimated effect on the measured L2 arm (+24.5pp answerable, −26.7pp `_abs`):

| keep answerable | top1 threshold | `_abs` blocked | estimated net |
|---|---|---|---|
| 100.0% | 0.4680 | 10.0% | answerable +0.0pp, `_abs` +2.7pp |
| 97.0% | 0.5078 | 16.7% | answerable −0.7pp, `_abs` +4.4pp |
| **94.0%** | **0.5456** | **43.3%** | **answerable −1.5pp, `_abs` +11.6pp** |
| 89.0% | 0.5720 | 60.0% | answerable −2.7pp, `_abs` +16.0pp |
| 82.0% | 0.5940 | 66.7% | answerable −4.4pp, `_abs` +17.8pp |

A deterministic threshold on a number the retriever already computes and the attach path
already discards. No model decision, no extra call — exactly the shape of control the
LongMemEval result said was needed.

⚠️ **These are estimates and the thresholds are IN-SAMPLE.** Two things must be true
before any of it is a result: (1) the trade is linear only if blocked questions are
average, and they are not — they are the weak-retrieval ones, where L2's gain was
probably smaller anyway; (2) the threshold was chosen by looking at this answerable
distribution, with 30 `_abs` total, so 43.3% is 13 questions. It needs held-out
validation and then an end-to-end arm. Nothing here replaces running it.

## The honest summary

The idea was good and the cheap test was right to run: it cost one afternoon and killed
a plausible mechanism that would otherwise have been built. **Set homogeneity does not
tell you whether a retrieved set answers the question — the plain similarity of the best
hit does, about as well as anything here, and it was already on the wire.**
