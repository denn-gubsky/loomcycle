"""Import the 652 ConditionalQA pages into the tenant at /cqa/<slug> and map sections to chunks.

    python3 -I bench/docs/kbrag/ingest.py --only cqa-001,cqa-002,cqa-003
    python3 -I bench/docs/kbrag/ingest.py                 # every page; resumable
    python3 -I bench/docs/kbrag/ingest.py --verify        # count + reindex dry run only

Only `header_md` is imported. Each imported tree is read back depth first (by
position) and its titles are checked against prepare.py's sections, so a page
import_md built differently is recorded as a failure instead of mis-scored.
Outputs (under /home/denn/workspace/kbrag-work/cqa/): docmap.json {cqa-NNN: document_id},
chunks.json {cqa-NNN: [chunk id per section index]}, ingest.jsonl (one line per page),
ingest_failures.json.
"""
import argparse, json, os, sys, threading, time
from concurrent.futures import ThreadPoolExecutor
from urllib.parse import urlparse
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import lc

WORK = "/home/denn/workspace/kbrag-work/cqa"
DOCS = os.path.join(WORK, "prepared", "docs.jsonl")
ROOT = "/cqa"
lock = threading.Lock()


def slugs(docs):
    out, seen = {}, set()
    for d in docs:
        s = "-".join(p for p in urlparse(d["url"]).path.split("/") if p).lower() or d["id"]
        if s in seen:
            s = s + "-" + d["id"]
        seen.add(s)
        out[d["id"]] = s
    return out


def tree_order(document_id, root_id):
    rows = lc.doc("query_chunks", scope="tenant",
                  sql="SELECT id, parent_id, position, title FROM chunks WHERE document_id = '%s'" % document_id)
    recs = [dict(zip(rows["columns"], r)) for r in rows["rows"]]
    kids = {}
    for r in recs:
        kids.setdefault(r["parent_id"] or "", []).append(r)
    out, stack = [], [next(r for r in recs if r["id"] == root_id)]
    while stack:
        r = stack.pop()
        out.append(r)
        stack.extend(sorted(kids.get(r["id"], []), key=lambda c: c["position"], reverse=True))
    return out


def existing(path):
    try:
        return lc.doc("get_document", scope="tenant", path=path)
    except lc.HTTPFail as e:
        if e.status == 422 and "no such path" in e.body:
            return None
        raise


def one(d, path):
    t0 = time.time()
    cur = existing(path)
    imported = False
    if cur is None:
        r = lc.doc("import_md", scope="tenant", path=path, markdown=d["header_md"])
        doc_id, root = r["document_id"], r["root_chunk_id"]
        imported = True
    else:
        doc_id = cur.get("document_id") or cur.get("id")
        root = cur.get("root_chunk_id")
    order = tree_order(doc_id, root)
    want = [d["title"]] + [s["path"][-1] for s in d["sections"][1:]]
    got = [c["title"] for c in order]
    rec = {"id": d["id"], "path": path, "document_id": doc_id, "imported": imported,
           "sections": len(want), "chunks": len(got), "wall_s": round(time.time() - t0, 2)}
    if got != want:
        rec["error"] = "tree mismatch: want %s got %s" % (want[:6], got[:6])
        return rec, None
    return rec, [c["id"] for c in order]


def load(name, default):
    p = os.path.join(WORK, name)
    return json.load(open(p)) if os.path.exists(p) else default


def save(name, obj):
    p = os.path.join(WORK, name)
    json.dump(obj, open(p + ".tmp", "w")); os.replace(p + ".tmp", p)


def count_under_root():
    n, cursor = 0, None
    while True:
        kw = {"scope": "tenant", "path": ROOT}
        if cursor: kw["cursor"] = cursor
        r = lc.path_op("ls", **kw)
        n += sum(1 for e in r.get("entries", []) if e.get("kind") != "directory")
        cursor = r.get("next_cursor")
        if not cursor:
            return n


def verify():
    print("documents under %s: %d" % (ROOT, count_under_root()))
    r = lc.doc("query_chunks", scope="tenant", sql="SELECT count(DISTINCT document_id), count(*) FROM chunks")
    print("tenant documents, chunks (all, incl. housekeeping):", r["rows"][0])
    tot, after = {"examined": 0, "up_to_date": 0, "reindexed": 0, "unindexed": 0, "failed": 0}, ""
    try:
        while True:
            q = "/v1/_document/reindex?scope=tenant&dry_run=true" + ("&after=" + after if after else "")
            r = lc.call("POST", q, retry=False)
            for k in tot:
                tot[k] += r.get(k) or 0
            after = r.get("next_cursor") or ""
            if not r.get("more") or not after:
                break
        print("reindex dry run:", tot)
    except lc.HTTPFail as e:
        print("reindex dry run refused:", e)
    try:
        r = lc.path_op("ls", scope="tenant", path="/documents")
        print("/documents entries (auto dirents):", len(r.get("entries", [])))
    except lc.HTTPFail as e:
        print("/documents:", e.body[:200])


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--limit", type=int)
    ap.add_argument("--only")
    ap.add_argument("--workers", type=int, default=4)
    ap.add_argument("--verify", action="store_true")
    a = ap.parse_args()
    if a.verify:
        return verify()
    docs = [json.loads(l) for l in open(DOCS)]
    slug = slugs(docs)
    if a.only:
        keep = set(a.only.split(","))
        docs = [d for d in docs if d["id"] in keep]
    if a.limit:
        docs = docs[:a.limit]
    docmap, chunks, fails = load("docmap.json", {}), load("chunks.json", {}), load("ingest_failures.json", {})
    todo = [d for d in docs if d["id"] not in chunks]
    print("pages: %d, to do: %d, server %s" % (len(docs), len(todo), lc.version()), flush=True)
    log = open(os.path.join(WORK, "ingest.jsonl"), "a")
    t0 = time.time()

    def work(d):
        try:
            rec, ids = one(d, ROOT + "/" + slug[d["id"]])
        except Exception as e:  # recorded, the rest goes on
            rec, ids = {"id": d["id"], "error": str(e)[:500]}, None
        with lock:
            log.write(json.dumps(rec) + "\n"); log.flush()
            if ids:
                docmap[d["id"]], chunks[d["id"]] = rec["document_id"], ids
                fails.pop(d["id"], None)
                save("docmap.json", docmap); save("chunks.json", chunks)
            else:
                fails[d["id"]] = rec.get("error")
            save("ingest_failures.json", fails)
            print(d["id"], rec.get("chunks"), "chunks", rec.get("wall_s"), "s", rec.get("error") or "", flush=True)

    with ThreadPoolExecutor(max(1, a.workers)) as ex:
        list(ex.map(work, todo))
    print("done: %d mapped, %d failed, %.0f s" % (len(chunks), len(fails), time.time() - t0), flush=True)


if __name__ == "__main__":
    main()
