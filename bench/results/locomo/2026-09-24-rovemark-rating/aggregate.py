#!/usr/bin/env python3
"""Pool the ten per-conversation reports into one LoCoMo rating (rovemark protocol: cats 1-4).

binary-J (correct=1, partial=0, wrong=0) is what the published tables report; partial-credit
(partial=0.5) is our own convention and is NOT comparable to them. The CI is a bootstrap over
QUESTIONS, clustered by nothing — it says how much the pooled number would move on a redraw
of the same corpus, not how a different judge would grade it (that is the larger error).
"""
import json, glob, os, random, sys
D = sys.argv[1]
CAT = {1: "multi-hop", 2: "temporal", 3: "open-domain", 4: "single-hop"}
rows = []
for p in sorted(glob.glob(f"{D}/B-conv-*/answer-report.json")):
    sid = p.split("/")[-2][2:]
    for x in json.load(open(p))["results"]:
        rows.append(dict(x, sample=sid))
g = [r for r in rows if r.get("verdict") in ("correct", "partial", "wrong")]
b = lambda rs: sum(r["verdict"] == "correct" for r in rs) / len(rs)
pc = lambda rs: sum({"correct": 1, "partial": .5, "wrong": 0}[r["verdict"]] for r in rs) / len(rs)
random.seed(7)
boot = sorted(b(random.choices(g, k=len(g))) for _ in range(2000))
out = {
    "questions": len(rows), "graded": len(g), "ungraded": len(rows) - len(g),
    "rejudged": sum(1 for r in rows if r.get("rejudged")),
    "not_found": sum(1 for r in g if r.get("not_found")),
    "binary_j": round(b(g), 4), "binary_j_ci95": [round(boot[50], 4), round(boot[1949], 4)],
    "partial_credit": round(pc(g), 4),
    "per_category": {CAT[c]: {"n": len(rs), "binary_j": round(b(rs), 4), "partial_credit": round(pc(rs), 4)}
                     for c in sorted(CAT) for rs in [[r for r in g if r["category"] == c]] if rs},
    "per_conversation": {s: {"n": len(rs), "binary_j": round(b(rs), 4), "partial_credit": round(pc(rs), 4)}
                         for s in sorted({r["sample"] for r in g}) for rs in [[r for r in g if r["sample"] == s]]},
}
print(json.dumps(out, indent=1))
