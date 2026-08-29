# wikidata bench — knowledge updates, ontology typing, cross-lingual recall

Three small harnesses that drive an externally-running loomcycle instance and score
memory subsystems against **Wikidata** as ground truth. Companion to `bench/cmd/locomo`,
which scores conversational retrieval; this scores the axes LoCoMo cannot reach.

Written for RFC CO. The results below are what it produced on v1.66.0 — including one
phase whose result was "do not build this", which is why the harness exists.

## Why Wikidata

Ground truth is free and machine-checkable. `P580` / `P582` (start/end time) map 1:1
onto `valid_at` / `invalid_at`; `P31` (instance-of) is a published type hierarchy; and
one item carries labels for the SAME identity in hundreds of languages, so a
cross-lingual test needs no translation and no answer-string matching.

It is **CC0**, which is why the fixtures under `fixtures/` are committed. LoCoMo's are
not, and cannot be — that corpus is CC BY-NC.

## Layout

    build_fixture.py    -> fixtures/chains.json     officeholder chains (CO-1)
    build_types.py      -> fixtures/types.json      entities by P31 class (CO-3)
    build_xlingual.py   -> fixtures/xlingual.json   multi-language labels (CO-4)

    build_facts.py      -> a bulk fact corpus (NOT committed — see below)
    import_facts.py        bulk-load that corpus into a loomcycle memory scope

    run_co1.py   knowledge updates: does a superseded fact stop being returned
    run_co3.py   ontology typing:   does an entity get the right base type
    run_co4.py   cross-lingual:     English corpus, non-English query

    answerer_prompt.txt / typer_prompt.txt   the agent prompts the runs install

Run outputs are NOT committed — `bench/results/` is gitignored and these follow the
same rule. Fixtures are inputs, like `bench/cases/*.yaml`.

## Running

Needs a tenant token in the environment and an instance to talk to:

    export WIKI_BENCH_TENANT_TOKEN=...          # a substrate:tenant token
    python3 run_co3.py fixtures/types.json     co3.json
    python3 run_co4.py fixtures/xlingual.json  co4.json
    python3 run_co1.py fixtures/chains.json 20 co1.json

Rebuild a fixture only when you want fresh data — they are deterministic from a fixed
class list and committed so a re-run scores the same corpus.

### Two things that will bite you

**The memory scope must be the token's SUBJECT.** In-band `scope=user` is resolved
server-side from the run identity, never from the wire, so a row written to any other
`scope_id` is invisible to the agent. `run_co1.py` reads it from `/v1/_me` rather than
choosing one — an earlier version chose `wiki-co1`, and every arm reported NOT_FOUND,
which reads as a memory failure and is not one.

**Read an AgentDef back after authoring it.** The mutable field set goes under
`overlay`; any other key (`def`, say) is accepted with a 200, a def_id, and an empty
definition. A run against that agent measures nothing and says so in no way at all.

## Bulk fact corpus, for testing models against a store that holds something

`build_facts.py` renders twelve Wikidata properties into self-contained English
sentences — *"Marie Curie was born on 1867-11-07"*, not triples, because the sentence
is what gets embedded and what a model reads back. `import_facts.py` loads them.

    python3 build_facts.py facts.json 1000        # ~8.5k facts, a few minutes
    python3 import_facts.py facts.json            # writes + embeds

**The corpus itself is NOT committed.** At ~1.3 MB it is an order of magnitude past the
other fixtures, and it is regenerable. Keep your own copy if you need a specific run to
be reproducible — see the caveat below.

### Two things this got wrong first, so you do not have to

**Sample across the index, or the corpus is degenerate.** QLever returns rows in index
order, grouped by object, so a single `LIMIT 1000` query grabs one contiguous block. A
first build produced **809 of 1000 "is located in Gabon"** and **810 of 996
"headquartered in Boston"**. That corpus is useless for retrieval testing — most queries
would have hundreds of equally good answers, and a model would score well or badly for
reasons having nothing to do with memory. `OFFSETS` spreads the sampling geometrically
and short-circuits on an empty chunk, because these properties differ in size by orders
of magnitude. Top-object share after the fix: 0.3%–17.6%.

**Bulk does not go through `Memory op=add`.** That is one LLM call per span and a
five-figure token bill at this size. Bulk is the off-run PUT with `embed: false`, then
`/v1/_memory/backfill_embeddings`, which **bounds by work DONE rather than rows seen**
— so it is driven in a loop until it reports nothing left. A single call silently leaves
most of the corpus unembedded, and an unembedded row is invisible to recall, which later
looks exactly like a memory failure. That endpoint also takes **query params, not a JSON
body**, and `dry_run` defaults to **true**.

### Caveat: regenerable, not byte-stable

A rebuild will not reproduce the same corpus. The builder is deterministic in its
property list and offsets, but QLever's index order shifts as Wikidata changes, so the
rows behind a given offset drift. If a number has to be comparable across time, keep the
`facts.json` that produced it.

Reference figures from the first build (2026-08-29): **8,541 facts**, 22s to write,
8,541 embedded over 19 backfill rounds with `bge-m3` (1024-dim, ~35 MB of vectors).

Expect genuine obscurity in the long tail — *"S is headquartered in Tokyo"* is a real
row. For memory testing that is arguably a feature: a model cannot answer from training
data, so a correct answer is attributable to retrieval. RFC CO-1's control arm exists
because the opposite case — famous facts the model already knows — silently inflates a
memory score.

## Results on v1.66.0 (RFC CO)

| phase | result |
|---|---|
| CO-1 knowledge updates | **stale-answer rate 0/20**; naive corpus 20/20 — the gate closed CO-2 |
| CO-3 ontology typing | **63/64 = 0.9844** |
| CO-4 cross-lingual | **0.976–1.000** recall@10 across en/de/fr/uk/ru/ja/ar |

**Read the caveats before quoting these.**

CO-1's control arm answered **14 of 20 from training alone**, on an empty scope. Only
the six the model did not know are attributable to memory. Any run of this harness must
keep the control, or it reports the model's own knowledge as a retrieval score.

CO-3 and CO-4 are measured on the **easy region** of their tasks by construction: CO-3
uses only `P31` values whose base type is unambiguous, CO-4 queries entity labels rather
than prose. They establish that the subsystems are not broken on clear cases; they do
not establish general accuracy.

## Wikidata access notes

- **QLever, not WDQS**, for any set-selection query. `qlever.cs.uni-freiburg.de`
  308-redirects to **`qlever.dev`**; it wants `Accept: application/sparql-results+json`,
  has no `wikibase:label` service (use `rdfs:label` + a `LANG` filter), and combining
  `wikibase:sitelinks` with `rdfs:label` and `schema:description` in one pattern returns
  zero rows.
- **WDQS will cut you off.** It began returning `429 Aggressively rate-limiting to
  1 req / min — active wdqs outage` after ~345 queries for one fixture. The Action API
  stayed up throughout and is the right tool for id-keyed lookups (50 ids per call).
- **Be polite**: contactable `User-Agent`, serial requests, exponential backoff. These
  builders do all three.
- **The data is dirty.** "Free ground truth" is true of the schema, not the data. A raw
  officeholder pull contained vandalism ("Phil Baker, President of the United States"),
  the same person repeated from re-election statements, and a generic `president` item
  holding unrelated people. `build_fixture.py`'s structural filter is not optional, and
  it rejects most offices: twenty hand-picked QIDs yielded two usable chains, while
  candidates discovered from `P1313`/`P1906` yielded 241 of 345.
