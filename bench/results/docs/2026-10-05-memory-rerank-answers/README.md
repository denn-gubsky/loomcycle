# Does reranking memory recall improve ANSWERS? (RFC DR P2, 2026-10-05)

- **Pre-registration:** `bench/docs/memory-rerank/PREREG.md`, committed before any store was
  built. It has three amendments, each committed before the data it affects was analysed:
  1. Fixes the (c) harness and adds checks 5–7.
  2. Changes the question count from 400 to 383: the sampler rounds each category down.
  3. Gives each conversation its own databases (see *Protocol deviation*).
- **Harness:** `bench/docs/memory-rerank/` (`run.sh`, `analyze.py` for (b); `depth.sh`,
  `depth.py` for (c)), on `bench/cmd/locomo` and `bench/cmd/rerankpool`.
- **Fully local:** every model ran on the Spark (Ollama 0.35.0): `qwen3.8` with thinking off
  and a 32K window (extractor, ontologist, memory judges, answerer, grading judge),
  `nimble` (reranker), `bge-m3` (embedder). No cloud call.
- **Not comparable with the 0.706 rating:** the answerer and the judge differ.
- **Licence:** LoCoMo is CC BY-NC, so only scores are committed here, no question or turn
  text.

## Verdict

**Reranking an agent's memory recall does not change its answers in this pipeline.** The
retrieval gain is real, and (c) enlarges it, but it does not reach the answers.

| hypothesis | result |
|---|---|
| **H1** rerank-on binary-J > rerank-off (383 paired questions) | **FAILS**: 0.7795 → 0.7822; discordant 10 on vs 9 off; exact McNemar p = 1.0 |
| **S1** (c) a 40-row pool beats 20 on multi-hop recall@5 | **HOLDS**: 0.517 → 0.595, +7.8pp, 95% CI [+5.5, +10.2] |

**Pre-registered decisions:**
- **H1 fails:** the retrieval gain is recorded as not reaching answers here. The setting
  (`memory.reranker.sources: [facts, notes]` + `memory_rerank`) stays available and is
  documented with both results.
- **S1 holds with a gain ≥ 3 points:** the decision rule says `candidates` moves from 20 to
  40. See *The candidates decision* below; it is not applied in this PR.

## (b) Answers: rerank off vs on

10 conversations. Each was built once, then answered with the same answerer
(`locomo/orn-l2k24-sp`), without and then with `memory_rerank: {enabled: true}`.

| arm | n | binary-J | partial | NOT_FOUND | answer p50 | model calls / answer |
|---|---|---|---|---|---|---|
| rerank off | 381 | 0.7795 | 0.8228 | 0.000 | 55.4 s | 2.91 |
| rerank on | 381 | **0.7822** | **0.8307** | 0.003 | **45.3 s** | **2.65** |

(Model calls per answer cover the nine conversations re-run in fresh databases, 346 answers
per arm. Two pairs had an unparsed verdict and are excluded from the 381.)

| category | n | off binary-J | on binary-J |
|---|---|---|---|
| single-hop | 211 | 0.882 | 0.877 |
| temporal | 81 | 0.864 | 0.889 |
| multi-hop | 68 | 0.485 | 0.471 |
| open-domain | 20 | 0.350 | 0.400 |

- **No conversation moves by more than 2 correct answers** (of 37–39), in either direction.
- **Reported, not tested:** rerank-on answers took about 9% fewer model calls and were 10 s
  faster at p50.
  - **Confound:** rerank-on always ran second on the same warm Ollama host.
  - **For:** the drop in model calls is not a host effect, and suggests the reordered
    recall settles some questions in fewer tool rounds.
- **Why a +15pp retrieval gain does not reach the answers.** These are hypotheses, not
  measured:
  1. **Partial reach:** the answerer has three routes into memory. The rerank reorders
     only `Memory op=recall`, not `History op=window` or `Document op=graph_recall`.
  2. **Ceiling:** single-hop and temporal are already at 0.86–0.88.
  3. **Mostly unaddressed:** on multi-hop, where #1571's retrieval gain was largest, the
     pool itself holds only about half of the evidence (see (c)).

## (c) Pool depth: 20 vs 40 candidates (multi-hop, retrieval only)

The setup is #1571's turn store (5,882 rows, `bge-m3`) and its questions. Each multi-hop
question's pool is the top 40 of `POST /v1/_memory/search`. The 20-row arm reranks the first
20 of that same pool. Both arms use the shipped listwise rerank (`qwen3.8`, 16K window).

| arm | recall@5 | rerank p50 |
|---|---|---|
| no rerank (first 20) | 0.334 | — |
| rerank 20 | 0.517 | 3.0 s |
| rerank 40 | **0.595** | 4.6 s |

- **Pool ceilings:** recall@20 is 0.546 and recall@40 is 0.671. The 40-row pool holds
  12.6pp more of the evidence, and the rerank brings 7.8pp of it into the top 5.

## The candidates decision (for the operator)

The pre-registered rule (S1 holds, gain ≥ 3 points) says to raise the default
`memory_rerank.candidates` from 20 to 40. It is not applied here, for three reasons:
- **Wrong reranker measured:** (c) measured the **listwise** rerank. `kind: decision`
  (`nimble`), the kind RFC DR recommends for memory, shows a model at most **26** options
  (`decisionMaxOptions`). With 40 candidates it reranks the first 26 and leaves the rest
  in place. The 40-row gain is unmeasured for it.
- **One category, one shape:** the gain was measured on multi-hop questions over raw
  turns. Documents and facts were not measured at 40.
- **Product change:** a default change affects every deployment, so it gets its own PR.

## Instrument checks

| # | check | result |
|---|---|---|
| 1 | coverage, per conversation: consolidated, nothing queued or skipped, ≤ 2% extractor runs failed | **pass**, all 10 (0 of 1,571 extractor runs failed; each build read exactly its own chats) |
| 2 | rerank-off makes no nimble call; rerank-on calls nimble on ≥ 80% of questions | **pass**: 0 off; 442 calls on, every rerank-on answer run (100%) |
| 3 | unparsed verdicts ≤ 2% per arm | **pass**: 0.5% off, 0.0% on |
| 4 | both arms graded the identical question set | **pass**: 383 pairs |
| 5 | (c) 282 multi-hop questions pooled | **pass** |
| 6 | (c) rerank applied on ≥ 95% in both arms | **pass**: 99.6% / 100% |
| 7 | (c) the 20-row arm's unreranked recall@5 within ±0.02 of #1571's 0.332 | **pass**: 0.3335 |

## Protocol deviation (amendment 3)

- **What happened:** the first pass shared one database across conversations. The
  harness's purge removes memory **rows** but not chat **transcripts**. The answerer's runs
  share the scribe's user, so each build also consolidated the previous conversation's
  answer runs as if they were chats:
  - conv-30 read 93 chats for its own 19;
  - conv-41 read 108 for its own 32 and failed check 1.
- **What was done:**
  - The driver was stopped, and each conversation now builds in its own empty databases.
  - conv-30 onward were re-run from scratch.
  - conv-26 was kept: it was built in an empty database, which is the fixed protocol's
    exact condition.
- **The published LoCoMo rating (0.706) is not affected** (checked 2026-10-06). Its
  `summary-checkpoints.json` shows all ten builds finishing (21:17 to 05:44) before the
  first question was answered (06:54), with one scope per conversation.
- **A caveat on (b), found while fixing the harness:**
  - **The exposure:** History list/search showed the answerer's own earlier sessions,
    since they were not `internal`. So a rerank-on answer **could** have found the
    rerank-off answer to the same question through History. A shortcut like that would
    pull the two arms together, toward the null.
  - **Undetermined:** whether any answer did. The reports record no tool calls, and the
    databases were dropped.
  - **Weighed against it:** the answerer's prompt points it at `History op=window` (a
    fact's source turns), not at search. And the two arms did differ: 19 discordant
    pairs, and about 9% fewer model calls with rerank on.
- **Fixed for later runs:** every LoCoMo agent but `locomo/scribe` is now `internal`.
  This closes both the build leak and the History exposure
  (`TestBenchAgents_OnlyTheScribeIsAConversation`).

## Files

- `answers-summary.json`: (b) `analyze.py` output.
- `depth-summary.json`: (c) `depth.py score` output.
