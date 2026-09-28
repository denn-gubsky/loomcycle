"""Ingest every policy as a Document in its own user scope, named under /policies, and
record which chunk holds which segment.

    python3 ingest.py --probe <dir>        -> <dir>/chunks.json {policy_id: [chunk id per segment]}
"""
import argparse, json, os
import lc


def doc(pid, payload):
    return lc.run("pqa/writer", pid, json.dumps(payload))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--probe", required=True)
    a = ap.parse_args()
    out = {}
    for line in open(os.path.join(a.probe, "policies.jsonl")):
        p = json.loads(line)
        r = doc(p["id"], {"op": "import_md", "scope": "user", "markdown": p["markdown"]})
        doc(p["id"], {"op": "set_path", "scope": "user", "id": r["document_id"], "path": "/policies/" + p["id"]})
        rows = doc(p["id"], {"op": "query_chunks", "scope": "user", "document_id": r["document_id"], "limit": 1000})["chunks"]
        by_title = {c["title"]: c["id"] for c in rows}
        ids = [by_title.get(str(i + 1)) for i in range(len(p["segments"]))]
        missing = [i for i, c in enumerate(ids) if c is None]
        if missing:
            raise SystemExit("%s: segments %s have no chunk" % (p["id"], missing))
        out[p["id"]] = ids
        print(p["id"], p["site"], len(ids), "segments", flush=True)
    json.dump(out, open(os.path.join(a.probe, "chunks.json"), "w"))


if __name__ == "__main__":
    main()
