# nodestats — RFC GM P0 measurement

A standalone program (not part of the `loomcycle` binary) that measures one
taxonomy scheme of one tenant against that tenant's annotation vectors, from a
restored database dump. It uses `internal/memory/nodestats`, the same package
the runtime pass will use.

## Run

Restore the runtime database (`memory`, `memory_embeddings`) and the SQL Memory
database, then:

```sh
export GM_P0_MAIN_DSN=postgres:///<runtime db>
export GM_P0_SQLMEM_DSN=postgres:///<sqlmem db>
go run ./scripts/measure/nodestats \
  --tenant <tenant id> \
  --scheme-root <root chunk id of the scheme> \
  --embed-url http://<ollama host>:11434/api/embed \
  --out <directory outside the repo>
```

It writes `nodestats.json` and `nodestats.md` into `--out`. `--help` lists the
other flags (current prompt version, permutation count, split seeds, misfit
margins, statement kinds, memory scope ids). The document schema is found in
`sqlmem_meta` unless `--doc-schema` names it. If the embedder cannot be reached
the definition baseline is skipped and the report says so.

## What it measures

0. Data quality per node: members, with and without a document vector, and the
   share without a current-prompt annotation (over 10% is "not gradable").
1. Leave-one-out agreement of the declared placement with the nearest centroid,
   at the top level (L1) and under each parent (L2), top-1 and top-3, in three
   arms (full, sections as units, 1/k weights), against a labels-permuted and a
   definition-only baseline, and per statement provenance.
4. Grading material per node: count, spread, exemplars, nearest siblings,
   distinctive terms.
5. Split candidates (nodes of 20 or more) over seeds, and the unplaced
   documents grouped the same way.
6. Date source shares and usable monthly periods per node.
Also misfit counts at several margins.

The output names documents by title and quotes their briefs: keep it out of the
repository.
