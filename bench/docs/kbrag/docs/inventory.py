"""Study D inventory: the tenant's documents, their Path names and their briefs.

    python3 -I bench/docs/kbrag/docs/inventory.py

-> <work>/inventory.json: every document of the tenant scope (id, title, type, status,
created/updated, brief, Path names under /loomcycle) and the corpus (PREREG check 1: the
tenant's documents less the operational types, dconfig). Read-only: query_documents, Path ls.
"""
import json, os, sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import dlc, dconfig  # noqa: E402


def all_documents():
    r = dlc.doc("query_documents", scope="tenant", limit=1000, with_summary=True)
    docs = r.get("documents") or []
    if len(docs) >= 1000:
        raise SystemExit("query_documents returned 1000 rows: the listing may be cut; page it")
    return docs


def tree(path):
    """[(full_path, document_id)] for every document named at or under `path`."""
    out, cursor = [], None
    while True:
        body = {"scope": "tenant", "path": path, "recursive": True}
        if cursor:
            body["cursor"] = cursor
        r = dlc.path_op("ls", **body)
        for e in r.get("entries") or []:
            ref = e.get("resource_ref") or {}
            if e.get("kind") == "document" and ref.get("document_id"):
                out.append((e["full_path"], ref["document_id"]))
        cursor = r.get("next_cursor")
        if not cursor:
            return out


def main():
    dlc.assert_tenant()
    docs = all_documents()
    names = tree(dconfig.CORPUS_ROOT)
    paths = {}
    for p, did in names:
        paths.setdefault(did, []).append(p)
    rows = []
    for d in docs:
        s = d.get("summary") or {}
        rows.append({"id": d["document_id"], "title": d.get("title"), "type": d.get("type"),
                     "status": d.get("status"), "created_at": d.get("created_at"), "updated_at": d.get("updated_at"),
                     "root_chunk_id": d.get("root_chunk_id"), "brief": s.get("brief") if s.get("level") == "document" else None,
                     "paths": sorted(paths.get(d["document_id"], []))})
    corpus = sorted(r["id"] for r in rows if r["type"] not in dconfig.OPERATIONAL_TYPES)
    unknown = sorted(set(paths) - {r["id"] for r in rows})
    os.makedirs(dconfig.WORK, exist_ok=True)
    with open(dconfig.INVENTORY, "w", encoding="utf-8") as f:
        json.dump({"documents": rows, "corpus": corpus, "named_but_unlisted": unknown, "server": dlc.version()}, f,
                  indent=1, ensure_ascii=False)
    types = {}
    for r in rows:
        if r["id"] in corpus:
            types[r["type"]] = types.get(r["type"], 0) + 1
    print(json.dumps({"tenant_documents": len(rows), "named_under_root": len(paths), "corpus": len(corpus),
                      "corpus_with_brief": sum(1 for r in rows if r["id"] in corpus and r["brief"]),
                      "named_but_unlisted": len(unknown), "corpus_types": dict(sorted(types.items()))}, indent=1))


if __name__ == "__main__":
    main()
