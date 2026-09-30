"""Fetch each subset question's 20-candidate pool: the order the shipped rerank starts from.

    python3 pool.py --probe <dir> [--arm pool|qwen_list]
      -> <dir>/results/<arm>.jsonl  {qid, ranked: [20 chunk ids], reranked, rerank_reason, ms}

`Document op=search` with limit 20 and no rerank, in the rebuilt `cqa-header` store. A
rerank reorders the first 20 of the same fused pool, so these are the candidates it saw.
`qwen_list` is the same search with the shipped rerank on (agent `cqa/pool-rr`): the
baseline, over exactly these pools (amendment 2). Resumable.
"""
import argparse, json, os, sys, time

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "measure"))
import lc  # noqa: E402


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--probe", required=True)
    ap.add_argument("--arm", default="pool", choices=["pool", "qwen_list"])
    a = ap.parse_args()
    agent = {"pool": "cqa/pool", "qwen_list": "cqa/pool-rr"}[a.arm]
    qs = {q["qid"]: q for q in map(json.loads, open(os.path.join(a.probe, "questions.jsonl")))}
    subset = json.load(open(os.path.join(a.probe, "reader_subset.json")))
    os.makedirs(os.path.join(a.probe, "results"), exist_ok=True)
    path = os.path.join(a.probe, "results", a.arm + ".jsonl")
    done = {json.loads(l)["qid"] for l in open(path)} if os.path.exists(path) else set()
    with open(path, "a") as f:
        for qid in subset:
            if qid in done:
                continue
            t0 = time.time()
            for attempt in range(4):
                try:
                    r = lc.run(agent, "cqa-header", qs[qid]["query"])
                    break
                except Exception as e:
                    err = e
                    time.sleep(10 * (attempt + 1))
            else:
                raise SystemExit("%s: %s" % (qid, err))
            f.write(json.dumps({"qid": qid, "ranked": [c["chunk_id"] for c in r.get("chunks", [])],
                                "reranked": r.get("reranked"), "rerank_reason": r.get("rerank_reason"),
                                "ms": int((time.time() - t0) * 1000)}) + "\n")
            f.flush()
    print("done", a.arm, flush=True)


if __name__ == "__main__":
    main()
