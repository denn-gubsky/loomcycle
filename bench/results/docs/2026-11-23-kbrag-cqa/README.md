# kb vs RAG, Study C: ConditionalQA (2026-11-23)

Pre-registration: `bench/docs/kbrag/PREREG.md`, Amendments 0, 1 and 5. Harness:
`bench/docs/kbrag/` (run order in `ARMS.md`). This file gives counts and scores only; no
question, answer or run file is in the repo.

- **Runtime:** loomcycle 1.165.0 @ `f74bba4ec`, the same version at the first and last run.
- **Tenant:** `conditional-qa` on the lab deployment. It was empty of ConditionalQA pages before the study; the harness ingested and annotated them.
- **Corpus:** ConditionalQA v1.0, 652 gov.uk guidance pages and 13,724 sections (`bench/docs/measure/prepare.py`, the `header` form).
- **Questions:** the pre-drawn 800 of `bench/docs/measure/reader_subset.json`, one run per arm. A fixed 100 of them got two more runs per arm, for spread only.
- **Models:** every model call that costs money went to Claude Haiku 5.5 (`anthropic`/`claude-haiku-5-5`). That covers the reader, the condense step, the query rewrites, `rag-ext`'s rerank, the judge, and the five forked model-backed `kb/*` agents. The free local models were the deployment's decision model (`ollama-local`/`nimble`: kb/ask's Decision call and rag-lc's rerank) and its embedder (`bge-m3`, 1024 dimensions).
- **Dates (UTC):**
  - Ingestion and annotation smoke: 22 Nov 20:42.
  - Bulk ingestion: 22 Nov, about 20:46 → 22:01.
  - Annotation: 23 Nov 00:37 → 07:37.
  - Arm smoke and pilot (never scored): 04:54 and 05:01.
  - `rag-lc`, `rag-ext` and the oracle: 05:15 → 08:15.
  - `kb`: 07:38 → 09:52.
  - Spread runs: 09:52 → 10:52.
  - Condense and judge: 10:52 → 11:46.
  - Analysis: 11:46.

## Verdict

**H3 and H4 are void.** Instrument check 1 (same corpus) failed. Under the pre-registration
a failed check 1 voids the study's hypotheses. `analyze.py` reports "void" for both, and
that verdict is not changed after the fact.

**Why check 1 failed.** `rag-ext` indexed 652 documents. The tenant's listing, less the
operational types, held 653.
- The one extra document is `15443a640b2228b0056a975a5e336d84`. The operator identified it as the tenant's housekeeping document "Deployment Context" at `/memory/tenant_root`.
- It is untyped and not a ConditionalQA page. The check's filter (Amendment 0 names the types to drop: `SourceStub`, `SyncReport`, `Annotation`) therefore kept it.
- So the failure comes from the check's filter. It is not a difference between the pages the arms searched.

**What the work files can and cannot show** (Amendment 5):
- **The document was already in the tenant before bulk ingestion.** It appears in the annotation smoke of 22 Nov 20:42 UTC, when the tenant held only the smoke pages. The annotator skipped it as `entity_or_memory_tree` then and on every later pass.
- **It reached no answer:**
  - It is in no run row of any arm: 4,000 rows, counting the spread runs.
  - `rag-lc`'s 1,000 searches returned 20,000 hits. Every hit maps to a ConditionalQA section, and none is this document.
  - No `kb` answer cites it.
- **It may still have been seen.** `kb/ask` cannot be restricted, and its internal search results are not recorded. So the files cannot show whether a `kb` sub-search ever saw the document. They show only that no answer cited it.
- **It also affected check 2.** Check 2's denominator included it: 652 annotated of 653 gives 0.9985; without it the figure would be 652 of 652.

**Descriptive reading (post hoc; not the verdict).** The paired test ran as pre-registered:
paired bootstrap over the 800 questions, 10,000 resamples, seed 1, 98.75% CI, on the
condensed answer's F1.

| comparison | n | mean diff | 98.75% CI | above −0.02 (non-inferior)? | above 0? |
|---|---|---|---|---|---|
| kb − rag-lc (H3) | 800 | **−0.0419** | [−0.0762, −0.0072] | no | no |
| kb − rag-ext (H4) | 800 | **−0.0469** | [−0.0798, −0.0137] | no | no |

Had check 1 passed, both tests would read "does not hold": non-inferiority is not shown
against either RAG arm. In both comparisons the whole interval lies below 0, so on these
questions `kb` answered worse than both RAG arms. The pre-registration expected the
knowledge base to be "level with or slightly behind passage RAG in Study C at several times
the cost"; it was behind, at 1.9× (`rag-ext`) and 9.4× (`rag-lc`) the estimated USD per
question.

Every number below is descriptive.

## Arms

| arm | what it is |
|---|---|
| `kb` | `kb/ask` at depth `standard`, with its shipped classifier and routing. Its five model-backed agents (`kb/classify`, `kb/search`, `kb/screen`, `kb/walker`, `kb/annotate`) are forked in the tenant with one change each: provider and model pinned to Haiku |
| `rag-lc` | `Document op=search` through the helper `kbrag/search-rr`: hybrid vector + full-text search, the index header, and `memory_rerank` on at its shipped defaults. No annotations. Limit 20; the first 5 readable sections go to one reader call |
| `rag-ext` | an outside pipeline (standard library only) over the same pages. Section chunks with 400-token windows; `title > path` headers; BM25 + dense retrieval fused by RRF; three Haiku query rewrites; a listwise Haiku rerank (not a cross-encoder, Amendment 0 §3); parent expansion. 5 sections go to the reader |
| oracle | the reader on the question's readable gold sections (at most 5) |

The reader is one Haiku call at temperature 0. It writes `ANSWER: …` and then
`SOURCES: loom:doc/<id>`.

Every arm's raw answer then goes through one condense call (Haiku, question + answer only).
That call returns the shortest span, or `NO ANSWER`. EM and F1 are scored on the condensed
span; citations are read from the raw answer.

| arm | EM | F1 | cited-page recall | (docs only) | judge C / P / W | in tok/q | out tok/q | USD/q (est) | wall s/q |
|---|---|---|---|---|---|---|---|---|---|
| kb | 0.4750 | 0.5999 | 0.9475 | 0.6112 | 525 / 190 / 85 | 14,064 (+100,747 cached) | 1,901 | 0.0034 | 40.0 |
| rag-lc | 0.5038 | 0.6419 | 0.9313 | 0.9313 | 593 / 58 / 149 | 1,187 | 491 | 0.0004 | 32.3 |
| rag-ext | 0.5088 | 0.6468 | 0.9350 | 0.9350 | 605 / 61 / 134 | 8,918 | 1,747 | 0.0018 | 16.9 |
| oracle | 0.5400 | 0.6765 | 0.9900 | 0.9900 | 621 / 42 / 137 | 629 | 425 | 0.0003 | 3.4 |

How to read the table:
- **n:** 800 questions per arm, every row ok and condensed. No run failed, so no retry was needed.
- **Cited-page recall:** counts a `loom:chunk` link for its page (Amendment 0 §5). `kb` cites sections far more often than pages, hence its low "docs only" figure.

## Hypotheses

| | comparison | bound | as pre-registered | descriptive interval |
|---|---|---|---|---|
| H3 | kb − rag-lc, mean condensed F1 | −0.02 | **void** (check 1) | [−0.0762, −0.0072]: below the bound and below 0 |
| H4 | kb − rag-ext, mean condensed F1 | −0.02 | **void** (check 1) | [−0.0798, −0.0137]: below the bound and below 0 |

`summary-study-c.json` beside this file is the unchanged `analyze.py` output (`summary.json` in the work directory; renamed to match the repo's `summary-*.json` allowlist). Re-running
`analyze.analyze` in memory on the same files reproduces it with no differences.

## Instrument checks

| # | check | result |
|---|---|---|
| 1 | same corpus | **fail**: 652 indexed, 653 listed, 0 only-indexed, 1 only-listed (the untyped `/memory` document above); voids H3 and H4 |
| 2 | knowledge base annotated | pass: coverage 0.9985 (652 / 653), stale 0.0 (the server checked 2,000 annotations); taken at 07:38:08, before the first `kb` run |
| 3 | rag-lc used no annotation | pass: 0 of 800 searches with `matched_annotation` or `summary` |
| 4 | reranks ran | pass. rag-lc: reranked 0.9988 (one timeout). rag-ext: listwise on all 800, 0 rerank failures, 0 expansion failures |
| 5 | retrieval works | pass: R@5 0.945 (rag-lc) and 0.9413 (rag-ext), against 10× chance = 0.0077 |
| 6 | the reader reads | pass: oracle F1 0.6765 > 0.6419 and 0.6468 |
| 7 | condense faithful | pass: oracle condensed 0.6765 vs direct (`ANSWER:` line) 0.6612, diff 0.0153 ≤ 0.02 |
| 8 | every run answered | pass: 4,000 run rows, all ok on the first attempt |
| 9 | one release | pass: 1.165.0 @ `f74bba4ec` first and last |

There was no spend-cap stop, no provider violation, and one profile (`haiku`) across all arms.

## Descriptive breakdowns (post hoc, none tested)

### 1. kb/ask's route

From each `kb` row's `ROUTE:` / `CLASSIFIER:` trailer and the `COVERAGE:` line where one
was written. Sections:
- `search`: rows with no `COVERAGE` line.
- `walker`: 13 rows with `COVERAGE` partial, 3 complete, 1 with none.
- `survey`: all 14 partial.

| route (classifier) | q | kb F1 | kb EM | kb page recall | kb USD/q | kb in / out tok/q | kb wall s | rag-lc F1 | rag-ext F1 | oracle F1 |
|---|---|---|---|---|---|---|---|---|---|---|
| search (decision) | 767 | 0.606 | 0.481 | 0.947 | 0.0029 | 12,224 / 1,491 | 39.4 | 0.644 | 0.652 | 0.678 |
| search (agent) | 1 | 0.160 | 0 | 1 | 0.0020 | 7,057 / 1,006 | 64.0 | 0.167 | 0.167 | 0.615 |
| walker (dated) | 17 | 0.650 | 0.529 | 0.941 | 0.0054 | 18,717 / 2,229 | 16.8 | 0.719 | 0.619 | 0.670 |
| survey (agent) | 14 | 0.294 | 0.143 | 1.000 | 0.0311 | 109,249 / 23,890 | 98.0 | 0.436 | 0.449 | 0.617 |
| search → walker (agent) | 1 | 0.154 | 0 | 1 | 0.0059 | 20,919 / 4,171 | 40.9 | 0.750 | 0.333 | 0.250 |
| all | 800 | 0.600 | 0.475 | 0.948 | 0.0034 | 14,064 / 1,901 | 40.0 | 0.642 | 0.647 | 0.677 |

- **Most questions went to `search`.** The decision model sent 96% of questions (767) there.
- **The deficit is on the main route.** On those questions `kb` is 0.04–0.05 F1 below both RAG arms. They account for about 30 of the 33.5 summed F1 by which `kb` trails `rag-lc` over all 800 questions (800 × 0.0419).
- **`survey` is the costliest route and the weakest:**
  - It handled the 14 questions the `kb/classify` agent sent there. All other routes cost $0.002–0.006 a question; `survey` cost $0.031, about 9× the arm's mean.
  - It scored lowest: F1 0.29, against 0.44 and 0.45 for the RAG arms on the same questions.
- **The `walker` (dated) questions:** `kb` 0.650, between `rag-ext` (0.619) and `rag-lc` (0.719).
- **Few questions behind the small routes.** The `survey`, `walker` and single-question rows rest on 1–17 questions each.

### 2. Run-to-run spread (the fixed 100 questions, 3 runs per arm)

| arm | F1 by run | EM by run | mean \|ΔF1\| between runs | mean per-question SD of F1 (n−1) | questions whose EM flips | questions whose F1 changes |
|---|---|---|---|---|---|---|
| kb | 0.538 / 0.549 / 0.563 | 0.37 / 0.42 / 0.40 | 0.141 | 0.119 | 18 | 42 |
| rag-lc | 0.602 / 0.593 / 0.592 | 0.42 / 0.41 / 0.42 | 0.101 | 0.086 | 13 | 32 |
| rag-ext | 0.620 / 0.638 / 0.625 | 0.45 / 0.46 / 0.46 | 0.079 | 0.066 | 10 | 34 |
| oracle | 0.646 / 0.622 / 0.623 | 0.49 / 0.46 / 0.46 | 0.065 | 0.054 | 11 | 29 |

- **Spread:** `kb` has the largest per-question spread. Even so, on each of the three runs its sample mean is below both RAG arms' by 0.03–0.09.
- **The sample is harder than the 800:** `kb` scores 0.538 on its first run, against 0.600 on all 800 questions.
- **Units:** "mean |ΔF1|" is the mean over questions and over the three pairs of runs. `summary-study-c.json`'s `mean_range_F1` is the same spread as a range (range = 1.5 × this figure for three runs).

### 3. The judge against EM

The judge is Haiku, the model that wrote every answer (Amendment 1). It saw the question,
the gold answers and one answer with links, trailers and the `ANSWER:` scaffold removed.

| arm | EM = 1 rated CORRECT | EM = 0 rated CORRECT | EM = 1 rated PARTLY | mean F1 of CORRECT / PARTLY / WRONG |
|---|---|---|---|---|
| kb | 298 / 380 (0.78) | 227 / 420 (0.54) | 81 | 0.718 / 0.529 / 0.032 |
| rag-lc | 400 / 403 (0.99) | 193 / 397 (0.49) | 3 | 0.837 / 0.261 / 0.013 |
| rag-ext | 404 / 407 (0.99) | 201 / 393 (0.51) | 3 | 0.830 / 0.220 / 0.014 |
| oracle | 427 / 432 (0.99) | 194 / 368 (0.53) | 5 | 0.849 / 0.313 / 0.006 |

**The ordering agrees.** The judge orders the arms as F1 does, by CORRECT count: oracle 621
> rag-ext 605 > rag-lc 593 > kb 525. It does so with PARTLY counted as half as well: 0.803
/ 0.794 / 0.778 / 0.775. On the half-credit score `kb` is 0.0025 below `rag-lc`.

**Where kb's PARTLY ratings come from.** `kb` gets 190 PARTLY against 42–61 for the
reader arms. Its EM-correct answers are rated PARTLY in 81 of 380 cases; the reader arms'
in 3–5.

We hand-read a seeded random sample (seed 20261123) of 20 `kb` rows with F1 < 0.5 rated
CORRECT or PARTLY, out of 233 such rows:
- **The answer is usually in the prose.** In most of them the gold answer, or its substance, is stated somewhere in `kb`'s prose.
- **The condensed span misses it.** The prose leads with something else: a hedged "may qualify", a figure computed for the asker's situation, a first-instance route before the gold one, or an introductory sentence. The condense step returns that lead, so F1 is low while the judge, reading the whole answer, credits it.
- **Where the PARTLY ratings come from.** The judge marks PARTLY for the hedge or for extra details it doubts. In 3 of the 20 it judged figures from the 2021 corpus against rates it believes are current.

The reader arms write one `ANSWER:` line, and a judge has little else to weigh.

### 4. Paired wins, ties and losses per question (condensed F1)

| comparison | questions | kb wins | ties | kb losses | mean diff |
|---|---|---|---|---|---|
| kb vs rag-lc | 800 | 127 | 466 | 207 | −0.0419 |
| kb vs rag-ext | 800 | 121 | 470 | 209 | −0.0469 |
| kb vs rag-lc, all three arms cited the gold page | 704 | 111 | 412 | 181 | −0.0427 |
| kb vs rag-ext, all three arms cited the gold page | 704 | 107 | 415 | 182 | −0.0465 |
| (rag-ext vs rag-lc) | 800 | 112 | 568 | 120 | +0.0049 |

- **kb found the right page and still answered worse.** Restricted to the 704 questions where every arm cited the gold page, the deficit is the same as overall.
- **Losses where kb missed the page are few.** Of `kb`'s 207 losses to `rag-lc`, `kb` cited the gold page in 194. Only 8 are losses where `rag-lc` cited the gold page and `kb` did not; against `rag-ext` the counts are 197 of 209, and 9.

**By answer shape.** Splitting on the shape of the gold answer, which is also post hoc:

| gold answer | q | kb F1 | rag-lc F1 | rag-ext F1 | oracle F1 | kb − rag-lc [98.75% CI] | kb − rag-ext [98.75% CI] |
|---|---|---|---|---|---|---|---|
| yes / no | 429 | 0.749 | 0.746 | 0.763 | 0.770 | +0.002 [−0.048, +0.051] | −0.014 [−0.061, +0.035] |
| a span | 371 | 0.428 | 0.521 | 0.513 | 0.569 | −0.093 [−0.138, −0.049] | −0.085 [−0.129, −0.042] |

On yes/no questions `kb` is level with the RAG arms. Its deficit is on questions whose
gold answer is a span: a fee, a body, a rule.

### 5. Answer length and the condense step

| arm | raw answer, mean chars (median) | raw answer, mean words | condensed span, mean words | condensed `NO ANSWER` | reader wrote `Unanswerable` |
|---|---|---|---|---|---|
| kb | 1,424 (1,310) | 185 | 4.9 | 49 (6.1%) | — |
| rag-lc | 109 (90) | 10.0 | 5.5 | 75 (9.4%) | 85 (10.6%) |
| rag-ext | 112 (100) | 10.2 | 5.3 | 53 (6.6%) | 71 (8.9%) |
| oracle | 100 (71) | 9.5 | 5.1 | 76 (9.5%) | 88 (11.0%) |

- **Every question has an answer.** All 800 are answerable, so every `Unanswerable` is a miss.
- **The reader abstains even with the gold sections.** The oracle says `Unanswerable` on 88 questions while holding the gold sections; 42 questions are called unanswerable by all three reader arms.
- **`kb` hedges rather than abstains.** In 226 of its 800 answers (by a phrase match) it says it could not find or confirm something, or that the knowledge base lacks it, yet only 49 condense to `NO ANSWER`.
- **The spans come out the same length.** Condensing brings every arm to about 5 words, so the length of the raw answers is not what is scored.

### 6. Cost

Cost per question, rep 0 (estimates at the ledger's prices: Haiku 5.5 $0.10 / $0.50 per
million input / output tokens, $0.01 cache read, $0.125 cache write; local models $0):

| arm | model calls | input | cached input | output | USD | wall s (median) | USD per EM-correct | USD per judge-CORRECT |
|---|---|---|---|---|---|---|---|---|
| kb | ≥ 1.24 | 14,064 | 100,747 | 1,901 | 0.00342 | 40.0 (40.1) | 0.0072 (380 correct) | 0.0052 (525) |
| rag-lc | 1 (+ local rerank) | 1,187 | 0 | 491 | 0.00036 | 32.3 (28.9) | 0.00072 (403) | 0.00049 (593) |
| rag-ext | 3 (+ 1 embed call, ~711 chars) | 8,918 | 0 | 1,747 | 0.00177 | 16.9 (12.5) | 0.0035 (407) | 0.0023 (605) |
| oracle | 1 | 629 | 0 | 425 | 0.00028 | 3.4 (2.5) | 0.00051 (432) | 0.00035 (621) |

- **kb's cost is a floor.**
  - Its call count is one per run in its tree that names a model, and most rows have a single `kb/search` run. The 100,747 cached input tokens per question show that each such run made several model requests.
  - Its Decision call and `rag-lc`'s rerank run on a local model and are not priced.
- **Haiku 5.5 thinks by default.** Every output-token count includes thinking, alike in every arm: a two-word `ANSWER:` costs about 400–500 output tokens.
- **Condense and judge are not in these figures.** They cost the same for every arm, about $0.07–0.18 per arm for condense and $0.13–0.32 per arm for the judge, over 1,000 rows each.
- **Wall time is not a clean comparison.** The arms ran at different hours on a shared deployment, with Study D running beside them. `rag-lc`'s wall time is mostly its search with the shared local rerank: its reader call alone takes about as long as the oracle's.

**One-off cost to make the corpus answerable:**

| what | for | volume | time | USD |
|---|---|---|---|---|
| ingestion (`ingest.py`, 4 workers) | kb and rag-lc (one tenant store) | 652 pages, 13,724 chunks, each embedded by `bge-m3` inside the server (about 4.6 M characters of section text) | 4,498 s for the 648 bulk pages | 0 (local embedder) |
| annotation (`kb/annotate`, 5 passes) | kb only | 4,670 model calls (4,669 planned by the report pass), 4,660 writes, 10 parse errors. Tokens: 4.08 M input, 14.94 M cached input, 1.64 M output | 25,107 s (7.0 h) | **1.38** from tokens. The ledger delta over the passes is 3.12–3.13; see below |
| rag-ext dense index (`ragext.py build`) | rag-ext only | 12,821 chunks, 401 embed calls, 5.67 M characters, 0 errors | 1,327 s | 0 (local embedder) |

**The annotation's dollar figure.** The pass log takes each pass's cost as the tenant
ledger's increase over the pass: $0.29, $0.30, $0.74, $1.22 and $0.58, which sums to
$3.12. Passes 1 and 2 ran alone, and their ledger increases match the token estimate to
$0.001. Passes 3–5 overlapped other spending:
- the `rag-lc`, `rag-ext` and oracle runs, $1.67 by their own row estimates;
- the smoke, about $0.02 at the corrected price;
- the pilot, $0.05.

With those removed, each pass matches its token estimate. The annotation itself cost about
$1.38. The $3.13 quoted elsewhere is the ledger over the annotation window, not the
annotation alone.

**Study spend.** The tenant ledger shows $8.33 for the whole study, against a $30 cap with
the runners stopping at $29. That figure was read from the ledger by the operator; it is
not in the work files. The per-row estimates account for it:

| item | USD |
|---|---|
| arm runs, all reps | 5.75 |
| condense | 0.40 |
| judge | 0.73 |
| annotation, from tokens | 1.38 |
| smoke, pilot and setup | about 0.09 |
| **total** | **8.33** |

### 7. Retrieval lists (RAG arms; over readable sections against the gold sections)

| arm | R@1 | R@3 | R@5 | R@10 | MRR@10 | gold page in top 5 |
|---|---|---|---|---|---|---|
| rag-lc | 0.7875 | 0.9313 | 0.9450 | 0.9525 | 0.8575 | 0.9788 |
| rag-ext | 0.7788 | 0.9275 | 0.9413 | 0.9637 | 0.8528 | 0.9613 |

- **Chance:** R@5 by chance is 0.00077 (mean over questions of `min(1, 5·|gold| / 12,800 readable sections)`).
- **What the reader received:** the 5 sections the reader read held a gold section for 0.945 (`rag-lc`) and 0.941 (`rag-ext`) of questions.
- **The two pipelines retrieve about equally well,** so the arms' answer scores differ little.

## Deviations and amendments

- **Amendment 0** (before any scored run):
  - Haiku in place of the planned self-hosted models, through five single-field `kb/*` forks.
  - `rag-lc` reranks through a helper agent.
  - A listwise Haiku rerank replaces the cross-encoder in `rag-ext`.
  - The reader uses the `ANSWER:`/`SOURCES:` format, and check 7 is read against it.
  - Chunk links count for their page.
  - `kb` has annotations but no taxonomy, ontology or relations.
  - The arms are not interleaved in time.
  - New idempotency-key prefix after the smoke; the smoke pages kept; the price list and the $30 cap.
- **Amendment 1** (before any judge call): the judge is Haiku, the reader model, instead of `ollama-local`/`ornith-1.5:35b`.
- **Amendment 5** (after the analysis; post hoc; changes no verdict): records check 1's failure, its cause, what was verified, and what a rerun of the check would need. The filter must also drop the tenant's untyped `/memory` system documents, as Study D's Amendment 2 did.

## Limits

- **The judge grades its own model's writing.** It is Haiku, the model behind every arm. It occasionally checks figures against rates it believes are current rather than against the corpus, and it rewards `kb`'s longer prose with PARTLY where EM gives 0.
- **ConditionalQA answers are often conditional** ("yes, if …"). F1 scores the answer span only, and the condense step favours a short lead. That counts against an arm that leads with a hedge or with a computed, situation-specific figure.
- **`kb`'s model-call count and cost are floors.** Its local Decision call and `rag-lc`'s local rerank are unpriced.
- **The breakdowns are post hoc and descriptive.** The routes, answer shapes and judge sample were not planned; the small routes rest on 1–17 questions.
- **The oracle is not a full ceiling.** It says `Unanswerable` on 11% of questions while holding the gold sections.

## What a reader should not conclude

- **Not a confirmed H3 or H4 failure.** Check 1 failed, so both are void. The intervals describe these 800 questions only.
- **Nothing about corpus-level questions.** Every question here is answered by one or two sections of one page. Overview and area questions are Study D's subject (`../2026-11-23-kbrag-docs/`).
- **Nothing about the full knowledge-base layer.** The `kb` arm had annotations, but the tenant had no ontology, taxonomy or relations, so those moves of the bundle had nothing to act on.
- **Not shipped-tier results.** All model-backed `kb/*` agents were Haiku forks, not the bundle's shipped tiers, and every arm used Haiku.
- **Not a general statement.** This is one corpus, one reader model and one run per question; the run-to-run spread for `kb` is about 0.14 F1 per question.
- **Not a cross-encoder result for `rag-ext`.** Its rerank is a listwise Haiku call, which makes it closer in kind to `rag-lc`'s than designed.
- **Nothing about wall time between arms.** The arms were not interleaved in time; they ran hours apart on a shared deployment, partly while the annotation pass and Study D were running.
- **Not that `kb` finds pages worse.** Its cited-page recall (0.948) is at least the RAG arms' (0.931–0.935). Its deficit is in the short answer span on non-yes/no questions.

## Files

- `summary-study-c.json`: the `analyze.py` output (`summary.json`), aggregates only, copied byte for byte from the work directory.

The run, condensed and judge files, the annotation log and the breakdown scripts stay
outside the repo, under the harness's work directory.
