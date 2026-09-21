#!/usr/bin/env python3
"""QPP probe: does the SHAPE of a retrieved set say whether the question is answerable?

Paired design. Each question is searched against BOTH conversation stores: its own
(the answer IS there) and the other one (it is NOT). The same question therefore
appears once as a positive and once as a negative, so phrasing and length cancel
exactly rather than being controlled for.

Retrieval only — the single model call is the query embedding.
"""
import json, sys, urllib.request, concurrent.futures as cf

BASE, TOK, STORE, OUT = sys.argv[1], open(sys.argv[2]).read().strip(), sys.argv[3], sys.argv[4]
TOPK = 24  # recallTraceTopK — the probe must see exactly what the grant attaches

def search(q):
    body = json.dumps({"query": q, "scope": "user", "scope_id": "bench",
                       "top_k": TOPK, "sources": ["traces"]}).encode()
    req = urllib.request.Request(BASE + "/v1/_memory/search", data=body,
        headers={"Authorization": "Bearer " + TOK, "Content-Type": "application/json"})
    for attempt in range(3):
        try:
            with urllib.request.urlopen(req, timeout=120) as r:
                d = json.load(r)
            es = d.get("entries", [])
            return {"q": q, "n": len(es),
                    "scores": [e["score"] for e in es],
                    "ranks": [e.get("rank_score", 0.0) for e in es],
                    "texts": [json.dumps(e.get("value"))[:1500] for e in es]}
        except Exception as ex:
            if attempt == 2:
                return {"q": q, "n": 0, "error": str(ex), "scores": [], "ranks": [], "texts": []}

jobs = []
for tag, path in (("conv26", sys.argv[5]), ("conv30", sys.argv[6])):
    for line in open(path):
        q = line.strip()
        if q:
            jobs.append((tag, q))

rows = []
with cf.ThreadPoolExecutor(max_workers=8) as ex:
    futs = {ex.submit(search, q): (tag, q) for tag, q in jobs}
    for i, f in enumerate(cf.as_completed(futs), 1):
        tag, q = futs[f]
        r = f.result(); r["qset"] = tag; r["store"] = STORE
        rows.append(r)
        if i % 50 == 0:
            print(f"  {i}/{len(jobs)}", flush=True)

with open(OUT, "w") as fh:
    for r in rows:
        fh.write(json.dumps(r) + "\n")
errs = sum(1 for r in rows if r.get("error"))
empty = sum(1 for r in rows if r["n"] == 0)
print(f"{STORE}: {len(rows)} searches, {errs} errors, {empty} empty -> {OUT}")
