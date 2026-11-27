"""Inputs of instrument checks 1 and 2 for one set's tenant, read before the first scored
run (after the annotator sweep, sweep.py). Read-only calls.

    python3 -I bench/docs/kbrag/hc/precheck.py --set cqa --expected <work>/cqa/docmap.json
    python3 -I bench/docs/kbrag/hc/precheck.py --set p7

-> <work>/<set>/corpus.json    every listed document, the corpus (filtered), the dropped ones
                               with their reason, the expected set when given
-> <work>/<set>/coverage.json  check 2 over that corpus, with the server's summarize answer
                               and its sample size

--expected: the documents the study put in the tenant (a JSON list of document ids, or a
map whose VALUES are document ids, such as the benchmark's docmap.json). Without it the
tenant's filtered listing is the corpus (the development tenant).
--ontology-id: the tenant ontology's document id (from describe_ontology); without it the
ontology is recognised by its name /memory/ontology.
"""
import argparse, datetime, json, os, sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import client, corpus, coverage, hcconfig  # noqa: E402


def expected_ids(path):
    with open(path, encoding="utf-8") as f:
        v = json.load(f)
    return sorted(v.values() if isinstance(v, dict) else v)


def take(lc, expected=None, ontology_id=None):
    docs = corpus.list_documents(lc)
    keep, dropped = corpus.filter_corpus(docs, ontology_id)
    anns = coverage.list_document_annotations(lc)
    summary = lc.doc("list_annotations", scope="tenant", summarize=True)
    at = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    c = {"at": at, "listed": docs, "corpus": keep, "dropped": dropped, "expected": expected}
    cov = dict(coverage.coverage(keep, anns, summary), at=at)
    return c, cov


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--set", required=True, choices=hcconfig.SETS)
    ap.add_argument("--expected")
    ap.add_argument("--ontology-id")
    a = ap.parse_args()
    lc = client.for_set(a.set)
    lc.assert_tenant()
    c, cov = take(lc, expected_ids(a.expected) if a.expected else None, a.ontology_id)
    d = os.path.join(hcconfig.work(), a.set)
    os.makedirs(d, exist_ok=True)
    for name, obj in (("corpus.json", c), ("coverage.json", cov)):
        with open(os.path.join(d, name), "w", encoding="utf-8") as f:
            json.dump(obj, f, indent=1)
    print(json.dumps({"listed": len(c["listed"]), "corpus": len(c["corpus"]), "dropped": len(c["dropped"]),
                      "check_2": {k: cov[k] for k in ("coverage", "stale_share", "status")},
                      "server_sample_size": (cov.get("server_summary") or {}).get("sample_size")}, indent=1))


if __name__ == "__main__":
    main()
