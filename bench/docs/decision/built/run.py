"""Search every ConditionalQA question through both arms; record order, report and time.

    LC_BASE=<server> python3 run.py --probe <dir>
      -> <dir>/results/built.jsonl
         {qid, none: {ranked, ms}, decision: {ranked, reranked, rerank_reason, ms}}

Both arms are the shipped `Document op=search` (limit 10) in the rebuilt `cqa-header`
store; `decision` is the same search by an agent with memory_rerank on, served by the
`kind: decision` reranker. The two arms of a question run back to back, so the latency
difference is measured under the same load. Resumable.
"""
import argparse, json, os, sys, time

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "measure"))
import lc  # noqa: E402


def timed(agent, query):
    for attempt in range(4):
        try:
            t0 = time.time()
            r = lc.run(agent, "cqa-header", query)
            return r, int((time.time() - t0) * 1000)
        except Exception as e:  # a transport fault retries; the arm never records a guess
            err = e
            time.sleep(10 * (attempt + 1))
    raise SystemExit("%s: %s" % (agent, err))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--probe", required=True)
    a = ap.parse_args()
    qs = [json.loads(l) for l in open(os.path.join(a.probe, "questions.jsonl"))]
    os.makedirs(os.path.join(a.probe, "results"), exist_ok=True)
    path = os.path.join(a.probe, "results", "built.jsonl")
    done = {json.loads(l)["qid"] for l in open(path)} if os.path.exists(path) else set()
    t_all, n = time.time(), 0
    with open(path, "a") as f:
        for q in qs:
            if q["qid"] in done:
                continue
            none, ms0 = timed("cqa/search", q["query"])
            rr, ms1 = timed("cqa/search-rr", q["query"])
            f.write(json.dumps({"qid": q["qid"],
                                "none": {"ranked": [c["chunk_id"] for c in none.get("chunks", [])], "ms": ms0},
                                "decision": {"ranked": [c["chunk_id"] for c in rr.get("chunks", [])],
                                             "reranked": rr.get("reranked"), "rerank_reason": rr.get("rerank_reason"),
                                             "ms": ms1}}) + "\n")
            f.flush()
            n += 1
            if n % 100 == 0:
                print("built", n, "%.2fs/q" % ((time.time() - t_all) / n), flush=True)
    print("done built", n, flush=True)


if __name__ == "__main__":
    main()
