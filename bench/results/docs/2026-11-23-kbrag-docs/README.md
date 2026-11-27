# kb vs RAG, Study D: the loomcycle development tenant (2026-11-23)

Pre-registration: `bench/docs/kbrag/PREREG.md`, Amendments 2–4. Harness:
`bench/docs/kbrag/docs/`. Deployment loomcycle 1.165.0 @ `f74bba4ec`, tenant `loomcycle-dev`.
All model calls went to Claude Haiku 5.5 (`anthropic`/`claude-haiku-5-5`), except the
deployment's local decision model (`ollama-local`/`nimble`: kb/ask's classifier and rag-lc's
rerank) and the embedder (`bge-m3`). The usage ledger for the study window shows no other model.
This file gives counts and scores only. The gold, the document ids and the internal titles
stay outside the repo.

## Verdict

| hypothesis | set | comparison (cited-document F1, paired over questions) | n | mean diff | 98.75% CI | as pre-registered |
|---|---|---|---|---|---|---|
| H1 | `p7` | kb − rag-lc | 21 | **+0.216** | [+0.101, +0.326] | **void** (instrument check 2 failed); descriptively the interval is above 0 |
| H2 | `p7` | kb − rag-ext | 21 | +0.106 | [−0.009, +0.215] | **void** (check 2); descriptively not shown: the interval includes 0 |

Instrument check 2 (the knowledge base is annotated) failed when the study started:
- 404 of 409 corpus documents had a document-level annotation (98.8%; the bar is 99%).
- 2.5% of the document annotations were stale (the bar is 1%).

Under the pre-registered rule this voids H1 and H2. Amendment 2 declared the failure before any scored run. The numbers are reported as descriptive results only.

**In plain words.** On the 21 corpus-level questions with document gold:
- `kb/ask` named the gold documents more completely than loomcycle's own passage search plus a reader.
- `kb/ask` was not shown to beat the outside advanced-RAG pipeline (`rag-ext`).
- `kb/ask`'s edge comes from recall, not precision. It cites about 20 documents per answer; the RAG reader cites 7–8.
- `kb/ask` cost about 5× more per run than either RAG arm.

## Question sets (never pooled)

| set | questions | gold | role |
|---|---|---|---|
| `p7` | 24: 13 overview, 7 area, 4 changed | the operator's RFC GM P7 gold (agent-drafted from listings and briefs, owner-approved); 21 document gold, 3 topic gold | **primary** (H1/H2). Used once before to compare two walker prompts |
| `waves` | 261: 133 overview, 44 specific, 36 known item, 18 general, 30 no-answer | the operator's 12 question waves with their keys; 226 with core documents | secondary. **The kb agents were developed and tuned on these waves**, so they favour `kb` |
| `known` | 30 known-item questions | one document each, drawn with a fixed seed; Haiku wrote the questions from that document's section text | secondary. Mechanical and annotation-free |

Runs per question per arm:
- `p7`: 3 runs; a question's score is the mean of its runs.
- `waves` and `known`: 1 run (Amendment 2 §3).

The corpus is the tenant's 409 non-operational documents; 408 have readable text.
- `rag-lc` searched the same 409 documents: the 23 operational documents are excluded by id.
- `rag-ext` indexed the same documents.
- `kb/ask` cannot be restricted. A citation outside the corpus counts as wrong for precision; the in-corpus-only precision is reported beside it.

## Arms per question kind

Columns:
- P is precision; `gold` and `also` documents both count as correct.
- R is recall over `gold`.
- F1 is the mean over questions of each question's mean over its runs.
- "F1 docs" counts only `loom:doc` links. The main figure also counts each `loom:chunk` link for its document.
- "read R" is the share of gold documents among the 20 sections the reader saw (RAG arms only).
- Topic recall is shown only where the gold is topics.

### `p7` (primary)

| kind | arm | q | runs | F1 | P | R | F1 docs | P in-corpus | topic R | read R | cited/run |
|---|---|---|---|---|---|---|---|---|---|---|---|
| all | kb | 24 | 72 | **0.589** | 0.552 | 0.712 | 0.623 | 0.560 | 0.522 | — | 20.2 |
| all | rag-lc | 24 | 72 | 0.372 | 0.538 | 0.325 | 0.372 | 0.538 | 0.236 | 0.350 | 7.3 |
| all | rag-ext | 24 | 72 | 0.483 | 0.626 | 0.424 | 0.483 | 0.629 | 0.204 | 0.477 | 7.8 |
| overview | kb | 13 | 39 | 0.631 | 0.580 | 0.789 | 0.670 | 0.585 | — | — | 16.3 |
| overview | rag-lc | 13 | 39 | 0.455 | 0.540 | 0.428 | 0.455 | 0.540 | — | 0.467 | 7.5 |
| overview | rag-ext | 13 | 39 | 0.580 | 0.671 | 0.535 | 0.580 | 0.674 | — | 0.612 | 7.1 |
| area | kb | 7 | 21 | 0.527 | 0.486 | 0.601 | 0.557 | 0.488 | 0.750 | — | 29.6 |
| area | rag-lc | 7 | 21 | 0.254 | 0.533 | 0.174 | 0.254 | 0.533 | 0.375 | 0.174 | 7.7 |
| area | rag-ext | 7 | 21 | 0.292 | 0.505 | 0.220 | 0.292 | 0.507 | 0.500 | 0.231 | 10.5 |
| changed | kb | 4 | 12 | 0.498 | 0.571 | 0.544 | 0.517 | 0.618 | 0.407 | — | 16.7 |
| changed | rag-lc | 4 | 12 | 0.190 | 0.542 | 0.117 | 0.190 | 0.542 | 0.167 | 0.117 | 6.1 |
| changed | rag-ext | 4 | 12 | 0.430 | 0.698 | 0.317 | 0.430 | 0.698 | 0.056 | 0.344 | 5.4 |

The paired tests per kind are reported, not tested; n is the number of document-gold questions.

| kind | kb − rag-lc: diff [98.75% CI] (wins/losses) | kb − rag-ext: diff [98.75% CI] (wins/losses) |
|---|---|---|
| overview (13) | +0.176 [+0.029, +0.316] (9/4) | +0.052 [−0.086, +0.176] (8/5) |
| area (6) | +0.273 [+0.096, +0.479] (6/0) | +0.235 [+0.083, +0.417] (6/0) |
| changed (2) | +0.308 [+0.256, +0.359] (2/0) | +0.068 [−0.131, +0.267] (1/1) |
| rag-ext − rag-lc, all (21) | +0.111 [+0.048, +0.179] (16/5) | |

Run-to-run spread (the mean over questions of the range of F1 across the 3 runs): kb 0.102, rag-lc 0.019, rag-ext 0.214.

### `waves` (secondary; tuned on, favours `kb`)

| kind | q | kb F1 | rag-lc F1 | rag-ext F1 | kb F1 docs |
|---|---|---|---|---|---|
| all (226 with core documents) | 261 | 0.613 | 0.519 | 0.577 | 0.496 |
| overview | 133 | 0.565 | 0.490 | 0.617 | 0.457 |
| specific | 44 | 0.657 | 0.606 | 0.563 | 0.410 |
| known | 36 | 0.816 | 0.620 | 0.524 | 0.826 |
| general | 18 | 0.442 | 0.330 | 0.403 | 0.313 |

| comparison (n = 226) | diff | 98.75% CI | wins/losses/ties |
|---|---|---|---|
| kb − rag-lc | +0.093 | [+0.036, +0.151] | 118/74/34 |
| kb − rag-ext | +0.036 | [−0.023, +0.096] | 98/103/25 |
| overview only, kb − rag-ext (132) | −0.052 | [−0.121, +0.018] | 45/81/6 |

On the waves' overview questions `rag-ext` scored above `kb`. The difference is not shown.

### `known` (secondary; mechanical gold)

| arm | F1 | P | R | F1 docs |
|---|---|---|---|---|
| kb | 0.667 | 0.622 | 0.767 | 0.533 |
| rag-lc | 0.553 | 0.486 | 0.767 | 0.553 |
| rag-ext | 0.656 | 0.568 | 0.933 | 0.656 |

- kb − rag-lc: +0.113 [−0.082, +0.313].
- kb − rag-ext: +0.011 [−0.200, +0.209].
- Neither difference is shown.

### Judge (secondary)

Counts are COMPLETE / PARTIAL / WRONG, over runs:

| set | kb | rag-lc | rag-ext |
|---|---|---|---|
| p7 (72 runs each) | 11 / 53 / 8 | 1 / 64 / 7 | 1 / 65 / 6 |
| waves (260 judged) | 114 / 135 / 11 | 116 / 127 / 17 | 135 / 122 / 3 |
| known (30) | 12 / 13 / 5 | 7 / 15 / 8 | 15 / 13 / 2 |

How to read the judge:
- The judge is Haiku, the model that wrote every answer.
- It saw the gold as document titles and machine-written briefs, which lean towards `kb`.
- On `p7` it called almost every answer PARTIAL, so it separates the arms little.
- On waves overview questions it rated kb 27/97/8 and rag-ext 32/97/3.

## kb/ask's route (`ROUTE` / `CLASSIFIER`, runs)

- **p7 overview:** survey 33 (27 sent there by the kb/classify agent, 6 by the decision model); search 5; search → walker 1. COVERAGE partial 34, none 5.
- **p7 area:** survey 21. COVERAGE partial 18, complete 3.
- **p7 changed:** walker (dated rule) 6; search 3; survey 2; search → walker 1.
- **waves overview:** survey 61; search 46; search → walker 14; walker (dated 7, listing 5). COVERAGE complete 11, partial 76, none 46.
- **waves specific, general, no-answer:** almost all search.
- **known:** search 29, search → walker 1.

## Cost

Per run, `p7`:

| arm | USD | input tokens | output tokens | model calls | wall s |
|---|---|---|---|---|---|
| kb | 0.024 | 88,043 | 18,737 | 11.1 | 77 |
| rag-lc | 0.004 | 23,678 | 2,636 | 1.0 (+ local rerank) | 65 |
| rag-ext | 0.005 | 30,723 | 3,632 | 3.0 | 32 |

Notes on the cost figures:
- On `waves`, kb averaged $0.011 a run: $0.016 on overview questions and $0.003 on specific ones.
- kb's call count is a floor (one per run in its tree). Its local decision call and rag-lc's local rerank are unpriced.
- rag-lc's wall time is mostly the shared local reranker's queue.

Spend, from the tenant's usage ledger:
- **$8.06 increase** over the baseline of $36.7924 taken at 06:34 UTC, against a $20 budget with a hard stop at $19.
- The per-row token estimates add up to $8.02 (runs $7.46, judge $0.44, quarantined rows $0.03, smoke and setup about $0.10). About $0.04 is not attributed to a row.
- No other user's spend appeared in the window.

## Instrument checks

| check | result |
|---|---|
| 1 same corpus | **pass**: 408 of 409 indexed (one has no readable text); rag-lc returned no hit outside the corpus |
| 2 annotated | **fail**: coverage 0.988, stale 0.025 (the server checked 2,000 annotations); voids H1 and H2 |
| 3 rag-lc used no annotation | **pass**: 363 searches, 0 with `matched_annotation` or `summary` |
| 4 reranks ran | **pass**: rag-lc reranked 100%; rag-ext 0 rerank or expansion failures |
| 8 every run answered | **pass** on the kept rows (one rag-lc failure, an embedder DNS hiccup, passed on retry) |
| 9 one release | **pass**: 1.165.0 throughout |

## Incident, routing windows and failures

The bundle walker was routed to Haiku by a tenant fork of `kb/walker`. While a fork is active, it shadows the bundle walker for every run in the tenant.

| window | fork active | helper active | rows |
|---|---|---|---|
| 1 | 06:51:13 → 07:15:43 | 06:51:14 → 07:19:38 | kept only if wholly inside the window (Amendment 3 rule) |
| 2 | 07:21:01 → 08:03:46 | 07:21:01 → 08:03:47 | `p7` completed |
| 3 | 08:09:49 → 10:20:58 | 08:09:50 → 10:20:58 | `waves`, `known`, retries |

Window 1 ended early through a harness defect: a follow-up script took a wrapper's PID for the runner's and restored the tenant while runs were in flight. The fix is in Amendment 3.

Rows quarantined from window 1, out of the analysis:

| arm | quarantined | why |
|---|---|---|
| kb | 4 | 2 cancelled by a harness restart, 1 ended after the restore, 1 idempotency-key refusal |
| rag-lc | 3 | 1 cancelled, 1 ended after the restore, 1 key refusal |
| rag-ext | 0 | |

- Kept from window 1: kb 18, rag-lc 19, rag-ext 22 (all `p7` run 1).
- Window 2 ended when the rag-lc guard stopped the runner on a transient embedder failure (Amendment 4).
- After each window the tenant was verified as before the study. Since 10:21 every kb name resolves to the def it had before the study and the helper is inactive.
- The forks and the helper remain in the tenant as retired AgentDef versions, i.e. history only.

The three tenant forks of `kb/classify`, `kb/search` and `kb/screen` already existed before the study and were run as they are, all Haiku:
- kb/search's prompt is the bundle's.
- kb/classify's differs by one character and lacks the `-Calc` denial.
- kb/screen lacks `-Calc`.

So `kb` is "kb/ask as this tenant runs it", which is close to but not exactly the shipped bundle.

## Deviations from the pre-registration (all in Amendments 2–4)

- The judge is Haiku, the reader model.
- `rag-ext` reranks listwise with Haiku, not with a cross-encoder.
- There is no condense step.
- The reader sees sections cut at 8,000 characters.
- `rag-lc` searches with `limit: 40` to fill 20 sections.
- The corpus is widened beyond `/loomcycle`.
- `waves` and `known` run once per arm.
- Check 2 failed before the study, voiding H1 and H2.

## What a reader should not conclude

- **Not that the knowledge base beats RAG in general.** This is the corpus the bundle was developed on. The kb agents were tuned on the waves, and the `p7` gold was built from the same briefs `kb` reads. That makes this an upper reading for `kb`.
- **Not a confirmed H1.** Check 2 failed, so H1 is void. The interval only describes these 21 questions.
- **Not that `kb` beats `rag-ext`.** On `p7` the interval includes 0. On the waves' overview questions `rag-ext` scored higher, and on the mechanical known-item set the arms are level.
- **Not that `kb` answers better per citation.** Its gain is recall from citing many more documents. Precision is level, and a RAG reader that sees only 20 sections cannot name more than those sections hold: the "read R" column bounds it.
- **Not anything from the judge.** It is Haiku grading Haiku against brief-based descriptions, and it rated nearly all `p7` answers PARTIAL.
- **Not that the per-kind results are reliable.** The `changed` (2 questions) and `area` (6) results rest on very few questions.
- **Not small differences.** Run-to-run spread is large for `kb` (0.10) and `rag-ext` (0.21). One run per question on `waves` and `known` is noisy per question.
