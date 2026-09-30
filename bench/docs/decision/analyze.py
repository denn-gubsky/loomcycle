"""Score the decision-model reranks against the pre-registration (PREREG.md).

    python3 analyze.py --probe <dir> --m-probe <phase M probe dir>   -> <dir>/summary.json

Chunk ids differ between phase M's store and the rebuilt one, so every ranking is compared
by SECTION identity (page, section index). Recall counts readable sections only, as in M.
The baseline `qwen_list` is phase M's `header_rr`: the shipped rerank (qwen3.8, listwise)
over the same pools, which instrument checks 1 and 2 establish.
"""
import argparse, json, math, os, random

POINT_ARMS = ("nimble_point", "tev1_point")
JEV_ARMS = ("nimble_choice",) + POINT_ARMS


def mcnemar(a, b):
    x = sum(1 for p, q in zip(a, b) if p and not q)
    y = sum(1 for p, q in zip(a, b) if q and not p)
    n = x + y
    if n == 0:
        return x, y, 1.0
    return x, y, min(1.0, 2 * sum(math.comb(n, i) for i in range(min(x, y) + 1)) / 2 ** n)


def boot_ci(d, level, n=10000, seed=1):
    rng = random.Random(seed)
    m = sorted(sum(rng.choice(d) for _ in d) / len(d) for _ in range(n))
    lo = (1 - level) / 2
    return round(m[int(lo * n)], 4), round(m[int((1 - lo) * n) - 1], 4)


def auc(pos, neg):
    """P(a positive outranks a negative), ties counted half."""
    if not pos or not neg:
        return None
    neg_s = sorted(neg)
    import bisect
    tot = 0.0
    for p in pos:
        lo, hi = bisect.bisect_left(neg_s, p), bisect.bisect_right(neg_s, p)
        tot += lo + 0.5 * (hi - lo)
    return round(tot / (len(pos) * len(neg)), 4)


def ece(pairs, bins=10):
    """Expected calibration error over (probability, label) pairs, equal-width bins."""
    b = [[] for _ in range(bins)]
    for p, y in pairs:
        b[min(int(p * bins), bins - 1)].append((p, y))
    tot = len(pairs)
    return round(sum(len(x) / tot * abs(sum(p for p, _ in x) / len(x) - sum(y for _, y in x) / len(x))
                     for x in b if x), 4), [
        {"bin": i, "n": len(x), "mean_p": round(sum(p for p, _ in x) / len(x), 3),
         "frac_gold": round(sum(y for _, y in x) / len(x), 3)} for i, x in enumerate(b) if x]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--probe", required=True)
    ap.add_argument("--m-probe", required=True)
    a = ap.parse_args()
    P, M = a.probe, a.m_probe
    docs = {d["id"]: d for d in map(json.loads, open(os.path.join(P, "docs.jsonl")))}
    qs = {q["qid"]: q for q in map(json.loads, open(os.path.join(P, "questions.jsonl")))}
    subset = json.load(open(os.path.join(P, "reader_subset.json")))

    def ident(cmap):
        return {cid: (did, i) for did, ids in cmap.items() for i, cid in enumerate(ids)}
    new_id = ident(json.load(open(os.path.join(P, "chunks-header.json"))))
    m_id = ident(json.load(open(os.path.join(M, "chunks-header.json"))))

    def load(path, key, idmap):
        return {r["qid"]: [idmap[c] for c in r[key] if c in idmap]
                for r in map(json.loads, open(path))}
    # Amendment 2: the baseline is the shipped rerank run over THESE pools (qwen_list.jsonl);
    # phase M's rankings stay only as the (failed) reproduction checks.
    rank = {"none": load(os.path.join(P, "results", "pool.jsonl"), "ranked", new_id),
            "qwen_list": load(os.path.join(P, "results", "qwen_list.jsonl"), "ranked", new_id)}
    m_header = load(os.path.join(M, "results", "header.jsonl"), "ranked", m_id)
    m_rr = load(os.path.join(M, "results", "header_rr.jsonl"), "ranked", m_id)
    qwen_raw = {r["qid"]: r for r in map(json.loads, open(os.path.join(P, "results", "qwen_list.jsonl")))}
    raw = {}
    for arm in JEV_ARMS:
        path = os.path.join(P, "results", arm + ".jsonl")
        if os.path.exists(path):
            raw[arm] = {r["qid"]: r for r in map(json.loads, open(path))}
            rank[arm] = {q: [new_id[c] for c in r["order"]] for q, r in raw[arm].items()}

    def readable(s):
        return [x for x in s if docs[x[0]]["sections"][x[1]]["text"].strip()]

    def gold(q):
        return {(qs[q]["doc_id"], i) for i in qs[q]["gold"]}

    def hit(arm, q, k):
        return any(x in gold(q) for x in readable(rank[arm][q])[:k])

    summary = {"n": len(subset), "arms": {}, "checks": {}, "verdicts": {}, "calibration": {}, "gate": {}, "latency": {}}
    complete = {arm: all(q in rank[arm] for q in subset) for arm in rank}
    for arm in rank:
        if not complete[arm]:
            summary["arms"][arm] = {"complete": False}
            continue
        s = {"complete": True}
        for k in (1, 3, 5, 10):
            s["R@%d" % k] = round(sum(hit(arm, q, k) for q in subset) / len(subset), 4)
        rr = []
        for q in subset:
            r = [i for i, x in enumerate(readable(rank[arm][q])[:10]) if x in gold(q)]
            rr.append(1 / (r[0] + 1) if r else 0)
        s["MRR@10"] = round(sum(rr) / len(rr), 4)
        summary["arms"][arm] = s

    # Co-primary: non-inferiority to the shipped rerank on R@5, margin 0.03, 97.5% CI.
    for name, arm in (("H1_nimble_choice_not_below_qwen_list_R@5", "nimble_choice"),
                      ("H2_nimble_point_not_below_qwen_list_R@5", "nimble_point"),
                      ("S1_tev1_point_not_below_qwen_list_R@5", "tev1_point")):
        if complete.get(arm):
            d = [float(hit(arm, q, 5)) - float(hit("qwen_list", q, 5)) for q in subset]
            lo, hi = boot_ci(d, 0.975)
            b, c, p = mcnemar([hit(arm, q, 5) for q in subset], [hit("qwen_list", q, 5) for q in subset])
            summary["verdicts"][name] = {"diff": round(sum(d) / len(d), 4), "ci97.5": [lo, hi], "margin": -0.03,
                                         "only_arm": b, "only_qwen_list": c, "mcnemar_p": p, "holds": lo > -0.03}
    for arm in JEV_ARMS:  # reported: does each beat no rerank at all
        if complete.get(arm):
            b, c, p = mcnemar([hit(arm, q, 5) for q in subset], [hit("none", q, 5) for q in subset])
            summary["verdicts"]["vs_none_" + arm] = {"only_arm": b, "only_none": c, "p": p}

    # Calibration and the gate, for the pointwise arms (one probability per pair).
    for arm in POINT_ARMS:
        if not complete.get(arm):
            continue
        pairs, pos, neg = [], [], []
        pools ={r["qid"]: r["ranked"] for r in map(json.loads, open(os.path.join(P, "results", "pool.jsonl")))}
        for q in subset:
            sc = raw[arm][q]["scores"]
            secs = [new_id[c] for c in pools[q]]
            g = gold(q)
            for x, p in zip(secs, sc):
                pairs.append((p, 1.0 if x in g else 0.0))
            if any(x in g for x in secs):  # minimal pair: the same pool with its gold removed
                pos.append(max(sc))
                rest = [p for x, p in zip(secs, sc) if x not in g]
                neg.append(max(rest) if rest else 0.0)
        e, bins = ece(pairs)
        brier = round(sum((p - y) ** 2 for p, y in pairs) / len(pairs), 4)
        summary["calibration"][arm] = {"pairs": len(pairs), "ECE": e, "brier": brier,
                                       "pair_AUC": auc([p for p, y in pairs if y], [p for p, y in pairs if not y]),
                                       "bins": bins}
        g_auc = auc(pos, neg)
        summary["gate"][arm] = {"pools_with_gold": len(pos), "AUC": g_auc,
                                "usable": g_auc is not None and g_auc >= 0.85 and e <= 0.10}

    for arm in JEV_ARMS:
        if complete.get(arm):
            ms = sorted(raw[arm][q]["ms"] for q in subset)
            summary["latency"][arm] = {"p50_ms": ms[len(ms) // 2], "p95_ms": ms[int(0.95 * len(ms))],
                                       "mean_input_tokens": int(sum(raw[arm][q]["input_tokens"] for q in subset) / len(subset))}
    if complete.get("nimble_choice"):
        summary["latency"]["nimble_choice"]["cut_below_1200"] = sum(1 for q in subset if raw["nimble_choice"][q]["max_chars"] < 1200)

    # Instrument checks. 1 and 2 (the pools equal phase M's) are reported as they failed;
    # amendment 2 replaces them with 1b and 2b, on the baseline run over these pools.
    summary["checks"]["1_pool_top10_equals_M_header_top10"] = round(
        sum(1 for q in subset if rank["none"][q][:10] == m_header[q][:10]) / len(subset), 4)
    summary["checks"]["2_M_rerank_top10_within_pool"] = round(
        sum(1 for q in subset if set(m_rr[q][:10]) <= set(rank["none"][q])) / len(subset), 4)
    summary["checks"]["1b_baseline_reranked_the_same_pool"] = round(
        sum(1 for q in subset if q in rank["qwen_list"] and set(rank["qwen_list"][q]) == set(rank["none"][q])) / len(subset), 4)
    summary["checks"]["2b_baseline_reranked_rate"] = round(
        sum(1 for q in subset if qwen_raw.get(q, {}).get("reranked")) / len(subset), 4)
    summary["checks"]["3_pools_of_20"] = round(sum(1 for q in subset if len(rank["none"][q]) == 20) / len(subset), 4)
    ms = sorted(qwen_raw[q]["ms"] for q in subset if q in qwen_raw)
    if ms:
        summary["latency"]["qwen_list"] = {"p50_ms": ms[len(ms) // 2], "p95_ms": ms[int(0.95 * len(ms))],
                                           "note": "search + rerank wall time via loomcycle, on the Spark"}
    # Cross-host agreement: the arm's partial TrueNAS run against its Spark run, same pools.
    for arm in POINT_ARMS + ("nimble_choice",):
        other = os.path.join(P, "results", arm + "-truenas.jsonl")
        if arm in raw and os.path.exists(other):
            o = {r["qid"]: r for r in map(json.loads, open(other))}
            common = [q for q in o if q in raw[arm]]
            dmax = max((abs(x - y) for q in common for x, y in zip(o[q]["scores"], raw[arm][q]["scores"])), default=None)
            top1 = sum(1 for q in common if o[q]["order"][0] == raw[arm][q]["order"][0]) / len(common) if common else None
            summary["checks"]["host_agreement_" + arm] = {"questions": len(common), "max_abs_diff": dmax,
                                                          "same_top1": top1}
    print(json.dumps(summary, indent=2))
    json.dump(summary, open(os.path.join(P, "summary.json"), "w"), indent=2)


if __name__ == "__main__":
    main()
