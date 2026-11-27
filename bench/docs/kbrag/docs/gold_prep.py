"""The store listing the walker-question gold is built from (Study D, before any arm run).

    python3 -I bench/docs/kbrag/docs/gold_prep.py

-> <work>/gold/listing.jsonl: one row per corpus document: id, title, type, status,
created (UTC date), paths, document brief, topic concepts (from `memberships`).
-> <work>/gold/topics.json: the `topic` scheme's concepts (name, path, aliases, meaning).

The listing is what the walker README prescribes for gold (query_documents, the briefs,
the taxonomy), read-only, and it holds no output of any arm.
"""
import datetime, json, os, sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import dlc, dconfig  # noqa: E402


def topics_of(mem):
    out = []
    for t in mem.get("taxonomies") or []:
        if (t.get("scheme") or t.get("name")) != "topic":
            continue
        for c in t.get("concepts") or t.get("placements") or []:
            name = "/".join(c["path"]) if c.get("path") else (c.get("concept") or c.get("name"))
            status = sorted({p.get("status") for p in c.get("placements") or []} - {None})
            if name and not c.get("implied"):
                out.append({"concept": name, "status": ",".join(status) or None})
    return out


def main():
    dlc.assert_tenant()
    inv = json.load(open(dconfig.INVENTORY, encoding="utf-8"))
    rows = {d["id"]: d for d in inv["documents"]}
    gdir = os.path.dirname(dconfig.GOLD)
    os.makedirs(gdir, exist_ok=True)
    raw = {}
    with open(os.path.join(gdir, "listing.jsonl"), "w", encoding="utf-8") as f:
        for did in inv["corpus"]:
            d = rows[did]
            mem = dlc.doc("memberships", scope="tenant", document_id=did)
            raw[did] = mem
            created = datetime.datetime.fromtimestamp(d["created_at"] / 1e9, datetime.timezone.utc).strftime("%Y-%m-%d")
            f.write(json.dumps({"id": did, "title": d["title"], "type": d["type"], "status": d["status"],
                                "created": created, "paths": d["paths"], "brief": d["brief"],
                                "topics": topics_of(mem)}, ensure_ascii=False) + "\n")
    with open(os.path.join(gdir, "memberships_raw.json"), "w", encoding="utf-8") as f:
        json.dump(raw, f, ensure_ascii=False)
    ont = json.load(open(os.path.join(dconfig.WORK, "ontology.json"), encoding="utf-8"))
    topic = [s for s in ont.get("schemes") or [] if s.get("name") == "topic"]
    concepts = [{k: c.get(k) for k in ("name", "path", "alt", "meaning", "members")}
                for s in topic for c in s.get("concepts") or []]
    with open(os.path.join(gdir, "topics.json"), "w", encoding="utf-8") as f:
        json.dump({"meaning": topic[0].get("meaning") if topic else None, "concepts": concepts}, f,
                  ensure_ascii=False, indent=1)
    placed = sum(1 for m in raw.values() if topics_of(m))
    print(json.dumps({"documents": len(raw), "with_topic": placed}, indent=1))


if __name__ == "__main__":
    main()
