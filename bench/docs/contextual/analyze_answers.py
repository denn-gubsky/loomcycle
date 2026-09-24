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
    a = ap.parse_args()
    A = collections.defaultdict(dict)
    for r in map(json.loads, open(os.path.join(a.probe, "answers.jsonl"))):
        A[r["arm"]][r["qid"]] = r
    Q = {q["qid"]: q for q in map(json.loads, open(os.path.join(a.probe, "questions.jsonl")))}
    qids = sorted(Q)
    types = collections.Counter(atype(Q[q]) for q in qids)
    rng = random.Random(7)
    base = A[a.baseline]
    order = ["oracle", "plain", "header", "context", "context/hidden-ctx", "both", "both/hidden-ctx"]
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
        print("%-20s %6.3f  %-18s %8s  %s  %.3f" % (arm, m, ci, wl, "  ".join("%11.3f" % by_t[t] for t in types), un))
        out[arm] = {"f1": m, "ci_vs_base": [lo, hi], "wins": wins, "losses": losses, "by_type": by_t, "unanswerable": un}
    if a.json_out:
        json.dump(out, open(a.json_out, "w"), indent=1)


if __name__ == "__main__":
    main()
