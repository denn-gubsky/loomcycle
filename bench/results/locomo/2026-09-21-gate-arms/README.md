# Arm 0 and the double call: the gate works, the problem it was built for does not reproduce

2026-09-21. Four arms on the 130-instance LongMemEval subset (100 answerable + all 30
`_abs`), ornith-1.5:35b reader, `deepseek-v4-flash` judge, per-question outcomes kept.

A gate on the trace grant only ever decides one thing per question: attach the retrieved
turns or not. So the two states were measured — **control** (never attach) and **L2**
(always attach) — and every gate is assembled from them per question by whichever
decider is named. That buys every threshold at once instead of one run per threshold,
and the arms share their answerer draws, which makes the comparison **paired**.

## ⚠️ First, the premise did not reproduce

|  | control | L2 | Δ |
|---|---|---|---|
| answerable | 0.5606 (nf 0.250) | **0.8196** (nf 0.040) | **+25.9pp** |
| `_abs` | 0.9000 | **0.8667** | **−3.3pp — one question of 30** |

The lever replicates (+25.9pp against the +24.5pp of 2026-09-20). **The cost does not**:
27 correct refusals became 26. The previous run measured −26.7pp, eight questions.

The one material difference is that these agent definitions were **rebuilt** — the
originals were configured from a file in `/tmp` that an interrupted session cleared. The
prompt came from `bench/synthesis/ingest.yaml`'s answerer. What changed is not the size
of the problem but **whether it is there at all**, which is worth more attention than
anything below: an abstention cost that swings from 8 questions to 1 on a prompt change
is not a property of the mechanism, it is a property of the configuration.

## Arm 0 — threshold on top1

| threshold | withheld | answerable | `_abs` | vs L2 (ans) |
|---|---|---|---|---|
| 0.508 | 9 | 0.8351 | 0.8667 | +2/−0 |
| **0.542** | **19** | **0.8367** | 0.8667 | **+3/−0** |
| 0.567 | 25 | 0.8265 | 0.8667 | +3/−1 |
| 0.600 | 43 | 0.7806 | 0.9000 | +4/−6 |
| 0.666 | 88 | 0.6939 | 0.9333 | +5/−16 |

**Withholding weak retrievals improves the ANSWERABLE slice** — 0.8367 against L2's
0.8196, strictly dominant, not one question lost. Weak material is not merely useless to
the reader; it misleads it. That is a different benefit from the one the gate was built
for, and the only one available here.

⚠️ **+3/−0 is p = 0.125** (exact binomial). A consistent hint, not an established effect.

`_abs` only moves from threshold 0.600 up, where the answerable slice already pays six
questions for one.

## Arm 2 — a local model asked whether the material answers the question

`locomo/sufficiency`, ornith-1.5:35b, given exactly the set the threshold saw.

| arm | answerable | `_abs` | vs L2 |
|---|---|---|---|
| L2 | 0.8196 | 0.8667 | — |
| arm 0 @ 0.542 | **0.8367** | 0.8667 | ans +3/−0 |
| arm 2 (63 withheld) | 0.7938 | 0.9000 | ans **+5/−7**, `_abs` +2/−1 |

**As a gate the verifier loses** — below L2 on the answerable slice and mixed rather
than dominant.

## ⭐ But as a DISCRIMINATOR the verifier is clearly better, and that is the transferable part

Scored on the 125 questions with a parseable verdict, against the threshold **set to
withhold the same number** so the two are compared at equal cost:

| | catches `_abs` | recall | withholds answerable | precision |
|---|---|---|---|---|
| **verifier** | **29 / 29** | **1.000** | 34 / 96 | **0.460** |
| threshold @ equal volume | 24 / 29 | 0.828 | 39 / 96 | 0.381 |

**The verifier missed nothing.** Every one of the 29 unanswerable questions was caught,
including the minimal-pair kind that cosine is blind to by construction — it separated
*"Senior Software Engineer"* from *"Software Engineer Manager"* where the retrieved set
is nearly the same set. Better recall AND better precision than the geometry.

⚠️ n = 29, so a perfect recall has a 95% lower bound near 0.88, not 1.0.

**Why it still loses as a gate: it is calibrated far too conservatively.** It withholds
34 of 96 answerable questions, **23 of which L2 had answered correctly** — spending 23
right answers to protect a cost that, in this configuration, is one question.

## What this leaves

- **The discriminator question is answered**: a model reading the material beats the
  geometry of its scores, decisively and on exactly the cases the geometry cannot reach.
  That is the finding that survives a configuration change.
- **The economics are not**: the verifier's operating point is binary and far too eager.
  It needs a confidence-graded verdict, or a prompt that says "no" only when sure,
  before its recall can be spent without paying 23 correct answers for it.
- **And the target has to be re-established first.** Before building anything on this,
  the `_abs` cost needs to be measured on the configuration that will ship, because the
  same lever cost 8 questions in one configuration and 1 in another.

## Files

- `arm-control.json` / `arm-l2.json` — per-question outcomes, 130 each
- `verifier-verdicts.json` — `{question: contains_answer}`, 125 parseable of 130
- `retrieval-shape.json` — per-question top1/perplexity/etc (bodies dropped for size)
- `verify.py` — the verifier runner ⚠️ note its `max_tokens: 2000` override: the agent
  def's 300 was entirely consumed by ornith's reasoning trace, and the first pass
  returned 69 of 130 with `stop_reason: max_tokens` and zero text
- assembled with `../2026-09-21-qpp-probe/assemble_gate.py`
