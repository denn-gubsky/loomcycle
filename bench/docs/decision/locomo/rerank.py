"""Rerank LoCoMo memory pools with a decision model on Ollama's /v1/systemone.

    JEV_HOST=<ollama> python3 rerank.py --pools pools.jsonl --arm <nimble_choice|nimble_point|tev1_point> --out <arm>.jsonl
      -> {qid, host, order: [keys], scores: [pool order], ms, input_tokens, max_chars}

The same calls as the ConditionalQA decision probe (../rerank.py: the same questions to
the model, the same 1,200-character cut, the same shrink-on-refusal for `choice`, ties in
pool order), over memory rows instead of document sections. A candidate's text is the
stored turn: "[<session date>] <speaker>: <text>". There is no header, because a memory
row has no document.
"""
import argparse, json, os, sys, time
from concurrent.futures import ThreadPoolExecutor

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
import rerank as cqa  # noqa: E402  the probe's client: post(), the question wording, ARMS

LETTERS = cqa.LETTERS


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--pools", required=True)
    ap.add_argument("--arm", required=True, choices=sorted(cqa.ARMS))
    ap.add_argument("--out", required=True)
    a = ap.parse_args()
    model, kind = cqa.ARMS[a.arm]
    done = {json.loads(l)["qid"] for l in open(a.out)} if os.path.exists(a.out) else set()
    t_all, n = time.time(), 0
    with open(a.out, "a") as f, ThreadPoolExecutor(4) as ex:
        for line in open(a.pools):
            p = json.loads(line)
            if p["qid"] in done or len(p["keys"]) < 2:
                continue
            keys, full, query = p["keys"], p["texts"], p["query"]
            t0, max_chars = time.time(), 1200
            if kind == "choice":
                while True:
                    crit = {LETTERS[j]: t[:max_chars] for j, t in enumerate(full)}
                    try:
                        r = cqa.post({"model": model, "state": {"question": query},
                                      "questions": {"best": {"type": "choice", "instructions": cqa.CHOICE, "criteria": crit}}})
                        break
                    except ValueError:
                        if max_chars == 300:
                            raise
                        max_chars //= 2
                probs = r["answers"]["best"]["probabilities"]
                scores = [float(probs.get(LETTERS[j], 0.0)) for j in range(len(keys))]
                tokens = r["usage"]["input_tokens"]
            else:
                def one(t):
                    return cqa.post({"model": model, "state": {"question": query, "passage": t[:max_chars]},
                                     "questions": {"rel": {"type": "noul", "instructions": cqa.POINT}}})
                rs = list(ex.map(one, full))
                scores = [float(x["answers"]["rel"]["noul"]) for x in rs]
                tokens = sum(x["usage"]["input_tokens"] for x in rs)
            order = [keys[j] for j in sorted(range(len(keys)), key=lambda j: (-scores[j], j))]
            f.write(json.dumps({"qid": p["qid"], "host": cqa.HOST, "order": order, "scores": scores,
                                "ms": int((time.time() - t0) * 1000), "input_tokens": tokens,
                                "max_chars": max_chars}) + "\n")
            f.flush()
            n += 1
            if n % 100 == 0:
                print(a.arm, n, "%.2fs/q" % ((time.time() - t_all) / n), flush=True)
    print("done", a.arm, n, flush=True)


if __name__ == "__main__":
    main()
