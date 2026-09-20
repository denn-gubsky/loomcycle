# `recallTraceTopK` 6 → 24 — the only bound that ever bit

2026-09-19/20. ornith-1.5:35b on Spark, LoCoMo conv-26 (150q) and conv-30 (81q),
`deepseek-v4-flash` judge. Two draws per cell.

## The constant nobody was measuring

`attachQuestionTurns` had two bounds and only one of them ever engaged:

| bound | value | what it did |
|---|---|---|
| `recallTraceTopK` | 6 | **300 of 300 recalls returned exactly 6 turns** |
| `recallTracesBudget` | 6000 | blocks averaged **1271 chars** — 4× headroom, never bound |

They are not interchangeable. The budget is content-aware and cuts when turns are
long; a fixed count cuts a short-turn corpus off regardless of cost. LoCoMo turns
average ~212 chars, so k=6 spent 21% of the budget and discarded the rest.

## Results

| | draw 1 | draw 2 | mean | spread |
|---|---|---|---|---|
| conv-26 k=6 | 0.7013/0.6242 | 0.6939/0.6190 | 0.6976/0.6216 | 0.0075 |
| **conv-26 k=24** | 0.7448/0.6828 | 0.7466/0.6824 | **0.7457/0.6826** | 0.0018 |
| conv-30 k=6 | 0.7654/0.7160 | 0.7531/0.7160 | 0.7593/0.7160 | 0.0123 |
| **conv-30 k=24** | 0.8063/0.7500 | 0.7963/0.7407 | **0.8013/0.7454** | 0.0100 |
| **both, k=6** | | | 0.7284/0.6688 | |
| **both, k=24** | | | **0.7735/0.7140** | |

**conv-26 +4.8pp, conv-30 +4.2pp** — within 0.6pp across corpora with different
question mixes and turn lengths.

⚠️ **The significance differs and the claim rests on the magnitudes, not the
p-values.** conv-26 reaches p=0.017–0.041 on three of four cross-pairings. conv-30
reaches p=0.18–0.73 on **none**: 81 questions and a high k=6 baseline leave only 5–7
moving, so the paired test has no power there. Consistent-and-underpowered is what
conv-30 shows; it is not independent confirmation.

## Why not higher — k=48 is a plateau

+11/−11 (p=1.0) and +11/−13 (p=0.839) against k=24, for **+1.1s latency**. At k=48
the **budget finally binds** (1 of 150 recalls reached the count; blocks ran 6427
chars against the 6000 cap) and buys nothing. So ~24 turns is where this corpus
saturates, and 24 is the smaller of two values that perform identically.

The full ladder: at k=6 the count bound and wasted 79% of the budget; at k=24 the
count still bound on 144/150; at k=48 the content bound took over and gained nothing.

## ⚠️ It is not free

k=24 **loses 5–6 questions that k=6 answered correctly.** More material dilutes
attention, which is the documented failure mode here — the coerced-prompt arm
collapsed to 0.0034 from added instructions. The net is clearly positive on both
corpora, but a corpus of long turns would pay more of that cost and collect less of
the benefit. The budget is what protects that case, which is the argument for letting
it be the real bound.

## ⚠️ "% of headroom captured" is retired as a metric

Earlier write-ups in this series quote figures like *"captures 96.2% of headroom"* and
*"92.5% of the ceiling"*, using the oracle arm as the denominator. **conv-30 at k=24
scores 0.8013 against an oracle of 0.7785 — 108.2% of it.** A percentage above 100 is
not a percentage of anything.

The cause is already documented in `../2026-09-18-oracle/`: the oracle renders only
LoCoMo's **annotated** evidence, and that annotation is off by one. Real retrieval
finds turns the annotation omits, so the oracle is a ceiling for *reading the
annotation*, not a bound on achievable accuracy. Every headroom fraction in this
series is measured against a bar that arms can step over, and the tables here report
control→arm deltas and McNemar only.

## Unaffected by any k

The three **vocabulary-gap** cases remain: *"What items has Melanie bought?"* against a
turn about pets that mentions shoes in passing; *"Where did Caroline move from?"*
against *"Caroline's grandma is from Sweden."* The question and the evidence share no
vocabulary and are not semantically close. No value of k reaches them — they need
query expansion, multi-query retrieval, or traversal.
