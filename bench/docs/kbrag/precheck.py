"""Inputs of instrument checks 1 and 2, read from the tenant at the start of the study.

    python3 -I bench/docs/kbrag/precheck.py

-> <work>/tenant_documents.json  the tenant's document ids (query_documents), less the
                                 operational types SourceStub, SyncReport and Annotation
-> <work>/annotation_summary.json  list_annotations summarize, as returned, plus when

Read-only calls (POST /v1/_document query_documents / list_annotations). analyze.py
reads both files; without them checks 1 and 2 are reported as not run.
"""
import argparse, datetime, os, sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import common, config  # noqa: E402

OPERATIONAL = {"SourceStub", "SyncReport", "Annotation"}
PAGE = 1000  # query_documents' maximum limit; it has no cursor, so a full page is split by time


def all_documents(lc, lo=0, hi=None):
    hi = hi or int(datetime.datetime.now(datetime.timezone.utc).timestamp() * 1e9) + 1
    r = lc.doc("query_documents", scope="tenant", limit=PAGE, created_after=lo - 1, created_before=hi)
    docs = r.get("documents") or []
    if len(docs) < PAGE:
        return docs
    if hi - lo <= 1:
        raise SystemExit("more than %d documents created in one nanosecond; cannot page" % PAGE)
    mid = (lo + hi) // 2
    return all_documents(lc, lo, mid) + all_documents(lc, mid, hi)


def main():
    argparse.ArgumentParser(description="Write tenant_documents.json and annotation_summary.json "
                                        "(read-only calls).").parse_args()
    import lc
    docs = {d["document_id"]: d for d in all_documents(lc)}
    keep = sorted(i for i, d in docs.items() if d.get("type") not in OPERATIONAL)
    common.jdump(keep, os.path.join(config.WORK, "tenant_documents.json"), indent=1)
    summ = lc.doc("list_annotations", scope="tenant", summarize=True)
    summ["at"] = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    common.jdump(summ, os.path.join(config.WORK, "annotation_summary.json"), indent=1)
    print("documents: %d listed, %d kept (operational types dropped); annotation levels: %s"
          % (len(docs), len(keep), sorted((summ.get("counts") or {}).keys())))


if __name__ == "__main__":
    main()
