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
  claims / questions / units
            mapall plus atomic claims / answerable questions / both as child chunks. A unit
            hit resolves to its section, shown with the matched unit above it — the memory
            pattern: find the small unit, mix the source into the reader's context
  <arm>/nomatch  a unit hit shows the section WITHOUT the `Matched:` line
  <arm>/pinmap   the reader always gets the map of the top result's document, then 4
                 sections — the map kept in context however the ranking went
  <arm>+rr  that arm's first 20 resolved items, reordered by the local model (one listwise
            call per question), then the top 5 to the reader. An unparseable ranking keeps
            the original order and is counted (rr_ok=false)

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


RERANK = """{qline}

Candidate passages:

{cands}

Rank the passages by how useful they are for answering the question, most useful first. Answer only with a JSON array of passage numbers, e.g. [3, 1, 7], listing at least the 8 most useful."""


def rerank(host, model, q, items, titled=True):
    """Listwise rerank. Returns (order, ok): indexes into items. A ranking the model gets
    wrong is repaired, not trusted — unknown or repeated numbers are dropped and anything
    it left out keeps its original relative order after the ranked ones."""
    cands = "\n\n".join("[%d] %s%s" % (i + 1, "(document map) " if k == "map" else "", b[:1200])
                         for i, (k, _, b) in enumerate(items))
    qline = (('Question about the paper "%s": %s' % (q["paper_title"], q["question"])) if titled
             else "Question: " + q["question"])
    prompt = RERANK.format(qline=qline, cands=cands)
    out = None
    for attempt in range(6):  # transport faults (a CUDA 500, a dropped socket) retry; only a
        try:                  # bad RANKING falls back, so a fault is never counted as one
            out = answer.ollama(host, model, prompt)
            break
        except Exception:
            time.sleep(30 * (attempt + 1))
    if out is None:
        raise RuntimeError("rerank: model unreachable after 6 attempts")
    try:
        arr = json.loads(out[out.index("["): out.index("]") + 1])
        order = []
        for x in arr:
            if isinstance(x, int) and 1 <= x <= len(items) and x - 1 not in order:
                order.append(x - 1)
        if not order:
            raise ValueError("empty ranking")
        return order + [i for i in range(len(items)) if i not in order], True
    except Exception:
        return list(range(len(items))), False


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
    ap.add_argument("--limit-q", type=int, default=0, help="first N questions only (smoke tests)")
    ap.add_argument("--tenant", default="qasper-probe", help="the tenant the probe's token belongs to")
    # QASPER questions never name their paper, so the pooled query carries the title; NQ is
    # open-domain, where finding the page IS the task, so its questions go in bare.
    ap.add_argument("--query-form", choices=["titled", "bare"], default="titled")
    ap.add_argument("--doc-kind", default="a scientific paper", help='"a Wikipedia article" for NQ')
    a = ap.parse_args()
    tok = open(os.path.join(a.probe, "..", ".tok")).read().strip()
    qs = [json.loads(l) for l in open(os.path.join(a.probe, "questions.jsonl"))]
    if a.limit_q:
        qs = qs[: a.limit_q]
    if a.shuffle_seed:
        import random
        random.Random(a.shuffle_seed).shuffle(qs)
    papers = {p["id"]: p for p in map(json.loads, open(os.path.join(a.probe, "papers.jsonl")))}
    def jl(name):
        path = os.path.join(a.probe, name)
        return list(map(json.loads, open(path))) if os.path.exists(path) else []
    desc = {(r["paper_id"], tuple(r["path"])): r["context"] for r in jl("describe.jsonl")}
    def ctx_set(name):
        path = os.path.join(a.probe, name)
        return {json.loads(l)["context"] for l in open(path)} if os.path.exists(path) else set()
    # The context each index prepended, which the reader must not see.
    mc = ctx_set("mapcontext.jsonl")
    hidden = {"mapctx": mc, "mapall": mc, "claims": mc, "questions": mc, "units": mc,
              "both": ctx_set("contexts.jsonl")}
    struct = {}  # (arm, paper_id) -> record
    for r in jl("structure.jsonl"):
        struct[(r["arm"], r["paper_id"])] = r
    bodies = retrieve.chunk_bodies("loomcycle_gate", a.tenant)
    # Root → its sections in document order, with their descriptions: the map's links.
    map_links = {}
    for (arm, pid), r in struct.items():
        if arm in ("map", "mapall") or r.get("map_root"):
            p = papers[pid]
            map_links[r["root"]] = [(r["sections"][" > ".join(s["path"])], desc[(pid, tuple(s["path"]))]) for s in p["sections"]]
    desc_of = {cid: sec for r in struct.values() for cid, sec in r["desc_of"].items()}
    unit_text = {cid: t for r in struct.values() for cid, t in (r.get("unit_text") or {}).items()}
    # section → its document's root, for /pinmap (only roots that hold a map)
    sec_root = {sec: r["root"] for r in struct.values() for sec in r["sections"].values() if r["root"] in map_links}
    link_vecs = {}

    out_path = os.path.join(a.probe, a.out)
    done = set()
    if os.path.exists(out_path):
        done = {(r["arm"], r["qid"]) for r in map(json.loads, open(out_path))}
    with open(out_path, "a") as f:
        for arm in a.arms.split(","):
            base_arm, *flag_list = arm.split("/")
            flags = set(flag_list)
            index = base_arm.split("+")[0]
            sid = index + "-pool"
            use_rr = base_arm.endswith("+rr")
            depth = 20 if use_rr else 10
            t0, n, rr_fail = time.time(), 0, 0
            for q in qs:
                if (arm, q["qid"]) in done:
                    continue
                text = ('In the paper "%s": %s' % (q["paper_title"], q["question"])
                        if a.query_form == "titled" else q["question"])
                res = answer.post(a.base, tok, "/v1/_memory/search",
                                  {"query": text, "scope": "user", "scope_id": sid, "top_k": 50, "sources": ["documents"]})
                visible, seen, qvec = [], set(), None  # visible: (kind, id, text)
                for e in res.get("entries") or []:
                    cid = e.get("chunk_id")
                    body = (e.get("value") or {}).get("body", "") if isinstance(e.get("value"), dict) else ""
                    if not body.strip():
                        continue
                    if cid in map_links:  # the document map
                        if base_arm == "map+hop":
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
                        kind = "chunk"
                        if sec != cid:
                            kind = "unit" if cid in unit_text else "desc"
                            if cid in unit_text and "nomatch" not in flags:
                                b = "Matched: " + unit_text[cid] + "\n\n" + b
                        visible.append((kind, sec, b))
                    if len(visible) >= depth:
                        break
                visible = visible[:depth]
                rr_ok = None
                if use_rr and len(visible) > 1:
                    order, rr_ok = rerank(a.host, a.model, q, visible, a.query_form == "titled")
                    rr_fail += 0 if rr_ok else 1
                    visible = [visible[i] for i in order]
                if "pinmap" in flags:
                    secs = [v for v in visible if v[0] != "map"]
                    root = sec_root.get(secs[0][1]) if secs else None
                    visible = ([("map", root, bodies.get(root, ""))] if root else []) + secs
                visible = visible[:10]
                ev = [retrieve.norm(x) for x in q["evidence"]]
                ev_rank = next((i for i, (_, _, b) in enumerate(visible, 1) if any(x in retrieve.norm(b) for x in ev)), None)
                excerpts = "\n\n".join("[%d] %s" % (i + 1, b) for i, (_, _, b) in enumerate(visible[: a.k]))
                pred = None
                for attempt in range(6):
                    try:
                        pred = answer.ollama(a.host, a.model, answer.PROMPT.format(doc_kind=a.doc_kind, excerpts=excerpts, question=q["question"]))
                        break
                    except Exception as e:
                        err = str(e) + " " + (e.read()[:300].decode("utf-8", "replace") if hasattr(e, "read") else "")
                        time.sleep(30 * (attempt + 1))
                if pred is None:
                    print("FAIL", arm, q["qid"], err[:200], file=sys.stderr, flush=True)
                    continue
                score = max((answer.f1(pred, g) for g in answer.gold_strings(q)), default=0.0)
                exact = max((answer.em(pred, g) for g in answer.gold_strings(q)), default=0.0)
                f.write(json.dumps({"arm": arm, "qid": q["qid"], "pred": pred, "f1": score, "em": exact, "ev_rank": ev_rank,
                                    "kinds": [k for k, _, _ in visible], "rr_ok": rr_ok}) + "\n")
                f.flush()
                n += 1
            print("done", arm, n, "in %.0fs" % (time.time() - t0), ("rerank_unparsed=%d" % rr_fail) if use_rr else "", flush=True)


if __name__ == "__main__":
    main()
