#!/usr/bin/env python3
"""Summarise answers.jsonl: mean Answer-F1 per arm, paired bootstrap CI of the difference
from `plain`, per answer type, and how often the reader gave up ("Unanswerable").

Paired because every arm answers the same questions: the per-question difference is what
is resampled, so between-question variance (most of it) cancels out.
"""
import argparse
import collections
import json
import os
import random


def atype(q):
    a = q["answers"][0]
    if a.get("yes_no") is not None:
        return "yes/no"
    return "extractive" if a.get("extractive_spans") else "abstractive"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--probe", required=True)
    ap.add_argument("--baseline", default="plain")
    ap.add_argument("--json-out")
    ap.add_argument("--file", default="answers.jsonl",
                    help="answers2.jsonl for the pipeline arms, which also carry ev_rank on what the reader saw")
    a = ap.parse_args()
    A = collections.defaultdict(dict)
    for r in map(json.loads, open(os.path.join(a.probe, a.file))):
        A[r["arm"]][r["qid"]] = r
    Q = {q["qid"]: q for q in map(json.loads, open(os.path.join(a.probe, "questions.jsonl")))}
    qids = sorted(Q)
    types = collections.Counter(atype(Q[q]) for q in qids)
    rng = random.Random(7)
    base = A[a.baseline]
    order = ["oracle", "plain", "header", "context", "context/hidden-ctx", "both", "both/hidden-ctx",
             "map", "map+hop", "desc", "mapctx"]
    order += sorted(k for k in A if k not in order)
    print("n=%d  types=%s" % (len(qids), dict(types)))
    print("%-20s %6s  %-18s %8s  %s  unans" % ("arm", "F1", "Δ vs base 95%CI", "+/-",
                                             "  ".join("%11s" % t for t in types)))
    out = {}
    for arm in order:
        d = A.get(arm)
        if not d or any(q not in d for q in qids):
            if d:
                print("%-20s incomplete (%d/%d)" % (arm, len(d), len(qids)))
            continue
        m = sum(d[q]["f1"] for q in qids) / len(qids)
        diff = [d[q]["f1"] - base[q]["f1"] for q in qids]
        boots = sorted(sum(diff[rng.randrange(len(diff))] for _ in diff) / len(diff) for _ in range(4000))
        lo, hi = boots[100], boots[3899]
        by_t = {t: sum(d[q]["f1"] for q in qids if atype(Q[q]) == t) / types[t] for t in types}
        un = sum(1 for q in qids if "unanswerable" in d[q]["pred"].lower()) / len(qids)
        wins, losses = sum(x > 0 for x in diff), sum(x < 0 for x in diff)
        ci = "" if arm == a.baseline else "[%+.3f, %+.3f]" % (lo, hi)
        wl = "" if arm == a.baseline else "+%d/-%d" % (wins, losses)
        rk = ""
        if all("ev_rank" in d[q] for q in qids):
            rk = "   R@1/3/5/10 " + "/".join("%.3f" % (sum(1 for q in qids if (d[q]["ev_rank"] or 99) <= k) / len(qids))
                                         for k in (1, 3, 5, 10))
            kinds = collections.Counter(x for q in qids for x in d[q].get("kinds", [])[:5])
            tot = sum(kinds.values()) or 1
            rk += "  top5 slots: " + " ".join("%s=%.0f%%" % (k, 100 * v / tot) for k, v in sorted(kinds.items()))
        print("%-20s %6.3f  %-18s %8s  %s  %.3f%s" % (arm, m, ci, wl, "  ".join("%11.3f" % by_t[t] for t in types), un, rk))
        out[arm] = {"f1": m, "ci_vs_base": [lo, hi], "wins": wins, "losses": losses, "by_type": by_t, "unanswerable": un}
    if a.json_out:
        json.dump(out, open(a.json_out, "w"), indent=1)


if __name__ == "__main__":
    main()
