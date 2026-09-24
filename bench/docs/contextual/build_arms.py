#!/usr/bin/env python3
"""Build the description-based arms on top of the `header` import (pooled layout only).

  map     header chunks; the document ROOT body becomes the document map — title plus every
          section's description in order — with a `describes` edge from the root to each
          section. The root used to be empty and was embedded from the title alone, so it
          ranked first for any query naming the paper with nothing to read.
  desc    header chunks; each section gets a CHILD chunk holding its description (with the
          header, so it too names its document), plus a `describes` edge child → section. A
          search that hits the description resolves to the section it came from: the light
          analogue of a fact carrying provenance to its source span.
  mapctx  header + a context generated from the map instead of the whole paper
          (contextualize.py --mode mapcontext), prepended for indexing only.
  mapall  mapctx's chunks AND map's root: the map-based context for retrieval, the map
          itself for the reader.

Records chunk ids in structure.jsonl so the pipeline can resolve a hit to what the reader
should see, without re-deriving it from the store.
"""
import argparse
import json
import os
import urllib.request

import ingest  # arm_markdown / prefix, so the header is byte-identical to the header arm


def call(base, tok, sid, body):
    r = urllib.request.Request(base + "/v1/_document?scope_id=" + sid, data=json.dumps(body).encode(),
                               method="POST", headers={"Authorization": "Bearer " + tok,
                                                       "Content-Type": "application/json"})
    with urllib.request.urlopen(r, timeout=600) as x:
        d = json.load(x)
    if isinstance(d, dict) and d.get("error"):
        raise SystemExit("%s failed: %s" % (body.get("op"), json.dumps(d)[:300]))
    return d


def section_ids(base, tok, sid, doc_id, paper):
    """Map each section's heading path to its chunk id by walking parents from the root."""
    rows = call(base, tok, sid, {"op": "query_chunks", "scope": "user", "document_id": doc_id, "limit": 1000})["chunks"]
    by_id = {r["id"]: r for r in rows}
    root = next(r["id"] for r in rows if not r.get("parent_id"))
    out = {}
    for r in rows:
        path, cur = [], r
        while cur.get("parent_id"):
            path.append(cur["title"])
            cur = by_id[cur["parent_id"]]
        out[tuple(reversed(path))] = r["id"]
    missing = [s["path"] for s in paper["sections"] if tuple(s["path"]) not in out]
    if missing:
        raise SystemExit("paper %s: no chunk for paths %s" % (paper["id"], missing[:3]))
    return root, out


def load(path):
    return {(r["paper_id"], tuple(r["path"])): r["context"] for r in map(json.loads, open(path))}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--probe", required=True)
    ap.add_argument("--base", default="http://127.0.0.1:8801")
    ap.add_argument("--arms", default="map,desc,mapctx")
    ap.add_argument("--scope-prefix", default="", help="e.g. smoke- for a throwaway build")
    ap.add_argument("--papers", type=int, default=0, help="first N papers only (0 = all)")
    a = ap.parse_args()
    tok = open(os.path.join(a.probe, "..", ".tok")).read().strip()
    papers = [json.loads(l) for l in open(os.path.join(a.probe, "papers.jsonl"))]
    if a.papers:
        papers = papers[: a.papers]
    desc = load(os.path.join(a.probe, "describe.jsonl"))
    struct_path = os.path.join(a.probe, a.scope_prefix + "structure.jsonl")
    done = set()
    if os.path.exists(struct_path):
        done = {(r["arm"], r["paper_id"]) for r in map(json.loads, open(struct_path))}
    with open(struct_path, "a") as sf:
        for arm in a.arms.split(","):
            sid = a.scope_prefix + arm + "-pool"
            if arm in ("mapctx", "mapall"):
                ctx = load(os.path.join(a.probe, "mapcontext.jsonl"))
            for p in papers:
                if (arm, p["id"]) in done:
                    continue
                if arm in ("mapctx", "mapall"):
                    md = ingest.arm_markdown("both", p, ctx)  # header + " " + map-based context
                else:
                    md = ingest.arm_markdown("header", p, {})
                doc_id = call(a.base, tok, sid, {"op": "import_md", "scope": "user", "markdown": md})["document_id"]
                root, ids = section_ids(a.base, tok, sid, doc_id, p)
                rec = {"arm": arm, "paper_id": p["id"], "document_id": doc_id, "root": root,
                       "sections": {" > ".join(k): v for k, v in ids.items()}, "desc_of": {}}
                if arm in ("map", "mapall"):
                    lines = ["Document map of the paper: " + p["title"], ""]
                    for s in p["sections"]:
                        lines.append("- %s: %s" % (" > ".join(s["path"]), desc[(p["id"], tuple(s["path"]))]))
                    rev = call(a.base, tok, sid, {"op": "get_chunk", "scope": "user", "id": root})["revision"]
                    call(a.base, tok, sid, {"op": "update_chunk", "scope": "user", "id": root,
                                            "body": "\n".join(lines), "revision": rev})
                    for s in p["sections"]:
                        call(a.base, tok, sid, {"op": "link_chunks", "scope": "user", "from_id": root,
                                                "to_id": ids[tuple(s["path"])], "kind": "describes"})
                elif arm == "desc":
                    for s in p["sections"]:
                        sec = ids[tuple(s["path"])]
                        body = ingest.prefix("header", p, s, "") + " " + desc[(p["id"], tuple(s["path"]))]
                        c = call(a.base, tok, sid, {"op": "create_chunk", "scope": "user", "document_id": doc_id,
                                                    "parent_id": sec, "title": "Summary: " + s["path"][-1],
                                                    "type": "summary", "body": body})
                        cid = c.get("id") or c.get("chunk_id")
                        call(a.base, tok, sid, {"op": "link_chunks", "scope": "user", "from_id": cid,
                                                "to_id": sec, "kind": "describes"})
                        rec["desc_of"][cid] = sec
                sf.write(json.dumps(rec) + "\n")
                sf.flush()
            print("built", arm, flush=True)


if __name__ == "__main__":
    main()
