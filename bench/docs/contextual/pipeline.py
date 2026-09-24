#!/usr/bin/env python3
"""Retrieve → resolve → read, for the description-based arms. One row per (arm, question)
with the evidence rank of what the READER sees and its Answer-F1.

A hit is not always what a reader should get, which is what these arms test:

  header    control, re-run through this code: hits are the section chunks themselves
  map       a hit on the document map shows the reader the map itself
  map+hop   a hit on the map follows its `describes` links to the two sections whose
            descriptions best match the question (bge-m3 cosine), instead of the map
  desc      a hit on a description resolves to the section it describes — the reader gets
            the source, as a fact resolves to its source span
  mapctx    header + map-based context in the index, context hidden from the reader
  mapall    mapctx's index plus the map in the root, shown to the reader as in `map`
  both      header + WHOLE-PAPER context in the index, hidden from the reader — the
            Anthropic arm, for comparison with the map-sourced context

Sections are deduplicated after resolution, so a description and its own section hitting
together occupy one slot. The first 5 go to the reader; up to 10 are kept for recall.
"""
import argparse
import json
import math
import os
import sys
import time
import urllib.request

import answer
import retrieve


def embed(host, texts):
    r = urllib.request.Request(host + "/api/embed", data=json.dumps({"model": "bge-m3", "input": texts}).encode(),
                               headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(r, timeout=300) as x:
        return json.load(x)["embeddings"]


def cos(a, b):
    na, nb = math.sqrt(sum(x * x for x in a)), math.sqrt(sum(x * x for x in b))
    return sum(x * y for x, y in zip(a, b)) / (na * nb) if na and nb else 0.0


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--probe", required=True)
    ap.add_argument("--base", default="http://127.0.0.1:8801")
    ap.add_argument("--host", default="http://100.112.7.68:11434")
    ap.add_argument("--model", default="ornith-1.5:35b")
    ap.add_argument("--arms", default="header,map,map+hop,desc,mapctx")
    ap.add_argument("--k", type=int, default=5)
    ap.add_argument("--hops", type=int, default=2)
    # A separate file per pass: the reader is NOT deterministic when another job shares
    # the GPU (Ollama batches concurrent requests), so a clean comparison is a fresh pass
    # on an idle GPU, and a repeated pass is how that is checked.
    ap.add_argument("--out", default="answers2.jsonl")
    # The preceding prompt shares a prefix with the next one, and Ollama's prefix cache makes
    # the result depend on it, so passes that are meant to sample reader noise shuffle it.
    ap.add_argument("--shuffle-seed", type=int, default=0)
    a = ap.parse_args()
    tok = open(os.path.join(a.probe, "..", ".tok")).read().strip()
    qs = [json.loads(l) for l in open(os.path.join(a.probe, "questions.jsonl"))]
    if a.shuffle_seed:
        import random
        random.Random(a.shuffle_seed).shuffle(qs)
    papers = {p["id"]: p for p in map(json.loads, open(os.path.join(a.probe, "papers.jsonl")))}
    desc = {(r["paper_id"], tuple(r["path"])): r["context"] for r in map(json.loads, open(os.path.join(a.probe, "describe.jsonl")))}
    def ctx_set(name):
        path = os.path.join(a.probe, name)
        return {json.loads(l)["context"] for l in open(path)} if os.path.exists(path) else set()
    # The context each index prepended, which the reader must not see.
    hidden = {"mapctx": ctx_set("mapcontext.jsonl"), "mapall": ctx_set("mapcontext.jsonl"),
              "both": ctx_set("contexts.jsonl")}
    struct = {}  # (arm, paper_id) -> record
    for r in map(json.loads, open(os.path.join(a.probe, "structure.jsonl"))):
        struct[(r["arm"], r["paper_id"])] = r
    bodies = retrieve.chunk_bodies("loomcycle_gate", "qasper-probe")
    # Root → its sections in document order, with their descriptions: the map's links.
    map_links = {}
    for (arm, pid), r in struct.items():
        if arm in ("map", "mapall"):
            p = papers[pid]
            map_links[r["root"]] = [(r["sections"][" > ".join(s["path"])], desc[(pid, tuple(s["path"]))]) for s in p["sections"]]
    desc_of = {cid: sec for r in struct.values() for cid, sec in r["desc_of"].items()}
    link_vecs = {}

    out_path = os.path.join(a.probe, a.out)
    done = set()
    if os.path.exists(out_path):
        done = {(r["arm"], r["qid"]) for r in map(json.loads, open(out_path))}
    with open(out_path, "a") as f:
        for arm in a.arms.split(","):
            index = arm.split("+")[0]
            sid = index + "-pool"
            t0, n = time.time(), 0
            for q in qs:
                if (arm, q["qid"]) in done:
                    continue
                text = 'In the paper "%s": %s' % (q["paper_title"], q["question"])
                res = answer.post(a.base, tok, "/v1/_memory/search",
                                  {"query": text, "scope": "user", "scope_id": sid, "top_k": 15, "sources": ["documents"]})
                visible, seen, qvec = [], set(), None  # visible: (kind, id, text)
                for e in res.get("entries") or []:
                    cid = e.get("chunk_id")
                    body = (e.get("value") or {}).get("body", "") if isinstance(e.get("value"), dict) else ""
                    if not body.strip():
                        continue
                    if cid in map_links:  # the document map
                        if arm == "map+hop":
                            if qvec is None:
                                qvec = embed(a.host, [text])[0]
                            links = map_links[cid]
                            if cid not in link_vecs:
                                link_vecs[cid] = embed(a.host, [d for _, d in links])
                            ranked = sorted(zip(links, link_vecs[cid]), key=lambda lv: -cos(qvec, lv[1]))
                            added = 0
                            for (sec, _), _v in ranked:
                                if sec in seen:
                                    continue
                                seen.add(sec)
                                visible.append(("hop", sec, bodies.get(sec, "")))
                                added += 1
                                if added == a.hops:
                                    break
                        elif cid not in seen:
                            seen.add(cid)
                            visible.append(("map", cid, body))
                    else:
                        sec = desc_of.get(cid, cid)
                        if sec in seen:
                            continue
                        seen.add(sec)
                        b = bodies.get(sec, "") if sec != cid else body
                        if index in hidden:  # index-only context: hide it, as hidden-ctx does
                            head, sep, rest = b.partition("\n\n")
                            for c in hidden[index]:
                                if c and head.endswith(c):
                                    head = head[: -len(c)].rstrip()
                                    break
                            b = (head + sep + rest) if head else rest
                        visible.append(("desc" if sec != cid else "chunk", sec, b))
                    if len(visible) >= 10:
                        break
                visible = visible[:10]
                ev = [retrieve.norm(x) for x in q["evidence"]]
                ev_rank = next((i for i, (_, _, b) in enumerate(visible, 1) if any(x in retrieve.norm(b) for x in ev)), None)
                excerpts = "\n\n".join("[%d] %s" % (i + 1, b) for i, (_, _, b) in enumerate(visible[: a.k]))
                pred = None
                for attempt in range(4):
                    try:
                        pred = answer.ollama(a.host, a.model, answer.PROMPT.format(excerpts=excerpts, question=q["question"]))
                        break
                    except Exception as e:
                        err = str(e) + " " + (e.read()[:300].decode("utf-8", "replace") if hasattr(e, "read") else "")
                        time.sleep(10 * (attempt + 1))
                if pred is None:
                    print("FAIL", arm, q["qid"], err[:200], file=sys.stderr, flush=True)
                    continue
                score = max((answer.f1(pred, g) for g in answer.gold_strings(q)), default=0.0)
                f.write(json.dumps({"arm": arm, "qid": q["qid"], "pred": pred, "f1": score, "ev_rank": ev_rank,
                                    "kinds": [k for k, _, _ in visible]}) + "\n")
                f.flush()
                n += 1
            print("done", arm, n, "in %.0fs" % (time.time() - t0), flush=True)


if __name__ == "__main__":
    main()
