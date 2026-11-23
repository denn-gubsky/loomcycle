# kbrag harness — Study D (the loomcycle development tenant)

Standard library Python, run with `python3 -I` from the repo root. Needs `LOOMCYCLE_DEV_TOKEN`
(a `substrate:tenant` bearer for tenant `loomcycle-dev`; every script refuses another tenant);
`LC_DEV_BASE` overrides the deployment URL. Working data — exported text, gold, runs — lives in
`/home/denn/workspace/kbrag-work/docs/` (`KBRAG_DOCS_WORK`) and never enters the repo. The Study C
files in `..` are imported read-only, never edited (`../common.py`, `../ragext.py`,
`../prompts.py`, `../passes.py`, `../judge.py`, `../analyze.py`); the gold parser and topic
rules are `scripts/measure/walker/drive.py`'s. PREREG Amendment 2 fixes the design.

| file | what it does |
|---|---|
| `dlc.py` | client: a copy of `../lc.py` pointed at this tenant (`assert_tenant`) |
| `dconfig.py` | paths, arms, k = 20, corpus rule, budget |
| `inventory.py` | the tenant's documents and the corpus (409) |
| `export.py` | `export_md` of every corpus document → sections; chunk → document map |
| `ragext_docs.py` | rag-ext over this corpus (`../ragext.py` + this corpus's prompts); `build` embeds |
| `known_items.py` | the mechanical known-item set (seeded draw, Haiku writes, Haiku checks) |
| `questions.py` | loads the `p7`, `waves` and `known` sets from gold files outside the repo |
| `forks.py` | record / fork kb agents to Haiku (names without a tenant overlay only) / check / restore |
| `register.py` | the rag-lc helper `kbrag/search-rr` (`agents/search-rr.json`): create / show / retire |
| `darms.py` | runs the arms; spend, deadline and provider guards; resumable |
| `djudge.py` | the blind Haiku judge |
| `danalyze.py` | scores, H1/H2, instrument checks, cost → `<root>/summary.json` |
| `probe.py`, `gold_prep.py` | read-only listings used while looking for gold |

Run order (single-line commands):

    python3 -I bench/docs/kbrag/docs/inventory.py
    python3 -I bench/docs/kbrag/docs/export.py
    python3 -I bench/docs/kbrag/docs/ragext_docs.py build
    python3 -I bench/docs/kbrag/docs/known_items.py --n 30
    python3 -I bench/docs/kbrag/docs/forks.py --record
    python3 -I bench/docs/kbrag/docs/forks.py --apply
    python3 -I bench/docs/kbrag/docs/register.py create
    python3 -I bench/docs/kbrag/docs/darms.py --out /home/denn/workspace/kbrag-work/docs/study --sets p7 --reps 3 --workers 4
    python3 -I bench/docs/kbrag/docs/darms.py --out /home/denn/workspace/kbrag-work/docs/study --sets waves,known --reps 1 --workers 4
    python3 -I bench/docs/kbrag/docs/darms.py --out /home/denn/workspace/kbrag-work/docs/study --sets p7 --reps 3 --retry-failed
    python3 -I bench/docs/kbrag/docs/forks.py --restore
    python3 -I bench/docs/kbrag/docs/register.py retire
    python3 -I bench/docs/kbrag/docs/forks.py --check
    python3 -I bench/docs/kbrag/docs/djudge.py --out /home/denn/workspace/kbrag-work/docs/study
    python3 -I bench/docs/kbrag/docs/danalyze.py --out /home/denn/workspace/kbrag-work/docs/study

**Restore, if a run died and left the forks active** (they shadow the bundle agents for every
run in the tenant): `forks.py --restore` retires every def this harness made (logged in
`<work>/forks.json`) and checks that each name resolves as before; `register.py retire` retires
the helper. Both are idempotent.

Tests (offline): `python3 -I -m unittest discover -s bench/docs/kbrag/docs/tests`
