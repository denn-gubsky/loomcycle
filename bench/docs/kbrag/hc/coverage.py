"""Instrument check 2: the knowledge base is annotated before the first scored run (RFC HC
§6.4 fix 2): document annotations on at least 99% of the corpus and at most 1% stale.

The denominator is corpus.filter_corpus's list, so a housekeeping document the filter drops
counts neither as covered nor as missing. Coverage and staleness come from the full listing
of document-level annotations (list_annotations level=document, paged; the server checks
staleness on every row of a listing page). The server's summarize answer is recorded beside
it with its own sample size: summarize checks staleness on at most 2,000 annotations and then
reports stale_checked, which this module records as sample_size.
"""
import hcconfig


def list_document_annotations(lc, limit=500):
    """[{document_id, stale}] for every document-level annotation of the tenant scope."""
    out, after = [], None
    while True:
        body = {"scope": "tenant", "level": "document", "limit": limit}
        if after:
            body["after"] = after
        r = lc.doc("list_annotations", **body)
        for a in r.get("annotations") or []:
            out.append({"document_id": a.get("document_id"), "stale": bool(a.get("stale"))})
        after = r.get("next_cursor")
        if not after:
            return out


def coverage(corpus, annotations, summary=None):
    """Check 2 over the filtered corpus. annotations: [{document_id, stale}] at level
    document; one outside the corpus is ignored. summary: the server's list_annotations
    summarize answer, recorded with its sample size when it sampled."""
    corpus = set(corpus)
    mine = {}
    for a in annotations:
        if a["document_id"] in corpus:
            # Two rows for one document (should not happen) count once, stale if either is.
            mine[a["document_id"]] = mine.get(a["document_id"], False) or a["stale"]
    covered, stale = len(mine), sum(1 for s in mine.values() if s)
    cov = covered / len(corpus) if corpus else 0.0
    stale_share = stale / covered if covered else 1.0
    out = {"documents": len(corpus), "covered": covered, "stale": stale, "coverage": round(cov, 4),
           "stale_share": round(stale_share, 4), "missing": sorted(corpus - set(mine))[:20],
           "status": "pass" if cov >= hcconfig.COVERAGE_MIN and stale_share <= hcconfig.STALE_MAX else "fail"}
    if summary is not None:
        d = (summary.get("counts") or {}).get("document") or {}
        out["server_summary"] = {"total": d.get("total"), "stale": d.get("stale"),
                                 "sample_size": summary.get("stale_checked")}
    return out
