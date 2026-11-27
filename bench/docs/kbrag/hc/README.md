# kbrag/hc: the RFC HC gate study harness

This harness measures the first call (`kb/answer` + `kb/read` inside `kb/ask`) against today's
`kb/ask` and passage RAG, under `../PREREG-HC.md` (a draft until its Amendment 0). It is
written in standard library Python. Run every script with `python3 -I` from the repo root. It
extends the benchmark harness one directory up. It reuses:
- the condense call, the scoring, the row usage and the bootstrap;
- the resumable passes and the annotator loop;
- Study D's cited-document P/R/F1 and its key rule.

## Environment

The harness reads variable names only; it never prints, logs or writes a value.

| variable | what |
|---|---|
| `KBRAG_HC_WORK` | the work directory, outside the repo |
| `KBRAG_HC_CQA_BASE`, `KBRAG_HC_CQA_TOKEN`, `KBRAG_HC_CQA_TENANT` | the ConditionalQA tenant: base URL, bearer, tenant name (the bearer must resolve to it) |
| `KBRAG_HC_DEV_BASE`, `KBRAG_HC_DEV_TOKEN`, `KBRAG_HC_DEV_TENANT` | the same for the development tenant (p7) |
| `KBRAG_P7_GOLD` | the p7 gold file, outside the repo |
| `KBRAG_HC_AF_ASK_AGENT`, `KBRAG_HC_AF_SEARCH_AGENT` | the answer-first copies' names (default `kbhc/ask-af`, `kbhc/search-af`) |

## Files

| file | does |
|---|---|
| `hcconfig.py` | arms, sets, thresholds, seeds, environment names |
| `client.py` | the loomcycle calls of `../lc.py`, bound per tenant (`segments`, `run_events` added) |
| `fetch_cqa.py`, `splits.py`, `splits.json` | the public data and the frozen split (test = the 800, dev = the other 1,715) |
| `corpus.py` | check 1 and the corpus filter (operational types, untyped `/memory` documents, the ontology) |
| `coverage.py` | check 2 over that filtered list |
| `sweep.py` | the annotator sweep before the run (`../annotate.py`'s loop, bound to a tenant) |
| `precheck.py` | writes `corpus.json` and `coverage.json` per set |
| `tenant_state.py` | records and checks the watched agents' defs, and that each is pinned to Haiku |
| `spendguard.py` | the baseline, plus the $19 stop on both ledgers summed |
| `cost.py` | the price model's `cost()`, and the server's usage keys mapped onto it |
| `hcarms.py` | the arms and the runner |
| `hccondense.py` | the condense pass |
| `hcanalyze.py` | T1–T3, the cost checks, the nine checks, the gate |

## Order

1. `spendguard.py --record`
2. `sweep.py --set cqa|p7`
3. `precheck.py --set cqa --expected <work>/cqa/docmap.json`, then `precheck.py --set p7`
4. Create the P1 tenant defs, then `tenant_state.py --set cqa|p7 --record`
5. Tune on the dev split (`hcarms.py --set cqa --split dev --out <work>/tune ...`)
6. Commit Amendment 0
7. `hcarms.py --set cqa --out <work>/study`, then `hcarms.py --set p7 --out <work>/study`
8. `hccondense.py --out <work>/study`
9. `hcanalyze.py --out <work>/study`

The ConditionalQA set also needs, under `<work>/cqa/`, the benchmark's ingest maps
(`docmap.json`, `chunks.json`) and `prepared/` (`bench/docs/measure/prepare.py` over the
`fetch_cqa.py` files).

## Tests (offline)

    python3 -I -m unittest discover -s bench/docs/kbrag/hc/tests
