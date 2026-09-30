"""Import every page into one pooled store per arm, and record which chunk holds which section.

    python3 ingest.py --probe <dir> --store <header|plain>
      -> <dir>/chunks-<store>.json {doc_id: [chunk id per section]}

A store is one user scope (`cqa-header`, `cqa-plain`) holding all 652 pages, so finding
the page is part of every search. The section -> chunk map is read back from the imported
tree (depth first, by position) and every title is checked against prepare.py's, so a
tree import_md built differently aborts the run instead of mis-scoring it. Resumable.
"""
import argparse, json, os
import lc


def doc(scope_id, payload):
    return lc.run("cqa/writer", scope_id, json.dumps(payload))


def tree_order(scope_id, document_id, root_id):
    rows = doc(scope_id, {"op": "query_chunks", "scope": "user",
                          "sql": "SELECT id, parent_id, position, title FROM chunks WHERE document_id = '%s'" % document_id})
    cols = rows["columns"]
    recs = [dict(zip(cols, r)) for r in rows["rows"]]
    kids = {}
    for r in recs:
        kids.setdefault(r["parent_id"] or "", []).append(r)
    out, stack = [], [next(r for r in recs if r["id"] == root_id)]
    while stack:
        r = stack.pop()
        out.append(r)
        stack.extend(sorted(kids.get(r["id"], []), key=lambda c: c["position"], reverse=True))
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--probe", required=True)
    ap.add_argument("--store", required=True, choices=["header", "plain"])
    a = ap.parse_args()
    scope_id = "cqa-" + a.store
    path = os.path.join(a.probe, "chunks-%s.json" % a.store)
    out = json.load(open(path)) if os.path.exists(path) else {}
    for line in open(os.path.join(a.probe, "docs.jsonl")):
        d = json.loads(line)
        if d["id"] in out:
            continue
        r = doc(scope_id, {"op": "import_md", "scope": "user", "markdown": d[a.store + "_md"]})
        order = tree_order(scope_id, r["document_id"], r["root_chunk_id"])
        want = [d["title"] if a.store == "header" else d["id"][4:]] + \
               [s["path"][-1] if a.store == "header" else str(i) for i, s in enumerate(d["sections"][1:], 1)]
        got = [c["title"] for c in order]
        if got != want:
            raise SystemExit("%s: the imported tree does not match the sections\n want %s\n got  %s" % (d["id"], want[:8], got[:8]))
        out[d["id"]] = [c["id"] for c in order]
        json.dump(out, open(path, "w"))
        print(a.store, d["id"], len(order), "sections", flush=True)
    print("done", a.store, len(out), "pages", flush=True)


if __name__ == "__main__":
    main()
