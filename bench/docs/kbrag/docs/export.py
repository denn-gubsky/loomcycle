"""Study D export: every corpus document as sections, and the tenant's chunk -> document map.

    python3 -I bench/docs/kbrag/docs/export.py            # resumable; one export_md per document
    python3 -I bench/docs/kbrag/docs/export.py --stats

-> <work>/export/<document id>.json  {id, title, sections: [{chunk_id, path, text}]}
-> <work>/sections.json              {docs: {id: {title, sections}}, chunk_doc: {chunk id: document id}}

A section is one chunk: its heading (with the `<!-- loom: {"id": ...} -->` line export_md
writes under it) and the body up to the next such heading. A heading without that line is
part of the body it stands in (a Markdown heading inside a chunk's text). The heading path
leaves out the document title, which the renderers put first. Read-only.
"""
import argparse, json, os, re, sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import dlc, dconfig  # noqa: E402

HEADING = re.compile(r"^(#{1,6})\s+(.*?)\s*$")
LOOM = re.compile(r"^<!--\s*loom:\s*(\{.*\})\s*-->\s*$")
EDGES = re.compile(r"^<!--\s*loom-edges:")


def parse(markdown):
    """[(chunk_id, level, title, text)] in document order; the first is the root."""
    lines = (markdown or "").splitlines()
    out, cur, body, fence, i = [], None, [], False, 0
    while i < len(lines):
        line = lines[i]
        if line.lstrip().startswith("```"):
            fence = not fence
        if EDGES.match(line):  # the trailing relations block is not text
            while i < len(lines) and "-->" not in lines[i]:
                i += 1
            i += 1
            continue
        m = HEADING.match(line) if not fence else None
        meta = LOOM.match(lines[i + 1]) if (m and i + 1 < len(lines)) else None
        if m and meta:
            try:
                cid = json.loads(meta.group(1)).get("id")
            except ValueError:
                cid = None
            if cid:
                if cur:
                    out.append(cur + ("\n".join(body).strip(),))
                cur, body = (cid, len(m.group(1)), m.group(2)), []
                i += 2
                continue
        if LOOM.match(line):  # a loom line not under a heading (a paragraph chunk's metadata)
            i += 1
            continue
        body.append(line)
        i += 1
    if cur:
        out.append(cur + ("\n".join(body).strip(),))
    return out


def sections(markdown):
    """[{chunk_id, path, text}]: heading paths by heading level, the root's path empty."""
    parsed = parse(markdown)
    out, stack = [], []
    for n, (cid, level, title, text) in enumerate(parsed):
        if n == 0:
            out.append({"chunk_id": cid, "path": [], "text": text})
            continue
        while stack and stack[-1][0] >= level:
            stack.pop()
        stack.append((level, title))
        out.append({"chunk_id": cid, "path": [t for _, t in stack], "text": text})
    return out


def chunk_doc():
    """Every chunk of the tenant scope -> its document (paged SQL read)."""
    out, off, page = {}, 0, 2000
    while True:
        r = dlc.doc("query_chunks", scope="tenant",
                    sql="SELECT id, document_id FROM chunks ORDER BY id LIMIT %d OFFSET %d" % (page, off))
        rows = r.get("rows") or []
        for cid, did in rows:
            out[cid] = did
        if len(rows) < page and not r.get("truncated"):
            return out
        off += len(rows)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--stats", action="store_true")
    a = ap.parse_args()
    inv = json.load(open(dconfig.INVENTORY, encoding="utf-8"))
    titles = {d["id"]: d["title"] for d in inv["documents"]}
    os.makedirs(dconfig.EXPORT, exist_ok=True)
    if not a.stats:
        dlc.assert_tenant()
        for n, did in enumerate(inv["corpus"]):
            p = os.path.join(dconfig.EXPORT, did + ".json")
            if os.path.exists(p):
                continue
            md = dlc.doc("export_md", scope="tenant", id=did).get("markdown") or ""
            with open(p + ".tmp", "w", encoding="utf-8") as f:
                json.dump({"id": did, "title": titles.get(did), "sections": sections(md), "md_chars": len(md)}, f,
                          ensure_ascii=False)
            os.replace(p + ".tmp", p)
            if n % 50 == 0:
                print("exported %d/%d" % (n + 1, len(inv["corpus"])), flush=True)
        cmap = chunk_doc()
    else:
        cmap = json.load(open(dconfig.SECTIONS, encoding="utf-8"))["chunk_doc"]
    docs = {}
    for did in inv["corpus"]:
        e = json.load(open(os.path.join(dconfig.EXPORT, did + ".json"), encoding="utf-8"))
        docs[did] = {"title": e["title"], "sections": e["sections"]}
    secs = [s for d in docs.values() for s in d["sections"]]
    stray = [s["chunk_id"] for s in secs if cmap.get(s["chunk_id"]) not in docs]
    if not a.stats:
        with open(dconfig.SECTIONS, "w", encoding="utf-8") as f:
            json.dump({"docs": docs, "chunk_doc": cmap}, f, ensure_ascii=False)
    print(json.dumps({"documents": len(docs), "sections": len(secs),
                      "readable_sections": sum(1 for s in secs if s["text"].strip()),
                      "tenant_chunks": len(cmap), "corpus_chunks": sum(1 for d in cmap.values() if d in docs),
                      "section_ids_not_in_chunk_map": len(stray),
                      "chars": sum(len(s["text"]) for s in secs)}, indent=1))


if __name__ == "__main__":
    main()
