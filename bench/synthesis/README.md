# RFC DB-1 — the cross-session synthesis feasibility rig

Ran 2026-09-15. **Oracle 20/20, no-memory 0/20** (every one an explicit `NOT_FOUND`).
Full write-up: RFC DB §9a in the document store (`/loomcycle/rfcs/cross-session-synthesis-benchmark`).

There is deliberately **no store and no memory** here. The oracle arm puts the
supporting facts in the prompt and the control arm gives nothing, so the two arms
cannot differ by anything except the facts. A store would only add a way for them to.

    ./run-server.sh <dir>            # dir holds db1.yaml; sources .env.local for the provider key
    python3 run_arms.py              # both arms + grading -> results.json
    python3 judge_control.py         # negative control: the judge must say WRONG on a decoy gold

⚠️ `deepseek-v4-flash` is a hybrid thinking model — it spends the reasoning trace
before the first visible token, so a tight `max_tokens` truncates a reply to the
EMPTY STRING rather than shortening it. An empty reply is an instrument fault, not
a verdict; `run()` retries and then reports it rather than bucketing it.
