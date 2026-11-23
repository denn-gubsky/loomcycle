"""Study D judge: each ok answer rated COMPLETE / PARTIAL / WRONG against a description of the gold.

    python3 -I bench/docs/kbrag/docs/djudge.py --out <root> [--sets p7,waves,known] [--workers 4]

-> <root>/judge/<arm>.jsonl {arm, qid, rep, attempt, verdict, judge_text, order, ...}

The judge (dconfig.JUDGE: Haiku, the reader model, per Amendment 1 and the operator) sees the
question, the gold described (each gold document's title and brief, or the gold topic names,
plus the expected answer text where the gold has one) and ONE answer with the accounting
lines, links and SOURCES line removed (../common.strip_for_judge). Per (question, rep) the
arms are judged in an order shuffled by a seeded RNG (../judge.items_and_order). Secondary
measure only. The spend cap is the tenant ledger's increase over the Study D baseline.
"""
import argparse, json, os, sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(1, os.path.dirname(HERE))
import dconfig, dprompts, questions  # noqa: E402
import common, passes  # noqa: E402
from judge import items_and_order  # noqa: E402  (../judge.py: the seeded shuffled order)


class Increase:
    """The client passes.run_pass needs, with spend() as the increase over the baseline."""

    def __init__(self, lc, base):
        self.lc, self.base = lc, base

    def spend(self):
        return self.lc.spend() - self.base


def describe(q, inv):
    if q["gold_topics"]:
        return dprompts.JUDGE_TOPICS, "\n".join("- " + t["name"] for t in q["gold_topics"])
    lines = []
    for d in q["gold"]:
        x = inv.get(d) or {}
        lines.append("- %s: %s" % (x.get("title") or "(untitled)", x.get("brief") or "(no summary)"))
    if q.get("expected_answer"):
        lines.append("Expected answer: %s" % q["expected_answer"])
    return dprompts.JUDGE_DOCS, "\n".join(lines) or "(the reference says the documentation does not answer this)"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--sets", default="p7,waves,known")
    ap.add_argument("--workers", type=int, default=4)
    ap.add_argument("--budget-usd", type=float, default=dconfig.BUDGET_USD)
    a = ap.parse_args()
    import dlc
    dlc.assert_tenant()
    base = json.load(open(os.path.join(dconfig.WORK, "baseline.json")))["tenant_spend_usd"]
    qs = questions.load([s for s in a.sets.split(",") if s])
    inv = {d["id"]: d for d in json.load(open(dconfig.INVENTORY))["documents"]}
    by_arm = {arm: {k: r for k, r in passes.latest_ok_runs(arm, os.path.join(a.out, "runs")).items() if k[0] in qs}
              for arm in dconfig.ARMS}
    items, order = items_and_order(by_arm)

    def call(src, usage):
        q = qs[src["qid"]]
        tmpl, gold = describe(q, inv)
        prompt = tmpl.format(question=q["text"], gold=gold,
                             answer=common.strip_for_judge(src.get("raw_answer")) or "(empty answer)")
        out, err, served = dprompts.ask(dlc, prompt, dconfig.JUDGE, usage, max_tokens=4096)
        v = dprompts.verdict(out)
        row = {"order": order[(src["arm"], src["qid"], src["rep"], src["attempt"])], "set": q["set"]}
        if v is None:
            return dict(row, status="failed", error="judge: %s" % (err or "no verdict in reply"), judge_text=out)
        return dict(row, verdict=v, judge_text=out, judge_served=list(served))

    try:
        n = passes.run_pass(Increase(dlc, base), items, call, lambda src: os.path.join(a.out, "judge", src["arm"] + ".jsonl"),
                            budget_usd=a.budget_usd, log=lambda s: print(s, flush=True), workers=a.workers)
    except passes.Stop as e:
        print("STOPPED: %s" % e, flush=True)
        sys.exit(2)
    print("judged %d new" % n, flush=True)


if __name__ == "__main__":
    main()
