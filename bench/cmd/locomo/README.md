# locomo — LoCoMo long-term conversational memory benchmark

Scores loomcycle's memory **retrieval** against the LoCoMo benchmark, on a
corpus held in an isolated tenant. It drives an externally-running instance, so
the numbers come from the real vector store and the real configured embedder —
not from a harness substitute for either.

## Why retrieval, and why this benchmark

LoCoMo's QA annotations carry an `evidence` field: the `dia_id`s of the
dialogue turns that support each answer. Ingest every turn as a memory row
**keyed by its `dia_id`**, and each annotation becomes retrieval ground truth:

```
question  ->  "When did Caroline go to the LGBTQ support group?"
expected  ->  ["D1:3"]
```

So the retrieval axis needs **no LLM judge and no answer-string matching**.
It is deterministic, cheap and re-runnable, and it measures the ranker and the
embedder rather than the answering model. That matters because LoCoMo's *answer*
axis is close to saturated (a plain full-context baseline beats reported memory
systems) while its retrieval axis is not.

The corpus is also small: ~5,900 turns across 10 conversations, so a full run
costs ~5,900 embeddings and nothing else.

## The dataset is not in this repo

LoCoMo ships under **CC BY-NC 4.0**. Nothing derived from it is committed here.
Download your own copy:

```sh
curl -sSLO https://raw.githubusercontent.com/snap-research/locomo/main/data/locomo10.json
```

Pass it with `-data`. Keep the file and any generated corpus out of version
control.

## Isolate the memory first

The memory routes derive the tenant from the **principal** — they honour no
`?tenant=` override — so the only way to keep ~5,900 synthetic conversational
rows out of real memory is a dedicated tenant's bearer.

Mint one (admin bearer required), then point the harness at it:

```sh
# 1. mint an OperatorTokenDef for a tenant of its own
curl -sS -X POST "$LOOMCYCLE/v1/_operatortokendef" \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"op":"create","name":"locomo-bench","tenant_id":"locomo-bench",
       "subject":"bench","scopes":["substrate:tenant"]}'

# 2. give it to the harness (preferred over reusing the operator bearer)
export LOOMCYCLE_LOCOMO_TOKEN='<the minted token>'
```

The harness calls `GET /v1/_me` before it writes anything, prints the tenant it
resolved, and **refuses to run** if that tenant is the default/legacy one.
`-allow-shared-tenant` overrides this deliberately for single-tenant
deployments.

Prerequisites on the instance: a vector-capable store
(`LOOMCYCLE_PGVECTOR_ENABLED=1` on Postgres, or a sqlite-vec build) and a
configured `memory.embedder`. Without either, ingest aborts on the first embed
warning rather than writing thousands of un-embedded rows.

## Modes

```sh
go build -o bin/locomo ./bench/cmd/locomo

# offline: emit one memory-eval dataset per conversation (no instance needed)
./bin/locomo -mode=convert -data locomo10.json -out /tmp/locomo-datasets
loomcycle memory-eval --dataset /tmp/locomo-datasets/locomo-conv-26.jsonl

# live: ingest, then score
./bin/locomo -mode=all -data locomo10.json -loomcycle http://127.0.0.1:8787

# smoke it on one conversation first
./bin/locomo -mode=all -data locomo10.json -conversations 1

# reclaim the scope
./bin/locomo -mode=purge -data locomo10.json
```

`convert` output runs through `loomcycle memory-eval`, but that command embeds
with a deterministic bag-of-tokens stub — those numbers validate the plumbing,
not retrieval quality. Use `-mode=all` for a semantic number.

One dataset **per conversation**, never combined: `dia_id`s are numbered per
conversation and collide heavily across them (5,882 turns, 1,033 distinct ids).
Each conversation is ingested under its own `scope_id` (`locomo-<sample_id>`)
for the same reason.

Useful flags: `-top-k` (default 10), `-categories` (default `1,2,3,4`),
`-conversations N`, `-concurrency`, `-no-embed` (write now, embed later via
`POST /v1/_memory/backfill_embeddings`), `-dry-run`.

## Output

`bench/results/locomo-<timestamp>/` gets `matrix.md` (overall, per-category and
per-conversation tables) and `report.json` (the same plus every per-query
result, so a surprising number can be traced rather than re-run).

Every report prints **which categories it included**, because differing filters
are precisely why published LoCoMo numbers cannot be compared with one another.

## What the dataset gets wrong

The harness counts and reports what it could not use, rather than quietly
shrinking the answer key underneath the metric. On the released file:

- **Category 5 (adversarial) is excluded by default.** 444 of its 446 questions
  have a `null` answer; both Mem0 and Zep dropped it too. Categories 1–4 are the
  1,540 questions everyone reports.
- **Evidence strings are dirty.** Alongside clean `"D23:1"` entries the file
  carries `"D8:6; D9:17"`, `"D9:1 D4:4 D4:6"`, `"D:11:26"` and a bare `"D"`. The
  parser splits packed entries and repairs the unambiguous transposed colon; it
  reports anything else instead of guessing.
- **A few evidence ids name no turn in their own conversation** (3 on the
  current file) — unretrievable by anyone, so they are dropped rather than
  charged to the memory stack.
- **5 questions have no usable evidence** and are dropped: open-domain questions
  legitimately draw on world knowledge rather than a turn.
- **16 sessions are declared with a timestamp but no turns.**
- Documented elsewhere and not correctable here: some questions attribute
  statements to the wrong speaker, some are ambiguous with several valid
  answers, some multimodal questions are unanswerable from the BLIP caption, and
  some gold answers are simply wrong.

A run on the current file scores **1,535 queries over 5,882 rows**.

## Two things the numbers depend on

- **Session timestamps are prefixed onto every row.** LoCoMo dates live on the
  session, not the turn, so a row without one is unretrievable for the temporal
  category however good the embedder is.
- **Rows embed with their JSON quotes.** `PUT /v1/_memory/.../keys/{key}` has no
  `embed_text` field and embeds the JSON-encoded value, so a stored row's
  embedded text carries surrounding quotes the query text does not. Two
  characters against a couple of hundred is noise for a dense embedder, but it
  is an asymmetry and the report says so.

## Not covered here

LoCoMo does not test **knowledge updates**, which is what a bi-temporal fact
tier exists for. That axis needs a different corpus — see the research document
`/loomcycle/docs/factual-corpus-sources-and-memory-benchmarks` in the document
store.
