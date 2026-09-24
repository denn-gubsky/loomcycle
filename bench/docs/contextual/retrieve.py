#!/usr/bin/env python3
"""Ask every question against every arm and record WHERE the evidence ranked.

No model and no judge anywhere here: a question is a hit at rank r when the r-th chunk
retrieved contains one of its gold evidence paragraphs verbatim (whitespace-normalised).
The embedder is the only model involved and it is local.

Query forms:
  pool   "In the paper "<title>": <question>" — QASPER questions say "they" and never name
         the paper (the original task hands the paper over), so a pooled store needs the
         identity in the query, as a real user asking about one document would give it.
  paper  the bare question, against that paper's own scope.

Retrievers:
  hybrid  POST /v1/_memory/search sources=[documents] — vector ∥ full-text fused by RRF,
          the path an agent's recall takes
  vector  Document op=search — cosine only, so the full-text leg's share is visible
"""
import argparse
import json
import os
import re
import subprocess
import urllib.request

WS = re.compile(r"\s+")


def norm(s):
    return WS.sub(" ", s).strip()


def post(base, tok, path, body, scope_id=None):
    url = base + path + ("?scope_id=" + scope_id if scope_id else "")
    r = urllib.request.Request(url, data=json.dumps(body).encode(), method="POST",
                               headers={"Authorization": "Bearer " + tok, "Content-Type": "application/json"})
    with urllib.request.urlopen(r, timeout=300) as x:
        return json.load(x)


def chunk_bodies(db, tenant):
    # Read-only lookup of the stored bodies, for the vector retriever, whose results carry
    # ids only. The bodies are the imported ones, so containment is checked against what
    # was actually indexed rather than against the source text.
    q = ("select substring(key from 11), value->>'body' from memory where tenant_id='%s' "
         "and scope='user' and key like 'doc.chunk:%%'" % tenant)
    out = subprocess.run(["psql", "-d", db, "-tA", "-F", "\x1f", "-R", "\x1e", "-c", q],
                         capture_output=True, text=True, check=True).stdout
    m = {}
    for row in out.split("\x1e"):
        if "\x1f" in row:
            cid, body = row.split("\x1f", 1)
            m[cid.strip()] = body
    return m


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--probe", required=True)
    ap.add_argument("--base", default="http://127.0.0.1:8801")
    ap.add_argument("--db", default="loomcycle_gate")
    ap.add_argument("--tenant", default="qasper-probe")
    ap.add_argument("--arms", default="plain,header,context,both")
    ap.add_argument("--layouts", default="pool,paper")
    ap.add_argument("--retrievers", default="hybrid,vector")
    ap.add_argument("--k", type=int, default=20)
    a = ap.parse_args()
    tok = open(os.path.join(a.probe, "..", ".tok")).read().strip()
    qs = [json.loads(l) for l in open(os.path.join(a.probe, "questions.jsonl"))]
    manifest = [json.loads(l) for l in open(os.path.join(a.probe, "manifest.jsonl"))]
    doc_paper = {m["document_id"]: m["paper_id"] for m in manifest}
    bodies = chunk_bodies(a.db, a.tenant)
    out_path = os.path.join(a.probe, "retrieval.jsonl")
    done = set()
    if os.path.exists(out_path):
        done = {(r["arm"], r["layout"], r["retriever"], r["qid"]) for r in map(json.loads, open(out_path))}
    with open(out_path, "a") as f:
        for arm in a.arms.split(","):
            for layout in a.layouts.split(","):
                for ret in a.retrievers.split(","):
                    for q in qs:
                        if (arm, layout, ret, q["qid"]) in done:
                            continue
                        sid = "%s-pool" % arm if layout == "pool" else "%s-%s" % (arm, q["paper_id"])
                        text = ('In the paper "%s": %s' % (q["paper_title"], q["question"])
                                if layout == "pool" else q["question"])
                        hits = []  # (chunk_id, document_id-or-title)
                        if ret == "hybrid":
                            res = post(a.base, tok, "/v1/_memory/search",
                                       {"query": text, "scope": "user", "scope_id": sid,
                                        "top_k": a.k, "sources": ["documents"]})
                            for e in res.get("entries") or []:
                                cid = e.get("chunk_id") or e["key"][len("doc.chunk:"):]
                                body = json.loads(e["value"]).get("body", "") if isinstance(e["value"], str) else (e["value"] or {}).get("body", "")
                                hits.append((cid, body, e.get("document") == q["paper_title"]))
                        else:
                            res = post(a.base, tok, "/v1/_document",
                                       {"op": "search", "scope": "user", "query": text, "limit": a.k}, sid)
                            for e in res.get("chunks") or []:
                                cid = e["chunk_id"]
                                hits.append((cid, bodies.get(cid, ""), doc_paper.get(e.get("document_id")) == q["paper_id"]))
                        ev = [norm(x) for x in q["evidence"]]
                        # ev_rank_content counts only chunks WITH a body. A document root and
                        # an empty parent heading are embedded from their title, so a query
                        # naming the paper ranks the root first every time — a real property
                        # of the store, but one that hides whether the content chunks moved.
                        ev_rank = paper_rank = ev_rank_content = None
                        content_i = 0
                        compact = []
                        for i, (cid, body, same_paper) in enumerate(hits, 1):
                            has_ev = same_paper and any(x in norm(body) for x in ev)
                            empty = not body.strip()
                            compact.append([int(same_paper), int(has_ev), int(empty)])
                            if not empty:
                                content_i += 1
                            if paper_rank is None and same_paper:
                                paper_rank = i
                            if ev_rank is None and has_ev:
                                ev_rank = i
                                ev_rank_content = content_i
                        f.write(json.dumps({"arm": arm, "layout": layout, "retriever": ret, "qid": q["qid"],
                                            "ev_rank": ev_rank, "ev_rank_content": ev_rank_content,
                                            "paper_rank": paper_rank, "n_hits": len(hits), "hits": compact}) + "\n")
                    f.flush()
                    print("scored", arm, layout, ret, flush=True)


if __name__ == "__main__":
    main()
