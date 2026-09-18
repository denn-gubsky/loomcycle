# L2 and L3 on ornith-1.5 — +31pp from runtime-supplied retrieval

> ## ⚠️ RETRACTION (replicate, 2026-09-19)
>
> An earlier version of this file claimed L2 was "the first local answerer to clear
> the gate". **It is not, and the claim is withdrawn.** A replicate of the identical
> arm was run because draw 1's strict sat 0.0014 above the 0.60 floor — 89 correct
> of 148, where one question flips it:
>
> | draw | accuracy | strict | correct | gate |
> |---|---|---|---|---|
> | L2 draw 1 | 0.6858 | 0.6014 | 89/148 | PASS |
> | **L2 draw 2** | **0.6588** | **0.5878** | **87/148** | **FAIL** |
> | **mean** | **0.6723** (+0.0023) | **0.5946** (−0.0054) | — | **FAIL** |
>
> Accuracy clears on the mean; **strict misses**. The gate needs both, and also asks
> for two conversations, which no local arm has attempted.
>
> **The lever is unaffected** — control→L2 is +54/−0 (p=1.1e-16) on draw 1 and
> +51/−4 (p=2.1e-11) on draw 2. L2 moves ornith 0.3758 → 0.66–0.69, captures 66–72%
> of the headroom to its ceiling, and cuts abstention 0.387 → 0.11–0.15. The
> L2-vs-L3, temporal and host-transfer results are all unchanged.
>
> The honest headline: **L2 is worth ~+31pp on a local answerer and lands ON the
> strict criterion rather than over it.** A threshold claim needs a different
> standard of evidence from an effect claim: +31pp is safe on one draw, a 0.0014
> margin is one question. Report a threshold as a mean over draws with its spread,
> or do not report it as met.

Run 2026-09-18/19. LoCoMo conv-26, categories 1–4, 150 questions, one judge
(`deepseek-v4-flash`, temperature 0). **All five arms on DGX Spark** (Ollama 0.34.2),
reader *and* embedder, so no arm depends on another host.

## Results

| arm | accuracy | strict | abstention | p50 | tool calls | gate |
|---|---|---|---|---|---|---|
| control — facts only | 0.3758 | 0.2953 | 0.387 | 6.8s | 151 | fail |
| P1 — fact-anchored turns (`recall_include_turns`) | 0.4799 | 0.3758 | 0.287 | 7.7s | 150 | fail |
| L3 — tool-free, pre-retrieved (`{{memory:recalled_context}}`) | 0.5933 | 0.4933 | 0.187 | **1.7s** | **0** | fail |
| **L2 — + question-anchored turns (`recall_attach_traces`)** | **0.6858** | **0.6014** | 0.113 | 8.3s | 150 | see retraction |
| L2 — draw 2 (replicate) | 0.6588 | 0.5878 | 0.147 | 8.1s | 150 | fail |
| oracle — reading ceiling | 0.8048 | 0.7397 | 0.087 | 1.0s | 0 | — |

Every step is significant, and the ladder is strict:

| pair | McNemar | p |
|---|---|---|
| control → P1 | +24/−3 | 4.9e-05 |
| P1 → L3 | +37/−17 | 9.1e-03 |
| L3 → L2 | +29/−12 | 1.2e-02 |
| P1 → L2 | +43/−5 | 1.4e-08 |
| **control → L2** | **+54/−0** | **1.1e-16** |

L2 captures **72.3%** of the headroom between control and ceiling on draw 1 (66% on
draw 2) and lands at **85.2%** of the ceiling. Against control it is strictly
dominant: 54 questions won, **zero lost**. On the gate itself, see the retraction
above — it clears accuracy on the mean of two draws and misses strict by 0.0054.

### What it says

**The local gap was invocation, and this is what closing it is worth.** L1 measured
ornith's reading ceiling at 0.8087 while its store arm sat 47 points below. Nothing
here changes the model or the prompt — only what the runtime hands over — and 31 of
those points come back.

**Temporal was never a temporal problem.** control 0.1892 → L2 0.7000, won 21 / lost
0. The programme spent months on dates; the answer was to give the reader the turn,
which carries its own timestamp while the distilled fact is tenseless. This is why L4
(deterministic temporal grounding) should be re-costed before it is built: most of
what it was aimed at has already moved.

**The architecture transfers from the cloud reader.** On deepseek, fact-anchored →
question-anchored was worth +24pp (0.5473 → 0.7877). On ornith with the runtime doing
the retrieval it is +20.6pp (0.4799 → 0.6858). Same structure, same size, on a local
model — once no model has to elect the call.

**P1's grant replicates on a second model.** qwen3.6 +11.4pp (p=1.2e-04); ornith
+10.4pp (p=4.9e-05). Within 1pp of each other on different models and hosts.

### ⚠️ The tool-free reader LOSES — the protocol is not pure cost

§7b asked whether a tool-free reader with pre-retrieved turns beats a tool-holding
reader with runtime-attached ones. **It does not: L2 beats L3 by 9.3pp, p=0.0115.**

This also settles the caveat L1 could not: L1 changed evidence and protocol together
and could only bound their combination. Removing the protocol while the runtime still
supplies the evidence costs 9.3 points, so **the tool loop has positive value**. Three
mechanisms separate them, and they are structural rather than incidental:

1. **L2's query is model-composed.** It turns "Would Caroline have Dr. Seuss books?"
   into `Caroline Dr. Seuss books bookshelf reading preferences`. L3 embeds the raw
   question verbatim.
2. **L2 carries both kinds of turn** — fact-anchored *and* question-anchored. L3 has
   facts plus question-anchored only.
3. **L2 can iterate.** A thin first recall can be followed by a second. L3 gets one
   shot before the first token.

The coerced-arm collapse (0.0034) was therefore about **mandating a procedure**, not
about holding a tool. Those are different things and this pair separates them.

**But L3 is the cost story.** 0.5933 at **1.7s and zero tool calls**, against L2's
8.3s — 50.7% of the available headroom for 20% of the latency, and it works on any
model that can read a dated block. For a latency-bound or tool-less deployment it is
the better trade; for accuracy it is not.

## Method notes

- **One host, reader and embedder.** TrueNAS went offline mid-run, so the TrueNAS
  P1/L2/L3 attempt was abandoned at 110 runs and everything was re-run here. Its
  partial data is retained under the un-suffixed agent names and is not mixed in.
- **The embedder move was verified, not assumed.** Corpus vectors were built with the
  TrueNAS bge-m3; a query embedded on Spark scores against them only if the two
  agree. Measured on a corpus turn: both 1024-dim, cosine 0.9999790, max element
  difference 7.4e-4 — enough to reorder exact near-ties and nothing more.
- **The control is clean.** The trace index is populated on this store and the control
  holds the Memory tool, so it *could* have elected `sources:["traces"]` for itself —
  ornith did that 17 times in 150 questions once. Checked: **0 of 151 calls elected
  traces, 0 results carried trace rows.**
- **Trace-index WRITES are off.** The live indexer has no agent filter, unlike the
  backfill, so every benchmark question — and every judge prompt, which quotes the
  gold answer — would otherwise be written into the index these arms search. A smoke
  run put its own question in before this was caught. The flag gates writing only;
  the 419 backfilled corpus turns stay readable.
- **The reader prompt is byte-identical** across control/P1/L2, asserted before the
  run. An earlier attempt rewrote it "cleaner", dropping `{{tool:Context.tools}}` and
  `{{tool:Context.guide}}`, and ornith collapsed to 0.0567 with 80% abstention and
  answers fragmented mid-sentence. A narrowed tool set is not a legible one.
- **No truncation.** Peak context 14112 of 32768 on L2; L3 peaked at 875.

## Host transfer — the standing caveat, measured

ornith existed on one host until now, so "do not compare arms across inference hosts"
could never be tested for it. With the same model on both and Ollama one patch apart
(0.34.1 / 0.34.2):

| task | TrueNAS | Spark | transfers? |
|---|---|---|---|
| reading (oracle, no tools) | 0.8087 / nf 0.100 | 0.8048 / nf 0.087 | **yes** — −0.4pp, inside churn |
| retrieval (control, tool) | 0.3129 / nf 0.513 | 0.3741 / nf 0.387 | **no** — nf −12.6pp; accuracy +6.1pp but p=0.143 |

So the rule can be narrowed rather than kept blanket: **reading ceilings transfer;
tool-driven arms do not.** The cause is visible in the store — the reached keyspace is
nearly identical (231 vs 234 distinct ids, 8 and 11 unique) while the *queries differ*,
because the model composes its own query text and different hardware yields different
tokens at temperature 0. The retrieval difference follows from the reader, not the
embedder.

Spark is also **10.7× faster** on this model (oracle p50 989ms vs 10618ms).
