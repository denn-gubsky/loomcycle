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
