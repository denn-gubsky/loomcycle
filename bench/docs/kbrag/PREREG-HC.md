# RFC HC gate study: the first call against today's kb/ask and passage RAG (pre-registration)

> **In force from Amendment 0 (2026-11-28, at the end of this file).** Amendment 0 records the model
> profile, every tenant fork with its `content_sha256`, the state of both tenants, and the
> confirmation of the points marked "to be confirmed". It is written after the tuning on the
> dev split and committed before the first scored run. Nothing scored runs before it.
> After that, every change is a dated amendment committed before the run it affects.

This restates RFC HC §6 (`/loomcycle/rfcs/rfc-hc-kb-rag`, confirmed 2026-11-27) as binding.
It also restates the P0–P1 plan §3–§5 (`/loomcycle/docs/rfc-hc-p0-p1-plan`). The benchmark's
pre-registration (`PREREG.md`, Amendments 0–5) is not changed by this file. The harness is
`bench/docs/kbrag/hc/`.

## Question

Does `kb/ask` with the first call (`kb/answer`: one reranked passage search, K sections read
by code, one `kb/read` call that answers first and cites) do three things?

- Answer passage-level questions at least as well as passage RAG.
- Answer them better than `kb/ask` does today.
- Keep corpus-level answers, at no more than the stated cost.

The result gates RFC HC phase P2: the first call becomes the default.

## Splits (RFC HC §6.1)

| set | used for | never used for |
|---|---|---|
| ConditionalQA dev: the 1,715 answerable questions outside the 800 (`hc/splits.json`, `dev`) | the `kb/read` prompt, the answer-first `kb/search` prompt, the option defaults | any test |
| ConditionalQA test: the 800 of `bench/docs/measure/reader_subset.json` (`hc/splits.json`, `test`) | T1, T2 | any tuning |
| the operator's 12 question waves | corpus-level development (already tuned on) | any test |
| the walker set p7: 24 questions, 21 with document gold (`scripts/measure/walker/questions.yaml` + a gold file outside the repo) | T3 | any tuning |

- **Frozen split.** `hc/splits.json` was built by `hc/splits.py` from
  `bench/docs/measure/prepare.py`'s 2,515 answerable questions over the public ConditionalQA
  v1.0 files: test = 800, dev = 1,715.
- **Split guard.** The runner refuses to mix sides: a test question in a dev invocation, or a
  dev question in a scored one, stops it before any run.
- **Past looks at p7.** It has been looked at three times:
  1. 2026-11-20, the RFC GM walker comparison.
  2. 2026-11-23, the kb-vs-RAG benchmark.
  3. `gm-p7/set-2026-11-26`.

  Amendment 0 lists any further look. Every look at the dev split is logged; none at the 800
  or at p7 before the scored runs.

## Tenants, models and setup

- **Two tenants of one deployment.** The ConditionalQA tenant (the benchmark's, already
  ingested) and the development tenant (p7). No host or tenant name is recorded here. The
  harness reads them from environment variables (`hc/hcconfig.py`).
- **Agents come from the merged P1 bundle text, not a deploy.** `kb/answer`, `kb/read` and
  the flagged `kb/ask` are created as tenant AgentDefs in both tenants, copied byte for byte
  from the merged bundle. Amendment 0 records:
  - the commit;
  - each def's `content_sha256`;
  - the state of every watched agent (`hc/tenant_state.py --record`).

  The runner re-reads that state before each invocation and every 20 runs. A changed or
  retired def stops it.
- **Model profile.** Every model-backed agent runs on Claude Haiku 5.5 (`anthropic` /
  `claude-haiku-5-5`), through single-field tenant forks (provider and model only):
  - `kb/classify`, `kb/search`, `kb/screen`, `kb/walker`;
  - `kb/read`;
  - the answer-first search.

  The decision model and the reranker stay the deployment's local models. A model-backed
  `kb/*` or `kbhc/*` run served by anything else stops the runner (provider violation).
- **The answer-first arm runs under its own names.** A tenant fork of `kb/search` would
  shadow it for every arm. So `kb-today+answer-first` runs:
  - a copy of `kb/ask` under its own name (default `kbhc/ask-af`), which spawns
  - the answer-first `kb/search` under its own name (default `kbhc/search-af`).

  Both are tenant-side setup, recorded in Amendment 0. This keeps the four arms interleaved
  on one tenant (interpretation; to be confirmed in Amendment 0).

## Arms (RFC HC §6.2 with §11; plan §3.3)

| arm | ConditionalQA 800 | p7 | what |
|---|---|---|---|
| `kb-today` | yes | yes | `kb/ask` as on main at the start of the study (with #3018 and #3019) |
| `kb-first` | yes | yes | `kb/ask` with run metadata `first_call: true` (P1 flag), defaults |
| `kb-today+answer-first` | yes | yes | `kb/ask` as shipped, with `kb/search`'s prompt putting the answer first on its own line (§11 added arm); secondary only |
| `rag-lc` | yes | yes | the reference: `kb/answer` spawned directly (its own search and reader) |
| `oracle` | yes | no | `kb/read` on the question's readable gold sections (at most 5), for checks 6 and 7 |

**Not run:**

- `rag-ext`. T1 is against `rag-lc` only; the benchmark's `rag-ext` numbers stay a
  descriptive reference.
- The `low`-reader arm. On this deployment it is the same model.
- The K = 10 / 20 arms. Those are P3a.

**How the oracle sees its sections.** Exactly as kb/answer hands them to kb/read, so the
oracle measures the reader alone. The prompt is kb/answer's `ASK + "\n\nQUESTION: " +
question`, as trusted text. The sections go in as one untrusted `tool_output` block built as
kb/answer's `block()`: its `HEAD` line, then one row per section,
`S<n> | <chunk id> | <document title> > <section title> | <text>`, rows joined by a newline,
each field passed through kb/answer's `neutral()` (so the text is one line), the text clipped
at `CONFIG.section_chars`, and the block held to `CONFIG.context_chars` with the `MIN_TEXT`
rule for the last row. The harness reads `HEAD`, `ASK`, `CONFIG` and `MIN_TEXT` from the
kb/answer agent in `bundles/knowledge-base/loomcycle.yaml` at run time and refuses to start the
oracle when they are missing. Amendment 0 confirms that this matches the merged `kb/answer`.

## Runs

- **ConditionalQA 800.** One run per arm. A fixed 100 (`random.Random(20261122).sample`,
  the benchmark's sample) get two more runs per arm; they are for run-to-run spread only and
  enter no test.
- **p7.** The 21 document-gold questions (and the 3 topic-gold ones, reported apart), 3 runs
  per question per arm.
- **Order and workers.**
  - Rep-major, with questions in an order seeded by 20261127.
  - Per (question, run), the arms run back to back in an order rotated by a seeded RNG.
  - 4 workers.
- **Resumable.** A failed run is retried once.
- **Idempotency keys.** Each key carries the output root, the split, the arm, the question
  and the run, so a tuning root never hands its runs to the study root.

## What is scored

- **ConditionalQA.**
  - **EM and SQuAD token F1** on the condensed answer, the max over the gold answers. The
    condense step is the benchmark's: one fixed Haiku call that sees only the question and
    the raw answer (accounting lines removed) and returns the shortest span, or `NO ANSWER`.
  - **Cited-page recall:** the question's page is among the documents cited. A
    `loom:chunk/` link counts for its page.
- **p7.** Per run, the cited-document set's precision (`gold` and `also` correct), recall
  (`gold`) and F1. A question's score is the mean over its runs.
- **Cost.** USD at the price model below, summed over each row's run tree: the root and
  every child. Also tokens, model calls and wall time.

## Hypotheses (RFC HC §6.3)

Three co-primary tests, each two-sided at α = 0.05/3 = 0.0167. Each uses a paired bootstrap
over questions, 10,000 resamples, seed 1. The interval is the **98.33% CI**; a test holds iff
its lower bound lies above its bound.

- **T1 (ConditionalQA 800):** `kb-first` − `rag-lc`, mean condensed F1. Lower bound
  **above −0.02** (non-inferiority).
- **T2 (ConditionalQA 800):** `kb-first` − `kb-today`, mean condensed F1. Lower bound
  **above 0** (superiority).
- **T3 (p7, 21 document-gold questions, 3 runs each):** `kb-first` − `kb-today`, mean
  cited-document F1. Lower bound **above −0.10** (non-inferiority).

A test that does not hold is "not shown".

**Cost checks** (thresholds, not tests), at this profile:

- ConditionalQA: `kb-first` mean USD per question ≤ 0.5 × `kb-today`'s.
- p7: `kb-first` mean USD per run ≤ 1.10 × `kb-today`'s.

**Secondary, reported with their cost.** All are paired, with 98.33% CIs.

- `kb-today+answer-first` against `kb-today` and against `kb-first` (condensed F1 on the 800,
  cited-document F1 on p7).
- `rag-lc` against `kb-today` on p7.

None of these gates anything; the answer-first arm ships only on a later decision.

**Reported, not tested:**

- per route and per answer shape (yes/no against span);
- the first call's outcomes (`FIRST`);
- the escalation rate and what escalated questions scored;
- `not_found` on questions whose gold section was among the K read;
- run-to-run spread on the fixed 100;
- cited-page recall per arm.

The judge is not run: it separated the arms little in the benchmark and graded its own model.

## Instrument checks (each can fail; a failed check voids the tests named)

The benchmark's nine checks stand, with the three changes of RFC HC §6.4 (checks 1, 2 and 4
below) and the owner's rulings of 2026-11-27 (checks 5, 6 and 7). Each has a unit test in
`hc/tests/` where it fails.

1. **Same corpus.**
   - **The filter (§6.4 fix 1).** The tenant's documents are listed with `graph_snapshot`.
     The filter drops:
     - the operational types (`SourceStub`, `SyncReport`, `Annotation`);
     - **every untyped document with a Path name under `/memory`**;
     - **the tenant ontology document** (by id, else by the name `/memory/ontology`).

     The same filtered list is check 2's denominator.
   - **ConditionalQA.** The filtered list must equal the ingested pages (less any page with
     no readable section).
   - **Both sets.** No document cited by a `rag-lc` row may lie outside it. `kb/ask` cannot
     be restricted, so a kb citation outside the corpus counts as a wrong citation, not a
     corpus failure.

   Voids that set's tests.
2. **Annotated before the run (§6.4 fix 2).**
   - **Sweep first.** An annotator sweep (`kb/annotator`, `mode: sweep`, `write: annotate`;
     `hc/sweep.py`) runs on each tenant before the first scored run. A tenant that cannot be
     annotated before the study is not used.
   - **Then the check.** Taken over check 1's list: document annotations on ≥ 99% of the
     corpus, and ≤ 1% stale.
   - **Sample size.** The full listing checks staleness on every row. The server's
     `summarize` sample size (`stale_checked`) is recorded beside it.

   Voids that set's tests.
3. **The first call used no annotation.** No `kb/answer` search (read from the run's
   replayed events) carries `annotations` or `with_summary`. Voids T1, T2 (ConditionalQA) or
   T3 (p7).
4. **The first call reranked (§6.4 fix 3).** `RERANKED: true` on ≥ 95% of the rows where
   `kb/answer` ran (every `rag-lc` row, and every `kb-first` row whose route starts with the
   first call), read from the trailer. Voids T1, T2 or T3.
5. **Retrieval works.** `rag-lc`'s cited-page recall on the 800 exceeds 10 × chance. Chance
   per question is `min(1, 5 × readable sections of the gold page / readable sections of the
   corpus)` (owner ruling; the benchmark used R@5, which a `kb/answer` row does not expose).
   Voids T1.
6. **The reader reads.** The oracle's condensed F1 is above `rag-lc`'s. Voids T1, T2.
7. **The condense step is faithful.** On the oracle's answers, condensed F1 is within
   **0.03** of the F1 of their own `ANSWER:` line scored directly. This is the owner's ruling
   (the benchmark used 0.02), **to be confirmed in Amendment 0**. Voids T1, T2.
8. **Every run answered.** No (arm, question, run) a test needs is left failed after one
   retry. A kb run without `ROUTE:`, or a `rag-lc` run without `FIRST:`, counts as failed.
   Voids that set's tests.
9. **One release.** The deployment reports the same version at the first and last run of
   each set. Voids that set's tests.

A test is computed only on a complete set: every arm it compares holds an ok row (condensed,
for F1) for every question and run it needs, with no spend-cap stop and no provider
violation. Anything less is reported as not tested; it is never scored on the part that ran.
Rows run under more than one recorded tenant state make the set incomplete.

## What would stop each piece (RFC HC §6.5; plan §5)

- **P2 opens** iff all of these hold:
  - T1 holds and T2 holds;
  - T3 does not fail with a point estimate below −0.05;
  - both cost checks hold.
- **Otherwise** the first call stays opt-in. The shortfall is read by hand before any
  tuning, and the result is reported as it is.
- **The routing changes** (#3018, #3019, already merged) stop if any dated or listing
  question of the existing tests changes route, or if the share of p7 overview runs reaching
  the survey drops below today's.
- **`kb-today+answer-first`** is reported against `kb-today` and `kb-first` with its cost.

## Price model and spend

- **Price model.** The measurement price model of the set-11 runs (`hc/cost.py`):
  - Haiku 5.5 at $0.10 per million input tokens and $0.50 per million output tokens;
  - cache read $0.01 per million, cache write $0.125 per million;
  - local models $0.

  The server's `cache_read_tokens` / `cache_creation_tokens` are mapped onto those names.
- **Spend cap: $20.**
  - **Baseline.** Both tenants' ledgers are recorded once, before the dev tuning and the
    sweeps (`hc/spendguard.py --record`).
  - **Stop rule.** Every runner stops before a step that could take the summed ledger
    increase past **$19**. That covers the runs, the condense pass and the sweep.
  - **Estimate.** About $15 (plan §3.5).

## Seeds

| what | seed |
|---|---|
| bootstrap | 1 |
| the spread sample | 20261122 |
| question order and arm rotation | 20261127 (and `order:<qid>:<rep>`) |
| the test set | RFC DM's `random.Random(20260929)` draw, unchanged |

## Amendment 0 — 2026-11-28 (committed before the first scored run)

**In force from this commit.** Every point below was settled after the dev tuning and before
any scored run. No test question and no p7 question has been run.

### Deployment and agents

- **One deployment, both tenants:** loomcycle **1.172.0**, commit `ad14d7d81`. The scored
  runs of each set run on this release only (check 9). The deployment upgraded from 1.171.0
  during the dev tuning (below); no scored row exists from before it.
- **The P1 agents are served by the deployed bundle, not by tenant copies.** The build with
  P1 (#3030, `0b0230c70`) is deployed, so `kb/answer`, `kb/ask` (with its `first_call` flag)
  and `kb/survey` are the bundle's static agents on both tenants, with no tenant def. This
  replaces "created as tenant AgentDefs ... copied byte for byte" in *Tenants, models and
  setup*. The repo's `bundles/knowledge-base/loomcycle.yaml` and the embedded bundle hold
  the same `kb/answer`, `kb/read`, `kb/ask` and `kb/search`.
- **Tenant defs, identical on both tenants** (`content_sha256`):

  | agent | what | content_sha256 |
  |---|---|---|
  | `kb/classify` | Haiku fork of the static def | `2aa44e04…aeb970` |
  | `kb/search` | Haiku fork | `f96dc990…db3fdb` |
  | `kb/screen` | Haiku fork | `2391e650…97022d` |
  | `kb/walker` | Haiku fork | `efc32fed…1a7026` |
  | `kb/read` | Haiku fork; prompt = the bundle's, untuned | `2ef94dc2…a29f62` |
  | `kbhc/search-af` | `kb/search` + Haiku + the answer-first sentence | `0c86a041…d9408b`¹ |
  | `kbhc/ask-af` | `kb/ask`'s code, search route → `kbhc/search-af` | `3881294d…af8dcb` |

  ¹ `kbhc/search-af`: `sha256:0c86a0414175e215f44b6e1cf4c9a3543220d7a7ddb329437b6004f817d9408b`;
  `kbhc/ask-af`: `sha256:3881294d7e24f5ea8718c11103b0a276922f219408c0a211f9e99a4a93af8dcb`.
  Full hashes of every row are in each tenant's `tenant_state.json` (work dir).

  Each Haiku fork differs from the 1.172.0 static def in provider and model only (the
  static `tier` cleared), checked field by field after the upgrade. `kbhc/ask-af`'s code is
  the static `kb/ask` code with that one replacement; `kbhc/search-af`'s prompt is the static
  `kb/search` prompt with "Search, read what you need, then answer in a few sentences."
  replaced by "Search, read what you need, then answer. The first line of your reply is the
  answer itself, on its own line: yes or no, a number, a date, a name or a short phrase when
  the question allows one, with no preamble and no hedge. Then explain in a few sentences."
  A `kb/annotate` Haiku fork also exists on both tenants, for the sweep only.
- **Recorded tenant state** (`hc/tenant_state.py --record`, on 1.172.0): ConditionalQA
  tenant digest `d83d52c125454d63`, development tenant digest `20b14f9ec7285285`.

### Model profile

- Every model-backed agent a scored run can reach is pinned to `anthropic` /
  `claude-haiku-5-5` by the defs above; the code agents (`kb/ask`, `kbhc/ask-af`,
  `kb/answer`, `kb/survey`) call no model. The decision model and the reranker are the
  deployment's local models.
- **Changed since the plan:** the deployment's tier routing now sends the `low` and `middle`
  tiers to local models (Haiku stays only in two user tiers). The plan's "the low tier is
  Haiku" no longer holds. It does not touch the study, since every reachable model agent is
  pinned and the runner stops on a model run served by anything else.

### Tenant state at the start

- **ConditionalQA tenant:** the RFC GM ontology (gov.uk taxonomy) stays. The walker arms
  `kb/walker-shipped` and `kb/walker-p7full` were retired on 2026-11-27 by another session
  before this amendment; the Haiku `kb/walker` fork above is the one walker.
- **Sweep** (`hc/sweep.py`, write annotate, Haiku, code agents): ConditionalQA tenant 27
  passes, development tenant 2 passes (442 calls); $1.93 together, more than the plan's
  $0.5 because the ConditionalQA tenant held about 4,800 stale annotations.
- **Checks 1 and 2** (after the sweep):

  | | ConditionalQA tenant | development tenant |
  |---|---|---|
  | documents listed / in the corpus / left out | 656 / 652 / 4 | 452 / 429 / 23 |
  | annotation coverage (pass ≥ 0.99) | 1.0 | 0.9907 |
  | stale annotations (pass ≤ 0.01) | 0.0 | 0.0024 |

  The ConditionalQA corpus is exactly the 652 mapped pages. The 4 left out are 2 annotation
  documents, the ontology and one untyped memory document.
- **Ingest maps rebuilt.** The benchmark's `docmap.json` / `chunks.json` were not reachable,
  so they were rebuilt read-only from the tenant with the benchmark ingest's own slug and
  tree-order rules: 652 of 652 pages mapped, every title checked, no page imported.

### Smoke test (4 dev questions × 5 arms, not scored)

20 of 20 runs ok, all on Haiku; $0.032. Per question: `kb-today` $0.0034, `kb-first`
$0.0011 (1 of 4 escalated "answer -> search"), `kb-today+answer-first` $0.0031, `rag-lc`
$0.0003, oracle $0.0002. Every first call reranked.

### Dev-split tuning log (every look)

A fixed sample of 100 dev questions, `random.Random(20261128).sample` of the 1,715, the
same 100 for every version; output roots `<work>/tune/v0`, `<work>/tune/v1`.

| look (UTC, 2026-11-28) | version | arm | n scored | F1 | F1 yes/no | F1 span |
|---|---|---|---|---|---|---|
| 06:27 | v0 (bundle `kb/read`) | `rag-lc` | 91² | 0.630 | 0.745 (47) | 0.508 (44) |
| 06:33 | v0 (answer-first as above) | `kb-today+answer-first` | 100 | 0.647 | 0.774 (53) | 0.504 (47) |
| 06:52 | v1 (`kb/read` + "never Possibly, Maybe or It depends") | `rag-lc` | 91² | 0.628 | 0.723 | 0.525 |
| 06:58 | v1 (answer-first + "no Probably, no label such as Answer:, no markdown") | `kb-today+answer-first` | 100 | 0.644 | 0.774 | 0.497 |

² 9 `rag-lc` v0 rows were cut by the server restarts (below) and are left out of both
versions' comparison. v1 had no cut row; on all 100 its `rag-lc` F1 is 0.634.

v1 removed the hedge words but moved no score: question by question 14 wins / 15 losses
(`rag-lc`) and 18 / 21 (answer-first), within the run-to-run noise of a 100-question
sample. **v0 is kept for both prompts**: `kb/read` runs the bundle's prompt unchanged, and
`kbhc/search-af` the answer-first sentence above. The v1 defs were retired and the v0 defs
promoted again. Most reader misses were ConditionalQA yes/no calls that turn on the
situation, which a prompt change would only overfit. Tuning cost $0.68.

### Harness change

- **A server restart fails the row.** The 1.171.0 → 1.172.0 upgrade restarted the server
  twice during the tuning. Each in-flight Document search was cut ("interrupted: the runtime
  shut down") and `kb/answer` reported `no_sections` as a normal answer, so the runner wrote
  9 ok rows with no answer. The runner now reads the server's uptime after each run; a
  process younger than the row fails it, and the one retry reruns it (check 8). A test
  covers it.
- The deployment is not upgraded or restarted on purpose while a set's scored runs are in
  progress.

### Confirmations

- **Check 7:** the bound is **0.03** (owner ruling), confirmed.
- **The oracle's block shape** matches the merged `kb/answer`: the harness reads `HEAD`,
  `ASK`, `CONFIG` and `MIN_TEXT` from the bundle at run time (#3028), and the repo bundle
  equals the deployed one for `kb/answer`.
- **The answer-first arm under its own names** (`kbhc/ask-af` → `kbhc/search-af`),
  confirmed (owner ruling 2026-11-27).
- **Dropped arms** as in *Arms*: `rag-ext`, the `low`-reader arm, K = 10/20.
- **Seeds** as in *Seeds*, unchanged.
- **p7:** no look since the three listed in *Splits*.

### Environment, output roots, spend

- Env variable names: `KBRAG_HC_WORK`, `KBRAG_HC_CQA_BASE`, `KBRAG_HC_CQA_TOKEN`,
  `KBRAG_HC_CQA_TENANT`, `KBRAG_HC_DEV_BASE`, `KBRAG_HC_DEV_TOKEN`, `KBRAG_HC_DEV_TENANT`,
  `KBRAG_P7_GOLD`. Values live in a file outside the repo and are never printed.
- Output roots: `<work>/study` for the scored runs; `<work>/tune` (tuning) and
  `<work>/smoke` (smoke test) hold no scored row.
- **Spend baseline** recorded 2026-11-27T14:00:08Z (both ledgers). Increase at this
  amendment: **$2.66** (sweep $1.93, smoke $0.03, tuning $0.68, and about $0.01–0.09 of
  another session's 50 Haiku runs on the development tenant). The $19 stop is unchanged and
  still counts from the baseline; the remaining estimate is about $12.5, so about $15 in all.
