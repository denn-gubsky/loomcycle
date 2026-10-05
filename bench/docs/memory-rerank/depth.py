"""RFC DR P2 (c): does a 40-candidate pool lift multi-hop recall over 20? See PREREG.md.

    python3 depth.py pools --convert <locomo -mode=convert dir> --data locomo10.json --dir <run dir>
        -> <dir>/pools40.jsonl and <dir>/pools20.jsonl (multi-hop questions only)
    python3 depth.py score --dir <run dir>   (after rerankpool wrote rr20.jsonl and rr40.jsonl)
        -> <dir>/depth-summary.json

The store and the questions are #1571's: `bench/cmd/locomo -mode=ingest -scope user`, one
row per turn, and the harness's own `-mode=convert` questions. Each pool is
`POST /v1/_memory/search` top_k 40 with no rerank; the 20-arm is its first 20, so the two
arms rerank the same head and differ only by the 20 rows behind it.
"""
import argparse, json, os, random, sys, urllib.request

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "decision", "locomo"))
import pool as p1571  # noqa: E402  the #1571 pool fetcher: text_of, the question join

BASE = os.environ.get("LC_BASE", "http://127.0.0.1:8825")
MULTI_HOP = 1


def search(scope_id, query, k):
    body = {"query": query, "scope": "user", "scope_id": scope_id, "top_k": k}
    req = urllib.request.Request(BASE + "/v1/_memory/search", data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    return json.load(urllib.request.urlopen(req, timeout=120))


def pools(a):
    p1571.BASE, p1571.search = BASE, lambda s, q: search(s, q, 40)
    tmp = os.path.join(a.dir, "pools-all40.jsonl")
    sys.argv = ["pool.py", "--convert", a.convert, "--data", a.data, "--out", tmp]
    p1571.main()
    with open(os.path.join(a.dir, "pools40.jsonl"), "w") as f40, open(os.path.join(a.dir, "pools20.jsonl"), "w") as f20:
        for line in open(tmp):
            p = json.loads(line)
            if p["category"] != MULTI_HOP:
                continue
            f40.write(json.dumps(p, ensure_ascii=False) + "\n")
            f20.write(json.dumps({**p, "keys": p["keys"][:20], "texts": p["texts"][:20]}, ensure_ascii=False) + "\n")


def boot_ci(d, n=10000, seed=1):
    rng = random.Random(seed)
    m = sorted(sum(rng.choice(d) for _ in d) / len(d) for _ in range(n))
    return round(m[int(0.025 * n)], 4), round(m[int(0.975 * n) - 1], 4)


def score(a):
    D = a.dir
    pl = {p["qid"]: p for p in map(json.loads, open(os.path.join(D, "pools40.jsonl")))}
    rr = {arm: {r["qid"]: r for r in map(json.loads, open(os.path.join(D, "rr%s.jsonl" % arm)))} for arm in ("20", "40")}
    qids = sorted(pl)

    def rec(order, q, k=5):
        e = set(pl[q]["expected"])
        return len(e & set(order[:k])) / len(e)

    def order(arm, q):
        r = rr[arm].get(q)
        return r["order"] if r else pl[q]["keys"][:int(arm)]

    diff = [rec(order("40", q), q) - rec(order("20", q), q) for q in qids]
    lo, hi = boot_ci(diff)
    gain = round(sum(diff) / len(diff), 4)
    s = {"n": len(qids),
         "recall@5": {"none20": round(sum(rec(pl[q]["keys"][:20], q) for q in qids) / len(qids), 4),
                      "rerank20": round(sum(rec(order("20", q), q) for q in qids) / len(qids), 4),
                      "rerank40": round(sum(rec(order("40", q), q) for q in qids) / len(qids), 4)},
         "pool_ceiling": {"recall@20": round(sum(rec(pl[q]["keys"], q, 20) for q in qids) / len(qids), 4),
                          "recall@40": round(sum(rec(pl[q]["keys"], q, 40) for q in qids) / len(qids), 4)},
         "S1_40_over_20_recall@5": {"gain": gain, "ci95": [lo, hi], "holds": lo > 0},
         "decision_candidates_40": lo > 0 and gain >= 0.03,
         "latency_p50_ms": {arm: sorted(r["ms"] for r in rr[arm].values())[len(rr[arm]) // 2] for arm in rr},
         "checks": {"5_multi_hop_282": len(qids) == 282,
                    "6_reranked_rate": {arm: round(sum(bool(r.get("reranked")) for r in rr[arm].values()) / len(qids), 4)
                                        for arm in rr},
                    "7_none20_reproduces_1571": None}}
    s["checks"]["7_none20_reproduces_1571"] = abs(s["recall@5"]["none20"] - 0.332) <= 0.02
    print(json.dumps(s, indent=2))
    json.dump(s, open(os.path.join(D, "depth-summary.json"), "w"), indent=2)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("cmd", choices=("pools", "score"))
    ap.add_argument("--dir", required=True)
    ap.add_argument("--convert")
    ap.add_argument("--data")
    a = ap.parse_args()
    pools(a) if a.cmd == "pools" else score(a)


if __name__ == "__main__":
    main()
