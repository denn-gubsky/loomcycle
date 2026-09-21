# conv-30 — the second conversation, and why L2 delivered half

> ## ⚠️ SUPERSEDED — this arm was measured before the op-coverage fix
>
> The diagnosis in this file is what produced the fix. With the grant covering
> `search` as well as `recall`, conv-30 goes **0.5617 → 0.7654 / strict 0.7160**,
> questions receiving turns go **36/81 → 81/81**, and the gate is met. See
> `../2026-09-19-gate-met/`.
>
> ⚠️ One conclusion here is **overturned**: temporal is NOT a local-reader reading
> limit. The fixed arm reaches 0.9038 on temporal, *beating the oracle* — because the
> oracle renders only LoCoMo's annotated evidence, which is off by one. The temporal
> deficit was coverage.

Run 2026-09-19. LoCoMo **conv-30**, categories 1–4, **81 scoreable questions**, same
judge (`deepseek-v4-flash`, temperature 0), all arms on Spark (reader + embedder).

Built in an **isolated store** (`loomcycle_lc2`): ingest purges memory, so running it
against conv-26's store would have destroyed 263 facts, 419 traces and every arm
measured on them. 236 facts from 369 turns (0.64/turn against conv-26's 0.63), 369
traces backfilled scoped to `locomo/scribe`, verified 369/369 from corpus sessions
with zero judge or answer-key framing.

## Results

| arm | accuracy | strict | abstention | gate |
|---|---|---|---|---|
| control — facts only | 0.4383 | 0.3827 | 0.407 | fail |
| **L2 — `recall_attach_traces`** | **0.5617** | **0.4815** | 0.247 | **fail** |
| oracle — reading ceiling | 0.7785 | 0.7342 | 0.086 | — |
| *deepseek, full toolset (incidental)* | *0.7716* | *0.7160* | *0.049* | — |

**The gate fails clearly** — 0.5617 against 0.67 and 0.4815 against 0.60. Not a margin
question: it misses by 11 and 12 points.

**L2's effect is half what conv-26 showed**, though still significant and one-directional
(+12/−1, p=0.0034):

| corpus | control → L2 | Δ | headroom captured |
|---|---|---|---|
| conv-26 | 0.3758 → 0.6723 | +29.6pp | 69.1% |
| conv-30 | 0.4383 → 0.5617 | **+12.3pp** | **36.3%** |

**So ~0.67 was a conv-26 property, not an architectural one.** Averaged over the two
conversations the gate requires, L2 does not come close.

## ⚠️ The cause: the grant does not cover `op=search`

`recall_attach_traces` enriches **`op=recall` only**. The model elects the op, and on
conv-30 it elected `search` on most calls:

| corpus | `op=recall` | `op=search` | L2 fires on |
|---|---|---|---|
| conv-26 | 136 | 14 | **91%** |
| conv-30 | 36 | 49 | **42%** |

Per question the partition is total — and the inert half is its own control:

| conv-30 questions | n | got turns | control | L2 | Δ |
|---|---|---|---|---|---|
| used `recall` — **L2 fires** | 36 | 36 (100%) | 0.5139 | **0.7917** | **+27.8pp** |
| used `search` — **L2 inert** | 45 | 0 (0%) | 0.3778 | 0.3778 | **+0.0pp** |

**The inert half moves by exactly zero.** That rules out reverse causation: if `search`
merely marked harder questions, L2 would still have shifted them. It did not, because
the mechanism cannot fire there. And the fired half's **+27.8pp** matches conv-26's
coverage-normalised **+32.7pp** (+29.6pp at 91% coverage).

**L2 is worth ~+28pp wherever it fires, on both conversations. The whole conv-30
shortfall is coverage.**

### This is the invocation problem, one level down

The programme's finding was *the retrieval decision must leave the model*. `recall_attach_traces`
removed the decision about **whether to search traces** — but left the decision about
**which op to call**, and `search` bypasses the grant entirely. The lesson generalises
past tool parameters: a tool PARAMETER is a decision the model makes (51 of 128), and
so is the OP.

**The fix** is to make the grant op-agnostic — apply it to `search` as well as `recall`
— or to name it for what it governs rather than for one op. That is a change to a
shipped surface and is deliberately **not** made here; it is the first thing to do
before any further arm, because every L2 number in this programme is a coverage-weighted
average of ~+28pp and an op distribution nobody controlled.

## Conversation-difficulty notes, so the numbers are read correctly

conv-30 is **not** conv-26 rescaled:

- **81 questions, not 152**, and no open-domain slice at all (conv-26 had 11, pinned near
  the floor for every reader because their golds are inferences the shared prompt forbids).
  That lifts conv-30's absolute numbers independently of the memory layer — which is why
  the within-conversation control→L2 delta is the result, not the headline accuracy.
- **Comparable difficulty for a strong reader**: deepseek with the full toolset scores
  0.7716 here against 0.7877 for the equivalent arm on conv-26.
- **The ceilings nearly match**: 0.7785 against 0.8048, so ornith reads both about equally.

⚠️ **Temporal is the exception and it cuts against the reader, not the corpus.** conv-30's
temporal questions are *easy* — deepseek scores **0.9423** on them. ornith **handed the gold
turns** manages **0.6154**. A 33-point gap on questions where retrieval cannot be blamed,
because the evidence is already in the prompt. That is L1's temporal finding reproduced and
sharpened: the temporal deficit is a local-reader READING limit, and L4 (deterministic
weekday/offset rendering) is a local-reader lever specifically — a cloud reader does not
need it.
