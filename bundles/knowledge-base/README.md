# knowledge-base bundle

A **report-only** relation finder for Documents. For each claim-bearing passage
of one document, `kb/linker` proposes which declared relation (if any) it has to
passages elsewhere in a corpus, with two quotes checked by code, and returns a
pass report. **It writes nothing to the store.**

| agent | provider | tools | role |
|---|---|---|---|
| `kb/linker` | `code-js` | `Document` (read ops only), `Agent` | the deterministic driver: candidates, budget, parsing, quote check, report |
| `kb/verdict` | your `low` tier | none (`tools: []`, `skills: [-*]`, `disable_context: true`) | one bounded relation verdict per call |

## Status: prototype, report only

This is the first phase of the links agent. Its purpose is to measure, on a real
corpus, how often a model-judged relation is wrong and what a document costs,
before anything is allowed to write a relation. So today:

- **One layer:** semantic relations between passages ("relate"). Citations,
  anchors, mentions, check-against and the maintenance of existing links are not
  built.
- **Report mode only.** `write` must be `"report"` or absent; any other value
  is refused. The pass issues `get_document`, `query_chunks`, `get_chunk` and
  `related`, and nothing else.
- **Vocabulary:** five built-in kinds — `elaborates`, `implements`,
  `stricter_than`, `conflicts_with`, `alternate_of` — or your own list in the
  run input. The tenant ontology is not read yet; `domain`/`range` are shown to
  the model but not enforced.

## Requirements

- `LOOMCYCLE_CODE_AGENTS_ENABLED=1` — a `code-js` agent selected without it
  **fails boot** by design.
- SQL Memory (`LOOMCYCLE_SQLMEM_ENABLED=1`), an embedder and the Postgres vector
  tier: `related` is a vector search, and sqlite has no vector leg for it.
- A runtime whose `Document op=related` takes the corpus filters (`under_paths`,
  `exclude_document_ids`). The pass checks that the server echoed the filter
  and stops with `status: "failed"` if it did not, rather than judging the whole
  scope.
- A `low` tier from another layer (select `base` alongside this bundle).

## Use

```bash
LOOMCYCLE_CODE_AGENTS_ENABLED=1 LOOMCYCLE_SQLMEM_ENABLED=1 \
  LOOMCYCLE_PRESETS=base,knowledge-base ./bin/loomcycle
```

The run prompt is a JSON object:

```bash
curl -sX POST localhost:8080/v1/runs \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -d '{"agent":"kb/linker","user_id":"me",
       "prompt":"{\"path\":\"/acts/incident-reporting\",\"corpus_paths\":[\"/laws\"],\"max_verdict_calls\":20}"}'
```

| field | default | meaning |
|---|---|---|
| `document_id` or `path` | — (exactly one) | the document whose passages are related |
| `corpus_paths` | — (required) | Path-tree paths the candidates must come from |
| `scope` | `user` | `user` or `tenant` (see below) |
| `exclude_document_types` | `[]` | a candidate whose chunk type or document type is listed is dropped; a passage of a listed chunk type is skipped |
| `candidate_floor` | `0.5` | minimum `related` score for a candidate |
| `max_candidates_per_chunk` | `8` | candidates sent per verdict call (1–20) |
| `max_verdict_calls` | `100` | hard cap on model calls for the pass (1–1000) |
| `max_chars` | `1500` | each text is truncated to this for the model; quotes are checked against the full text |
| `min_body_chars` | `80` | shorter passages are skipped as structural |
| `relations` | the five kinds | `[{name, meaning, domain?, range?}]` replaces the vocabulary |
| `include_quotes` | `false` | put the quote text in each finding |
| `write` | `report` | the only accepted value |

An unknown field or an out-of-range value refuses the run with
`{"status":"refused","reason":…}` before any tool call.

## The report

The run's final output is one JSON object:

- `status` — `complete`, `budget_stopped`, `failed` or `refused`; `failure`
  names what stopped a failed pass.
- `counts` — per stage: chunks (`chunks_total`, `chunks_seen`, `chunks_skipped`
  with its reasons, `chunks_without_candidates`, `chunks_unvisited`), candidates
  (`candidates_considered`, `_below_floor`, `_self`, `_excluded_type`,
  `_over_cap`, `_unreadable`, `_sent`), verdicts (`verdict_calls`,
  `verdict_failures`, `verdicts_none`, `verdicts_out_of_vocabulary`,
  `verdicts_kind` per kind, `quotes_rejected`, `parse_errors`). Every candidate
  sent ends as exactly one of a kind, none, or a rejected quote.
- `budget` — `max_verdict_calls`, `verdict_calls`, and `stopped`.
- `findings` — `{from_chunk_id, to_chunk_id, to_document_id, kind, direction,
  confidence, quote_from_span, quote_to_span}`. `from` is always the passage of
  this document; `direction` is `this_to_candidate` or `candidate_to_this`.
  Spans are `[start, end)` offsets in UTF-16 code units (`span_unit`).

By default the report carries ids, kinds, spans and counts only — no title,
body or quote text. `include_quotes: true` adds the two quotes per finding, for
reading the findings by hand.

A verdict is dropped when its kind is not in the vocabulary, when it is
malformed, or when a quote does not occur verbatim (or is shorter than 8
characters) in the text it claims to come from.

## Scope

`scope: user` works with the bundle as shipped. A tenant corpus needs both
tenant grants on the linker, which the bundle deliberately leaves out (a tenant
Document grant also allows tenant writes). Add them in your own overlay:

```yaml
agents:
  kb/linker:
    memory_scopes: [tenant]
    sql_scopes: [tenant]
```

## Cost

The linker makes no model call. Each claim-bearing passage that has candidates
costs one `kb/verdict` call, capped by `max_verdict_calls`. Nothing is
scheduled: the bundle spends only when someone runs it.
