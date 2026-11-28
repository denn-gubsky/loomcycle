# RFC HC gate study: the first call against today's kb/ask and passage RAG (2026-11-28)

Pre-registration: `bench/docs/kbrag/PREREG-HC.md`, Amendment 0 (before the scored runs) and
Amendment 1 (post hoc, after them). Harness: `bench/docs/kbrag/hc/`. This file gives counts
and scores only; no question, answer or run file is in the repo.

- **Runtime:** loomcycle 1.172.0 @ `ad14d7d81`, the same version at the first and last run of
  each set (check 9).
- **Models:** every model call that costs money went to Claude Haiku 5.5. The decision model
  and embedder are the deployment's free local ones, as in Amendment 0.
- **Sets:** ConditionalQA, the pre-drawn 800 test questions, 5 arms, one run each, plus two
  more runs on a fixed 100 for spread (5,000 runs). p7: the 21 document-gold walker questions,
  4 arms, 3 runs each (288 runs with the 3 topic-gold questions, reported apart).
- **Dates (UTC, 28 Nov):** ConditionalQA 09:14 → 15:29; p7 15:29 → 16:33; condense 16:35 →
  17:13; analysis 17:15. No scored row failed; both retry passes had nothing to rerun.
- **Spend:** $15.26 ledger increase over both tenants since the 27 Nov baseline, against the
  $19 stop (sweep, smoke, tuning, the scored runs and the condense pass).

## Verdict

**Registered result: all three tests are void, so the gate is closed.** Check 7 failed on
ConditionalQA, which voids T1 and T2. Check 1 failed on p7, which voids T3. That verdict is
not changed after the fact.

**Neither failure is the fault the check guards against** (Amendment 1 has the detail).
Check 7 scored the reader's `ANSWER:` line against short gold answers, but the bundle's
`kb/read` writes a sentence there, so the check could not pass with this reader. Check 1
failed because `rag-lc` cited two tenant documents that were in the corpus snapshot and that
the filter leaves out (the ontology and one memory document). The corpus did not change, and
`rag-lc` is not one of the arms T3 compares.

**Exploratory reading (Amendment 1, post hoc, owner ruling):** with those two checks replaced
by ones that can fail (both pass), **T1, T2 and T3 all hold and the gate would open.** This
is evidence for the P2 decision, not a registered result.

| test | comparison | diff | 98.33% CI | bound | registered | Amendment 1 |
|---|---|---|---|---|---|---|
| T1 | kb-first − rag-lc, condensed F1, CQA 800 | +0.0042 | [−0.0134, +0.0221] | −0.02 | void (check 7) | holds |
| T2 | kb-first − kb-today, condensed F1, CQA 800 | +0.0833 | [+0.0535, +0.1144] | 0 | void (check 7) | holds |
| T3 | kb-first − kb-today, cited-document F1, p7 21 | −0.0452 | [−0.0998, −0.0016] | −0.10 | void (check 1) | holds |

T3 holds by a margin of 0.0002 on its lower bound. Its point estimate (−0.045) is above the
−0.05 stop point, so the gate rule's T3 clause is met either way. Question by question: T1 92
wins / 98 losses; T2 212 / 108; T3 8 / 13.

## ConditionalQA (800 test questions)

| arm | EM | F1 | F1 yes/no (429) | F1 span (371) | cited-page recall | $/question | wall s |
|---|---|---|---|---|---|---|---|
| `kb-today` | 0.471 | 0.594 | 0.729 | 0.437 | 0.943 | 0.0030 | 22.1 |
| `kb-first` | 0.536 | 0.677 | 0.820 | 0.512 | 0.960 | 0.0006 | 25.3 |
| `kb-today+answer-first` | 0.499 | 0.633 | 0.792 | 0.450 | 0.953 | 0.0030 | 18.3 |
| `rag-lc` | 0.534 | 0.673 | 0.814 | 0.509 | 0.954 | 0.0003 | 20.0 |
| oracle | 0.548 | 0.689 | 0.815 | 0.542 | 0.958 | 0.0002 | 1.6 |

- **Routes.** `kb-first` answered on the first call in 761 of 800 questions and escalated on
  37 (4.6%, all "not found"); 2 went to survey. `kb-today` searched on 786, walked on 13.
- **Cost check:** `kb-first` $0.0006 against `kb-today` $0.0030 per question, inside the 0.5×
  limit. It holds.
- **Spread** (the fixed 100, three runs each), mean per-question F1 range: `kb-today` 0.198,
  `kb-first` 0.097, answer-first 0.113, `rag-lc` 0.119, oracle 0.071.
- **Secondary (reported):** `kb-today+answer-first` − `kb-today` +0.040 [+0.013, +0.067]
  (holds); `kb-today+answer-first` − `kb-first` −0.044 [−0.069, −0.019].

## p7 (21 document-gold questions × 3 runs)

| arm | cited-document F1 | $/run |
|---|---|---|
| `kb-today` | 0.541 | 0.0236 |
| `kb-first` | 0.496 | 0.0216 |
| `kb-today+answer-first` | 0.553 | 0.0236 |
| `rag-lc` | 0.139 | 0.0006 |

- **Cost check:** `kb-first` $0.0216 against `kb-today` $0.0236 per run, inside the 1.1× limit.
  It holds.
- **Secondary (reported):** answer-first − `kb-today` +0.012 [−0.012, +0.041]; `rag-lc` −
  `kb-today` −0.402 [−0.486, −0.312] (21 losses of 21).

## Instrument checks

| check | ConditionalQA | p7 |
|---|---|---|
| 1 same corpus | pass (652 = 652, no hit outside) | **fail**: 2 cited documents outside the filtered corpus (both in the snapshot) |
| 1 under Amendment 1 | pass | pass (no document new since the snapshot) |
| 2 annotated | pass (coverage 1.0, stale 0) | pass (0.991, stale 0.002) |
| 3 first call without annotations | pass (1,598 rows) | pass (89 rows) |
| 4 first call reranked | pass (100%) | pass (100%) |
| 5 rag-lc retrieval > 10× chance | pass (0.954 vs 0.077) | — |
| 6 oracle above rag-lc | pass (0.689 vs 0.673) | — |
| 7 condense faithful | **fail**: condensed 0.689 vs direct 0.246 (bound 0.03) | — |
| 7 under Amendment 1 | pass: 796 of 800 extractive (0.995 ≥ 0.97) | — |
| 8 every run answered | pass | pass |
| 9 one release | pass (1.172.0) | pass (1.172.0) |

## Reproduce

```
python3 -I bench/docs/kbrag/hc/hcanalyze.py --out <work>/study
```

It prints the registered reading first and the Amendment 1 reading after it.
