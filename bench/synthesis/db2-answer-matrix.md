# LoCoMo answer axis — 2026-09-15T15:43:24Z

- instance: `http://127.0.0.1:8874`   tenant: `db2-bench`   memory subject: `bench`
- answerer: `locomo/answerer`   judge: `locomo/judge`
- **answerer's store: 376 consolidated facts only** (no -seed-turns; the raw turns are not in the partition it reads)
- ingested: 878 turns across 63 sessions from 1 conversation(s); 376 facts written by consolidation
- **categories included: 1 (multi-hop), 2 (temporal), 3 (open-domain), 4 (single-hop)**
- questions asked: 120

## Overall

| slice | asked | graded | accuracy | correct | partial | wrong | unparsed | NOT_FOUND | p50 ms |
|---|---|---|---|---|---|---|---|---|---|
| overall | 120 | 120 | 0.0000 | 0 | 0 | 120 | 0 | 1.000 | 5007 |

## By category

| slice | asked | graded | accuracy | correct | partial | wrong | unparsed | NOT_FOUND | p50 ms |
|---|---|---|---|---|---|---|---|---|---|
| category-202 | 48 | 48 | 0.0000 | 0 | 0 | 48 | 0 | 1.000 | 4673 |
| category-203 | 48 | 48 | 0.0000 | 0 | 0 | 48 | 0 | 1.000 | 5193 |
| category-204 | 24 | 24 | 0.0000 | 0 | 0 | 24 | 0 | 1.000 | 6519 |

Accuracy is the LoCoMo convention: correct 1, partial 0.5, wrong 0, averaged over GRADED answers. Unparsed verdicts are excluded from accuracy and counted separately — grading a judge malfunction as a memory miss would understate the system under test. NOT_FOUND is the answerer's abstention rate; every category here is answerable, so an abstention is scored wrong.

## Notes

- Ingest is deterministic: turns are handed to `Memory op=add` verbatim over MCP, so a miss is attributable to consolidation or recall rather than to an ingesting agent's transcription.
- The answerer holds the Memory tool and nothing else, and is instructed to answer NOT_FOUND rather than fall back on general knowledge.
- Conversations run in sequence with the memory layer purged between them: scope_id is server-derived from the run identity, so an off-run caller cannot give each conversation its own partition.
