# The annotated knowledge base against advanced RAG (pre-registration)

Drafted 2026-11-22 on loomcycle `main` @ 13c608083. **Not yet in force:** the items under
"Open items" are unset. They are filled in by one commit ("Amendment 0") before any
ingestion, search or agent run that is scored. **Amendment 0 (2026-11-23, at the end of
this file) puts Study C in force.** From that commit on, nothing below may
change after the first result is seen; any change is an amendment, dated and committed
before the run it affects. A mechanics smoke test (a handful of questions per study, never
scored, its output discarded) runs first.

## Question

loomcycle's knowledge-base bundle answers a question through `kb/ask`, which reads a layer
written *about* the documents (annotations: a brief and an abstract per section, document
and folder; the taxonomy; relations) as well as their passages. An advanced RAG pipeline
answers from passages alone, however well it finds them.

Does the annotated knowledge base give better answers than advanced RAG, on which kinds of
question, and at what cost?

## Studies

Two studies, on two tenants. They are reported separately and never averaged.

| | Study D (loomcycle docs) | Study C (ConditionalQA) |
|---|---|---|
| tenant | the development knowledge base, already annotated | a second tenant holding ConditionalQA |
| corpus | loomcycle's own docs, RFCs, plans and research | 652 gov.uk guidance pages, 13,724 sections (`bench/docs/measure/prepare.py`, the `header` form) |
| questions | the operator's golden questions: corpus-level (`overview`, `area`, `changed`) and any known-item questions the set holds | 2,515 answerable questions; answers are scored on the pre-drawn 800 of `bench/docs/measure/reader_subset.json` |
| gold | document ids (and `gold_topics` where given), in a gold file outside the repo | the evidence sections and the dataset's answers |
| what it favours | the knowledge base: its agents were developed on this corpus | passage retrieval: every question is answered by one or two sections of one page |

Study D's corpus is the one the bundle was tuned on. Its result is therefore an upper
reading for the `kb` arm and is said so wherever it is quoted. Study C is the transfer
check.

## Arms

Every arm answers with the **same reader model** and, where it embeds, the **same
embedder** (Open items). Arms differ in what is retrieved and how.

| arm | what it is |
|---|---|
| `kb` | `kb/ask` as shipped, on the annotated tenant, default depth (`standard`). Its routing to `kb/search`, `kb/survey` and `kb/walker` is part of the arm |
| `rag-lc` | loomcycle's passage pipeline only: `Document op=search` (hybrid vector + full-text, the index header, `memory_rerank` on at its shipped defaults), `annotations` not set, no `concept` filter, no relation hop. The first *k* readable sections go to one reader call |
| `rag-ext` | a pipeline outside loomcycle over the same documents (below). The first *k* sections go to the same reader call as `rag-lc` |

*k* is 5 in Study C (as RFC DM phase M) and 20 in Study D, where an answer names many
documents.

**`rag-ext`, fixed here** (component versions in Open items):

1. **Corpus.** The same documents as the tenant holds, read with `export_md` (Study D) or
   taken from `prepare.py` (Study C), keeping each document's id.
2. **Chunking.** One chunk per heading section; a section over 400 tokens is cut into
   400-token windows with 50 overlap.
3. **Contextual header.** Every chunk is indexed with `<document title> > <heading path>`
   before its text.
4. **Retrieval.** BM25 (k1 1.2, b 0.75) and dense vectors, 50 candidates each, fused by
   reciprocal rank (k = 60).
5. **Query expansion.** The question and three rewrites of it by the reader model; the
   four fused lists are fused again the same way.
6. **Rerank.** A cross-encoder over the top 50 fused chunks.
7. **Parent expansion.** Each kept chunk is replaced by its whole section; duplicates
   are dropped; the first *k* sections are kept.

It uses no annotation, taxonomy or relation of the tenant, and no text any loomcycle agent
wrote.

**The reader call** (`rag-lc`, `rag-ext`, and the oracle): one prompt, the same in both
arms, holding the question and the *k* sections, each rendered as
`[<document id>] <document title> > <heading path>` plus its text. It is told to answer
from the sections only and to cite the documents it used as `loom:doc/<id>`, the form
`kb/ask` cites in. Temperature 0.

**The condense step.** `kb/ask` writes prose with accounting lines; the reader writes what
its prompt asks. So that neither format is scored, every arm's raw answer goes through one
fixed call (the reader model, temperature 0) that sees only the question and that answer
and returns the shortest span that states it, or `NO ANSWER`. It is given no document. EM
and F1 are computed on its output; citations are read from the raw answer.

## What is scored

**Citations (both studies, all arms alike).** The documents an answer cites as
`loom:doc/<id>`. This is the one retrieval measure that means the same thing in an agent
and in a pipeline.

- Study D: per question, recall, precision and **F1 of the cited document set** against
  the gold (`gold` and `also` both count as correct for precision; only `gold` counts for
  recall). A question with `gold_topics` is scored on topics named, by the rule of
  `scripts/measure/walker/README.md`, and reported apart.
- Study C: **cited-page recall**: the question's page is among the documents cited.

**Retrieval lists (`rag-lc` and `rag-ext` only; reported, not tested).** R@1/3/5/10 and
MRR@10 over readable sections against the gold sections (Study C), and document recall at
*k* (Study D). `kb` has no ranked list, so it has no row.

**Answers.**

- Study C: EM and SQuAD-style token F1 on the condensed answer, the max over the
  question's gold answers.
- Both studies: a judge (Open items; never the reader model) rates each raw answer
  against the gold as correct, partly correct or wrong. It sees the question, the gold
  and one answer with the trailer lines and arm name removed, in shuffled order.

**Cost, per question and per arm.** Model calls, input / output / cached tokens, cost in
USD at the price list recorded in Amendment 0, and wall time. `rag-ext`'s rewrite and
rerank calls and `rag-lc`'s rerank are counted. Reported beside it, once per arm: what
it cost to make the corpus answerable (embedding and indexing for all; `kb/annotator` and
the taxonomy for `kb`, from the runs' own usage where it is on record, else estimated
with `kb/annotator write: report` and marked as an estimate).

## Runs

- **Study D:** every question, three runs per arm, interleaved by question so drift on
  the server reaches all arms alike. A question's score is the mean over its runs.
- **Study C:** the 800 questions, one run per arm. A fixed 100 of them
  (`random.Random(20261122).sample`) get two more runs per arm, to report run-to-run
  spread; these extra runs enter no test.
- The driver stops before a run that could pass the spend cap. A study stopped by the cap
  is reported as incomplete and its hypotheses as not tested; it is never scored on the
  part that ran.

## Hypotheses

Four co-primary tests, α = 0.0125 each (two-sided). Paired bootstrap over questions,
10,000 resamples, seed 1. The interval is the **98.75% CI** of the paired difference; a
hypothesis holds iff that interval lies wholly on the stated side of its bound.

- **H1 (Study D)** — `kb` has higher cited-document F1 than `rag-lc` on document-gold
  questions.
- **H2 (Study D)** — `kb` has higher cited-document F1 than `rag-ext`, same questions.
- **H3 (Study C)** — `kb` answers are not worse than `rag-lc`: `kb` − `rag-lc` mean F1 has
  a lower bound **above −0.02** (non-inferiority, margin 0.02).
- **H4 (Study C)** — the same against `rag-ext`.

Expected, and written down so the result can contradict it: the knowledge base wins
Study D by a wide margin on `overview` and `area` questions, and is level with or slightly
behind passage RAG in Study C at several times the cost.

Reported, not tested: every measure per question kind; whether `kb` is *superior* in
Study C (the same interval against 0); the judge's ratings; topic recall; `kb`'s route per
question (`ROUTE`, `CLASSIFIER`, `COVERAGE`); cost per question and cost per correct
answer; `rag-ext` against `rag-lc`.

**Power.** Study D has as many units as the gold file has questions. With a few dozen, only
a large difference can reach α = 0.0125. A result that does not hold there is "not shown",
not "no difference".

## Instrument checks (each can fail; a failed check voids the hypotheses that depend on it)

1. **Same corpus.** The document ids `rag-ext` indexed are exactly those the tenant's
   `query_documents` lists (less the operational types `SourceStub`, `SyncReport`,
   `Annotation`), in each study. Voids that study.
2. **The knowledge base is annotated.** `list_annotations summarize`: at least 99% of
   documents have a `document` annotation and at most 1% are stale, at the start of the
   study. Voids H1, H2 (Study D) or H3, H4 (Study C).
3. **`rag-lc` used no annotation.** No `rag-lc` search row carries `matched_annotation` or
   `summary`. Voids H1, H3.
4. **The rerank ran.** `reranked: true` on ≥ 95% of `rag-lc` searches, and the
   cross-encoder scored every `rag-ext` query. Voids the hypotheses of the arm that fails.
5. **Retrieval works.** In Study C, `rag-lc` and `rag-ext` R@5 are each more than 10×
   chance (`min(1, 5·|gold| / readable sections)` per question). Voids H3 or H4.
6. **The reader reads.** In Study C, one oracle pass (the gold sections, at most 5) scores
   above both RAG arms' F1. Voids H3, H4.
7. **The condense step is faithful.** On the oracle answers, condensed F1 is within 0.02
   of the F1 of the same answers scored directly. Voids H3, H4.
8. **Every run answered.** No (arm, question, run) is left failed; a failed run is retried
   once and, if it fails again, voids its study. A `kb` run that ends without `ROUTE:`
   counts as failed.
9. **One release.** The deployment reports the same version at the first and last run of
   a study. Voids that study.

## Decision

- **H1 and H2 hold:** the annotated knowledge base is documented as answering
  corpus-level questions that advanced RAG does not, with the cost beside it and the
  caveat that the corpus is its own.
- **H1 holds, H2 fails:** an outside pipeline closes the gap that loomcycle's passage
  search leaves; what `rag-ext` does that `Document op=search` does not is investigated.
- **H1 fails:** recorded as the annotation layer not shown to help on its home corpus.
  The answers are read by hand before anything else is concluded.
- **H3 and H4 hold:** `kb/ask` is documented as safe to use for passage-level questions,
  with its cost multiple.
- **H3 or H4 fails:** documented as the knowledge base costing answer quality on
  passage-level questions; whether `kb/ask` should hand such questions to plain search
  sooner is an operator decision this informs. Nothing changes automatically.

## Open items (set by Amendment 0, 2026-11-23, for Study C; Study D's stay open)

| item | value |
|---|---|
| deployment base URL, Study D tenant | `https://loomcycle.lab.dglabs.dev`, tenant `loomcycle-dev` (Study D not started) |
| deployment base URL, Study C tenant | `https://quality-qa.lab.dglabs.dev`, tenant `conditional-qa` |
| token env var names (values never recorded) | `LOOMCYCLE_BENCH_TOKEN_CQA` (Study C) |
| deployed loomcycle version and commit | 1.165.0 @ `f74bba4ec` (both tenants, one deployment) |
| Study D gold file | open |
| Study C tenant | empty before this study; ingested and annotated by this harness (`ingest.py`, `annotate.py`) |
| reader model and provider | `anthropic` / `claude-haiku-5-5`, through the deployment |
| embedder | the deployment's: `ollama` / `bge-m3:latest`, 1024 dimensions; `rag-ext` embeds through `POST /v1/embeddings` |
| judge model | `ollama-local` / `ornith-1.5:35b` |
| `rag-ext` components | Python standard library only: regex tokenizer `\w+\|[^\w\s]`, own BM25, exact cosine over the embedder's vectors; rerank per Amendment 0 §3 |
| price list used for cost | the deployment's ledger: Haiku 5.5 at $0.10 / $0.50 per million input / output tokens, $0.01 cache read, $0.125 cache write; local models $0 |
| spend cap | $30 for the study (operator, 2026-11-23); the runners stop at $29 cumulative tenant spend |

## Limits

- An agent is compared with pipelines. `kb` may make many model calls where the RAG arms
  make one reader call; that difference is the cost column, not a flaw to correct.
- `rag-ext` is one reasonable advanced pipeline, built for this study. It is not a tuned
  product and no claim is made about any named framework.
- The judge is a model. Its ratings are secondary and are reported with a hand-read
  sample of its disagreements with F1.
- Study D's gold names a real tenant's documents and never enters this repo; only counts
  and scores are published.

## Amendment 0 — 2026-11-23 05:20 UTC, before any scored run

Study C only. At this point the corpus is ingested (652 pages, 13,724 chunks, every tree
checked against `prepare.py`'s sections), the annotation pass is running (about 58% of
documents), and the only questions any arm has been run on are an unscored smoke (4
questions) and an unscored pilot (20 questions outside the reader subset, RAG arms and
oracle only). No scored run exists and no score of a study question has been computed.

**Data.** ConditionalQA v1.0, `v1_0/{documents,dev,train}.json` from
github.com/haitian-sun/ConditionalQA, sha256 `1c977c1b…856c59`, `a06cfe3e…2fb7c7`,
`2976f448…18bc2d`. `bench/docs/measure/prepare.py` unchanged: 652 pages, 13,724 sections,
2,515 questions; the 800 of `bench/docs/measure/reader_subset.json`.

**1. Models.** The operator first asked for a self-hosted provider, then (2026-11-23)
for Claude Haiku under a price cap, with `ollama-local` models allowed. The deployment's
tiers do not resolve to Haiku and a run's model pin does not reach a child agent of
another definition, so the five model-backed agents of the bundle are forked in the
tenant with one change each, `provider: anthropic`, `model: claude-haiku-5-5` in place of
their tier: `kb/classify`, `kb/search`, `kb/screen`, `kb/walker`, `kb/annotate`
(`setup.py`; `--check` shows no other field differs). `kb/ask`'s Decision call stays on
the deployment's decision model (`ollama-local` / `nimble`). The reader, condense and
query-rewrite calls are Haiku through `POST /v1/_llm/chat`. Every run is sent with
`user_tier: free`. A call served by any model other than Haiku, an `ollama-local` model
or the embedder stops the runner. Haiku 5.5 thinks by default in every arm alike, so
output-token counts include thinking.

**2. The `rag-lc` rerank.** The deployment has a reranker (`ollama-local` / `nimble`).
It applies only inside a run of an agent that opts in, so `rag-lc` searches through a
tenant helper agent `kbrag/search-rr` (code-js, `Document` read-only,
`memory_rerank: {enabled: true}`, search `limit: 20`, no annotation option).

**3. The `rag-ext` rerank is not a cross-encoder.** None is reachable. Step 6 becomes a
listwise rerank by the reader model: one call sees the 50 fused candidates, each cut to
600 characters, and returns the 20 most relevant in order; the rest follow in fused
order. A reply with fewer than 10 valid numbers keeps the fused order and is counted as
`rerank_failed` against check 4. This makes `rag-ext`'s rerank closer in kind to
`rag-lc`'s than the design intended; it is the best the allowed models offer.

**4. The reader's format, and check 7.** The reader is told to write `ANSWER: <short
answer>` on its first line and then `SOURCES:` with `loom:doc/<id>` links. "Scored
directly" in check 7 means EM/F1 on that `ANSWER:` line. The oracle reads the readable
gold sections, at most 5.

**5. Citations.** `kb/ask` cites sections (`loom:chunk/<id>`) more often than documents.
Cited-page recall counts a chunk link for the page it belongs to; the documents-only
figure is reported beside it.

**6. What the `kb` arm has.** Annotations (section, document, folder) and passages. The
tenant has no ontology, no taxonomy and no relations, and none is made for this study, so
the bundle's taxonomy moves have nothing to act on. `kb/ask` runs at depth `standard`,
with its shipped classifier.

**7. Run order and concurrency.** The arms are not interleaved in time. `rag-lc`,
`rag-ext` and the oracle run while the annotation pass is still writing (a plain search
reads no annotation; check 3 verifies it per row). `kb` runs after the annotation pass
has finished and check 2 has been taken (`precheck.py`). Runs go 4 at a time, submitted
in the seeded order; completion order is not fixed. The 100-question spread repetitions
run after the 800 primary runs of each arm.

**8. Keys and seen questions.** The smoke used four subset questions (`dev-20`,
`train-1337`, `train-1927`, `train-948`) under the study's idempotency keys; the study's
keys were moved to a new prefix so no smoke run can be returned as a study run. Those
four questions stay in the 800. No prompt was changed after the smoke.

**9. Smoke pages.** Four pages (`cqa-001`, `cqa-002`, `cqa-003`, `cqa-086`) were ingested
and annotated during the smoke by the same procedure as the rest and are kept.

**10. Cost.** Per-row cost is computed from tokens at the ledger's prices (it matched the
ledger to four digits on the pilot). `kb`'s cost is the sum over its run tree and is a
floor: its Decision call and the rerank of `rag-lc` are local and unpriced. The
annotation pass's cost is taken from the ledger.

**11. Judge.** About 12 s a call on a shared GPU, one at a time. It runs last and may
finish after the other measures are reported; a report without it says so.

Nothing else changes: arms, metrics, hypotheses H3 and H4, α, the bootstrap and the
instrument checks stand as written above.

## Amendment 1 — 2026-11-23 05:40 UTC, before any judge call on a study row

**What happened.** The judge of Amendment 0 (`ollama-local` / `ornith-1.5:35b`) takes
about 12 s a call, one at a time, on a GPU it shares: about 13 hours for the study. The
operator asked for the judge to run on Haiku for this benchmark. No judge call has been
made on a study row; the judge has only seen the unscored smoke and pilot rows.

**Change.** The judge is `anthropic` / `claude-haiku-5-5`, the reader model, with the same
prompt, the same shuffled order and the same inputs. The design's "never the reader
model" is given up.

**What that costs.** A model grading text it wrote may favour it. Here every arm's answer
is written by Haiku (the reader in `rag-lc`, `rag-ext` and the oracle; the bundle's forked
agents in `kb`), so such a preference would act on all arms alike and should not favour
one. It can still make the ratings lenient overall. The judge stays a secondary measure:
H3 and H4 are tested on token F1 and do not depend on it, and the report gives its
agreement with EM beside its ratings.

Nothing else changes.

## Amendment 2 — 2026-11-23 07:00 UTC, Study D, before any scored run

Study D only; it runs in parallel with Study C on the same deployment (1.165.0 @
`f74bba4ec`), tenant `loomcycle-dev`, harness `bench/docs/kbrag/docs/` (its README has the
commands). Before this commit the only arm runs were an unscored smoke: two questions
(`p7-q03`, `k01`), one run per arm, under their own idempotency-key prefix; both questions
stay in the study and no prompt or setting was changed after the smoke.

**1. Question sets and gold.** Three sets, scored and reported separately, never pooled. No
gold file was found on this machine at first; the operator then pointed to the existing gold
in a separate internal repo, which is used as is (read-only; only hashes are recorded here).
- **`p7` (primary).** The 24 questions of `scripts/measure/walker/questions.yaml` (13
  `overview`, 7 `area`, 4 `changed` as the file stands; its README's 12/6/6 is out of date)
  with the gold `gm-p7/p7_gold.yaml` (sha256 `1ce00b292fd4d90f6c741877f0e52d28909f52f10420ac0126f0ccae4f00dcac`), drafted 2026-11-20 by an
  agent from read-only store listings and briefs, and approved by the owner before the RFC GM
  P7 walker measurement. 21 questions carry document gold, 3 (`q18`, `q20`, `q22`) topic gold.
  One `q21` gold document no longer exists in the tenant; it stays in the gold (it lowers
  every arm's recall alike). Caveats: the gold was built from the same annotation layer
  (briefs) the `kb` arm reads, a bias towards `kb`; and the set was used once, on 2026-11-20,
  to compare two `kb/walker` prompts, after which the walker kept only its "what changed"
  move (#2818). It was not used to tune `kb/ask`, `kb/survey`, `kb/screen` or `kb/search`.
- **`waves` (secondary).** The operator's 12 question waves (`doc-internal/kb-walker-questions-
  loomcycle-dev*.md` with their keys), normalised to 261 questions (133 overview, 44 specific
  fact, 36 known item, 18 general, 30 no-answer; sha256 of the normalised file `e7143979eb6d1682d0d853be0e9c8d1c18df8b7dc0d4464c1e10974ae498b7d1`):
  a question's `core` documents are `gold`, its borderline `(?)` documents `also`. **These
  waves were used to develop the kb agents**: the results files record that wave 1 tuned the
  walker prompt and classifier wording, and each of waves 3–12 measured a code change and
  decided the next one; wave 2 states that nothing was tuned on it but it served as the
  routing design's acceptance gate. They therefore favour `kb` and are a labelled secondary.
  Some keys are time- or state-dependent (listings by date or status as of 2026-11-17/18) and
  have drifted. A question with no core document (the 30 no-answer and 5 others) has no
  citation score and is rated by the judge only.
- **`known` (secondary, mechanical).** 30 known-item questions, gold = one document: 30
  documents drawn with `random.Random(20261123)`, one 600+-character section each, a question
  written by Haiku from that section's text alone, then checked by a second, separate Haiku
  call that saw only the question and the section (all 30 passed; the check is lenient). No
  annotation, brief or taxonomy entered this set. sha256 `88dd522a69547c69a4f2416165165e9e0bbd05a5f020e7ac79d31b932c3cc080`.

**2. Hypotheses.** H1 and H2 as pre-registered, tested on the `p7` set's 21 document-gold
questions: paired bootstrap over questions on cited-document F1, 10,000 resamples, seed 1,
98.75% CI; a question's score is the mean of its runs. Reported per kind as well. The same
paired comparison on `waves` and `known` is reported, labelled secondary, and enters no
decision. With 21 questions the test can only detect a large difference; a result that does
not hold is "not shown". Topic questions are scored on topics named (walker README rules).

**3. Runs.** `p7`: 3 runs per question per arm (216 runs). `waves`: 1 run per arm (783 runs).
`known`: 1 run per arm (90 runs). Arms per (question, run) in a seeded rotated order,
run-major, 4 workers, `p7` first, then `waves` and `known`. If the cap or time runs short,
`known` is cut first, then `waves` (a seeded half), never `p7` or an arm.

**4. Corpus.** All 409 documents of the tenant scope less the operational types and the two
untyped system documents (deployment context, tenant ontology): 23 excluded. Not only
`/loomcycle`: the gold names documents filed elsewhere. `rag-lc` searches the tenant with
these 23 excluded by id (`exclude_document_ids`, applied inside every leg); `rag-ext` indexes
the same 409 (one has no readable text). `kb/ask` cannot be restricted: a citation outside
the corpus counts as wrong for precision; precision over in-corpus citations is reported
beside it. `kb/ask`'s chunk citations count for their document; documents-only is reported
beside. Sections are chunks as `export_md` renders them (heading path, body).

**5. Arms.** k = 20. `rag-lc`: helper `kbrag/search-rr` (code-js, `Document` read only,
`memory_rerank` on at the shipped defaults: the deployment's decision-kind reranker, pool 20),
search `limit: 40` so that 20 distinct readable sections remain. `rag-ext`: Amendment 0 §3's
listwise Haiku rerank in place of the cross-encoder, 3 Haiku rewrites, RRF k 60, parent
expansion; 11,421 chunks embedded by the deployment's `bge-m3`. Reader: one Haiku call, the
question and 20 sections (each cut at 8,000 characters; 165 of 7,326 readable sections are
longer), answering in prose and ending `SOURCES:` with `loom:doc/<id>` links. No condense
step (Study D scores citations, not answer spans). `kb`: `kb/ask` at depth `standard`.

**6. Haiku routing and its side effect.** In this tenant `kb/classify`, `kb/search` and
`kb/screen` already had active tenant overlays (2026-11-13/14/16, model `haiku` =
`anthropic/claude-haiku-5-5`; kb/search's prompt equals the bundle's; kb/classify's differs by
one character and lacks the `-Calc` tool denial; kb/screen lacks `-Calc`). They are run as
they are, untouched. Only `kb/walker` was forked (`def_6d0b431adb12fb12`, provider+model only,
checked against the static definition) and promoted at 06:51:13 UTC; it shadows the bundle
walker for every run in the tenant until it is retired right after the last scored run
(`forks.py --restore`, then `--check`). `kb/annotate` is not forked. `kb/ask`'s Decision call
stays on `ollama-local`/`nimble`. Every run is sent with `user_tier: free`. A call served by
anything but Haiku, an `ollama-local` model, code-js or the embedder stops the runner.

**7. Judge.** Haiku (operator approval; Amendment 1's caveat stands: a model grading text it
wrote may be lenient, alike for all arms). Blind to arm, arms shuffled with a seeded RNG per
(question, run), trailers and links removed. It sees the question and the gold described:
each gold document's title and brief (the annotation layer — another lean towards `kb`), or
the topic names, plus the expected answer text where the key has one. Ratings `COMPLETE` /
`PARTIAL` / `WRONG`. Secondary only.

**8. Instrument check 2 fails at the start.** `list_annotations summarize` at 06:50 UTC:
document annotations 404 for 409 corpus documents (98.8%, bar 99%), 10 of 404 stale (2.5%,
bar 1%; the server checked 2,000 annotations for staleness). Nothing is re-annotated (this
study writes nothing to the tenant's documents). As pre-registered, H1 and H2 are therefore
**void**; they are still computed and reported as descriptive results, marked void.

**9. Cost.** Cap $20; the runner stops at a $19 increase of the tenant ledger over the
baseline taken at 06:34 UTC ($36.7924). The smoke cost $0.085 by the ledger. Projection from
the smoke: a `kb` survey run about $0.05 and 145 s, a `kb` search run $0.006, a RAG run
$0.002–0.006; about $16 for all runs and the judge. Other users' runs in the tenant would
also raise the ledger; the per-row token estimate is kept beside it.

Nothing else changes.

## Amendment 3 — 2026-11-23 07:25 UTC, Study D, incident and resume

**What happened.** The scored runs of Amendment 2 started at 07:00 UTC. A follow-up script,
meant to restore the tenant after the last run, waited on the wrong process: it took a
short-lived wrapper's PID for the runner's and concluded at 07:15:42 that the runs had ended.
It retired the `kb/walker` fork at **07:15:43** (the walker resolved to its static tier,
`ollama-local`/`qwen3.6`, from then on) and started the judge while runs were in flight. Its
retire of the rag-lc helper silently did nothing (the script read the wrong key of the
AgentDef listing); the helper stayed active until it was retired by hand at **07:19:38**. The
orchestrator killed every Study D process at 07:16:38–07:16:49. Separately, the three wave-12
question ids that carry an apostrophe made idempotency keys the server refuses (HTTP 400), and
three p7 rep-0 rows (kb q04, kb q08, rag-lc q08) had failed earlier: a runner restart at 07:00
disconnected their streams, the server cancelled them, and their keys replayed the cancelled
runs. No run tree was found served by anything but Haiku, `ollama-local`, code-js or the
embedder, and no scored row records a non-Haiku kb agent.

**Windows.** First: `kb/walker` fork `def_6d0b431adb12fb12` active 06:51:13 → 07:15:43; helper
`def_915664b3d21f0619` active 06:51:14 → 07:19:38. At 07:19:39 the tenant was verified as before
the study (every kb name resolves to the def it had before; the helper inactive). The second
window opens when the driver below re-creates both, and closes when it restores them; its
times are in the results.

**Rows kept and quarantined.** Rule: a run row is kept if and only if its status is ok, it
ended (start + wall time) no later than 07:15:43, every model-backed `kb/*` run in its tree
(kb rows) was served by `anthropic`/`claude-haiku-5-5`, and (rag-lc rows) its search was
reranked. Everything else moved to `study/quarantine/` with the reason, never deleted. Kept:
kb 18, rag-lc 19, rag-ext 22, all `p7` rep 0. Quarantined: kb 4 (q04 and q08 cancelled, q06
ended 07:16:04, one wave-12 key refusal), rag-lc 3 (q08 cancelled, q06 ended 07:15:44, one key
refusal), rag-ext 0. The premature judge rows and analysis are quarantined too. The cancelled
and refused rows are failures of the harness, not of an arm; their questions are run afresh
as attempt 1 (check 8 is read on the resumed rows).

**Defects fixed** (`bench/docs/kbrag/docs/`, with tests in `tests/test_guards.py`):
1. One driver, `study.py`, runs every step in sequence in its own foreground: route
   `kb/walker` to Haiku and create the helper, then preflight + scored runs per set, retry
   passes, then restore + verify in a `finally` that SIGTERM and SIGHUP reach (a runner still
   going is stopped first), then judge and analysis. Nothing is inferred from a PID file.
   `forks.py --verify-restored` and `register.py retire` now fail loudly when the tenant is not
   as before.
2. Idempotency keys map the question id to `[A-Za-z0-9._-]` plus a short hash of the
   original id; the resumed study uses a new key prefix, `kbrag-docs-study2`, so no
   cancelled or refused run can be replayed.
3. Guards: a kb row whose tree holds a model-backed `kb/*` run not served by Haiku is a
   violation and stops the runner; a failed or unreranked rag-lc search among the first five
   of an invocation, or three in a row later, stops it; a preflight (every model-backed kb
   agent resolves to Haiku, the helper is active) runs before each set and every 20 runs, and
   a failure stops it.

**Resume.** Same design, same order: `p7` at 3 runs per arm first (the kept rows count), then
`waves` and `known` at 1 run per arm, retry passes, restore + verify, judge, analysis. The cap
is unchanged: a $19 increase over the 06:34 baseline ($36.792425); about $0.75 is spent.

Nothing else changes.
