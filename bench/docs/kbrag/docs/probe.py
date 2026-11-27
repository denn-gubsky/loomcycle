"""Read-only probe of the Study D tenant, for building gold from store listings.

    python3 -I bench/docs/kbrag/docs/probe.py search "<query>" [--annotations only|include] [--limit 20] [--under /loomcycle/rfcs]
    python3 -I bench/docs/kbrag/docs/probe.py doc <document id>          # metadata + document brief and abstract
    python3 -I bench/docs/kbrag/docs/probe.py resolve <path> [<path> ...]   # Path name -> document id
    python3 -I bench/docs/kbrag/docs/probe.py list [--type Rfc] [--status draft] [--under /loomcycle/docs] [--title-contains x] [--created-after 2026-11-01]

Only read ops; prints compact JSON lines (document id, title, type, heading, brief). Runs no
agent and makes no model call (a search embeds the query, which the ledger prices at $0).
"""
import argparse, datetime, json, os, sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import dlc  # noqa: E402


def ns(date):
    return int(datetime.datetime.fromisoformat(date).replace(tzinfo=datetime.timezone.utc).timestamp() * 1e9)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("cmd", choices=["search", "doc", "list", "resolve"])
    ap.add_argument("arg", nargs="?")
    ap.add_argument("more", nargs="*")
    ap.add_argument("--annotations", choices=["only", "include"])
    ap.add_argument("--limit", type=int, default=20)
    ap.add_argument("--under")
    ap.add_argument("--type")
    ap.add_argument("--status")
    ap.add_argument("--title-contains")
    ap.add_argument("--created-after")
    ap.add_argument("--created-before")
    a = ap.parse_args()
    if a.cmd == "search":
        body = {"scope": "tenant", "query": a.arg, "limit": a.limit, "with_summary": True}
        if a.annotations:
            body["annotations"] = a.annotations
        if a.under:
            body["under_path"] = a.under
        r = dlc.doc("search", **body)
        for h in r.get("chunks") or r.get("results") or []:
            s = h.get("summary") or {}
            print(json.dumps({"document_id": h.get("document_id"), "chunk": h.get("title"), "type": h.get("type"),
                              "matched_annotation": h.get("matched_annotation"),
                              "brief": s.get("brief")}, ensure_ascii=False))
    elif a.cmd == "resolve":
        for p in [a.arg] + a.more:
            try:
                r = dlc.path_op("resolve", scope="tenant", path=p)
                ref = r.get("resource_ref") or (r.get("entry") or {}).get("resource_ref") or {}
                print(json.dumps({"path": p, "document_id": ref.get("document_id"), "kind": r.get("kind") or (r.get("entry") or {}).get("kind")}))
            except dlc.HTTPFail as e:
                print(json.dumps({"path": p, "document_id": None, "error": str(e)[:120]}))
    elif a.cmd == "doc":
        r = dlc.doc("get_document", scope="tenant", id=a.arg, with_summary=True)
        keep = {k: r.get(k) for k in ("document_id", "title", "type", "status", "created_at", "summary")}
        print(json.dumps(keep, ensure_ascii=False, indent=1))
    else:
        body = {"scope": "tenant", "limit": 1000, "with_summary": True}
        for k, v in (("type", a.type), ("status", a.status), ("under_path", a.under), ("title_contains", a.title_contains)):
            if v:
                body[k] = v
        if a.created_after:
            body["created_after"] = ns(a.created_after)
        if a.created_before:
            body["created_before"] = ns(a.created_before)
        r = dlc.doc("query_documents", **body)
        for d in r.get("documents") or []:
            print(json.dumps({"document_id": d["document_id"], "title": d.get("title"), "type": d.get("type"),
                              "status": d.get("status"), "brief": (d.get("summary") or {}).get("brief")},
                             ensure_ascii=False))


if __name__ == "__main__":
    main()
