# knowledge-base bundle

The links agent. `kb/linker` builds and maintains the relations (statements)
between a document's passages and the rest of the store, using only rules the
tenant's ontology declares. A model-judged relation is only ever proposed, and
only of the kinds the bundle lets the relate layer write (`elaborates`).

| agent | provider | tools | role |
|---|---|---|---|
| `kb/linker` | `code-js` | `Document`, `Agent`, `Channel` (subscribe `documents/ingested` and `memory/subjects` only) | the deterministic driver: owns the pass, every check and every write |
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
| **L0 classify** | the runtime's `classify` op on the document: the ontology's class rules (`@title_pattern`, `@identifier_pattern`), its Instances, and the neighbours' classes | in `propose` mode the op writes: exactly one class matched by its own rule is applied (type, identity fields, so the work key, and an `issued_by` an adopted instance can take); anything else is proposed for an operator, and a type set by hand is kept. `report` mode asks with `write: report`. The linker never accepts a proposal: that is an operator's act. With `file: {root}` a class in force also files the document: it gets a name at `<root>/<Class>/<title>` |
| **L1 anchors** | each live part's anchor and stable id | nothing. It reports parts without an anchor, duplicate anchors (`…~2`, by id) and parts without a stable id |
| **L2 cite** | the citations the document class's `@citation_grammar` finds | one statement per citation, from the citing passage to the cited part, `method: rule:citation`. A citation to a held work whose part is missing points at the work's root; one to an unheld work gets a placeholder. The grammar's `kind` is used only when the ontology declares it and it is not reserved; otherwise the statement is `cites` |
| **L3 terms** | the concepts of the scheme named by `terms_scheme`: their name and alternative labels | `uses_term` from each passage to each concept it mentions, `method: rule:terms`; or `defines`, `method: rule:definitions`, for a term a definitions passage defines (see below). Matches are exact or case-folded with whitespace collapsed, on word boundaries, and the longest match wins. **Tenant scope only**, because concepts live there; at user or agent scope the matches are counted and nothing is written |
| **L4 relate** | semantic neighbours from `related`, judged by `kb/verdict` | in `propose` mode, `elaborates` only: one proposal per passage and target document, with both quotes as evidence, by `extractor:relate`. Every other kind stays in the report. It runs only when `corpus_paths` is given or `L4` is listed |
| **L5 check-against** | each passage's norms under `check_against`: first the parts of the works it already cites, then the closest by meaning, up to `max_norms` (12); one `kb/verdict` call per passage | **a report document, no statements**: `<report_root>/<title>-<date>` (default root `/reports/check-against`), each finding (`addresses`, `complies_with`, `possible_gap`, `possible_conflict`) with both quotes, under a *not a legal opinion* banner. It runs only when `check_against` is given or `L5` is listed |
| **L6 maintain** | every live statement with an end in the document, or with evidence citing it | see below |

L2 and L3 need a confirmed ontology with relation kinds. Without one they skip
and the report says why. L0 needs a confirmed ontology, with or without relation
kinds. L1 and L4 never need it. L6 still runs, without its kind and redundancy
checks.

`defines` is a seed kind. `uses_term` is not: declare it in the ontology's
Relations section (`### uses_term`). Until then, L3 files only `defines`, and
counts its other matches as `hits_no_kind`.

### L0: filing

With `file: {"root": "/board/by-class"}`, a document whose class is in force gets
a name in its class's home, `/board/by-class/<Class>/<title>` (the title reduced
to letters, digits, `.`, `_` and `-`; the document id when nothing is left).
Filing **adds** a name through `set_path`: the document keeps every name it
had, so a folder tree you arranged is never moved, and `Path rm` takes the added
name away. A name another document already holds is left to it: the document's
id is appended to its own. Without `file`, nothing is filed. The report says
which class a document was filed under (`layers.L0.filing`), never the path,
which carries the title.

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

### L4: what a relation proposal needs

A verdict becomes a proposal only when all of these hold:
- its kind is one the bundle lets L4 write (`relate_propose_kinds`, `elaborates`
  today) and the tenant's ontology declares it. The other kinds are judged and
  reported, never written (`propose_kind_off`, `propose_kind_not_declared`);
- both quotes occur verbatim, the passage's in the passage and the candidate's
  in the candidate. There is no confidence floor: on a real corpus, confidence
  did not separate right verdicts from wrong ones;
- neither quote lies in a paragraph the document's class marks with
  `@boilerplate` (`quotes_in_boilerplate`). Those paragraphs are also left out
  of what the verdict is shown; a passage with too little left is skipped, and
  a candidate with nothing left is not sent;
- it is the best-supported finding for its passage and target document: the
  highest confidence, then the candidate's rank. The others are counted
  (`propose_capped`) and stay in the report's `findings`.

A proposal is always `proposed`, by `extractor:relate`, with the verdict's
confidence and both quotes as evidence. An operator's rejection stands: the
runtime refuses to file a rejected relation again.

### L5: check-against

`{"document_id": …, "check_against": ["/board/regulatory-external"]}` compares
each passage of an internal document with the norms it answers to. A finding
needs both quotes verbatim and outside boilerplate, like an L4 one. The findings
are written to a report document, one section per passage that has any, and the
document opens with a banner: the findings are a model's, checked only for their
quotes, and **not a legal opinion**. Nothing is filed in the statement graph. A
report pass compares and counts, and writes no document. A report name already
taken that day gets `-2`, `-3`, ….

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
6. **Semantics** (only with `rejudge: true`). A model-made statement
   (`extractor:*`) that passed 1–5, of a kind the relate vocabulary knows, is
   judged again by `kb/verdict` when its passage's or its target's body was
   rewritten since it was last verified (or filed). Nothing that did not change
   costs a model call.
   - The same kind, same direction, with quotes that occur: verified.
   - A kind the relate layer may file: proposed as a revision of it
     (`propose_statement` with its `id`), and the statement is flagged.
   - Anything else (none, an unquotable answer, a kind L4 may not file):
     flagged.

   A second opinion never expires or demotes a statement: a person decides. A
   person's statement, or one an operator ruled on, is never re-judged.

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
| `drain` | `queue: documents`, the `documents/ingested` queue (at `scope: user` or `tenant`); `queue: subjects`, the `memory/subjects` queue (at `scope: tenant`); up to `max_messages` | L0, L1, L2, L3, L6 per document message; L6 per subject message |
| `subject` | the subject's document (`document_id` or `path`), whose root is the subject | L6 |
| `sweep` | the statements with an end under `path` (a Path subtree) | L6, then the purge report |

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
- A document message names the scope it was published at. The bundle declares
  `documents/ingested` `publisher: system`, as it does `memory/subjects`: only
  the runtime writes it, and an agent's publish is refused even with a publish
  grant. So a message naming another scope than the drain's can only mean the
  deployment declares the channel at another scope than its drain reads. The
  drain stops there with `status: failed`, the message unacked
  (`scope_mismatch`), and `failure` says which declaration to change. A scope
  the runtime never publishes at is malformed (acked, never a stop), and a
  message from before the runtime named its scope is processed as usual.

Each queue is declared at one scope and names that scope's documents. `queue`
picks the queue; without it the scope does (`user` → `documents`, `tenant` →
`subjects`):
- `queue: documents` drains `documents/ingested`, the documents imports
  created or changed. The runtime publishes it in the document's own scope.
  The bundle declares it at `scope: user`, so `scope: user` reads a user's
  imports. A deployment whose documents live in the tenant scope re-declares
  it at `scope: tenant` (below) and drains it with
  `{"scope":"tenant","queue":"documents"}`.
- `queue: subjects` (at `scope: tenant` only) drains `memory/subjects`, the
  subjects the runtime announces when one is adopted, merged, re-typed, removed
  or erased. Adopted subjects live in the tenant scope; a merge is an operator's `POST /v1/_ontology/subjects/merge`
  (or its gRPC, MCP and client twins), which has already re-pointed the scope's
  statements by the time the message arrives. An `erased` message comes from
  erasing a user that a subject is bound to (`POST
  /v1/_ontology/subjects/principal`); it names the subject, never the user, and
  the subject is kept. Each message is one subject pass (L6 only):

| `op` | the pass |
|---|---|
| `adopted`, `retyped`, `erased` | checks the statements touching the subject |
| `merged` | re-points what remains on the duplicate to the survivor (`surviving_document_id`), then checks the survivor |
| `removed` | nothing: the scope's statements went with the document (`nothing_to_check`) |

A subject message is trusted for its ids only; the pass re-reads the subject and
its statements under the linker's own grants. `scope: tenant` needs the tenant
grants on `kb/linker` (see [Scope](#scope) below). `scope: agent` drains nothing and is
refused, as are `queue: subjects` at `scope: user` and `queue` outside drain
mode.

**Subject** checks every statement touching a subject, for a curator to run after
changing one. With `merged_into` (the surviving subject's document id), each
statement with the subject as an end is filed again with the survivor in its
place (same kind, evidence and date, `attributes.revision_of` the original) and
then expired; a person's statement, or one an operator ruled on, is flagged
instead. The other statements touching the subject get the ordinary checks.

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
  kb-linker-subjects:                 # the subject queue: needs the tenant grants
    agent: kb/linker
    schedule: "*/10 * * * *"
    concurrency_policy: forbid
    enabled: true
    prompt:
      - role: user
        content: [ { type: trusted-text, text: '{"mode":"drain","scope":"tenant","write":"propose"}' } ]
```

A deployment that imports into the tenant scope (every document under one
tenant, not per user) re-declares the ingest queue at tenant scope in its own
config layer, which merges over the bundle and keeps its bound and semantic, and
drains it with `queue: documents`. The drain needs the tenant grants on
`kb/linker` (see [Scope](#scope)):

```yaml
channels:
  documents/ingested:
    scope: tenant                     # the bundle declares user
scheduled_runs:
  kb-linker-ingest:
    agent: kb/linker
    schedule: "0 3 * * *"             # daily: import latency is up to a day
    concurrency_policy: forbid
    enabled: true
    prompt:
      - role: user
        content: [ { type: trusted-text, text: '{"mode":"drain","scope":"tenant","queue":"documents","write":"propose"}' } ]
```

Or as a runtime schedule, over MCP `scheduledef` (or `POST /v1/_scheduledef`):

```json
{"op": "create", "name": "kb-linker-ingest",
 "overlay": {"agent": "kb/linker", "schedule": "0 3 * * *", "concurrency_policy": "forbid", "enabled": true,
   "prompt": [{"role": "user", "content": [{"type": "trusted-text",
     "text": "{\"mode\":\"drain\",\"scope\":\"tenant\",\"queue\":\"documents\",\"write\":\"propose\"}"}]}]}}
```

A drain reads at most `max_messages` (default 10, up to 100) per run, so a
daily drain behind a busy import day needs a larger `max_messages` or a shorter
interval. Run it once with `write: report` first.

**Sweep** checks the statements with an end under `path`, asserted ones first,
each least recently verified first, up to `max_statements`; `sweep.exhausted`
says whether it reached the end. What it verifies moves to the back of the
order, so repeated sweeps work through a large subtree without a cursor to
carry. A statement already flagged for a person is passed over
(`skipped_flagged`) until they rule.

Every sweep then reports, under `purge`, the retired statements a purge may
delete: expired, made by code, retired more than `purge_after_days` ago (default
180). It deletes them only when the run says `purge: true` (with `write:
propose`); never from a schedule you did not write that way. A purge never
deletes a person's statement, one an operator ruled on, or a **rejected** one: a
rejection is what keeps the linker from proposing that relation again.

A sweep schedule is not installed by the bundle. To add one:

```yaml
scheduled_runs:
  kb-linker-sweep:
    agent: kb/linker
    schedule: "0 3 * * *"             # nightly
    user_id: me
    concurrency_policy: forbid
    enabled: true
    prompt:
      - role: user
        content: [ { type: trusted-text, text: '{"mode":"sweep","path":"/docs","scope":"user","write":"propose","max_statements":500}' } ]
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
| `mode` | `document` | `document`, `maintain`, `drain`, `subject` or `sweep` |
| `document_id` or `path` | exactly one (none in drain; sweep takes `path` only) | the document; for a sweep, the subtree |
| `scope` | `user` | `user`, `agent` or `tenant` (drain: `user` or `tenant`) |
| `queue` | by scope | drain only: `documents` (`documents/ingested`, at `user` or `tenant`) or `subjects` (`memory/subjects`, at `tenant`); `user` → `documents`, `tenant` → `subjects` |
| `layers` | per mode | any of `L0`, `L1`, `L2`, `L3`, `L4`, `L5`, `L6`; `maintain` takes `L6` only |
| `write` | `report` | `report` or `propose` |
| `terms_scheme` | none (L3 skips) | the concept scheme L3 matches |
| `as_of` | each part's own date | unix nanoseconds; citations resolve as of it |
| `max_writes` | `500` | cap on writes per run, also counted in report mode (1–5000) |
| `max_statements` | `500` | cap on statements L6 checks per document (1–5000) |
| `max_messages` | `10` | messages a drain processes (1–100) |
| `corpus_paths` | — | L4 only: where its candidates come from |
| `exclude_document_types`, `candidate_floor`, `max_candidates_per_chunk`, `max_verdict_calls`, `max_chars`, `min_body_chars`, `relations`, `include_quotes` | as before | L4 only. `min_body_chars` also bounds which passages L3 reads; `max_verdict_calls` and `relations` also bound and name what L6's `rejudge` asks |
| `rejudge` | `false` | L6 check 6: judge a model-made statement again when its text changed |
| `merged_into` | — | subject mode: the document of the subject this one was merged into |
| `purge` | `false` | sweep mode, with `write: propose`: delete what the purge report lists |
| `file` | — | `{"root": "/…"}`, with L0: file a document whose class is in force under `<root>/<Class>/` |
| `check_against` | — | L5: the Path-tree paths of the norms a passage is compared with |
| `max_norms`, `report_root` | `12`, `/reports/check-against` | L5 only: norms per passage (1–20); where the report documents go |
| `purge_after_days` | `180` | sweep mode: how long a statement stays retired before a purge may delete it (1–3650) |

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
- `queue` and `messages` (drain only): the queue read, and each message's
  outcome and whether it was acked. A document message carries its `reason`, a
  subject message its `op`. `scope_mismatch` is a document message of another
  scope, left unacked.

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

The linker makes no model call except in L4, L5 and L6's `rejudge`. L4 spends one
`kb/verdict` call per claim-bearing passage that has candidates, capped by
`max_verdict_calls`, and runs only when asked for; L5 spends one per passage
that has norms, and `rejudge` one per changed statement, each under the same
cap. Nothing is scheduled unless you add a schedule.
