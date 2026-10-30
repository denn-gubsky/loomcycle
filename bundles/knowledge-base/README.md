# knowledge-base bundle

The links agent. `kb/linker` builds and maintains the relations (statements)
between a document's passages and the rest of the store, using only rules the
tenant's ontology declares. Every model-judged relation stays report-only.

| agent | provider | tools | role |
|---|---|---|---|
| `kb/linker` | `code-js` | `Document`, `Agent`, `Channel` (subscribe `documents/ingested` only) | the deterministic driver: owns the pass, every check and every write |
| `kb/verdict` | your `low` tier | none (`tools: []`, `skills: [-*]`, `disable_context: true`) | one bounded relation verdict per call, for L4 only |

## What it needs

- `LOOMCYCLE_CODE_AGENTS_ENABLED=1` and SQL Memory.
- The `statement_methods: [rule, extractor]` grant, which the bundle declares.
  It lets the linker stamp `rule:*` and `extractor:*` methods on its
  statements and hold the linker lease. Without it, a `propose` run fails at
  its lease.
- For L4 (relate): an embedder on the Postgres vector tier, and a `low` tier
  for `kb/verdict`.

Under `LOOMCYCLE_ACL=enforce`, the access rules of whoever starts the linker must
allow these verbs on the documents it works on:
- `propose`, to file statements, take the lease, and verify, flag or re-point
  evidence;
- `link`, on both ends' documents (and on the survivor's, for a merge), for
  `maintain` mode to expire, demote or merge.

Without `link`, those three actions are refused one by one, and the report
counts them under `refusals`.

## Layers

One pass covers one document. The layers run in this order:

| layer | reads | writes (`propose` mode) |
|---|---|---|
| **L0 classify** | the runtime's `classify` op on the document: the ontology's class rules (`@title_pattern`, `@identifier_pattern`), its Instances, and the neighbours' classes | in `propose` mode the op writes: exactly one class matched by its own rule is applied (type, identity fields, so the work key, and an `issued_by` an adopted instance can take); anything else is proposed for an operator, and a type set by hand is kept. `report` mode asks with `write: report`. The linker never accepts a proposal: that is an operator's act |
| **L1 anchors** | each live part's anchor and stable id | nothing. It reports parts without an anchor, duplicate anchors (`…~2`, by id) and parts without a stable id |
| **L2 cite** | the citations the document class's `@citation_grammar` finds | one statement per citation, from the citing passage to the cited part, `method: rule:citation`. A citation to a held work whose part is missing points at the work's root; one to an unheld work gets a placeholder. The grammar's `kind` is used only when the ontology declares it and it is not reserved; otherwise the statement is `cites` |
| **L3 terms** | the concepts of the scheme named by `terms_scheme`: their name and alternative labels | `uses_term` from each passage to each concept it mentions, `method: rule:terms`; or `defines`, `method: rule:definitions`, for a term a definitions passage defines (see below). Matches are exact or case-folded with whitespace collapsed, on word boundaries, and the longest match wins. **Tenant scope only**, because concepts live there; at user or agent scope the matches are counted and nothing is written |
| **L4 relate** | semantic neighbours from `related`, judged by `kb/verdict` | **nothing, ever.** It runs only when `corpus_paths` is given or `L4` is listed, and its findings go in the report |
| **L6 maintain** | every live statement with an end in the document, or with evidence citing it | see below |

L2 and L3 need a confirmed ontology with relation kinds. Without one they skip
and the report says why. L0 needs a confirmed ontology, with or without relation
kinds. L1 and L4 never need it. L6 still runs, without its kind and redundancy
checks.

`defines` is a seed kind. `uses_term` is not: declare it in the ontology's
Relations section (`### uses_term`). Until then, L3 files only `defines`, and
counts its other matches as `hits_no_kind`.

### L3: which passage defines a term

A class's `@definitions` pattern marks its definitions passages: the runtime
flags each one `definitional` when the linker reads the document's chunks. In
such a passage, a term defines its concept when it starts the body or a line,
after at most indentation, a list marker (`1)`, `1.2.`, `а)`, a bullet) and an
opening quote. So in "1) договір — угода …" the passage defines договір; in "У
цьому Законі договір …" it only uses it.
- Each concept the passage defines gets one `defines` statement, with its
  defining occurrences first in the evidence. The passage's other terms get
  `uses_term`.
- If the ontology doesn't declare `defines`, those terms get `uses_term`
  instead, counted as `defines_not_declared`.
- Terms are matched in the passage's body, so a term that appears only in a
  chunk title is not seen.

### L6: maintaining what exists

For each statement, the first check that fails decides the outcome:

1. **Endpoints.** An end chunk that no longer exists: expire.
2. **Kind.**
   - A kind the ontology renamed: the statement is proposed again under the new
     name, then the old one expires.
   - A kind no longer declared: demote it to `proposed`.
3. **Validity.** An end replaced by a newer edition: expire it, after proposing
   it again on the successor (`attributes.revision_of`) when it still holds there.
   The new proposal keeps the original's `valid_at`, but not its confidence.
   - **A citation** (`rule:citation`) is re-read by the document's citation
     grammar on the new part. It moves when a hit there still cites the same work
     with the same kind, with that hit's quotes as its evidence: a citation that
     lost its number form but kept its title, or whose line broke elsewhere,
     still moves. One the new part no longer makes is not moved, even if an old
     quote survives word for word (the new edition may name a newer act in the
     same words), and the copy the re-import made of it is expired too. If the
     grammar cannot be read, the rule below applies.
   - **Any other statement** moves when every quote it had on the old part is
     still in the new one. A quote whose words survive with different line
     breaks or spacing counts, and the new evidence quotes the new part's own
     text. A statement with only some of its quotes left is not moved: what is
     left cannot vouch for what the passage now says.
4. **Evidence.**
   - A quote no longer in its chunk but present in the live part with the same
     stable id: the evidence moves to that part.
   - Otherwise the evidence row is marked stale.
   - When all of a statement's evidence is stale, it is demoted.
5. **Redundancy.** Statements left identical once a kind dropped its
   discriminator are merged into the oldest of the same status. A duplicate of
   another status is flagged.

If every check passes, the statement is verified.

These rules always hold:
- A statement a person wrote (`manual`, or a method the linker does not know),
  or one an operator ruled on, is only ever **verified or flagged**. Statements
  written by `rule:`, `extractor:`, import and canvas are all maintained the
  same way.
- A synced statement is judged by the method that first made it
  (`attributes.origin_method`). One with no `origin_method`, synced before the
  field existed, may be a person's, so it is only verified or flagged.
- A statement the runtime wrote (the edition chain's `supersedes`, the
  consolidator's links, `[[name]]` links) and any reserved kind are **not
  touched**.
- An auto link whose text is gone from its source is counted
  (`auto_rule_a_candidates`), and nothing else happens to it.
- Nothing is ever deleted.

## Modes

| `mode` | over | layers by default |
|---|---|---|
| `document` (default) | `document_id` or `path` | L0, L1, L2, L3, L6 (+ L4 with `corpus_paths`) |
| `maintain` | `document_id` or `path` | L6 |
| `drain` | the `documents/ingested` queue, up to `max_messages` | L0, L1, L2, L3, L6 per message |

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
| `layers` | per mode | any of `L0`, `L1`, `L2`, `L3`, `L4`, `L6`; `maintain` takes `L6` only |
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
  - A layer that proposes counts each write by what the runtime made of it:
    `proposed` or `asserted` (a new statement), `evidence_added` (a new quote on
    a statement already held), `unchanged` (statement and quote both held), or
    `already_asserted`. A re-run over the same text proposes nothing: its writes
    count as `unchanged`.
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
