"""Score the LoCoMo memory reranks against the pre-registration (PREREG.md).

    python3 analyze.py --dir <run dir>   (pools.jsonl + <arm>.jsonl)  -> <dir>/summary.json

Ground truth is LoCoMo's evidence turn ids (the harness's `expected`), no judge. Per
question: recall@k = |evidence in the top k| / |evidence|, hit@k = any evidence in the
top k, and the reciprocal rank of the first evidence turn within 20.
"""
import argparse, collections, json, os, random

DECISION = ("nimble_choice", "nimble_point", "tev1_point")
CAT = {1: "multi-hop", 2: "temporal", 3: "open-domain", 4: "single-hop"}


def boot_ci(d, level, n=10000, seed=1):
    rng = random.Random(seed)
    m = sorted(sum(rng.choice(d) for _ in d) / len(d) for _ in range(n))
    lo = (1 - level) / 2
    return round(m[int(lo * n)], 4), round(m[int((1 - lo) * n) - 1], 4)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dir", required=True)
    a = ap.parse_args()
    D = a.dir
    pools = {p["qid"]: p for p in map(json.loads, open(os.path.join(D, "pools.jsonl")))}
    qids = sorted(pools)
    rank = {"none": {q: p["keys"] for q, p in pools.items()}}
    raw = {}
    for arm in ("qwen_list",) + DECISION:
        path = os.path.join(D, arm + ".jsonl")
        if os.path.exists(path):
            raw[arm] = {r["qid"]: r for r in map(json.loads, open(path))}
            rank[arm] = {q: r["order"] for q, r in raw[arm].items()}
    # A pool too small to rerank (< 2 candidates) keeps its own order in every arm.
    for arm in rank:
        for q in qids:
            rank[arm].setdefault(q, pools[q]["keys"])

    def rec(arm, q, k):
        e = set(pools[q]["expected"])
        return len(e & set(rank[arm][q][:k])) / len(e)

    def hit(arm, q, k):
        return float(bool(set(pools[q]["expected"]) & set(rank[arm][q][:k])))

    def rr(arm, q):
        e = set(pools[q]["expected"])
        for i, key in enumerate(rank[arm][q][:20]):
            if key in e:
                return 1 / (i + 1)
        return 0.0

    def stats(arm, qs):
        return {"n": len(qs), **{"recall@%d" % k: round(sum(rec(arm, q, k) for q in qs) / len(qs), 4) for k in (1, 5, 10)},
                **{"hit@%d" % k: round(sum(hit(arm, q, k) for q in qs) / len(qs), 4) for k in (1, 5)},
                "MRR@20": round(sum(rr(arm, q) for q in qs) / len(qs), 4)}

    complete = {arm: arm == "none" or all(q in raw[arm] or len(pools[q]["keys"]) < 2 for q in qids) for arm in rank}
    summary = {"n": len(qids), "arms": {}, "by_category": {}, "verdicts": {}, "checks": {}, "latency": {}}
    for arm in rank:
        if complete[arm]:
            summary["arms"][arm] = stats(arm, qids)
            summary["by_category"][arm] = {CAT.get(c, str(c)): stats(arm, [q for q in qids if pools[q]["category"] == c])
                                           for c in sorted({pools[q]["category"] for q in qids}, key=str)}

    # Co-primary: a decision rerank beats no rerank on recall@5 (97.5% CI lower bound > 0).
    for name, arm in (("H1_nimble_choice_over_none_recall@5", "nimble_choice"),
                      ("H2_nimble_point_over_none_recall@5", "nimble_point"),
                      ("S1_qwen_list_over_none_recall@5", "qwen_list"),
                      ("S2_tev1_point_over_none_recall@5", "tev1_point")):
        if complete.get(arm):
            d = [rec(arm, q, 5) - rec("none", q, 5) for q in qids]
            lo, hi = boot_ci(d, 0.975)
            summary["verdicts"][name] = {"diff": round(sum(d) / len(d), 4), "ci97.5": [lo, hi], "holds": lo > 0}
    for name, arm in (("S3_nimble_choice_not_below_qwen_list_recall@5", "nimble_choice"),
                      ("S4_nimble_point_not_below_qwen_list_recall@5", "nimble_point")):
        if complete.get(arm) and complete.get("qwen_list"):
            d = [rec(arm, q, 5) - rec("qwen_list", q, 5) for q in qids]
            lo, hi = boot_ci(d, 0.975)
            summary["verdicts"][name] = {"diff": round(sum(d) / len(d), 4), "ci97.5": [lo, hi], "margin": -0.03, "holds": lo > -0.03}

    for arm in raw:
        ms = sorted(r["ms"] for r in raw[arm].values())
        summary["latency"][arm] = {"p50_ms": ms[len(ms) // 2], "p95_ms": ms[int(0.95 * len(ms))]}

    # Instrument checks.
    summary["checks"]["1_questions"] = len(qids)
    summary["checks"]["1_pools_of_20"] = round(sum(1 for q in qids if len(pools[q]["keys"]) == 20) / len(qids), 4)
    summary["checks"]["2_none_recall@10"] = summary["arms"]["none"]["recall@10"]
    summary["checks"]["2_matches_harness_baseline_0.6675"] = abs(summary["arms"]["none"]["recall@10"] - 0.6675) <= 0.01
    if "qwen_list" in raw:
        summary["checks"]["3_qwen_list_reranked_rate"] = round(sum(1 for r in raw["qwen_list"].values() if r.get("reranked")) / len(raw["qwen_list"]), 4)
    summary["checks"]["pool_ceiling_recall@20"] = round(sum(rec("none", q, 20) for q in qids) / len(qids), 4)
    summary["checks"]["categories"] = dict(collections.Counter(CAT.get(pools[q]["category"], str(pools[q]["category"])) for q in qids))
    print(json.dumps(summary, indent=2))
    json.dump(summary, open(os.path.join(D, "summary.json"), "w"), indent=2)


if __name__ == "__main__":
    main()
