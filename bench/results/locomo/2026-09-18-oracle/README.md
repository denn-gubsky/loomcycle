# L1 — the reading-ceiling oracle, four readers

Run 2026-09-18. LoCoMo conv-26, categories 1–4, 150 scoreable questions, one judge
(`deepseek-v4-flash`, temperature 0) across every arm.

Each reader is handed the question's **gold evidence turns in the prompt** and holds
no tools, no memory scopes and no store. It only reads. That makes the number the
ceiling every retrieval lever is working toward for that reader: no arrangement of
retrieval can beat the score a model gets when the right turns are already in front
of it.

Reproduce:

    ./bin/locomo -mode=answer -dataset locomo -data <locomo10.json> -conversations 1 \
      -answer-only -oracle -answerer locomo/oracle-<reader> -out <dir>

## Results

| reader | host | store arm (best) | **oracle** | strict | p50 | abstains | headroom, McNemar |
|---|---|---|---|---|---|---|---|
| deepseek-v4-flash | cloud | 0.7877 traces | **0.8288** | 0.7808 | 1.1s | 14/146 | +20/−11, **p=0.15 (NS)** |
| ornith-1.5:35b | TrueNAS | 0.3345 traces | **0.8087** | 0.7383 | 10.6s | 15/149 | +85/−5, **p=7.5e-20** |
| qwen3.6:latest | TrueNAS | 0.4463 turns attached | **0.7138** | 0.6207 | 11.8s | 19/145 | +61/−15, **p=9.8e-08** |
| qwen3.8:latest | TrueNAS | *none clean* — see below | **0.7138** | 0.6276 | 30.3s | 25/145 | not computable |

All four clear the project gate (accuracy ≥ 0.67 **and** strict ≥ 0.60) at their
ceiling — qwen3.6 and qwen3.8 by ~0.02 on strict, ornith with real margin.

**qwen3.8 has no clean store baseline and so no headroom figure.** Its control arm was
contaminated (the tool schema advertises `traces` in its enum, so the "no traces" arm
found them on 45 of 144 runs) and its traces arm graded only 94 of 150 because 52
answers came back empty under a `max_tokens` budget too small for a thinking model.
Both are recorded here rather than quietly reused.

### L6 is answered: the reader is ornith-1.5

§7b deferred the reader choice to L6, "once retrieval is code". L1 settles it early, on
both axes at once:

- **qwen3.8 buys nothing over qwen3.6.** 0.7138 against 0.7138 — identical to four
  decimals (91/25/29 vs 90/27/28 over 145 graded) — for **2.6× the latency**. The
  dense-27B bet in §7b ("its reading may be worth the latency") does not pay.
- **ornith is best on accuracy AND fastest**, 0.8087 at 10.6s against qwen3.6's 0.7138
  at 11.8s, despite being the largest of the three. It is +9.5pp of ceiling for less
  wall-clock.

qwen3.6 clears the gate by 0.021 on strict *at its ceiling*, which means a perfect
retriever still leaves it on the line. ornith clears it by 0.138. Build L2/L3 on
ornith.

### What it says

**The local-model gap is not a reading gap.** On plain single-hop reading the three
are within 3pp of each other — deepseek 0.9357, ornith 0.9143, qwen3.6 0.9071 — and
ornith's overall ceiling is within 2.0pp of deepseek's. What separates them is that
deepseek's retrieval arm has already reached its own ceiling (p=0.15, no detectable
headroom) while ornith's sits 47 points below it. The small local models are not weak
readers; they are starved ones.

**Abstention is evidence-starvation, not timidity** (§7b L7). Of the 68 questions
ornith declined with a store, 58 are answered once the evidence is in the prompt, and
those 58 grade **51 correct / 5 partial / 1 wrong — quality 0.9386**. qwen3.6 frees 41
of 51 at 0.8000. deepseek barely abstained with a store (9 of 150) so it has almost
nothing to free, which is the same fact from the other side. This clears L7's
LongMemEval worry directly: the freed answers are right, not confabulated, so handing
a local reader evidence does not trade abstention for confabulation.

**Temporal is a reading limit, not a retrieval one.** deepseek's oracle wins 3
temporal questions over its traces arm and loses 5 — handing it the exact gold turns
does not fix the category. Whatever the temporal residue is, more retrieval is not it.

## ⚠️ LoCoMo's `evidence` annotation is off by one — read this before reusing the arm

The first instrument check rendered `qa.evidence` verbatim (`-oracle-window 0`).
deepseek scored **0.6747, BELOW its own traces arm at 0.7877**, and abstained on 33 of
150 with the "gold" evidence in front of it. It was reading correctly. The annotation
names the turn that **asks**, not the one that **answers**:

- *"What are the new shoes used for?"* gold **Running** → evidence `D7:19` is Caroline
  asking *"For walking or running?"*; Melanie's reply is the next turn, unnamed.
- *"How long has Melanie been creating art?"* gold **7 years** → evidence `D16:7` is
  Caroline answering about *herself* and asking Melanie; the reply is the next turn.

This is not a harness fault — all 150 questions resolve every annotated id, 0 partial.
Across the corpus the gold tokens are fully present in 39 of 70 single-hop annotations
at width 0 and 48 at width 2.

So the arms above run at `-oracle-window 2`, which renders the adjacency pair the
annotation points into (bounded by the session, deduped, chronological). The repair is
**+30/−6, p=7.0e-05**, and single-hop wins 17 / loses 0 — one-directional, the
signature of fixing an off-by-one rather than of adding context. `summary-deepseek-w0.json` is
kept because it is the ceiling of *LoCoMo's own annotation*, which is worth having.

Anyone building an upper-bound arm on LoCoMo will hit this. A width-0 oracle measures
the dataset, not the model.

## ⚠️ What this arm does NOT separate

An oracle arm differs from a store arm in **two** ways at once: it supplies better
evidence, and it removes the tool protocol (no inventory, no guide, no op bullets, no
tool call). §7b hoped L1 would tell you "whether the *content* or the *protocol* was
the problem in the coerced arm". It cannot. It bounds their **combination**.

This matters for what to build next. If most of ornith's 47 points are the evidence,
L2 (runtime-attached question-anchored turns, reader keeps its tools) captures them.
If a large share is the protocol, only L3 (tool-free reader, evidence pre-retrieved
into the prompt) captures them — and L2 would underdeliver for a reason L1 cannot see.

The arm that separates them is cheap and is the recommended next measurement: **the
oracle evidence with the store arm's full system prompt** — same gold turns, but the
reader carries the tool inventory, the guide and the op instructions. If that arm
lands near the oracle, protocol is free and L2 is the lever. If it falls toward the
store arm, protocol is most of the cost and L3 should move first.

Until it runs, read the headroom figures as "reachable by some combination of L2 and
L3", not as "reachable by L2".

## Method notes

- **One host for the local readers.** All three ran on TrueNAS (Ollama 0.34.1), not
  Spark — ornith-1.5:35b exists only there, and the same model has differed by 20
  points of abstention across the two hosts' Ollama versions. Three readers on one
  host compare to each other; three on two hosts do not. deepseek is cloud and
  unaffected.
- **No procedure in the reader prompt.** One instruction and one abstention rule. The
  coerced arm took qwen3.6 to 0.0034 by adding a mandatory protocol, so this adds none.
  The same "never answer from what you already know" rule as the store arms is kept,
  for comparability — it is also why open-domain (category 3, gold answers like
  *"Likely no"*) sits near the floor for every reader: those questions ask for an
  inference the prompt forbids, and the window cannot and should not fix it.
- **Zero tool calls on every arm**, verified against the store per agent rather than by
  a time window.
- Accuracy is the LoCoMo convention (correct 1, partial 0.5, over graded answers);
  strict counts only `correct`. The McNemar figures binarise a three-level verdict and
  are reported as an effect direction and magnitude, not a precise p.
