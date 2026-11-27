# RFC HC gate study: the first call against today's kb/ask and passage RAG (pre-registration)

> **DRAFT. Not in force until Amendment 0 is committed.** Amendment 0 records the model
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

## Amendment 0 — (stub; filled after the dev tuning, committed before the first scored run)

- Date and time (UTC); loomcycle version and commit (both tenants, one deployment).
- The P1 bundle commit; each tenant def created for the study (`kb/answer`, `kb/read`, the
  flagged `kb/ask`, the Haiku forks, `kbhc/ask-af`, `kbhc/search-af` or the names used) with
  its `content_sha256`; `tenant_state.json` digests per tenant.
- The model profile as served (provider/model per agent), the decision model and reranker.
- The state of both tenants (RFC GM ontology, the walker arms and the Haiku `kb/walker`
  override on the ConditionalQA tenant; corpus and coverage at the start: check 1 and 2
  inputs, the sweep logs).
- The dev-split tuning log: every prompt version tried, on which questions, with its scores.
- Confirmation or change of: check 7's 0.03, the oracle's block shape, the answer-first arm
  under its own names.
- The env variable names used (never their values), and the output roots.
- The spend baseline.
