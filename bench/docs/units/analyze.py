"""Score the arms against the pre-registration (PREREG.md) and print the verdicts.

    python3 analyze.py --probe <dir>   -> <dir>/summary.json
"""
import argparse, json, math, os, collections


def mcnemar(a_hits, b_hits):
    """Exact two-sided McNemar on paired hits. Returns (b, c, p): b = only A hit, c = only B hit."""
    b = sum(1 for x, y in zip(a_hits, b_hits) if x and not y)
    c = sum(1 for x, y in zip(a_hits, b_hits) if y and not x)
    n = b + c
    if n == 0:
        return b, c, 1.0
    k = min(b, c)
    p = 2 * sum(math.comb(n, i) for i in range(k + 1)) / 2 ** n
    return b, c, min(1.0, p)


def load(probe, name):
    path = os.path.join(probe, "results", name + ".jsonl")
    return {r["qid"]: r for r in map(json.loads, open(path))} if os.path.exists(path) else None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--probe", required=True)
    a = ap.parse_args()
    qs = {q["qid"]: q for q in map(json.loads, open(os.path.join(a.probe, "questions.jsonl")))}
    chunks = json.load(open(os.path.join(a.probe, "chunks.json")))
    gold = {qid: {chunks[q["policy_id"]][s] for s in q["gold"]} for qid, q in qs.items()}
    subset = json.load(open(os.path.join(a.probe, "rr_subset.json")))

    def scores(res, qids):
        out = {"n": len(qids)}
        for k in (1, 5, 10):
            out["R@%d" % k] = round(sum(any(c in gold[q] for c in res[q]["ranked"][:k]) for q in qids) / len(qids), 4)
        rr = []
        for q in qids:
            ranks = [i for i, c in enumerate(res[q]["ranked"][:10]) if c in gold[q]]
            rr.append(1 / (ranks[0] + 1) if ranks else 0)
        out["MRR@10"] = round(sum(rr) / len(rr), 4)
        return out

    def hits5(res, qids):
        return [any(c in gold[q] for c in res[q]["ranked"][:5]) for q in qids]

    summary = {"arms": {}, "checks": {}, "verdicts": {}}
    allq = sorted(qs)
    header, units = load(a.probe, "header-p1"), load(a.probe, "units-p1")
    summary["arms"]["header"] = scores(header, allq)
    summary["arms"]["units"] = scores(units, allq)
    b, c, p = mcnemar(hits5(units, allq), hits5(header, allq))
    summary["verdicts"]["H1_units_over_header_R@5"] = {"only_units": b, "only_header": c, "p": p,
        "holds": p < 0.025 and b > c}

    # Rerank arms: three passes each on the pre-registered subset; a question counts as a
    # hit when at least two of the three passes hit (PREREG).
    rr = {}
    for arm in ("header_rr", "units_rr"):
        passes = [load(a.probe, "%s-p%d" % (arm, i)) for i in (1, 2, 3)]
        passes = [x for x in passes if x]
        rr[arm] = passes
        summary["arms"][arm] = {"passes": [scores(x, subset) for x in passes]}
        summary["arms"][arm]["reranked_rate"] = round(
            sum(1 for x in passes for q in subset if x[q].get("reranked")) / (len(passes) * len(subset)), 4)
    if len(rr["header_rr"]) == 3 and len(rr["units_rr"]) == 3:
        maj = lambda passes: [sum(hits5(x, [q])[0] for x in passes) >= 2 for q in subset]
        b, c, p = mcnemar(maj(rr["units_rr"]), maj(rr["header_rr"]))
        summary["verdicts"]["H2_units_rr_over_header_rr_R@5"] = {"only_units_rr": b, "only_header_rr": c, "p": p,
            "holds": p < 0.025 and b > c}
    # Same subset, no rerank — how much of the rerank's effect is there without units.
    summary["arms"]["header_on_subset"] = scores(header, subset)
    summary["arms"]["units_on_subset"] = scores(units, subset)

    # Instrument checks (PREREG): each can fail.
    kinds = collections.Counter(k for q in allq for k, c in zip(units[q]["matched"][:5], units[q]["ranked"][:5])
                                if k and c in gold[q])
    summary["checks"]["units_arm_hits_through_a_unit"] = sum(kinds.values())
    summary["checks"]["matched_kind_among_gold_top5"] = dict(kinds)
    summary["checks"]["header_arm_matched_units"] = sum(1 for q in allq for k in header[q]["matched"] if k)
    chance5 = sum(min(1.0, 5 * len(gold[q]) / len(chunks[qs[q]["policy_id"]])) for q in allq) / len(allq)
    summary["checks"]["chance_R@5"] = round(chance5, 4)
    summary["checks"]["header_beats_chance_by_2x"] = summary["arms"]["header"]["R@5"] > 2 * chance5
    print(json.dumps(summary, indent=2))
    json.dump(summary, open(os.path.join(a.probe, "summary.json"), "w"), indent=2)


if __name__ == "__main__":
    main()
