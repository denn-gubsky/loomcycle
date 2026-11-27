"""The judge: each raw answer rated CORRECT / PARTLY / WRONG against the gold answers.

    python3 -I bench/docs/kbrag/judge.py [--arms kb,rag-lc,rag-ext,oracle]

-> <work>/judge/<arm>.jsonl {arm, qid, rep, attempt, verdict, judge_text, order, ...}

The judge (config.JUDGE; the reader model since Amendment 1) sees the question, the gold answers and ONE
answer with the accounting lines, links and reader scaffold removed (common.strip_for_judge),
so its text does not say which arm wrote it. Per (question, rep) the arms are judged in an
order shuffled by a seeded RNG, so the order carries no arm signal either.
"""
import argparse, json, os, random, sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import common, config, passes, prompts  # noqa: E402


def judge_call(lc, data, pair=config.JUDGE):
    def call(src, usage):
        q = data.qs[src["qid"]]
        prompt = prompts.JUDGE.format(question=prompts.question_text(q), gold=json.dumps(q["answers"], ensure_ascii=False),
                                      answer=common.strip_for_judge(src["raw_answer"]) or "(empty answer)")
        out, err, served = prompts.ask(lc, prompt, pair, usage, max_tokens=4096)
        v = prompts.verdict(out)
        if v is None:
            return {"status": "failed", "error": "judge: %s" % (err or "no verdict in reply"), "judge_text": out}
        return {"verdict": v, "judge_text": out, "judge_served": list(served)}
    return call


def schedule(by_arm):
    """[(arm, key, src)] grouped by (qid, rep), arms in a seeded shuffled order per group."""
    groups = {}
    for arm, runs in by_arm.items():
        for (qid, rep), r in runs.items():
            groups.setdefault((qid, rep), []).append((arm, r))
    out = []
    for (qid, rep) in sorted(groups):
        g = sorted(groups[(qid, rep)], key=lambda x: x[0])
        random.Random("judge:%s:%d" % (qid, rep)).shuffle(g)
        for pos, (arm, r) in enumerate(g):
            out.append((arm, pos, r))
    return out


def items_and_order(by_arm):
    """The judge's work list in schedule order, and each answer's position in its group."""
    items, order = [], {}
    for arm, pos, r in schedule(by_arm):
        key = (r["qid"], r["rep"], r["attempt"])
        items.append((key, r))
        order[(arm,) + key] = pos
    return items, order


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--arms", default="kb,rag-lc,rag-ext,oracle")
    ap.add_argument("--budget-usd", type=float, default=config.BUDGET_USD)
    ap.add_argument("--out", default=config.WORK, help="root holding runs/; writes <out>/judge (default %(default)s)")
    ap.add_argument("--workers", type=int, default=1,
                    help="calls in flight (default 1: the judge is one local GPU; more only queues there)")
    a = ap.parse_args()
    data = common.Data()
    import lc
    by_arm = {arm: passes.latest_ok_runs(arm, os.path.join(a.out, "runs")) for arm in a.arms.split(",")}
    call = judge_call(lc, data)
    items, order = items_and_order(by_arm)

    def with_pos(src, usage):
        return dict(call(src, usage), order=order[(src["arm"], src["qid"], src["rep"], src["attempt"])])
    try:
        n = passes.run_pass(lc, items, with_pos, lambda src: os.path.join(a.out, "judge", src["arm"] + ".jsonl"),
                            budget_usd=a.budget_usd, log=lambda s: print(s, flush=True), workers=a.workers)
    except passes.Stop as e:
        print("STOPPED: %s" % e, flush=True)
        sys.exit(2)
    print("judged %d new" % n, flush=True)


if __name__ == "__main__":
    main()
