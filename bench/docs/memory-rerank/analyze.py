"""Score RFC DR P2 (b) against the pre-registration: rerank-off vs rerank-on answers.

    python3 analyze.py --out <run dir>   -> <run dir>/summary.json

Reads B-off-<conv>/answer-report.json and B-on-<conv>/answer-report.json, pairs the two
arms by (conversation, question), and applies the pre-registered test and checks.
"""
import argparse, glob, json, math, os

CAT = {1: "multi-hop", 2: "temporal", 3: "open-domain", 4: "single-hop"}


def mcnemar(a, b):
    x = sum(1 for p, q in zip(a, b) if p and not q)
    y = sum(1 for p, q in zip(a, b) if q and not p)
    n = x + y
    if n == 0:
        return x, y, 1.0
    return x, y, min(1.0, 2 * sum(math.comb(n, i) for i in range(min(x, y) + 1)) / 2 ** n)


def load(path):
    rep = json.load(open(path))
    rows = rep.get("results") or rep.get("answers") or []
    # Keyed by (text, occurrence): a sample can hold two questions with the same text.
    seen, out = {}, {}
    for r in rows:
        seen[r["question"]] = seen.get(r["question"], 0) + 1
        out[(r["question"], seen[r["question"]])] = r
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    a = ap.parse_args()
    O = a.out
    pairs, voided, nimble = [], [], {}
    convs = sorted(os.path.basename(d)[len("B-on-"):] for d in glob.glob(os.path.join(O, "B-on-conv-*")) if os.path.isdir(d))
    for c in convs:
        alog = open(os.path.join(O, "A-%s.log" % c)).read() if os.path.exists(os.path.join(O, "A-%s.log" % c)) else ""
        if "consolidated in" not in alog or "work still queued" in alog or "SKIPPED" in alog:
            voided.append(c)
            continue
        off = load(os.path.join(O, "B-off-%s" % c, "answer-report.json"))
        on = load(os.path.join(O, "B-on-%s" % c, "answer-report.json"))
        for q in off:
            if q in on:
                pairs.append((c, off[q], on[q]))
        for arm in ("off", "on"):
            p = os.path.join(O, "B-%s-%s.nimble" % (arm, c))
            nimble[(arm, c)] = int(open(p).read().strip()) if os.path.exists(p) else None

    graded = [(c, x, y) for c, x, y in pairs if x["verdict"] != "unparsed" and y["verdict"] != "unparsed"]

    def stats(rows):
        n = len(rows)
        return {"n": n,
                "binary_J": round(sum(r["verdict"] == "correct" for r in rows) / n, 4) if n else None,
                "partial": round(sum({"correct": 1, "partial": 0.5}.get(r["verdict"], 0) for r in rows) / n, 4) if n else None,
                "not_found_rate": round(sum(bool(r.get("not_found")) for r in rows) / n, 4) if n else None,
                "latency_p50_ms": sorted(r.get("latency_ms", 0) for r in rows)[n // 2] if n else None}

    summary = {"conversations": convs, "voided_by_coverage": voided, "pairs": len(pairs), "graded_pairs": len(graded),
               "arms": {"off": stats([x for _, x, _ in graded]), "on": stats([y for _, _, y in graded])},
               "by_category": {}, "verdicts": {}, "checks": {}}
    for cat, name in CAT.items():
        rows = [(x, y) for _, x, y in graded if x.get("category") == cat]
        if rows:
            summary["by_category"][name] = {"off": stats([x for x, _ in rows]), "on": stats([y for _, y in rows])}
    b, c, p = mcnemar([y["verdict"] == "correct" for _, _, y in graded], [x["verdict"] == "correct" for _, x, _ in graded])
    summary["verdicts"]["H1_rerank_on_over_off_binary_J"] = {"only_on": b, "only_off": c, "p": p, "holds": p < 0.05 and b > c}

    for arm in ("off", "on"):
        rows = [x if arm == "off" else y for _, x, y in pairs]
        summary["checks"]["3_unparsed_rate_%s" % arm] = round(sum(r["verdict"] == "unparsed" for r in rows) / max(1, len(rows)), 4)
    summary["checks"]["2_nimble_calls_off"] = sum(v for (arm, _), v in nimble.items() if arm == "off" and v is not None)
    summary["checks"]["2_nimble_calls_on"] = sum(v for (arm, _), v in nimble.items() if arm == "on" and v is not None)
    summary["checks"]["2_nimble_calls_on_per_question"] = round(summary["checks"]["2_nimble_calls_on"] / max(1, len(pairs)), 3)
    # Amendment 2: the sampler draws floor(40 x category share) per conversation, 383 in all.
    same = all(set(load(os.path.join(O, "B-off-%s" % c, "answer-report.json"))) ==
               set(load(os.path.join(O, "B-on-%s" % c, "answer-report.json"))) for c in convs if c not in voided)
    summary["checks"]["4_same_questions_both_arms"] = same
    summary["checks"]["4_pairs_383_less_voided"] = len(pairs)
    print(json.dumps(summary, indent=2))
    json.dump(summary, open(os.path.join(O, "summary.json"), "w"), indent=2)


if __name__ == "__main__":
    main()
