"""Answer the reader subset from each arm's top 5 readable sections; record the answers.

    python3 answer.py --probe <dir> --arm <oracle|plain|header|header_rr> --pass-no <1|2|3>
      -> <dir>/answers/<arm>-p<N>.jsonl  {qid, answer}

The reader sees every excerpt rendered the same way in every arm, from the corpus, not from
the store: "<page title> > <heading path>" and the section text. The plain store's numbered
titles exist only so its index carries no header; what an agent reads is the real page. So
arms differ only in WHICH sections were retrieved. A bodyless heading has nothing to read
and is skipped, in rank order, as an agent reading these results would skip it.

`oracle` hands the reader the gold sections (at most 5): the reader's ceiling here.
Direct Ollama calls, no path to a paid model; a transport fault retries and aborts after 4.
"""
import argparse, json, os, time, urllib.request

PROMPT = """Answer the question using only the excerpts below from UK government guidance.

{excerpts}

Situation: {scenario}
Question: {question}

Answer as briefly as possible: a short phrase, preferably copied from the excerpts. If the question is a yes/no question, answer only "Yes" or "No". If the excerpts do not contain the answer, answer "Unanswerable". Give only the answer."""

SEEDS = {1: 0, 2: 11, 3: 23}
STORE = {"plain": "plain", "header": "header", "header_rr": "header"}


def ollama(host, model, prompt, seed):
    body = {"model": model, "stream": False, "think": False,
            "options": {"num_ctx": 16384, "temperature": 0, "seed": seed, "num_predict": 64},
            "messages": [{"role": "user", "content": prompt}]}
    r = urllib.request.Request(host + "/api/chat", data=json.dumps(body).encode(),
                               headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(r, timeout=900) as x:
        return json.load(x)["message"]["content"].strip()


def readable(probe, arm, docs, k=5):
    """qid -> the first k (doc, section index) pairs with text, in the arm's rank order."""
    cmap = json.load(open(os.path.join(probe, "chunks-%s.json" % STORE[arm])))
    where = {cid: (did, i) for did, ids in cmap.items() for i, cid in enumerate(ids)}
    out = {}
    for r in map(json.loads, open(os.path.join(probe, "results", arm + ".jsonl"))):
        picks = []
        for cid in r["ranked"]:
            w = where.get(cid)
            if w and docs[w[0]]["sections"][w[1]]["text"].strip():
                picks.append(w)
            if len(picks) == k:
                break
        out[r["qid"]] = picks
    return out


def render(docs, picks):
    parts = []
    for n, (did, i) in enumerate(picks, 1):
        d = docs[did]
        s = d["sections"][i]
        where = " > ".join([d["title"]] + s["path"])
        parts.append("[%d] %s\n%s" % (n, where, s["text"]))
    return "\n\n".join(parts)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--probe", required=True)
    ap.add_argument("--arm", required=True, choices=["oracle", "plain", "header", "header_rr"])
    ap.add_argument("--pass-no", type=int, default=1, choices=[1, 2, 3])
    ap.add_argument("--host", default="http://100.112.7.68:11434")
    ap.add_argument("--model", default="qwen3.8:latest")
    a = ap.parse_args()
    docs = {d["id"]: d for d in map(json.loads, open(os.path.join(a.probe, "docs.jsonl")))}
    qs = {q["qid"]: q for q in map(json.loads, open(os.path.join(a.probe, "questions.jsonl")))}
    subset = json.load(open(os.path.join(a.probe, "reader_subset.json")))
    picks = None if a.arm == "oracle" else readable(a.probe, a.arm, docs)
    os.makedirs(os.path.join(a.probe, "answers"), exist_ok=True)
    path = os.path.join(a.probe, "answers", "%s-p%d.jsonl" % (a.arm, a.pass_no))
    done = {json.loads(l)["qid"] for l in open(path)} if os.path.exists(path) else set()
    t0, n = time.time(), 0
    with open(path, "a") as f:
        for qid in subset:
            if qid in done:
                continue
            q = qs[qid]
            p = [(q["doc_id"], i) for i in q["gold"][:5]] if picks is None else picks[qid]
            prompt = PROMPT.format(excerpts=render(docs, p) or "(no excerpts)",
                                   scenario=q["scenario"], question=q["question"])
            for attempt in range(4):
                try:
                    ans = ollama(a.host, a.model, prompt, SEEDS[a.pass_no])
                    break
                except Exception as e:
                    err = e
                    time.sleep(15 * (attempt + 1))
            else:
                raise SystemExit("%s: %s" % (qid, err))
            f.write(json.dumps({"qid": qid, "answer": ans}) + "\n")
            f.flush()
            n += 1
            if n % 100 == 0:
                print(a.arm, "p%d" % a.pass_no, n, "%.2fs/q" % ((time.time() - t0) / n), flush=True)
    print("done", a.arm, "p%d" % a.pass_no, n, flush=True)


if __name__ == "__main__":
    main()
