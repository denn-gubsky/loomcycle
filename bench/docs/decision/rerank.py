"""Rerank each pool with a decision model on Ollama's /v1/systemone; record order, scores, time.

    python3 rerank.py --probe <dir> --arm <nimble_choice|nimble_point|tev1_point>
      -> <dir>/results/<arm>.jsonl
         {qid, order: [chunk ids], scores: [per candidate, pool order], ms, input_tokens, max_chars}

Every candidate is shown as the shipped rerank shows it: its INDEX TEXT, rendered from the
corpus as "<page title> — <heading path>" + newline + text (the header alone for a bodyless
heading), truncated to 1,200 characters (code points, like the shipped truncation).

- choice: ONE call per question. The candidates are the options A, B, C, ... (in pool
  order), the question is the state, and the order is by option probability. nimble scores
  the whole prompt within 8,192 tokens; a request refused for size is retried with every
  candidate cut to 600, then 300 characters, and the cut is recorded.
- point: ONE `noul` call per (question, candidate) pair, 4 in flight; the order is by the
  probability that the passage answers the question. `ms` is the wall time of the
  question's 20 calls.

Ties keep pool order. A transport fault or runner crash retries with backoff; a question
that still fails aborts the run, so no arm records a guess.
"""
import argparse, json, os, time, urllib.error, urllib.request
from concurrent.futures import ThreadPoolExecutor

HOST = os.environ.get("JEV_HOST", "http://100.109.53.100:11434")  # TrueNAS over Tailscale
ARMS = {"nimble_choice": ("nimble", "choice"), "nimble_point": ("nimble", "point"),
        "tev1_point": ("tev1:4b", "point")}
CHOICE = "Which passage best answers the question?"
POINT = "Does the passage contain the answer to the question?"
LETTERS = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"


def post(body, attempts=6):
    for i in range(attempts):
        try:
            req = urllib.request.Request(HOST + "/v1/systemone", data=json.dumps(body).encode(),
                                         headers={"Content-Type": "application/json"})
            return json.load(urllib.request.urlopen(req, timeout=900))
        except urllib.error.HTTPError as e:
            msg = e.read().decode(errors="replace")
            if e.code == 400:  # a refusal, not a fault: the caller decides
                raise ValueError(msg)
            err = "%d %s" % (e.code, msg[:200])
        except (urllib.error.URLError, TimeoutError, ConnectionError) as e:
            err = str(e)
        time.sleep(10 * 2 ** i)
    raise RuntimeError(err)


def index_text(doc, i):
    s = doc["sections"][i]
    header = doc["title"] + (" — " + " > ".join(s["path"]) if s["path"] else "")
    return header + ("\n" + s["text"] if s["text"].strip() else "")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--probe", required=True)
    ap.add_argument("--arm", required=True, choices=sorted(ARMS))
    a = ap.parse_args()
    model, kind = ARMS[a.arm]
    P = a.probe
    docs = {d["id"]: d for d in map(json.loads, open(os.path.join(P, "docs.jsonl")))}
    qs = {q["qid"]: q for q in map(json.loads, open(os.path.join(P, "questions.jsonl")))}
    cmap = json.load(open(os.path.join(P, "chunks-header.json")))
    where = {cid: (did, i) for did, ids in cmap.items() for i, cid in enumerate(ids)}
    pools = [json.loads(l) for l in open(os.path.join(P, "results", "pool.jsonl"))]
    path = os.path.join(P, "results", a.arm + ".jsonl")
    done = {json.loads(l)["qid"] for l in open(path)} if os.path.exists(path) else set()
    t_all, n = time.time(), 0
    with open(path, "a") as f, ThreadPoolExecutor(4) as ex:
        for p in pools:
            if p["qid"] in done:
                continue
            query = qs[p["qid"]]["query"]
            ids = p["ranked"]
            full = [index_text(docs[where[c][0]], where[c][1]) for c in ids]
            t0, tokens, max_chars = time.time(), 0, 1200
            if kind == "choice":
                while True:
                    crit = {LETTERS[j]: t[:max_chars] for j, t in enumerate(full)}
                    try:
                        r = post({"model": model, "state": {"question": query},
                                  "questions": {"best": {"type": "choice", "instructions": CHOICE, "criteria": crit}}})
                        break
                    except ValueError:
                        if max_chars == 300:
                            raise
                        max_chars //= 2
                probs = r["answers"]["best"]["probabilities"]
                scores = [float(probs.get(LETTERS[j], 0.0)) for j in range(len(ids))]
                tokens = r["usage"]["input_tokens"]
            else:
                def one(t):
                    return post({"model": model, "state": {"question": query, "passage": t[:max_chars]},
                                 "questions": {"rel": {"type": "noul", "instructions": POINT}}})
                rs = list(ex.map(one, full))
                scores = [float(x["answers"]["rel"]["noul"]) for x in rs]
                tokens = sum(x["usage"]["input_tokens"] for x in rs)
            ms = int((time.time() - t0) * 1000)
            order = [ids[j] for j in sorted(range(len(ids)), key=lambda j: (-scores[j], j))]
            f.write(json.dumps({"qid": p["qid"], "order": order, "scores": scores, "ms": ms,
                                "input_tokens": tokens, "max_chars": max_chars}) + "\n")
            f.flush()
            n += 1
            if n % 50 == 0:
                print(a.arm, n, "%.2fs/q" % ((time.time() - t_all) / n), flush=True)
    print("done", a.arm, n, flush=True)


if __name__ == "__main__":
    main()
