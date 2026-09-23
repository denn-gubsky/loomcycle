# The cascade on a HONEST negative class — the mechanism holds, the routing is corpus-dependent

2026-09-23. Three LoCoMo conversations, **60 answerable + 60 adversarial** (category 5:
444 of its 446 questions have a null answer AND are built as minimal pairs of answerable
ones — *"what did **Caroline** realize after **her** race"* beside *"what did **Melanie**
realize after the race"*). 117 scored after 3 unparseable verdicts.

This replaces a first attempt that had to be thrown away; both faults are recorded at the
bottom because each would silently produce a plausible table.

## The finding that changes the recommendation

**Cosine routes well on LongMemEval `_abs` and poorly on minimal pairs**, and the reason
is visible rather than inferred:

```
adversarial questions by cosine decile (low → high)
  LoCoMo minimal pairs :  9  9  6  5  5  8  6  4  2  5    — 18 of 59 in the bottom two
  LongMemEval `_abs`   : 16 of 29 in the bottom two deciles
```

Minimal pairs retrieve on-topic material **by construction**, so their cosine is not low.
A threshold therefore cannot tell where the verifier is needed — which is exactly what the
cascade asks it to do.

## All four strategies, same 117 questions, threshold 0.595

| strategy | answerable | adversarial | verifier calls |
|---|---|---|---|
| L2 — always attach | 0.7759 | 0.7288 | 0 |
| pure threshold | 0.7759 | 0.7458 | 0 |
| cascade | 0.7759 | 0.7458 | 17 |
| **verifier everywhere** | 0.7672 | **0.8136** | 117 |

**The verifier is the only thing that moves the adversarial slice**: +10/−5 paired
(p = 0.302), at a cost of **+0/−1** on the answerable slice — near-free here, unlike
LongMemEval where it spent 23 correct answers.

And it is again the better discriminator, at equal withhold volume:

| | catches adversarial | recall | precision |
|---|---|---|---|
| **verifier** | **57 / 59** | **0.966** | **0.792** |
| threshold, same volume | 43 / 59 | 0.729 | 0.597 |

## ⭐ On the COST axis the cascade is favourable — which is the axis it was designed for

Evaluating it on accuracy alone was the wrong test. The whole prize (L2 → verifier
everywhere) is **+0.0847 on the adversarial slice, five questions**. What each call buys:

| calls | % of calls | adversarial | % of the prize | answerable |
|---|---|---|---|---|
| **11** | **9%** | 0.7627 | **40%** | 0.7759 — no loss |
| **46** | **39%** | 0.7797 | **60%** | 0.7759 — no loss |
| 93 | 79% | 0.7797 | 60% | 0.7672 |
| 117 | 100% | 0.8136 | 100% | 0.7672 |

Nine percent of the calls buy 40% of the prize and cost the answerable slice nothing. The
expensive decider is not needed on every question — the cascade's premise.

⚠️ **But this curve cannot be read as a frontier.** At 50% of the calls it returns 0% of
the prize, and at 39% it returns 60%. That is scatter, not a curve: the prize is five
questions of fifty-nine, so neighbouring points differ by one or two. The data is
*consistent with* the efficiency claim and does not *establish* it.

## What transfers and what does not

**Transfers** — a model reading the material discriminates better than the geometry of its
scores: 57/59 here, 29/29 on LongMemEval, against 43/59 and 24/29 for a threshold at equal
volume. Two corpora, two negative-class constructions, same direction.

**Does not transfer** — the routing. Whether cosine can cheaply identify *which* questions
need the verifier depends on how the corpus's unanswerable questions distribute over
cosine, which is a property of the corpus. A previous conclusion ("the cascade beats both
arms") was drawn on LongMemEval and does not hold as a general rule.

**Decision recorded 2026-09-23:** the cascade is adopted as the shape to implement in the
memory layer — cheap router, expensive decider, verifier only where the router is unsure.
The mechanism is sound on both corpora. ⚠️ The operating point is NOT settled by this data
and must be calibrated on the deployment's own distribution: the threshold that routes
well here is not the one that routes well on `_abs`, and the prize here is five questions.

## ⚠️ Two faults that destroyed the first attempt

**1. The dump was taken last.** The live trace indexer writes each answering run's prompt
into the index as a user turn, so the dump — an off-run REST search with no own-run filter
— found every question matching *itself* at cosine 1.000. It also poisoned the L2 arm: the
polluting row was written by the *control* arm's run, so L2's own-run filter never fired.
Fixed by taking the dump **before any answering**, and by running both arms with
`LOOMCYCLE_MEMORY_TRACE_INDEX=0` (the flag gates writes only; reads were verified working
with it off before the arms were launched).

**2. A CUDA fault was counted as the model refusing to answer.** 116 of 120 verdicts came
back empty. The runner returned `None` for both a transport failure and an unparseable
answer, so the two were reported as one number — and the fix would have been aimed at the
prompt. They were `ollama-local 500: CUDA error: an illegal memory access was encountered`,
raised by four concurrent 8 KB prompts against one 35B model; the same questions passed on
retry. Fixed by separating the counts, retrying provider faults (they arrive as an SSE
`error` event on a **200**, invisible to the HTTP client), and halving concurrency. Re-run:
**0 request failures, 3 unparseable.**

⚠️ `LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS` does **not** reach the consolidator — the
`memory` bundle pins `run_timeout_seconds: 1500` per-agent, and per-agent wins. It cannot
simply be raised: the same file requires it stay under `lease_ttl_ms` (1800 s). One
conversation (conv-26, 419 turns) exceeds 25 minutes of consolidation and was dropped.

## Files

`summary-arm-control.json` / `summary-arm-l2.json` (120 each) · `summary-verifier-verdicts.json` (117 parseable)
· `summary-retrieval-shape.json` · assembled with `../2026-09-21-gate-arms/cascade.py`
