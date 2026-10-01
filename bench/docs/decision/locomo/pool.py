"""Fetch each LoCoMo question's 20-candidate memory pool, with the turn texts.

    python3 pool.py --convert <dir of locomo -mode=convert jsonl> --data locomo10.json --out pools.jsonl

The corpus is the LoCoMo harness's (`bench/cmd/locomo -mode=ingest -scope user`): one
memory row per turn, keyed by its dia_id, in user scope `locomo-<sample_id>`. Questions
and their evidence come from the harness's own `-mode=convert` output, so the filtering
(categories 1-4, unusable evidence dropped) is the harness's, not re-implemented here;
only the CATEGORY is joined back from the dataset, by (conversation, question).

Each pool is `POST /v1/_memory/search` with top_k 20 and no rerank: the candidates a
rerank would reorder. Resumable.
"""
import argparse, collections, glob, json, os, time, urllib.request

BASE = os.environ.get("LC_BASE", "http://127.0.0.1:8817")


def search(scope_id, query):
    body = {"query": query, "scope": "user", "scope_id": scope_id, "top_k": 20}
    req = urllib.request.Request(BASE + "/v1/_memory/search", data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    return json.load(urllib.request.urlopen(req, timeout=120))


def text_of(value):
    """A stored row's text: the harness stores the turn body as the value."""
    if isinstance(value, str):
        return value
    if isinstance(value, dict):
        for k in ("text", "body", "content", "value"):
            if isinstance(value.get(k), str):
                return value[k]
    return json.dumps(value, ensure_ascii=False)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--convert", required=True)
    ap.add_argument("--data", required=True)
    ap.add_argument("--out", required=True)
    a = ap.parse_args()
    cats = collections.defaultdict(dict)
    for s in json.load(open(a.data)):
        for q in s.get("qa", []):
            cats["locomo-" + s["sample_id"]].setdefault(q["question"].strip(), q.get("category"))
    done = {json.loads(l)["qid"] for l in open(a.out)} if os.path.exists(a.out) else set()
    with open(a.out, "a") as f:
        for path in sorted(glob.glob(os.path.join(a.convert, "*.jsonl"))):
            lines = [json.loads(l) for l in open(path)]
            scope_id = lines[0]["name"]
            for i, q in enumerate(lines[1:]):
                qid = "%s#%d" % (scope_id, i)
                if qid in done:
                    continue
                for attempt in range(4):
                    try:
                        r = search(scope_id, q["query"])
                        break
                    except Exception as e:
                        err = e
                        time.sleep(10 * (attempt + 1))
                else:
                    raise SystemExit("%s: %s" % (qid, err))
                ents = r.get("entries", [])
                f.write(json.dumps({"qid": qid, "conv": scope_id, "query": q["query"], "expected": q["expected"],
                                    "category": cats[scope_id].get(q["query"].strip()),
                                    "keys": [e["key"] for e in ents],
                                    "texts": [text_of(e.get("value")) for e in ents]}, ensure_ascii=False) + "\n")
                f.flush()
    print("done pools", flush=True)


if __name__ == "__main__":
    main()
