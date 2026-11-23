# kbrag harness — Study C (ConditionalQA) setup

Standard library Python, run with `python3 -I` from the repo root. Needs `LOOMCYCLE_BENCH_TOKEN_CQA`
(a `substrate:tenant` bearer for tenant `conditional-qa`); `LC_BASE` overrides the deployment URL.
Working data lives in `/home/denn/workspace/kbrag-work/cqa/`.

| file | what it does |
|---|---|
| `lc.py` | shared client: `doc`, `run`, `tree_usage`, `chat`, `embed`, `version`, `spend`, provider policy (`disallowed`) |
| `setup.py` | tenant forks pinning kb/classify, kb/search, kb/screen, kb/walker, kb/annotate to `anthropic/claude-haiku-5-5` (`--target local` retires them; `--check` verifies) |
| `ingest.py` | imports the 652 pages (`header_md`) to `/cqa/<slug>`, checks each tree, writes `docmap.json` / `chunks.json` |
| `annotate.py` | drives `kb/annotator` (report, then `write: annotate` sweeps) with provider, budget and deadline guards |

Run order (single-line commands):

    python3 -I bench/docs/kbrag/setup.py
    python3 -I bench/docs/kbrag/setup.py --check
    python3 -I bench/docs/kbrag/ingest.py
    python3 -I bench/docs/kbrag/ingest.py --verify
    python3 -I bench/docs/kbrag/annotate.py --report-only
    nohup setsid python3 -I bench/docs/kbrag/annotate.py --deadline 2026-11-24T08:00:00Z --budget-usd 6 > /home/denn/workspace/kbrag-work/cqa/logs/annotate.log 2>&1 &
    python3 -I bench/docs/kbrag/annotate.py --status

Every run is started with `user_tier: "free"`, so an agent that is not pinned can only reach
`ollama-local`, never the paid fallback cascade. The annotation loop stops and writes
`annotate.STOP` when a run tree or the usage ledger shows a provider/model outside
`anthropic/claude-haiku-5-5`, `ollama-local` and the embedder, when tenant spend reaches
`--budget-usd`, or at `--deadline`. Stop it by hand with `kill <pid from annotate.pid>`
(a pass in flight on the server finishes on its own; the annotator's lease is released).
