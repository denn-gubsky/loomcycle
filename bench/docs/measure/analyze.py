"""Score the arms against the pre-registration (PREREG.md) and print the verdicts.

    python3 analyze.py --probe <dir>   -> <dir>/summary.json

Recall counts READABLE sections only (a bodyless heading is skipped in rank order, as the
reader skips it): R@k = a gold section among the first k readable hits.
"""
import argparse, collections, json, math, os, random, re, string


def mcnemar(a_hits, b_hits):
    """Exact two-sided McNemar on paired hits. Returns (b, c, p): b = only A hit, c = only B hit."""
    b = sum(1 for x, y in zip(a_hits, b_hits) if x and not y)
    c = sum(1 for x, y in zip(a_hits, b_hits) if y and not x)
    n = b + c
    if n == 0:
        return b, c, 1.0
    p = 2 * sum(math.comb(n, i) for i in range(min(b, c) + 1)) / 2 ** n
    return b, c, min(1.0, p)


def normalize(s):
    s = "".join(ch for ch in s.lower() if ch not in set(string.punctuation))
    s = re.sub(r"\b(a|an|the)\b", " ", s)
    return " ".join(s.split())


def f1(pred, gold):
    p, g = normalize(pred).split(), normalize(gold).split()
    if not p or not g:
        return float(p == g)
    same = sum((collections.Counter(p) & collections.Counter(g)).values())
    if same == 0:
        return 0.0
    pr, rc = same / len(p), same / len(g)
    return 2 * pr * rc / (pr + rc)


def score(pred, golds):
    return max(float(normalize(pred) == normalize(g)) for g in golds), max(f1(pred, g) for g in golds)


def bootstrap_ci(diffs, n=10000, seed=1):
    rng = random.Random(seed)
    means = sorted(sum(rng.choice(diffs) for _ in diffs) / len(diffs) for _ in range(n))
    return round(means[int(0.025 * n)], 4), round(means[int(0.975 * n) - 1], 4)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--probe", required=True)
    a = ap.parse_args()
    P = a.probe
    docs = {d["id"]: d for d in map(json.loads, open(os.path.join(P, "docs.jsonl")))}
    qs = {q["qid"]: q for q in map(json.loads, open(os.path.join(P, "questions.jsonl")))}
    subset = json.load(open(os.path.join(P, "reader_subset.json")))
    maps = {s: json.load(open(os.path.join(P, "chunks-%s.json" % s))) for s in ("plain", "header")}
    store = {"plain": "plain", "header": "header", "header_rr": "header"}
    gold = {s: {qid: {maps[s][q["doc_id"]][i] for i in q["gold"]} for qid, q in qs.items()} for s in maps}
    has_text = {s: {cid for did, ids in maps[s].items() for i, cid in enumerate(ids)
                    if docs[did]["sections"][i]["text"].strip()} for s in maps}
    res = {arm: {r["qid"]: r for r in map(json.loads, open(os.path.join(P, "results", arm + ".jsonl")))}
           for arm in store}

    def readable(arm, qid):
        return [c for c in res[arm][qid]["ranked"] if c in has_text[store[arm]]]

    def hit(arm, qid, k):
        return any(c in gold[store[arm]][qid] for c in readable(arm, qid)[:k])

    allq = sorted(qs)
    summary = {"arms": {}, "answers": {}, "checks": {}, "verdicts": {}}
    for arm in store:
        s = {"n": len(allq)}
        for k in (1, 3, 5, 10):
            s["R@%d" % k] = round(sum(hit(arm, q, k) for q in allq) / len(allq), 4)
        rr = []
        for q in allq:
            ranks = [i for i, c in enumerate(readable(arm, q)[:10]) if c in gold[store[arm]][q]]
            rr.append(1 / (ranks[0] + 1) if ranks else 0)
        s["MRR@10"] = round(sum(rr) / len(rr), 4)
        summary["arms"][arm] = s
    for name, x, y in (("H1_header_over_plain_R@5", "header", "plain"), ("H2_header_rr_over_header_R@5", "header_rr", "header")):
        b, c, p = mcnemar([hit(x, q, 5) for q in allq], [hit(y, q, 5) for q in allq])
        summary["verdicts"][name] = {"only_" + x: b, "only_" + y: c, "p": p, "holds": p < 0.025 and b > c}

    # Answers: the reader subset, three passes per arm (one for the oracle), the per-question
    # mean over passes, then paired differences with a bootstrap CI.
    per = {}
    for arm, passes in (("oracle", (1,)), ("plain", (1, 2, 3)), ("header", (1, 2, 3)), ("header_rr", (1, 2, 3))):
        got = []
        for p in passes:
            path = os.path.join(P, "answers", "%s-p%d.jsonl" % (arm, p))
            got.append({r["qid"]: r["answer"] for r in map(json.loads, open(path))} if os.path.exists(path) else {})
        complete = all(len(g) == len(subset) for g in got)
        em = {q: sum(score(g[q], qs[q]["answers"])[0] for g in got) / len(got) for q in subset} if complete else None
        f = {q: sum(score(g[q], qs[q]["answers"])[1] for g in got) / len(got) for q in subset} if complete else None
        per[arm] = (em, f)
        summary["answers"][arm] = {"complete": complete, "passes": len(passes),
                                   "EM": round(sum(em.values()) / len(subset), 4) if complete else None,
                                   "F1": round(sum(f.values()) / len(subset), 4) if complete else None}
    for name, x, y, bound in (("H3_header_not_below_plain_F1", "header", "plain", -0.02),
                              ("H4_header_rr_over_header_F1", "header_rr", "header", 0.0)):
        if per[x][1] and per[y][1]:
            d = [per[x][1][q] - per[y][1][q] for q in subset]
            lo, hi = bootstrap_ci(d)
            summary["verdicts"][name] = {"diff": round(sum(d) / len(d), 4), "ci95": [lo, hi], "bound": bound, "holds": lo > bound}
            de = [per[x][0][q] - per[y][0][q] for q in subset]
            summary["verdicts"][name]["EM_diff"] = round(sum(de) / len(de), 4)
            summary["verdicts"][name]["EM_ci95"] = list(bootstrap_ci(de))

    # Instrument checks (PREREG): each can fail.
    n_readable = len(has_text["header"])
    chance5 = sum(min(1.0, 5 * len(q["gold"]) / n_readable) for q in qs.values()) / len(qs)
    summary["checks"]["pages_in_both_stores"] = [len(maps["plain"]), len(maps["header"])]
    summary["checks"]["plain_titles_letterless"] = all(
        not re.search(r"[A-Za-z]", h) for d in docs.values() for h in re.findall(r"(?m)^#+ (.*)$", d["plain_md"]))
    summary["checks"]["chance_R@5"] = round(chance5, 5)
    summary["checks"]["plain_R@5_over_10x_chance"] = summary["arms"]["plain"]["R@5"] > 10 * chance5
    summary["checks"]["rerank_applied_rate"] = round(sum(1 for r in res["header_rr"].values() if r.get("reranked")) / len(allq), 4)
    if summary["answers"]["oracle"]["F1"] is not None and summary["answers"]["plain"]["F1"] is not None:
        summary["checks"]["oracle_F1_over_plain_F1"] = summary["answers"]["oracle"]["F1"] > summary["answers"]["plain"]["F1"]
    print(json.dumps(summary, indent=2))
    json.dump(summary, open(os.path.join(P, "summary.json"), "w"), indent=2)


if __name__ == "__main__":
    main()
