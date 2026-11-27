# Study C arms: run order and design notes

Study C of `PREREG.md` (ConditionalQA, tenant `conditional-qa`): the `kb`, `rag-lc` and
`rag-ext` arms, the oracle, the condense step, the judge, and the analysis (H3, H4,
instrument checks 1-9). Standard library only. Working data lives under
`/home/denn/workspace/kbrag-work/cqa/` (`KBRAG_CQA_WORK` overrides it). The token is read
from `LOOMCYCLE_BENCH_TOKEN_CQA` by `lc.py` and is never written anywhere.

## Run order

Every command is one line. Pick ONE profile for the whole study (`haiku` or `local`) and
pass the same one to every runner; the run files refuse a second profile.

1. Offline tests (no network):
   `python3 -I -m unittest discover -s bench/docs/kbrag/tests -p 'test_*.py'`
2. The rag-ext dense index (the only step that calls the embedder; resumable, sequential
   batches of 32):
   `python3 -I bench/docs/kbrag/ragext.py build --pace 0.5`
   then `python3 -I bench/docs/kbrag/ragext.py stats` (chunks, progress, embed calls, chars, wall).
3. The rag-lc helper agent `kbrag/search-rr` (POST /v1/_agentdef):
   `python3 -I bench/docs/kbrag/register_agents.py create`
   and `python3 -I bench/docs/kbrag/register_agents.py show` to read back its def_id.
4. The inputs of checks 1 and 2, taken at the study's start (read-only):
   `python3 -I bench/docs/kbrag/precheck.py`
5. Smoke test (never scored; its own output root, so its rows and its idempotency keys
   never meet the study's):
   `python3 -I bench/docs/kbrag/arms.py --profile haiku --arms kb,rag-lc,rag-ext,oracle --qids dev-20,train-1337,train-1927,train-948 --reps 1 --out /home/denn/workspace/kbrag-work/cqa/smoke`
   then `condense.py`, `judge.py` and `analyze.py` with the same `--out`.
6. The primary runs (rep 0 of the 800) for the RAG arms and the oracle, detached, 4 workers:
   `nohup setsid python3 -I bench/docs/kbrag/arms.py --profile haiku --arms rag-lc,rag-ext,oracle --no-spread --workers 4 --budget-usd 19 --deadline 2026-11-24T08:00:00Z > /home/denn/workspace/kbrag-work/cqa/logs/arms-rag.log 2>&1 &`
7. The kb arm later, into the same runs directory (rows are per arm, so it simply adds `kb.jsonl`):
   `nohup setsid python3 -I bench/docs/kbrag/arms.py --profile haiku --arms kb --no-spread --workers 4 --budget-usd 19 --deadline 2026-11-24T08:00:00Z > /home/denn/workspace/kbrag-work/cqa/logs/arms-kb.log 2>&1 &`
8. The spread repetitions (reps 1-2 of the fixed 100), afterwards, all arms:
   `python3 -I bench/docs/kbrag/arms.py --profile haiku --arms kb,rag-lc,rag-ext,oracle --spread-only --workers 4 --budget-usd 19`
   (Without `--no-spread`/`--spread-only` one invocation does both: rep 0 of every question
   first, then the spread reps.)
9. Retry each failed run once (PREREG check 8):
   `python3 -I bench/docs/kbrag/arms.py --profile haiku --arms kb,rag-lc,rag-ext,oracle --retry-failed --workers 4 --budget-usd 19`
10. Condense every answer:
    `python3 -I bench/docs/kbrag/condense.py --profile haiku --workers 4 --budget-usd 19`
11. Judge every answer (one local GPU: keep 1 worker; measured ~12 s per call on the smoke):
    `python3 -I bench/docs/kbrag/judge.py --budget-usd 19`
12. Score:
    `python3 -I bench/docs/kbrag/analyze.py`
    → `summary.json` and `summary.md` in the work directory.

`--out DIR` (arms, condense, judge, analyze) moves `runs/`, `condensed/`, `judge/` and the
summary under DIR; the default is the work directory (the study). Inputs (`prepared/`,
`docmap.json`, `chunks.json`, `ragext/`, the precheck files) always come from the work
directory.

Exit code 2 from a runner means a guard stopped it; the message and `runs/events.jsonl` say
which. Re-running the same command resumes.

## Files

| file | what it is |
|---|---|
| `config.py` | profiles (reader per profile, the judge), the allowed serving pairs, price estimate, paths, constants |
| `common.py` | data and id maps, citation parsing, trailer stripping, EM/F1, readable sections, R@k/MRR, per-row usage |
| `prompts.py` | the reader, condense, rewrite, rerank and judge prompts; `ask` (one chat call, retry once on empty) |
| `ragext.py` | the rag-ext pipeline: chunking, BM25, dense index build/load, RRF, expansion, rerank, parent expansion |
| `arms.py` | the runner: arms, plan, resumability, guards |
| `passes.py` | the resumable condense/judge pass with the same guards |
| `condense.py`, `judge.py` | the two passes |
| `analyze.py` | measures, H3/H4, instrument checks, completeness |
| `precheck.py` | `tenant_documents.json` (check 1) and `annotation_summary.json` (check 2) |
| `agents/search-rr.json`, `register_agents.py` | the rag-lc helper agent and its registration |
| `tests/` | offline unit tests with a fake `lc` |

## Outputs (under the work directory)

- `runs/<arm>.jsonl`: one row per (arm, qid, rep, attempt): `profile`, `reader`, `status`
  (`ok` / `failed` / `provider_violation`), `error`, `raw_answer`, `answer_line` (reader
  arms), `citations {docs, chunks}`, `retrieval` (rag arms), `route` (kb), `run_ids`,
  `usage {total, by_model, calls_estimated}`, `served` (every serving provider/model),
  `reader_served`, `wall_s`.
- `runs/versions.jsonl` (check 9), `runs/events.jsonl` (start, end, every stop).
- `condensed/<arm>.jsonl` (`condensed`, `direct`), `judge/<arm>.jsonl` (`verdict`, `order`).
- `ragext/manifest.json` + `ragext/vectors.f32` (the dense index).

## Design notes

- **Models are configuration.** `--profile haiku` reads with `anthropic/claude-haiku-5-5`,
  `--profile local` with `ollama-local/qwen3.6:latest`; the reader, condense and rewrite
  calls (and rag-ext's listwise rerank) use the profile's reader. The judge is
  `ollama-local/ornith-1.5:35b` in both profiles. Every row records the profile and what
  actually served each call; `analyze.py` reports "mixed profile" and does not test the
  hypotheses when the arms' scored rows used different profiles or reader models (for `kb`
  the reader is the model that wrote the most output tokens in its run tree).
- **Provider guard.** After every run or call, any serving pair outside `config.ALLOWED`
  (Haiku, any `ollama-local` model; code-js runs with no tokens are ignored) marks the row
  `provider_violation` and stops the runner. Every 25 runs and at the end the tenant's
  usage ledger since the session started is checked the same way (`lc.disallowed`), which
  also catches a call no run tree names, such as a rerank booked on a code-js run.
- **Spend guard.** Before every run or call: `lc.spend()` plus the p90 estimated cost of
  the arm's completed rows (the floor — kb $0.05, others $0.01 — until 5 rows exist) must
  stay within `--budget-usd` (default 19). A spend-cap stop is recorded and makes the
  study incomplete in `analyze.py` (PREREG "Runs"). Row costs are estimates from tokens at
  Haiku 5.5 list price (`config.PRICE`: $0.10 in / $0.50 out / $0.01 cache read / $0.125
  cache write per million, which is what the deployment's ledger charges); the guard's ground
  truth is `lc.spend()`. (Until 2026-11-23 `config.PRICE` held Haiku 4.5's $1/$5, so rows
  written before then carry estimates 10x too high.)
- **Deadline guard.** Before every run: now + the p90 of that arm's completed durations
  must not pass `--deadline`.
- **Workers.** `--workers N` (arms, condense, judge; default 1) keeps N runs or calls in
  flight on threads. They are STARTED in the plan order (per question and rep, the seeded arm
  rotation); with N > 1 they complete in a nondeterministic order, so the order of rows in a
  file is not the plan order (every reader keys rows by (arm, qid, rep, attempt)). A row is
  appended as one whole line under a lock. The spend guard runs under a lock and counts every
  run already in flight at its arm's estimate (none of them is in the ledger yet). Any guard
  stop (spend, deadline, provider) lets the runs in flight finish and write their rows,
  starts nothing new, and exits 2 once every worker has stopped. Resumability is unchanged.
- **Idempotency keys** are durable on the server (a unique index on `runs.idempotency_key`),
  so a key used once is answered with that run forever. Keys are therefore namespaced by
  output root: `kbrag-cqa-study:<arm>:<qid>:<rep>` for the study, `kbrag-cqa-<dir name>:…`
  for any other `--out` (smoke, pilot). The 2026-11-23 smoke ran with the bare `kbrag-cqa`
  prefix on four subset questions, which is why the study's prefix differs from it.
- **Resumability and retries.** A row whose latest attempt is ok is never redone. A failed
  one is retried only with `--retry-failed`, and never past attempt 2 (`--max-attempts`):
  PREREG check 8 retries once and a second failure voids the study. A retry of a `kb` or
  `rag-lc` run uses a fresh idempotency key (`…:a2`); the brief's key is kept for attempt 1.
  The condense and judge passes redo a failed row on their next invocation.
- **Arm order** per (question, rep) is the arm list rotated by `Random("order:<qid>:<rep>")`.
  **Judge order** per (question, rep) is shuffled by `Random("judge:<qid>:<rep>")`.
- **kb arm.** `kb/ask` with the brief's prompt (`Situation: …\nQuestion: …`), metadata
  `{depth: standard, bench, qid, rep}` (no `classifier` key: the arm is kb/ask as
  shipped), `user_tier: free`. A run without a `ROUTE:` line is failed. Usage is the
  whole run tree (`lc.tree_usage`); its model-call count is a floor (one per run that
  names a model), flagged `calls_estimated`.
- **rag-lc arm.** One run of `kbrag/search-rr` with the question's `query` (scenario +
  question), which runs `Document op=search` (tenant, limit 20, the agent's
  `memory_rerank: {enabled: true}` at its shipped defaults; no `annotations`,
  `with_summary`, `concept` or `expand`). Chunk ids map to sections through `chunks.json`;
  the first 10 readable, distinct sections are the retrieval list, the first 5 go to the
  reader. `reranked`, `rerank_reason`, and the count of hits carrying `matched_annotation`
  or `summary` are recorded per search (checks 3 and 4). The deployment HAS a reranker: the
  live search returned `"reranked": true`, and the usage ledger books it under purpose
  `rerank` on `ollama-local/nimble` (a decision model, 0 output tokens). That call is not on
  the run tree (the code-js run reports 0 tokens), so it is not in the row's `usage`; it is
  free (ollama-local) and the ledger guard covers its provider.
- **rag-ext arm.** See `ragext.py`. Chunks: one per non-empty section, sections over 400
  tokens (regex `\w+|[^\w\s]`) cut into 400-token windows overlapping 50; index text
  `<title> > <path>\n<text>`. BM25 (k1 1.2, b 0.75, lowercased, Lucene idf) on an inverted
  index; dense = exact cosine over unit vectors from `lc.embed` (the deployment's
  embedder, as the tenant uses); 50 per leg, RRF k 60. Query expansion: one reader call for
  three rewrites (JSON list) of `Situation/Question`; the original `query` plus the
  rewrites are each fused, then fused again by RRF (top 50). Rerank (below). Parent
  expansion to whole sections, deduped; 10 kept for retrieval metrics, 5 for the reader.
  Measured offline (12,821 chunks, random vectors): about 1.3 s per question for the four
  queries (≈0.32 s per query), plus the live embed call.
- **The reader call** (rag-lc, rag-ext, oracle): one chat call, temperature 0, the
  question and at most 5 sections, each `[<tenant document id>] <title> > <path>` plus
  text; the first line must be `ANSWER: …`, then `SOURCES: loom:doc/<id> …`. The oracle
  reads the question's readable gold sections (at most 5).
- **Condense** sees only the question and the raw answer (kb's without its `ROUTE`,
  `COVERAGE` and `UNCOVERED` lines) and returns the shortest span or `NO ANSWER` (scored
  as empty). `direct` = the reader's `ANSWER:` line, for check 7.
- **Judge** sees the question, the gold answers and one answer with the accounting lines,
  `loom:` links (a link keeps its title text), `SOURCES:` line and `ANSWER:` label removed,
  so neither the format nor the order identifies the arm.
- **Cited-page recall**: the question's page among the cited documents, a
  `loom:chunk/<id>` counting for its page (`chunks.json`); also reported docs-only.
- **Retrieval metrics** (rag arms): R@1/3/5/10 and MRR@10 over the readable sections
  against `gold`; chance per question `min(1, 5·|gold| / readable sections in the corpus)`.
- **Cost** per question and per correct answer (judge CORRECT, and EM = 1), in tokens and
  estimated USD; the arm's own runs only (condense and judge are the same for every arm
  and are not counted).
- **Hypotheses** H3/H4: paired bootstrap over the 800 questions, 10,000 resamples, seed 1,
  the 98.75% percentile CI of (kb − RAG arm) mean condensed F1; holds iff the lower bound
  is above −0.02; also reported against 0 (superiority). Tested only on a complete study
  (every main arm and the oracle ok and condensed on all 800 rep-0 questions, no spend-cap
  stop, no provider violation, one profile); a failed instrument check voids the
  hypotheses PREREG names; a not-run check makes the verdict "provisional".
- **Spread** (the 100-question sample, reps 0-2): condensed F1 per rep and the mean
  per-question range, reported only.

## Deviations (to be recorded as amendments by the orchestrator)

1. **No cross-encoder.** PREREG fixes "a cross-encoder over the top 50 fused chunks"; none
   is available on the deployment. `config.RAGEXT_RERANK = "listwise"` (default): one
   reader-model call receives the question and the 50 candidates (each cut to 600 chars,
   numbered) and returns a JSON list of the 20 most relevant; fewer than 10 valid numbers
   (or an unparsable reply) keeps the fused order and flags `rerank_failed`; candidates
   it leaves out follow in fused order. `"none"` skips the step. The strategy is recorded
   on every row; check 4 for rag-ext passes only for `listwise` with no `rerank_failed`.
   On the haiku profile this call costs money (≈5-6k input tokens per question).
2. **Models.** The operator's 2026-11-22 policy replaces the single-provider plan: Haiku
   (or `ollama-local` as the fallback profile) for reader/condense/rewrite/rerank, a
   local judge, a $20 study cap (`--budget-usd 19` leaves headroom for the ledger's lag).
3. **Retry idempotency keys** gain an `:a<attempt>` suffix after the first attempt.
4. **Check 2** reads `list_annotations summarize`: coverage = the `document` level's total
   over the tenant's documents, staleness = its `stale` over its total (the server checks
   staleness for at most a capped number of rows and says so with `stale_checked`).
5. **Model-call counts for kb and rag-lc** come from the run tree, one per run that names
   a model: a floor, flagged.
6. **Idempotency-key prefix** is `kbrag-cqa-study` (was `kbrag-cqa`), see Design notes.
7. **Reader thinking.** Haiku 5.5 thinks by default (adaptive, effort medium) and
   `/v1/_llm/chat` sends no thinking or effort setting, so a two-word `ANSWER:` costs ~350-500
   output tokens. The same for every arm's reader, condense, rewrite and rerank call.

## Response shapes confirmed on the live smoke (2026-11-23, loomcycle 1.165.0)

| shape | found | harness |
|---|---|---|
| `POST /v1/_agentdef` create with `overlay` + `promote` | accepted, def `kbrag/search-rr` v1 | unchanged |
| search-rr final text | `{"chunks":[{chunk_id, document_id, path, rank_score, score, title}], "reranked": true}`; no `rerank_reason` when reranked; no `matched_annotation`/`summary` | unchanged |
| chunk ids → sections (`chunks.json`) | all 80 rag-lc hits mapped (`unmapped` empty) | unchanged |
| `kb/ask` text | prose with `[title](loom:chunk/<id>?scope=tenant)` / `loom:doc/<id>?scope=tenant` links and a final `ROUTE: … \| SCORE \| CLASSIFIER \| RUNS` line | unchanged |
| run-tree rows (`GET /v1/runs`) | code-js runs report `provider: code-js`, `model: loomcycle/code-js`, 0 tokens | FIXED: were counted as model calls (`common.Usage.add_tree`, `main_model`) |
| `/v1/_llm/chat` | names the serving provider/model; usage `input_tokens`/`output_tokens` | unchanged |
| judge served pair | `ollama-local/ornith-1.5:35b` (not `ollama-spark`) | unchanged |
| ledger price of Haiku 5.5 | $0.10 / $0.50 / $0.01 / $0.125 per million | FIXED `config.PRICE` (was 10x) |
| idempotency keys | durable, never expire | FIXED: namespaced per output root |
| reader format | `ANSWER: …` then `SOURCES: loom:doc/<id> …` on all 12 reader rows (one with a blank line between) | unchanged |
| condense | one short span (`Yes`/`No`/a phrase) on all 16 | unchanged |
| judge | verdict word alone on line 1, one sentence after, on all 16 | unchanged |
