# walker — RFC GM P7 measurement (prepared, not run)

Does `kb/walker` answer overview questions better with the taxonomy moves of
RFC GM P7 (the concept chain in `lineage`, `place`, facet `baseline`, entering
by topic, and "what changed" over `measure compare`) than without them?

This directory holds the question set and the driver. Nothing here has been
run against a deployment; run it only in a slot agreed with the KB session
(implementation plan §8), never during one of its measurement sets.

## The question set

`questions.yaml`: 24 overview questions written for this measurement and used
to tune nothing — 12 `overview` ("what do we have on X"), 6 `area` ("what is
in this part, by topic") and 6 `changed` ("what is new in X"). They are about
the loomcycle knowledge base of the development tenant. Never edit a question
after the first run; add a new id.

### Gold

Each question has an empty `gold` list. Before the first run an operator fills
it with the ids of the documents a complete answer names, from a listing of
the store rather than from either arm's answers (so neither arm defines what
is right): `query_documents` by `under_path` and `type`, `search` with
`facets`, and reading the briefs. A question whose gold cannot be agreed stays
empty and is reported without recall. Record who filled it and when in the
commit that adds the ids.

## The arms

- `tax` — `kb/walker` as built by P7 (this branch, released).
- `notax` — the same walker without the taxonomy moves: a runtime fork
  `kb/walker-notax` whose `system_prompt` is the walker's prompt as of
  `90c2f6b30` (main before P7). Create it with `AgentDef op=create` (overlay
  key `overlay`, same `tools`, `tool_modes`, scopes and tier as `kb/walker`)
  and retire it after the measurement.

Both arms run on the same release, the same tenant, the same model, pinned per
run (`--provider`, `--model`; Haiku, as the KB session's sets), three runs per
question per arm, interleaved so that drift on the server hits both alike.

Owner question: "without taxonomies" is read here as "without the P7 moves".
The pre-P7 walker can still filter by `concept` and count the concept facet,
so this measures the moves, not the taxonomy as a whole. A stricter control
(the same prompt with `{{memory:ontology}}` and every concept form removed)
is a third arm if wanted.

## Run

```sh
export LOOMCYCLE_TOKEN=...        # an operator token of the tenant; never printed
python3 -I scripts/measure/walker/drive.py --dry-run \
  --arm tax=kb/walker --arm notax=kb/walker-notax
python3 -I scripts/measure/walker/drive.py \
  --base-url https://<host> \
  --arm tax=kb/walker --arm notax=kb/walker-notax \
  --provider anthropic --model <haiku model id> \
  --runs 3 --budget-usd 5 --out <directory outside the repo>
```

It writes `runs.jsonl` (one line per run: answer text, tool calls by op,
token usage, cost, score) and `summary.json` (per arm and kind). The driver
stops before a run that could take the spend past `--budget-usd` (a run is assumed to cost at most $0.04). `--score-only` rescores an
existing `runs.jsonl`, e.g. after the gold is filled.

## What is scored

Per run: the documents cited (`loom:doc/<id>` links), recall and precision
against the gold, the `COVERAGE` line, whether `place` and `measure` were
called, tool calls, cost. Per arm and kind: mean recall, mean documents cited,
share of runs reporting `complete`, failed runs, spend. The P7 moves pass if
`tax` raises mean recall on `overview` and `area` without lowering it on any
kind by more than the spread between its own three runs, and answers
`changed` questions with dated material at least as often. A `changed`
question also needs stored series (`node_stats compute`, plan P2.2); without
them the walker is told to fall back to listing by date, and the report says
how often it did.

## Cost (Haiku)

Price model: input $0.10/M, output $0.50/M, cache read $0.01/M, cache write
$0.125/M. An overview walk is at most 14 tool calls, so about 15 model turns.
Estimated per run: a cached system prompt of about 7k tokens read every turn,
tool results of about 3k tokens each written once and read on every later
turn (about 365k cache-read and 57k cache-write tokens), about 5k output
tokens: about $0.004 + $0.007 + $0.003 = **$0.014 per run**. 24 questions × 2
arms × 3 runs = 144 runs: **about $2.0**, with a ceiling of $5.76 at $0.04 per
run (`--dry-run` prints it). The gold listing is an operator's reads and costs
no model tokens.
