"""Run one arm over the questions and record each ranking.

    python3 search.py --probe <dir> --arm <header|units|header_rr|units_rr> [--subset rr_subset.json] [--pass-no N]
      -> <dir>/results/<arm>-p<N>.jsonl  {qid, ranked: [chunk ids], matched: [kind or null]}

Resumable: questions already written for the arm+pass are skipped.
"""
import argparse, json, os, time
import lc

AGENTS = {"header": "pqa/search-header", "units": "pqa/search-units",
          "header_rr": "pqa/search-header-rr", "units_rr": "pqa/search-units-rr"}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--probe", required=True)
    ap.add_argument("--arm", required=True, choices=sorted(AGENTS))
    ap.add_argument("--subset")
    ap.add_argument("--pass-no", type=int, default=1)
    a = ap.parse_args()
    qs = [json.loads(l) for l in open(os.path.join(a.probe, "questions.jsonl"))]
    if a.subset:
        keep = set(json.load(open(os.path.join(a.probe, a.subset))))
        qs = [q for q in qs if q["qid"] in keep]
    os.makedirs(os.path.join(a.probe, "results"), exist_ok=True)
    path = os.path.join(a.probe, "results", "%s-p%d.jsonl" % (a.arm, a.pass_no))
    done = set()
    if os.path.exists(path):
        done = {json.loads(l)["qid"] for l in open(path)}
    t0, n = time.time(), 0
    with open(path, "a") as f:
        for q in qs:
            if q["qid"] in done:
                continue
            for attempt in range(4):
                try:
                    r = lc.run(AGENTS[a.arm], q["policy_id"], q["question"])
                    break
                except Exception as e:  # a transport fault retries; the arm never records a guess
                    err = e
                    time.sleep(10 * (attempt + 1))
            else:
                raise SystemExit("%s: %s" % (q["qid"], err))
            chunks = r.get("chunks", [])
            f.write(json.dumps({"qid": q["qid"], "ranked": [c["chunk_id"] for c in chunks],
                                "matched": [(c.get("matched_unit") or {}).get("kind") for c in chunks],
                                "reranked": r.get("reranked"), "rerank_reason": r.get("rerank_reason")}) + "\n")
            n += 1
            if n % 100 == 0:
                f.flush()
                print(a.arm, "p%d" % a.pass_no, n, "%.2fs/q" % ((time.time() - t0) / n), flush=True)
    print("done", a.arm, "p%d" % a.pass_no, n, flush=True)


if __name__ == "__main__":
    main()
