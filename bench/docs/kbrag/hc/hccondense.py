"""The condense step for the ConditionalQA rows: ../condense.py's fixed call, unchanged.

    python3 -I bench/docs/kbrag/hc/hccondense.py --out <work>/study [--arms kb-today,kb-first,...]

-> <out>/cqa/condensed/<arm>.jsonl. Every arm's raw answer, with its accounting lines removed
(kb/ask's ROUTE trailer and kb/answer's FIRST fields alike), goes through one Haiku call that
sees only the question and that answer and returns the shortest span, or NO ANSWER. `direct`
is the oracle's own ANSWER: line, scored directly for instrument check 7. The spend guard is
the study-wide one (both tenants' ledger increase against $19).
"""
import argparse, os, sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(1, os.path.dirname(HERE))
import hcarms, hcconfig  # noqa: E402
import condense, passes  # noqa: E402  (../condense.py, ../passes.py)


def hc_condense_call(lc, data, pair=hcconfig.HAIKU):
    """../condense.condense_call over an answer already stripped of the HC trailer fields
    (handed on as a `kb` row, so condense.py strips kb/ask's lines once more, a no-op)."""
    inner = condense.condense_call(lc, data, pair)

    def call(src, usage):
        out = inner(dict(src, arm="kb", raw_answer=hcarms.strip_trailer(src.get("raw_answer"))), usage)
        if "condensed" in out:
            out["direct"] = src.get("answer_line")
        return out
    return call


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--arms", default=",".join(hcconfig.ARMS_FOR["cqa"]))
    ap.add_argument("--budget-usd", type=float, default=hcconfig.BUDGET_USD)
    ap.add_argument("--workers", type=int, default=4)
    a = ap.parse_args()
    import spendguard
    clients = spendguard.all_clients()
    for c in clients.values():
        c.assert_tenant()
    view = spendguard.View(clients["cqa"], spendguard.Spend(clients, spendguard.load_baseline()))
    data = hcarms.load_set("cqa", hcconfig.work()).cqa
    for arm in a.arms.split(","):
        runs = passes.latest_ok_runs(arm, os.path.join(a.out, "cqa", "runs"))
        items = [((r["qid"], r["rep"], r["attempt"]), r) for _, r in sorted(runs.items())]
        try:
            n = passes.run_pass(view, items, hc_condense_call(view, data),
                                os.path.join(a.out, "cqa", "condensed", arm + ".jsonl"), budget_usd=a.budget_usd,
                                log=lambda s: print(s, flush=True), workers=a.workers)
        except passes.Stop as e:
            print("STOPPED: %s" % e, flush=True)
            sys.exit(2)
        print("condensed %s: %d new" % (arm, n), flush=True)


if __name__ == "__main__":
    main()
