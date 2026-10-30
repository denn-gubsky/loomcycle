# knowledge-base bundle

The links agent. `kb/linker` builds and maintains the relations (statements)
between a document's passages and the rest of the store, using only rules the
tenant's ontology declares. Every model-judged relation stays report-only.

| agent | provider | tools | role |
|---|---|---|---|
| `kb/linker` | `code-js` | `Document`, `Agent`, `Channel` (subscribe `documents/ingested` only) | the deterministic driver: owns the pass, every check and every write |
| `kb/verdict` | your `low` tier | none (`tools: []`, `skills: [-*]`, `disable_context: true`) | one bounded relation verdict per call, for L4 only |

## Status: coded against runtime ops not yet on main

The writing path calls ops that are still being built:
- the `statement_methods` grant and the `method` field on statement writes;
- `maintain_statement`;
- the `list_statements` filters;
- `describe_ontology`;
- `extract_citations`;
- the document `lease` / `release`.

Until they ship:
- a `report` run of L1 (anchors) and L4 (relate) works on today's runtime;
- every other layer skips with a reason, or fails on the op it needs;
- a `propose` run fails at its lease.

The `statement_methods: [rule, extractor]` grant is a marked comment in the yaml.
Config load rejects the field until the runtime knows it.

## Layers

One pass covers one document. The layers run in this order:

| layer | reads | writes (`propose` mode) |
|---|---|---|
| **L1 anchors** | each live part's anchor and stable id | nothing. It reports parts without an anchor, duplicate anchors (`…~2`, by id) and parts without a stable id |
| **L2 cite** | the citations the document class's `@citation_grammar` finds | one statement per citation, from the citing passage to the cited part, `method: rule:citation`. A citation to a held work whose part is missing points at the work's root; one to an unheld work gets a placeholder. The grammar's `kind` is used only when the ontology declares it and it is not reserved; otherwise the statement is `cites` |
| **L3 terms** | the concepts of the scheme named by `terms_scheme`: their name and alternative labels | `uses_term` from each passage to each concept it mentions, `method: rule:terms`. Matches are exact or case-folded with whitespace collapsed, on word boundaries, and the longest match wins. **Tenant scope only**, because concepts live there; at user or agent scope the matches are counted and nothing is written. `defines` is deferred |
| **L4 relate** | semantic neighbours from `related`, judged by `kb/verdict` | **nothing, ever.** It runs only when `corpus_paths` is given or `L4` is listed, and its findings go in the report |
| **L6 maintain** | every live statement with an end in the document, or with evidence citing it | see below |

L2 and L3 need a confirmed ontology with relation kinds. Without one they skip
and the report says why. L1 and L4 never need it. L6 still runs, without its kind
and redundancy checks.

### L6: maintaining what exists

For each statement, the first check that fails decides the outcome:

1. **Endpoints.** An end chunk that no longer exists: expire.
2. **Kind.**
   - A kind the ontology renamed: the statement is proposed again under the new
     name, then the old one expires.
   - A kind no longer declared: demote it to `proposed`.
3. **Validity.** An end replaced by a newer edition: expire it. When every quote
   the statement had on the old part is still in the new one, the statement is
   first proposed again on the successor (`attributes.revision_of`). The new
   proposal keeps the original's `valid_at`, but not its confidence.
4. **Evidence.**
   - A quote no longer in its chunk but present in the live part with the same
     stable id: the evidence moves to that part.
   - Otherwise the evidence row is marked stale.
   - When all of a statement's evidence is stale, it is demoted.
5. **Redundancy.** Statements left identical once a kind dropped its
   discriminator are merged into the oldest.

If every check passes, the statement is verified.

These rules always hold:
- A statement a person wrote (`manual`, or a method the linker does not know) is
  only ever **verified or flagged**. Statements written by `rule:`,
  `extractor:`, import, sync and canvas are all maintained the same way.
- A statement the runtime wrote (the edition chain's `supersedes`, the
  consolidator's links, `[[name]]` links) and any reserved kind are **not
  touched**.
- An auto link whose text is gone from its source is counted
  (`auto_rule_a_candidates`), and nothing else happens to it.
- Nothing is ever deleted.

## Modes

| `mode` | over | layers by default |
|---|---|---|
| `document` (default) | `document_id` or `path` | L1, L2, L3, L6 (+ L4 with `corpus_paths`) |
| `maintain` | `document_id` or `path` | L6 |
| `drain` | the `documents/ingested` queue, up to `max_messages` | L1, L2, L3, L6 per message |

| `write` | does |
|---|---|
| `report` (default) | Issues read ops only, takes no lease and acks nothing. It counts what it would write in `would_write`. **Run this first.** |
| `propose` | Takes the scope's `linker` lease first; a busy lease means `status: busy` and nothing else. Renews the lease as the pass goes and releases it on every exit path. Writes through `propose_statement` and `maintain_statement` and never asserts anything itself: the runtime decides auto-assertion. |

**Drain** reads one message at a time without consuming it:
- Each message is acked only after its pass is recorded.
- A malformed message, or one naming a document the scope no longer holds, is
  acked and reported as such.
- A failed pass, or one cut by the budget, stops the drain and leaves its
  message unacked. Acking any later message would consume the failed one.
- A `reimported` message runs L2 and L3 on its changed parts only. L6 still
  covers the whole document.

Drain runs at user scope only: the queue the bundle declares is user-scoped.

To link documents on import, add an opt-in schedule. Import latency is then the
schedule's interval:

```yaml
scheduled_runs:
  kb-linker-drain:
    agent: kb/linker
    schedule: "*/10 * * * *"
    user_id: me                       # whose documents/ingested queue to drain
    concurrency_policy: forbid        # the lease would refuse a second one anyway
    enabled: true
    prompt:
      - role: user
        content: [ { type: trusted-text, text: '{"mode":"drain","write":"propose"}' } ]
```

## Use

```bash
LOOMCYCLE_CODE_AGENTS_ENABLED=1 LOOMCYCLE_SQLMEM_ENABLED=1 \
  LOOMCYCLE_PRESETS=base,knowledge-base ./bin/loomcycle
```

```bash
curl -sX POST localhost:8080/v1/runs \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -d '{"agent":"kb/linker","user_id":"me",
       "prompt":"{\"path\":\"/acts/incident-reporting\"}"}'
```

| field | default | meaning |
|---|---|---|
| `mode` | `document` | `document`, `maintain` or `drain` |
| `document_id` or `path` | exactly one (none in drain) | the document |
| `scope` | `user` | `user`, `agent` or `tenant` (drain: `user`) |
| `layers` | per mode | any of `L1`, `L2`, `L3`, `L4`, `L6`; `maintain` takes `L6` only |
| `write` | `report` | `report` or `propose` |
| `terms_scheme` | none (L3 skips) | the concept scheme L3 matches |
| `as_of` | each part's own date | unix nanoseconds; citations resolve as of it |
| `max_writes` | `500` | cap on writes per run, also counted in report mode (1–5000) |
| `max_statements` | `500` | cap on statements L6 checks per document (1–5000) |
| `max_messages` | `10` | messages a drain processes (1–100) |
| `corpus_paths` | — | L4 only: where its candidates come from |
| `exclude_document_types`, `candidate_floor`, `max_candidates_per_chunk`, `max_verdict_calls`, `max_chars`, `min_body_chars`, `relations`, `include_quotes` | as before | L4 only. `min_body_chars` also bounds which passages L3 reads |

An unknown field or an out-of-range value refuses the run with
`{"status":"refused","reason":…}`, before any tool call or lease.

## The report

The run's final output is one JSON object:

- `status`:
  - `complete`;
  - `budget_stopped`;
  - `busy`;
  - `failed` (`failure` says why). This includes a pass in which no write went
    through and at least one was refused for a reason other than "already
    accepted" or "rejected". Such a refusal usually means a configuration fault,
    such as a missing method grant.
  - `refused`.
- `ontology_version`: the version the pass ran under. `ontology` shows whether
  it was usable and why not.
- `lease`: whether a lease was taken, whether it was acquired, `held_by`,
  `renewals`, `lost` and `released`.
- `layers.L1` … `layers.L6`: each layer's `status` (`run`, `skipped` with
  `reason`) and its `counts`.
  - L6 adds `checks` (which check decided) and `actions` (per action).
  - L4 keeps its findings and spans in UTF-16 units (`span_unit`).
- `writes`, `would_write`: per op. `refusals`: per op and refusal code.
- `budget`: the caps and what was spent.
- `queue` and `messages` (drain only): each message's outcome and whether it
  was acked.

Every evidence span the pass sends is in code points (`evidence_span_unit:
"rune"`). The report carries ids, counts and fixed reason strings, never store
text. L4 quote text appears only with `include_quotes: true`.

## Scope

`scope: user` works with the bundle as shipped. A tenant pass (which L3 needs
in order to write) requires both tenant grants on the linker. The bundle leaves
them out on purpose, because a tenant Document grant is a tenant write grant:

```yaml
agents:
  kb/linker:
    memory_scopes: [tenant]
    sql_scopes: [tenant]
```

## Cost

The linker makes no model call except in L4. L4 spends one `kb/verdict` call per
claim-bearing passage that has candidates, capped by `max_verdict_calls`, and
runs only when asked for. Nothing is scheduled unless you add a schedule.
