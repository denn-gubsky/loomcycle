# LongMemEval — the lever transfers, and the abstention slice inverts

2026-09-20. ornith-1.5:35b on Spark, `deepseek-v4-flash` judge. **130-instance subset**
of `longmemeval_oracle.json` (100 proportional by question type + **all 30 `_abs`**),
chat-ingested per instance with traces indexed live.

## ⚠️ Read the slices separately. The overall number is meaningless.

| slice | n | control | L2 | Δ | paired |
|---|---|---|---|---|---|
| **answerable** | 100 | 0.5408 (nf 0.260) | **0.7857** (nf 0.030) | **+24.5pp** | **+28/−3, p=4.7e-06** |
| **`_abs` — scored INVERTED** | 30 | 0.7667 (nf 0.767) | **0.5000** (nf 0.500) | **−26.7pp** | **+0/−8, p=0.0078** |
| ~~overall~~ | 130 | ~~0.5938~~ | ~~0.7188~~ | — | **do not quote** |

The overall row pools a slice where answering is right with one where answering is
*wrong*, at an `_abs` rate of 23% chosen for statistical power rather than realism
(the true rate is ~6%). It flatters L2 and means nothing.

## The finding: feeding evidence lowers abstention INDISCRIMINATELY

**+0/−8 on `_abs`.** Not one abstention question improved; eight regressed. Abstention
there fell 0.767 → 0.500, and on `_abs` **every abstention was correct** — these
questions have no answer in the history. The model stopped declining and started
confabulating.

⚠️ **This falsifies the generalisation of the LoCoMo abstention result.** That work
concluded local abstention is *"evidence-starvation, not timidity"* — the reader
declines because it lacks evidence and commits correctly when given it (58 of 68
freed, grading 0.9386). That held on LoCoMo, **where every question is answerable by
construction**. On a corpus containing genuinely unanswerable questions, more evidence
makes the model answer those too.

The mechanism does not make a reader better at *judging* when to abstain. It lowers
abstention, and whether that is a gain depends entirely on whether the questions have
answers. §7b L7 anticipated exactly this and required the conclusion be "re-earned"
here. It was re-earned and it failed.

## On the answerable slice the transfer is strong and broad

| type | control | L2 |
|---|---|---|
| single-session-user | 0.9000 | 0.9000 |
| **single-session-assistant** | **0.3333** | **0.9167** |
| single-session-preference | 0.8333 | 1.0000 |
| multi-session | 0.5556 | 0.6053 |
| temporal-reasoning | 0.4545 | 0.6061 |
| knowledge-update | 0.6667 | 0.7619 |

**`single-session-assistant` 0.3333 → 0.9167 is the most informative cell.** The
extractor prompt deliberately writes no facts for assistant turns (§8 item 4), so the
fact tier structurally cannot answer "what did the assistant recommend". L2 supplies
raw turns and reaches content the fact tier cannot represent at all. That is a
different kind of win from the others — not better retrieval of facts, but access to
material the fact tier discards.

## What this means for shipping

`recall_attach_traces` is **not safe as a blanket default** where abstention matters.
The net here is positive only because `_abs` is over-sampled; at the true ~6% it would
look better still — but "better on average" is the wrong frame when the failure mode
is confabulating answers to questions that have none.

Open and testable on these same 30 instances: whether the `_abs` cost is recoverable
by PROMPT — an explicit instruction to abstain unless the evidence *answers* the
question rather than merely relates to it — instead of by withholding the grant.

## ⚠️ Not comparable to published LongMemEval figures

Mem0, Zep and the LongMemEval paper report on the **S variant**: ~40 sessions per
instance. This is the **oracle variant** — only the evidence sessions, ~1.9 sessions
per instance — which is a far easier retrieval task. Ingesting S through real chat
ingestion and LLM extraction is ~230,000 turns, about **460 hours** at the measured
rate. That is a wall, not a scoping choice.

## Method

- Each instance is its own conversation; the harness purges memory between them, so
  there is no after-the-fact backfill window and the **live trace indexer runs**.
- **The gold cannot leak**: the judge is spawned with an empty `user_id`
  (`SpawnRun(ctx, judge, "", prompt)`) and the indexer skips turns with no user. Its
  prompt, which contains the gold answer, is never indexed.
- **The answerer's own question is filtered** by run id, so the question asked cannot
  return as its own top hit (it matches itself better than anything else does).
- 2 arms × 130 instances, 0 failed runs of ~4,600. `recallTraceTopK=24`,
  `-consolidate-passes 8`, `-concurrency 8`.
- The corpus is not vendored (MIT, but large); `subset-instance-ids.json` lists the
  130 `question_id`s so the subset is re-derivable.
