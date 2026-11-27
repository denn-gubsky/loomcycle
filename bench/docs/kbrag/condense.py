"""The condense step: every arm's raw answer -> the shortest span that states it.

    python3 -I bench/docs/kbrag/condense.py --profile haiku [--arms kb,rag-lc,rag-ext,oracle]

-> <work>/condensed/<arm>.jsonl {arm, qid, rep, attempt, condensed, direct, ...}

One fixed call per answer (the reader model of the profile, temperature 0) that sees only
the question and the answer — kb's with its accounting lines removed — and no document.
`direct` is the reader's own ANSWER: line, scored directly for instrument check 7.
Resumable; a failed row is retried on the next invocation.
"""
import argparse, os, sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import common, config, passes, prompts  # noqa: E402


def condense_call(lc, data, pair):
    def call(src, usage):
        q = data.qs[src["qid"]]
        answer = common.strip_trailer(src["raw_answer"]) if src["arm"] == "kb" else src["raw_answer"]
        out, err, served = prompts.ask(lc, prompts.CONDENSE.format(question=prompts.question_text(q), answer=answer),
                                       pair, usage, max_tokens=1024)
        if out is None:
            return {"status": "failed", "error": "condense: %s" % err}
        # A model may still wrap the span in quotes or a label; keep its first line only.
        span = out.strip().splitlines()[0].strip().strip('"').strip()
        return {"condensed": span, "direct": src.get("answer_line"), "condense_served": list(served)}
    return call


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--profile", default=config.DEFAULT_PROFILE, choices=sorted(config.PROFILES))
    ap.add_argument("--arms", default="kb,rag-lc,rag-ext,oracle")
    ap.add_argument("--budget-usd", type=float, default=config.BUDGET_USD)
    ap.add_argument("--out", default=config.WORK, help="root holding runs/; writes <out>/condensed (default %(default)s)")
    ap.add_argument("--workers", type=int, default=1, help="calls in flight at once (threads; default 1)")
    a = ap.parse_args()
    pair = config.reader(a.profile)
    data = common.Data()
    import lc
    for arm in a.arms.split(","):
        runs = passes.latest_ok_runs(arm, os.path.join(a.out, "runs"))
        other = {r.get("profile") for r in runs.values()} - {a.profile}
        if other:
            raise SystemExit("%s runs were made under profile %s; condense with that profile" % (arm, other))
        items = [((r["qid"], r["rep"], r["attempt"]), r) for _, r in sorted(runs.items())]
        try:
            n = passes.run_pass(lc, items, condense_call(lc, data, pair), os.path.join(a.out, "condensed", arm + ".jsonl"),
                                budget_usd=a.budget_usd, log=lambda s: print(s, flush=True), workers=a.workers)
        except passes.Stop as e:
            print("STOPPED: %s" % e, flush=True)
            sys.exit(2)
        print("condensed %s: %d new" % (arm, n), flush=True)


if __name__ == "__main__":
    main()
