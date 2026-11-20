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

Each question in `questions.yaml` keeps an empty `gold` list: the gold names
documents of a real tenant and **never enters this repo**. It lives in a
separate gold file outside the tree (for example
`~/work/loomcycle-internal/gm-p7/gold.yaml`) and is passed with `--gold PATH`;
the driver merges it into the questions by id.

An operator fills it before the first run from a listing of the store rather
than from either arm's answers (so neither arm defines what is right):
`query_documents` by `under_path` and `type`, `search` with `facets`, and
reading the briefs. Record who filled it and when in the gold file itself
(`gold_notes`).

The gold file has `questions.yaml`'s shape; per question id:

```yaml
questions:
  - id: q01
    gold: [<document id>, ...]   # the documents a complete answer names
    also: [<document id>, ...]   # optional: fine to cite, never required
    confidence: high             # optional, free text
    gold_notes: |                # optional, free text
      Who filled it, when, from which listing.
  - id: q13
    gold_topics:                 # optional: score on topics, not documents
      - name: security and access control
        aliases: [security, access control, authz]
      - name: sandboxing
```

The driver refuses an id that `questions.yaml` does not have (and a duplicate
id or an unknown key), and warns on every question left without gold; such a
question is reported without recall. The file is read by a small YAML subset
parser (block mappings and lists, `[a, b]` flow lists, quoted scalars, `|`/`>`
blocks, comments); anything else is an error.

### Topic gold

A question whose gold entry has `gold_topics` (owner ruling) is scored on
whether the answer **names** each topic: a case-insensitive, word-boundary
match of the topic's name or any alias (`authz` does not match `authzx`;
the words of a multi-word name may be split by any whitespace). Its recall is
topics named / topics listed. It may carry no document gold; if it does, the
document scores are kept on the run as `doc_recall` / `doc_precision` but do
not enter the summary. This suits `area` questions, whose answer is a grouping
by topic rather than a list of documents.

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
GOLD=~/work/loomcycle-internal/gm-p7/gold.yaml   # outside the repo
python3 -I scripts/measure/walker/drive.py --dry-run --gold "$GOLD" \
  --arm tax=kb/walker --arm notax=kb/walker-notax
python3 -I scripts/measure/walker/drive.py \
  --base-url https://<host> --gold "$GOLD" \
  --arm tax=kb/walker --arm notax=kb/walker-notax \
  --provider anthropic --model <haiku model id> \
  --runs 3 --budget-usd 5 --out <directory outside the repo>
```

It writes `runs.jsonl` (one line per run: answer text, tool calls by op,
token usage, cost, score) and `summary.json`. The driver stops before a run
that could take the spend past `--budget-usd` (a run is assumed to cost at
most $0.04). `--dry-run` also counts the questions scored on documents and on
topics. `--score-only` rescores an existing `runs.jsonl` against `--gold`
(required: without it the driver refuses rather than replace `summary.json`
with one that has no recall), e.g. after the gold is filled or corrected;
`--arm` is not needed. It writes
the rescored runs to `runs.scored.jsonl` beside it and leaves `runs.jsonl`
untouched.

## Running the tests

```sh
python3 -I -m unittest discover -s scripts/measure/walker -p 'test_*.py'
```

Standard library only, no network; the fixtures use synthetic ids.

## What is scored

Per run: the metric kind (`docs`, `topics`, or none without gold), the
documents cited (`loom:doc/<id>` links), recall against the gold (documents,
or topics named), precision for documents (`gold` and `also` both count as
correct citations), the `COVERAGE` line, whether `place` and `measure` were
called, tool calls, cost. `summary.json` holds `rows` per arm, question kind
and metric kind (mean recall, mean documents cited, share of runs reporting
`complete`, failed runs, spend), and `by_metric`: the arms' mean recall side
by side, separately for document recall and topic recall, which are never
averaged together. The P7 moves pass if
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
