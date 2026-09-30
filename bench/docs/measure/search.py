"""Run one arm over the questions and record each ranking.

    python3 search.py --probe <dir> --arm <plain|header|header_rr>
      -> <dir>/results/<arm>.jsonl  {qid, ranked: [chunk ids], reranked, rerank_reason}

Every arm is `Document op=search` (limit 10) with the question's query, in the arm's pooled
store. Resumable: questions already written are skipped.
"""
import argparse, json, os, time
import lc

ARMS = {"plain": ("cqa/search", "cqa-plain"), "header": ("cqa/search", "cqa-header"),
        "header_rr": ("cqa/search-rr", "cqa-header")}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--probe", required=True)
    ap.add_argument("--arm", required=True, choices=sorted(ARMS))
    a = ap.parse_args()
    agent, scope_id = ARMS[a.arm]
    qs = [json.loads(l) for l in open(os.path.join(a.probe, "questions.jsonl"))]
    os.makedirs(os.path.join(a.probe, "results"), exist_ok=True)
    path = os.path.join(a.probe, "results", a.arm + ".jsonl")
    rows = [json.loads(l) for l in open(path)] if os.path.exists(path) else []
    if a.arm.endswith("_rr"):
        # A rerank that timed out measured the shared GPU's load, not the rerank: drop the
        # row so the question is searched again (PREREG amendment 1).
        kept = [r for r in rows if r.get("rerank_reason") != "timeout"]
        if len(kept) != len(rows):
            print("re-running %d searches whose rerank timed out" % (len(rows) - len(kept)), flush=True)
            with open(path, "w") as f:
                f.writelines(json.dumps(r) + "\n" for r in kept)
        rows = kept
    done = {r["qid"] for r in rows}
    t0, n = time.time(), 0
    with open(path, "a") as f:
        for q in qs:
            if q["qid"] in done:
                continue
            for attempt in range(4):
                try:
                    r = lc.run(agent, scope_id, q["query"])
                    break
                except Exception as e:  # a transport fault retries; the arm never records a guess
                    err = e
                    time.sleep(10 * (attempt + 1))
            else:
                raise SystemExit("%s: %s" % (q["qid"], err))
            f.write(json.dumps({"qid": q["qid"], "ranked": [c["chunk_id"] for c in r.get("chunks", [])],
                                "reranked": r.get("reranked"), "rerank_reason": r.get("rerank_reason")}) + "\n")
            f.flush()
            n += 1
            if n % 100 == 0:
                print(a.arm, n, "%.2fs/q" % ((time.time() - t0) / n), flush=True)
    print("done", a.arm, n, flush=True)


if __name__ == "__main__":
    main()
