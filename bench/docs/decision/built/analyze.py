"""Score the built decision reranker against the pre-registration (PREREG.md).

    python3 analyze.py --probe <dir>   -> <dir>/summary.json

Recall counts READABLE sections only, as in phase M and the decision probe: a bodyless
heading is skipped in rank order. R@k = a gold section among the first k readable hits.
"""
import argparse, json, math, os

PROBE_R5 = 0.9437          # nimble_choice in the decision probe (#1544)
PROBE_RERANK_P50_MS = 1464  # its p50 rerank time, the same host
M_HEADER_R5 = 0.8604       # phase M's `header` arm (no rerank), all 2,515 questions


def mcnemar(a, b):
    x = sum(1 for p, q in zip(a, b) if p and not q)
    y = sum(1 for p, q in zip(a, b) if q and not p)
    n = x + y
    if n == 0:
        return x, y, 1.0
    return x, y, min(1.0, 2 * sum(math.comb(n, i) for i in range(min(x, y) + 1)) / 2 ** n)


def wilson(k, n, z=1.96):
    p = k / n
    d = 1 + z * z / n
    c = p + z * z / (2 * n)
    h = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n))
    return round((c - h) / d, 4), round((c + h) / d, 4)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--probe", required=True)
    a = ap.parse_args()
    P = a.probe
    docs = {d["id"]: d for d in map(json.loads, open(os.path.join(P, "docs.jsonl")))}
    qs = {q["qid"]: q for q in map(json.loads, open(os.path.join(P, "questions.jsonl")))}
    cmap = json.load(open(os.path.join(P, "chunks-header.json")))
    where = {c: (d, i) for d, ids in cmap.items() for i, c in enumerate(ids)}
    rows = {r["qid"]: r for r in map(json.loads, open(os.path.join(P, "results", "built.jsonl")))}
    allq = sorted(q for q in qs if q in rows)

    def readable(ranked):
        return [where[c] for c in ranked if c in where and docs[where[c][0]]["sections"][where[c][1]]["text"].strip()]

    def hit(arm, q, k):
        g = {(qs[q]["doc_id"], i) for i in qs[q]["gold"]}
        return any(x in g for x in readable(rows[q][arm]["ranked"])[:k])

    summary = {"n": len(allq), "complete": len(allq) == len(qs), "arms": {}, "verdicts": {}, "checks": {}, "latency": {}}
    for arm in ("none", "decision"):
        s = {}
        for k in (1, 3, 5, 10):
            s["R@%d" % k] = round(sum(hit(arm, q, k) for q in allq) / len(allq), 4)
        summary["arms"][arm] = s
    k5 = sum(hit("decision", q, 5) for q in allq)
    lo, hi = wilson(k5, len(allq))
    summary["verdicts"]["H1_built_R@5_within_0.03_of_probe"] = {
        "R@5": round(k5 / len(allq), 4), "wilson95": [lo, hi], "threshold": round(PROBE_R5 - 0.03, 4),
        "holds": k5 / len(allq) >= PROBE_R5 - 0.03}
    diffs = sorted(rows[q]["decision"]["ms"] - rows[q]["none"]["ms"] for q in allq)
    p50 = diffs[len(diffs) // 2]
    summary["verdicts"]["H2_rerank_p50_within_25pct_of_probe"] = {
        "p50_rerank_ms": p50, "threshold_ms": int(PROBE_RERANK_P50_MS * 1.25), "holds": p50 <= PROBE_RERANK_P50_MS * 1.25}
    b, c, p = mcnemar([hit("decision", q, 5) for q in allq], [hit("none", q, 5) for q in allq])
    summary["verdicts"]["S1_decision_over_none_R@5"] = {"only_decision": b, "only_none": c, "p": p}
    b, c, p = mcnemar([hit("decision", q, 1) for q in allq], [hit("none", q, 1) for q in allq])
    summary["verdicts"]["S2_decision_over_none_R@1"] = {"only_decision": b, "only_none": c, "p": p}

    reasons = {}
    for q in allq:
        r = rows[q]["decision"]
        key = "applied" if r.get("reranked") else (r.get("rerank_reason") or "none")
        reasons[key] = reasons.get(key, 0) + 1
    summary["checks"]["1_rerank_applied_rate"] = round(reasons.get("applied", 0) / len(allq), 4)
    summary["checks"]["1_reasons"] = reasons
    summary["checks"]["2_none_R@5"] = summary["arms"]["none"]["R@5"]
    summary["checks"]["2_none_matches_phase_M_within_0.02"] = abs(summary["arms"]["none"]["R@5"] - M_HEADER_R5) <= 0.02
    for arm in ("none", "decision"):
        ms = sorted(rows[q][arm]["ms"] for q in allq)
        summary["latency"][arm] = {"p50_ms": ms[len(ms) // 2], "p95_ms": ms[int(0.95 * len(ms))]}
    summary["latency"]["rerank_p95_ms"] = diffs[int(0.95 * len(diffs))]
    print(json.dumps(summary, indent=2))
    json.dump(summary, open(os.path.join(P, "summary.json"), "w"), indent=2)


if __name__ == "__main__":
    main()
