# The gate is met — both conversations, with the op-coverage fix

2026-09-19. ornith-1.5:35b on Spark, LoCoMo conv-26 and conv-30, `deepseek-v4-flash`
judge. Gate: **accuracy ≥ 0.67 AND strict ≥ 0.60 on two conversations.**

| arm | accuracy | strict | gate |
|---|---|---|---|
| conv-26 draw 1 | 0.7013 | 0.6242 | PASS |
| conv-26 draw 2 | 0.6939 | 0.6190 | PASS |
| **conv-26 mean** | **0.6976** | **0.6216** | **PASS** |
| **conv-30** | **0.7654** | **0.7160** | **PASS** |
| **both-conversation mean** | **0.7315** | **0.6688** | **PASS** |

Against control: **+57/−1, p=4.1e-16** (conv-26) and **+32/−1, p=7.9e-09** (conv-30).
Margins are +4.1 / +3.2 questions on conv-26 and +7.7 / +9.4 on conv-30. Both conv-26
draws pass **independently**, spread 0.0075 accuracy and 0.0051 strict.

## ⚠️ Why this is not the claim that was retracted earlier the same day

An earlier arm was reported as clearing the gate and then **withdrawn** — see the
retraction in `../2026-09-19-l2-l3-spark/README.md`. That retraction was correct and
still stands **for the implementation it described**. What changed is the code, not
the interpretation:

| | retracted claim | this claim |
|---|---|---|
| implementation | grant fired on `op=recall` only | grant fires wherever the capability is reachable |
| evidence | one draw | two draws on conv-26, plus conv-30 |
| accuracy margin | **+0.34 questions** | +4.1 (conv-26), +7.7 (conv-30) |
| strict | **missed** on the replicate (−0.80 q) | +3.2 (conv-26), +9.4 (conv-30) |
| conversations | one | **both**, as the gate asks |

The first claim rested on a margin smaller than a single question and died on its
replicate. This one clears on every individual draw, on the mean, and on both
conversations.

## What actually changed

`recall_attach_traces` enriched `op=recall` only, and **the op is a decision the model
makes**. Fixing that to cover `search` moved both corpora — by amounts that track the
gap each had:

| corpus | coverage before | before | after | Δ |
|---|---|---|---|---|
| conv-26 | 91% of calls | 0.6723 | 0.6976 | +2.5pp |
| conv-30 | 42% of calls | 0.5617 | 0.7654 | **+20.4pp** |

**The size of each gain tracks the size of the gap it closed**, which is what makes
the coverage explanation credible rather than fitted — a post-hoc story would not have
predicted the *small* conv-26 number. conv-30 now captures **96.2%** of the headroom
to its reading ceiling and sits at **98.3%** of it; questions receiving turns went
36/81 → **81/81**, while the op distribution was unchanged (`search` 47, `recall` 36).
The model behaves identically; the runtime simply serves it now.

## ⚠️ Two earlier conclusions this overturns

**1. Every pre-fix L2 number was a coverage-weighted average**, not a property of the
lever, and was reported as though it were. The corrected figure is ~+28pp wherever the
grant fires.

**2. Temporal is NOT a local-reader reading limit.** That was claimed from the oracle —
ornith 0.6154 holding gold evidence against deepseek's 0.9423. conv-30's fixed arm
reaches **0.9038**, *beating the oracle*. That is possible because the oracle renders
only LoCoMo's **annotated** evidence, and that annotation is off by one (documented in
`../2026-09-18-oracle/README.md`): real retrieval finds dated turns the annotation
omits. **The oracle is a ceiling for reading the annotation, not for reading what is
retrievable.**

Consequence: the deterministic temporal-grounding lever should **not** be built on the
evidence that motivated it. The temporal deficit was retrieval coverage. Anything
claimed from an oracle arm on this corpus inherits the annotation's blind spot and
should be re-checked against a real-retrieval arm before it is acted on.

## Caveats

- conv-30 has **no open-domain slice** and 81 questions against conv-26's 152, so its
  higher absolute number is partly the question mix. The within-conversation
  control→L2 delta is the comparable quantity.
## ⚠️ The L2+L3 combination LOSES once both are measured fairly

Pre-fix the combo matched L2 (0.6690 vs 0.6858, +15/−20, p=0.5) at 3.8× lower latency,
and was recommended on that basis. **That recommendation is withdrawn.**

| conv-26 arm | accuracy | strict | p50 | gate |
|---|---|---|---|---|
| L2 pre-fix (mean of 2) | 0.6723 | 0.5946 | 8.2s | fail |
| combo pre-fix | 0.6690 | 0.5724 | 2.2s | fail |
| **L2 FIXED (mean of 2)** | **0.6976** | **0.6216** | 8.1s | **PASS** |
| combo FIXED | 0.6610 | 0.5822 | 2.5s | fail |

The mechanism is exact: **the combo makes 14 tool calls in 150 questions** — the
pre-retrieved block answers most of them, so the model rarely reaches for the tool.
The fix improves only the TOOL path. L2 makes 150 calls and gained 2.5pp; the combo
makes 14 and gained nothing. Their earlier equivalence was an artifact of L2 being
handicapped.

The honest trade is now **3.2× lower latency for ~4 points of accuracy and the gate** —
real for a latency-bound deployment, but a trade rather than a free win. It also
sharpens L2 vs L3: a model-composed query against the full trace index beats one
embedding of the raw question, and the combo suppresses exactly that mechanism.

## The residue — where the remaining work is

`residue.py` classifies what the best arm still misses, into buckets with different
fixes:

| bucket | conv-26 | conv-30 | what fixes it |
|---|---|---|---|
| UNREACHABLE (oracle fails too) | 16 | 9 | nothing retrieval-side |
| RETRIEVAL (wrong; oracle right) | 16 | 4 | better retrieval |
| **VAGUE** (partial; oracle right) | **12** | **4** | **prompt/answering** |
| ABSTAINED | 11 | 5 | evidence or willingness |
| REGRESSED | 1 | 1 | — |

**VAGUE is the cheapest bucket and is invisible in any aggregate.** These are answers
that found the evidence and dropped the specific — *"from her home country"* when the
gold is **Sweden**; *"counseling and mental health"* when the gold is *"counseling and
mental health for Transgender people"*. They score 0.5 and look exactly like retrieval
failures in the headline number. With ABSTAINED that is 23 of conv-26's 56 — over 40%
of the residue reachable without touching the memory layer.

⚠️ **UNREACHABLE over-counts.** It is defined by the oracle also failing, and the
oracle renders only LoCoMo's ANNOTATED evidence, which is off by one. Some of those
questions are reachable by real retrieval; the oracle simply never saw the turn.
- Both stores were built chat-ingested with traces backfilled scoped to
  `locomo/scribe`, and trace-index **writes off** while measuring. A queue-ingested
  store yields zero turns silently.
