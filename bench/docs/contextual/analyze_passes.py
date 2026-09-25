#!/usr/bin/env python3
"""Compare arms over SEVERAL reader passes.

One pass is not enough on a local reader: with retrieval held fixed, about 27 of 146
questions change score between passes (Ollama's prefix cache and request batching change
the arithmetic at temperature 0), and the mean moves by up to 0.02 F1 — the size of the
effects being compared. So each question's F1 is averaged over the passes first, and the
paired bootstrap then runs on those averages.

Also reports the pass-to-pass spread of each arm's mean, which is the reader's own noise
band, and retrieval recall on what the reader saw (deterministic across passes).
"""
import argparse
import collections
import json
import math
import os
import random


def mcnemar_exact(b, c):
    n = b + c
    if n == 0:
        return 1.0
    return min(1.0, 2 * sum(math.comb(n, i) for i in range(min(b, c) + 1)) / 2 ** n)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--probe", required=True)
    ap.add_argument("--passes", default="answers3a.jsonl,answers3c.jsonl,answers3d.jsonl")
    ap.add_argument("--baseline", default="header")
    ap.add_argument("--json-out")
    ap.add_argument("--metric", choices=["f1", "em"], default="f1", help="em needs rows that carry it (NQ runs)")
    a = ap.parse_args()
    passes = []
    for name in a.passes.split(","):
        d = collections.defaultdict(dict)
        for r in map(json.loads, open(os.path.join(a.probe, name))):
            d[r["arm"]][r["qid"]] = r
        passes.append(d)
    qids = sorted(json.loads(l)["qid"] for l in open(os.path.join(a.probe, "questions.jsonl")))
    pref = ["plain", "header", "both", "map", "map+hop", "desc", "mapctx", "mapall",
            "header+rr", "mapall+rr", "claims", "questions", "units", "units+rr"]
    present = [x for x in pref if all(x in p for p in passes)]
    arms = present + sorted(x for x in passes[0] if x not in present and all(x in p for p in passes))
    incomplete = [x for x in arms if any(len(p[x]) < len(qids) for p in passes)]
    if incomplete:
        print("⚠️ incomplete arms (a pass is missing questions):", incomplete)

    def per_q(arm):
        return {q: sum(p[arm][q][a.metric] for p in passes) / len(passes) for q in qids}

    base = per_q(a.baseline)
    rng = random.Random(7)
    out = {}
    print("passes=%d n=%d baseline=%s metric=%s" % (len(passes), len(qids), a.baseline, a.metric))
    print("%-8s %6s  %-13s  %-18s %9s   %s" % ("arm", a.metric.upper(), "pass means", "Δ vs base 95%CI", "+/-", "R@1/3/5/10 (reader view)"))
    for arm in arms:
        m = per_q(arm)
        mean = sum(m.values()) / len(qids)
        pm = [sum(p[arm][q][a.metric] for q in qids) / len(qids) for p in passes]
        diff = [m[q] - base[q] for q in qids]
        boots = sorted(sum(diff[rng.randrange(len(diff))] for _ in diff) / len(diff) for _ in range(4000))
        lo, hi = boots[100], boots[3899]
        w, l = sum(x > 1e-9 for x in diff), sum(x < -1e-9 for x in diff)
        first = passes[0][arm]
        rec = "/".join("%.3f" % (sum(1 for q in qids if (first[q]["ev_rank"] or 99) <= k) / len(qids)) for k in (1, 3, 5, 10))
        if arm != a.baseline:
            # the pre-registered retrieval test: R@5 on pass a, paired, exact McNemar
            bfirst = passes[0][a.baseline]
            bw = sum(1 for q in qids if (first[q]["ev_rank"] or 99) <= 5 < (bfirst[q]["ev_rank"] or 99))
            bl = sum(1 for q in qids if (bfirst[q]["ev_rank"] or 99) <= 5 < (first[q]["ev_rank"] or 99))
            rec += "  R@5 +%d/-%d p=%.2g" % (bw, bl, mcnemar_exact(bw, bl))
        ci = "" if arm == a.baseline else "[%+.3f, %+.3f]" % (lo, hi)
        wl = "" if arm == a.baseline else "+%d/-%d" % (w, l)
        print("%-8s %6.3f  %-13s  %-18s %9s   %s" % (arm, mean, "/".join("%.3f" % x for x in pm), ci, wl, rec))
        out[arm] = {"f1": mean, "pass_means": pm, "ci_vs_base": [lo, hi], "wins": w, "losses": l, "recall_reader_view": rec}
    if a.json_out:
        json.dump(out, open(a.json_out, "w"), indent=1)


if __name__ == "__main__":
    main()
