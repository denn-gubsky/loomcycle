#!/usr/bin/env python3
"""Summarise retrieval.jsonl: recall@k, MRR, and each arm's paired difference from `plain`.

The comparison is PAIRED — every arm answers the same questions — so the test is an exact
McNemar on the discordant pairs at a fixed k, not two independent proportions. That is
what makes 146 questions enough to see a real effect: agreement is common, and only the
questions an arm flips count.
"""
import argparse
import json
import math
import os
from collections import defaultdict

KS = (1, 3, 5, 10, 20)


def mcnemar_exact(b, c):
    # Two-sided exact binomial test on the discordant pairs.
    n = b + c
    if n == 0:
        return 1.0
    k = min(b, c)
    p = sum(math.comb(n, i) for i in range(k + 1)) / 2 ** n
    return min(1.0, 2 * p)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--probe", required=True)
    ap.add_argument("--baseline", default="plain")
    ap.add_argument("--json-out")
    ap.add_argument("--rank", default="ev_rank", choices=["ev_rank", "ev_rank_content"],
                    help="ev_rank_content ignores bodyless chunks (document roots, empty parent headings)")
    a = ap.parse_args()
    rows = [json.loads(l) for l in open(os.path.join(a.probe, "retrieval.jsonl"))]
    by = defaultdict(dict)  # (layout, retriever, arm) -> qid -> row
    for r in rows:
        by[(r["layout"], r["retriever"], r["arm"])][r["qid"]] = r
    arms_order = ["plain", "header", "context", "both"]
    summary = {}
    for layout in ("pool", "paper"):
        for ret in ("hybrid", "vector"):
            print(f"\n== layout={layout} retriever={ret}")
            print("arm       n    " + "  ".join(f"R@{k:<3}" for k in KS) + "   MRR   paper@1" + "   Δ@5 vs base (p)   Δ@10 (p)")
            base = by.get((layout, ret, a.baseline), {})
            for arm in arms_order:
                d = by.get((layout, ret, arm))
                if not d:
                    continue
                n = len(d)
                rec = {k: sum(1 for r in d.values() if r[a.rank] and r[a.rank] <= k) / n for k in KS}
                mrr = sum(1 / r[a.rank] for r in d.values() if r[a.rank]) / n
                p1 = sum(1 for r in d.values() if r["paper_rank"] == 1) / n
                deltas = {}
                for k in (5, 10):
                    common = [q for q in d if q in base]
                    b = sum(1 for q in common if (d[q][a.rank] or 99) <= k < (base[q][a.rank] or 99))
                    c = sum(1 for q in common if (base[q][a.rank] or 99) <= k < (d[q][a.rank] or 99))
                    deltas[k] = ((b - c) / max(1, len(common)), mcnemar_exact(b, c), b, c)
                line = f"{arm:<8} {n:4}  " + "  ".join(f"{rec[k]:.3f}" for k in KS) + f"  {mrr:.3f}  {p1:.3f}"
                if arm != a.baseline:
                    line += "   %+.3f (p=%.3g, +%d/-%d)   %+.3f (p=%.3g)" % (
                        deltas[5][0], deltas[5][1], deltas[5][2], deltas[5][3], deltas[10][0], deltas[10][1])
                print(line)
                summary[f"{layout}/{ret}/{arm}"] = {"n": n, "recall": rec, "mrr": mrr, "paper_at_1": p1,
                                                    "delta_vs_base": {str(k): {"delta": v[0], "p": v[1], "wins": v[2], "losses": v[3]}
                                                                      for k, v in deltas.items()}}
    if a.json_out:
        json.dump(summary, open(a.json_out, "w"), indent=1)


if __name__ == "__main__":
    main()
