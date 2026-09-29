# RFC DM C3 — derived search units on PolicyQA (2026-09-28)

Pre-registration: [`bench/docs/units/PREREG.md`](../../../docs/units/PREREG.md) (committed
before any ingestion; Amendment 1 before any search). Harness: `bench/docs/units/`.
Everything below ran through the **shipped feature** — C1 storage + resolution and C2
generation (`POST /v1/_document/derive_units`) — on an isolated server with a throwaway
Postgres + pgvector store, `bge-m3` embeddings and `qwen3.6:latest` (`effort: low`) as
both generator and reranker, all on the TrueNAS Ollama host.

## Verdict

**Units show no measured benefit on policy text.** The option stays documented as
"no measured benefit yet", which is the pre-registered decision when H1 does not hold.

| | result |
|---|---|
| **H1** `units` > `header`, R@5, 2,643 pairs | **VOID** (instrument check 5 failed). The observed difference is also null: 0.6065 vs 0.6008, discordant 138 / 123, p = 0.386 |
| **H2** `units_rr` > `header_rr`, R@5, 400-pair subset, majority of 3 passes | **FAILS**: 0.7075 vs 0.6875 (majority of 3 passes), discordant 26 / 18, p = 0.291 |

## Arms

| arm | pairs | R@1 | R@5 | R@10 | MRR@10 |
|---|---|---|---|---|---|
| header | 2,643 | 0.2463 | 0.6008 | 0.7809 | 0.3990 |
| units | 2,643 | 0.2467 | 0.6065 | 0.7787 | 0.3979 |
| header_on_subset | 400 | 0.2400 | 0.5950 | 0.7850 | 0.3918 |
| units_on_subset | 400 | 0.2375 | 0.6000 | 0.7725 | 0.3885 |
| header_rr (passes 1 / 2 / 3) | 400 | 0.3075 ×3 | 0.6875 ×3 | 0.8450 ×3 | 0.4714 ×3 |
| units_rr (passes 1 / 2 / 3) | 400 | 0.2925 ×3 | 0.7100 / 0.7075 / 0.7075 | 0.8650 ×3 | 0.4661 ×3 |

The rerank arms were near-deterministic: across three passes, one question moved, in `units_rr`. `*_on_subset` are the no-rerank arms scored on
the same 400 pairs, for comparison with the rerank arms.

## Instrument checks

| # | check | result |
|---|---|---|
| 1 | every one of the 500 segments maps to a chunk | pass (500/500) |
| 2 | units for ≥ 95% of chunks with prose, 0 failures | pass (500/500 chunks, 4,300 units, 0 failed) |
| 3 | `header` arm returns no `matched_unit` | pass (0) |
| 4 | `units` arm finds ≥ 1 gold hit through a unit | pass (470 gold top-5 hits: 408 question, 33 description, 29 claim) |
| 5 | `header` R@5 more than twice chance | **FAIL**: 0.6008 vs 2 × 0.3141 = 0.6282 |
| 6 | rerank arms `reranked: true` on ≥ 95% of searches | pass (`header_rr` 100%, `units_rr` 99.75%) |

**Why check 5 failed.** The pre-registration estimated chance R@5 at about 0.2. The
pre-registered analysis computes it per question as `min(1, 5·|gold| / |segments|)`,
which gives 0.314. Five of the 20 test policies have fewer than 10 segments (the
smallest has 2), so any top 5 holds the gold there and chance is 1.0. The check was
meant to show the pipeline retrieves at all. The rule says it voids H1, and it does.
The breakdown below, which is post hoc and not a test, shows the pipeline does
retrieve where retrieval is possible. It also shows the void changes nothing: units do
not help in any stratum.

Check 5 names the `header` arm, which is H1's comparator, so it is read as voiding H1.
A stricter reading ("the pipeline", shared by every arm) would void H2 as well. Under
the pre-registration, H2 never changes the decision.

## Exploratory (post hoc, not tested)

**The rerank holds, again.** On the same 400 pairs the listwise rerank lifts R@5 from
0.595 to 0.6875 for `header` (discordant 64 / 27, p = 1.3e-4) and from 0.600 to 0.7075
for `units` (69 / 26, p = 1.2e-5). That replicates the QASPER held-out and NQ results
on a third corpus. This comparison was not a pre-registered hypothesis here.

**By policy size** (R@5, `units` vs `header`):

| segments | policies | pairs | chance | header | units | discordant (units / header) | p |
|---|---|---|---|---|---|---|---|
| 2–9 | 5 | 198 | 1.000 | 1.000 | 1.000 | 0 / 0 | 1.0 |
| 10–29 | 8 | 1,085 | 0.335 | 0.640 | 0.647 | 60 / 52 | 0.51 |
| 30+ | 7 | 1,360 | 0.197 | 0.512 | 0.517 | 78 / 71 | 0.62 |

**Why units help as often as they hurt.**
- Units do reach the gold. Of the 138 pairs only `units` hit, the gold arrived through a
  generated **question** 127 times, through a description 7 times and through a claim 4 times.
- Units also reach the wrong segments. In the 123 pairs only `header` hit, an average of
  1.93 of the five slots went to a non-gold segment that arrived through a unit. Across
  all pairs, 1.59 of the five slots come through a unit.
- The likely reason is the corpus. PolicyQA questions are generic (the most frequent,
  asked of 18 policies: "Do you use my information?"). A privacy policy repeats the same topics across
  many segments, so the model writes near-identical questions for several segments,
  and a unit that matches the reader's wording matches a neighbour's just as well.

## Generation

- 20 policies, 500 chunks, 4,300 units (8.6 per chunk), 0 failures.
- 6,159 s of generation on one TrueNAS GPU: 12.3 s per chunk for two model calls.
- A second pass would make no model call; C2's staleness check was verified live on a
  separate policy.
- `derive.jsonl` has the per-policy pass reports.

## Files

- `summary.json` — `analyze.py` output (the pre-registered analysis, unchanged).
- `derive.jsonl` — the generation pass, one report per policy.
- `results/*.jsonl.gz` — each arm's ranking per question
  (`{qid, ranked, matched, reranked, rerank_reason}`).
- `explore.py`, `strata.py` — the post hoc breakdowns above.

## Reproduce

```sh
cd bench/docs/units
python3 prepare.py --out <probe>   # PolicyQA test split -> policies, questions, rr_subset
# start loomcycle with loomcycle.yaml (throwaway Postgres + pgvector; Ollama with bge-m3 + qwen3.6)
python3 ingest.py --probe <probe> && python3 derive.py --probe <probe>
for arm in header units; do python3 search.py --probe <probe> --arm $arm; done
for arm in header_rr units_rr; do for p in 1 2 3; do
  python3 search.py --probe <probe> --arm $arm --subset rr_subset.json --pass-no $p; done; done
python3 analyze.py --probe <probe>
```
