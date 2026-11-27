"""Instrument check 1 (same corpus) and the corpus filter whose list is also check 2's
denominator (RFC HC §6.4 fix 1).

The benchmark's filter dropped only the operational types, so the tenant's untyped
housekeeping document under /memory stayed in, and check 1 failed on it (PREREG Amendment 5).
This filter drops, from the tenant scope's documents:
  - the operational types (SourceStub, SyncReport, Annotation);
  - every UNTYPED document with a Path name at or under /memory;
  - the tenant ontology document (by its id when given, else by its name /memory/ontology).
A typed document under /memory stays, and so does an untyped one elsewhere (the imported
ConditionalQA pages carry no type).

The tenant's documents are listed with graph_snapshot (documents section only; columnar,
paged by whole documents, and it carries each document's Path names, which query_documents
does not).
"""
import hcconfig


def snapshot_documents(page):
    """One graph_snapshot page -> [{id, title, type, paths}]."""
    d = page.get("documents") or {}
    types = (page.get("codes") or {}).get("types") or []
    ids = d.get("ids") or []
    out = []
    for i, did in enumerate(ids):
        code = (d.get("type_code") or [-1] * len(ids))[i]
        out.append({"id": did, "title": (d.get("titles") or [None] * len(ids))[i],
                    "type": types[code] if isinstance(code, int) and 0 <= code < len(types) else None,
                    "paths": list((d.get("paths") or [[]] * len(ids))[i] or [])})
    return out


def list_documents(lc, page_documents=1000):
    """Every document of the tenant scope, through graph_snapshot pages."""
    out, after = [], None
    while True:
        body = {"scope": "tenant", "include": ["documents"], "page_documents": page_documents}
        if after:
            body["after"] = after
        page = lc.doc("graph_snapshot", **body)
        out += snapshot_documents(page)
        after = (page.get("page") or {}).get("next_cursor")
        if not after:
            return out


def under(path, root):
    return path == root or path.startswith(root.rstrip("/") + "/")


def drop_reason(doc, ontology_id=None):
    """Why a listed document is not in the corpus, or None when it is."""
    t = doc.get("type") or None
    paths = doc.get("paths") or []
    if ontology_id and doc["id"] == ontology_id:
        return "ontology"
    if not ontology_id and hcconfig.ONTOLOGY_PATH in paths:
        return "ontology"
    if t in hcconfig.OPERATIONAL_TYPES:
        return "operational:" + t
    if t is None and any(under(p, hcconfig.MEMORY_ROOT) for p in paths):
        return "untyped_under_memory"
    return None


def filter_corpus(docs, ontology_id=None):
    """(sorted corpus ids, {dropped id: reason})."""
    keep, dropped = [], {}
    for d in docs:
        why = drop_reason(d, ontology_id)
        if why:
            dropped[d["id"]] = why
        else:
            keep.append(d["id"])
    return sorted(keep), dropped


def check_same_corpus(corpus, expected=None, empty=(), hit_docs=()):
    """Check 1. corpus: the filtered listing. expected: the documents the study put there
    (the ingested pages' ids for ConditionalQA), or None where the tenant itself defines the
    corpus. empty: expected documents with no readable section (they can hold no hit and are
    not a gap). hit_docs: every document a scored row cited or read. Passes iff the filtered
    listing is exactly the expected set (less the empty ones) and no hit lies outside it."""
    corpus, empty = set(corpus), set(empty)
    out = {"listed": len(corpus)}
    ok = True
    if expected is not None:
        exp = set(expected) - empty
        out.update(expected=len(exp), only_listed=sorted(corpus - exp - empty)[:20], n_only_listed=len(corpus - exp - empty),
                   n_only_expected=len(exp - corpus))
        ok = (corpus - empty) == exp
    outside = set(hit_docs) - corpus
    out["hits_outside_corpus"] = len(outside)
    ok = ok and not outside
    out["status"] = "pass" if ok else "fail"
    return out
