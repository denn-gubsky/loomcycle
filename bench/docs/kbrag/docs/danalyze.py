"""Score Study D: per set, per arm, per question kind; H1/H2; instrument checks; cost.

    python3 -I bench/docs/kbrag/docs/danalyze.py --out <root>   -> <root>/summary.json, <root>/summary.md

Sets are never pooled. H1/H2 (PREREG) are tested on the `p7` set's document-gold questions
(a question with gold_topics is scored on topics and reported apart, per the walker README);
the same paired test on `waves` and `known` is reported as secondary and labelled so.

Per run: the cited documents (loom:doc links, plus the document of every loom:chunk link;
the documents-only variant beside it), recall over `gold`, precision with `gold` + `also`
counted correct (a citation outside the corpus counts as wrong; the in-corpus-only
precision is reported beside it), F1 (0 when nothing is cited). A question's score is the
mean over its runs. Bootstrap: paired, over questions, 10,000 resamples, seed 1, 98.75%.
"""
import argparse, json, os, sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(1, os.path.dirname(HERE))
import dconfig, questions  # noqa: E402
import common  # noqa: E402
from analyze import bootstrap_ci  # noqa: E402  (../analyze.py: the pre-registered bootstrap)

sys.path.insert(0, questions.WALKER_DIR)
import drive  # noqa: E402  (topic matching rules of the walker README)

ARMS = dconfig.ARMS


def prf(cited, gold, also, corpus=None):
    gold, ok = set(gold), set(gold) | set(also)
    den = cited if corpus is None else (cited & corpus)
    p = len(cited & ok) / len(den) if den else 0.0
    r = len(cited & gold) / len(gold) if gold else None
    f = 2 * p * r / (p + r) if (r is not None and p + r) else 0.0
    return p, r, f


def metric(q):
    if q["gold_topics"]:
        return "topics"
    if q["gold"]:
        return "docs"
    return None


def score_run(q, row, cmap, corpus):
    out = {}
    m = metric(q)
    cites = row.get("citations") or {"docs": [], "chunks": []}
    docs = set(cites["docs"]) | {cmap[c] for c in cites["chunks"] if c in cmap}
    docs_only = set(cites["docs"])
    out["n_cited"] = len(docs)
    out["n_cited_outside_corpus"] = len(docs - corpus)
    if q["gold"]:
        out["P"], out["R"], out["F1"] = prf(docs, q["gold"], q["also"])
        out["P_docs_only"], out["R_docs_only"], out["F1_docs_only"] = prf(docs_only, q["gold"], q["also"])
        out["P_in_corpus"], _, out["F1_in_corpus"] = prf(docs, q["gold"], q["also"], corpus)
    if m == "topics":
        topics = [dict(t) for t in q["gold_topics"]]
        for t in topics:
            if drive.topic_spans(q["text"], t):
                t["unscorable"] = True
        sc = [t for t in topics if not t.get("unscorable")]
        named = [n for n in drive.named_topics(row.get("raw_answer") or "", topics) if n in {t["name"] for t in sc}]
        out["topic_recall"] = len(named) / len(sc) if sc else None
    if row["arm"] != "kb" and row.get("retrieval"):
        read = {s[0] for s in (row.get("read_sections") or [])}
        if q["gold"]:
            out["read_doc_recall"] = len(read & set(q["gold"])) / len(q["gold"])
    return out


def mean(xs):
    xs = [x for x in xs if x is not None]
    return round(sum(xs) / len(xs), 4) if xs else None


def load_runs(out):
    rows = {a: common.latest(common.read_jsonl(os.path.join(out, "runs", a + ".jsonl")),
                             key=lambda r: (r["qid"], r["rep"])) for a in ARMS}
    return rows


def per_question(qs, rows, cmap, corpus):
    """{arm: {qid: {"runs": [scores], "mean": {...}, "rows": [row]}}} over ok rows."""
    out = {}
    for a in ARMS:
        out[a] = {}
        for (qid, rep), r in rows[a].items():
            if qid not in qs or r["status"] != "ok":
                continue
            e = out[a].setdefault(qid, {"runs": [], "rows": []})
            e["runs"].append(score_run(qs[qid], r, cmap, corpus))
            e["rows"].append(r)
        for qid, e in out[a].items():
            keys = sorted({k for s in e["runs"] for k in s})
            e["mean"] = {k: mean(s.get(k) for s in e["runs"]) for k in keys}
    return out


def test(pq, qids, a, b, field="F1"):
    both = [q for q in qids if q in pq[a] and q in pq[b] and pq[a][q]["mean"].get(field) is not None
            and pq[b][q]["mean"].get(field) is not None]
    if len(both) < 2:
        return {"n": len(both), "verdict": "not tested (too few questions)"}
    d = [pq[a][q]["mean"][field] - pq[b][q]["mean"][field] for q in both]
    lo, hi = bootstrap_ci(d)
    return {"n": len(both), "missing": len(qids) - len(both), "diff": round(sum(d) / len(d), 4),
            "ci_98.75": [round(lo, 4), round(hi, 4)], "holds": lo > 0, "wins": sum(1 for x in d if x > 0),
            "losses": sum(1 for x in d if x < 0), "ties": sum(1 for x in d if x == 0)}


def arm_table(qs, pq, qids):
    t = {}
    for a in ARMS:
        have = [q for q in qids if q in pq[a]]
        rows = [r for q in have for r in pq[a][q]["rows"]]
        tot = [r["usage"]["total"] for r in rows]
        m = {k: mean(pq[a][q]["mean"].get(k) for q in have) for k in
             ("P", "R", "F1", "F1_docs_only", "P_in_corpus", "F1_in_corpus", "topic_recall", "read_doc_recall",
              "n_cited", "n_cited_outside_corpus")}
        m.update(questions=len(have), runs=len(rows),
                 usd_per_run=mean(x["usd_est"] for x in tot), in_tok_per_run=mean(x["input_tokens"] for x in tot),
                 out_tok_per_run=mean(x["output_tokens"] for x in tot), cached_tok_per_run=mean(x["cached_tokens"] for x in tot),
                 calls_per_run=mean(x["calls"] for x in tot), wall_s_per_run=mean(r["wall_s"] for r in rows),
                 usd_total=round(sum(x["usd_est"] for x in tot), 4))
        t[a] = m
    return t


def routes(pq, qs, qids):
    out = {}
    for q in qids:
        for r in (pq["kb"].get(q) or {}).get("rows", []):
            rt = r.get("route") or ""
            parts = {p.split(":", 1)[0].strip(): p.split(":", 1)[1].strip() for p in rt.split("|") if ":" in p}
            k = qs[q]["kind"]
            d = out.setdefault(k, {})
            key = "%s / %s" % (parts.get("ROUTE", "?"), parts.get("CLASSIFIER", "?"))
            d[key] = d.get(key, 0) + 1
            cov = (r.get("coverage") or "none").replace("COVERAGE:", "").strip().split(" ")[0] or "none"
            c = out.setdefault(k + " COVERAGE", {})
            c[cov] = c.get(cov, 0) + 1
    return out


def checks(out_root, rows, pq, corpus):
    c = {}
    # 1. Same corpus: the documents rag-ext indexed vs the corpus (a document with no readable
    # section has no chunk and cannot be indexed); rag-lc searched the same (exclusion by id).
    m = json.load(open(os.path.join(dconfig.RAGEXT, "manifest.json")))
    indexed = {i.split("#")[0] for i in m["order"]}
    secs = json.load(open(dconfig.SECTIONS))["docs"]
    empty = {d for d, x in secs.items() if not any(s["text"].strip() for s in x["sections"])}
    c["1_same_corpus"] = {"status": "pass" if indexed == corpus - empty else "fail", "indexed": len(indexed),
                          "corpus": len(corpus), "empty_documents": len(empty)}
    lc_docs = {d for e in pq["rag-lc"].values() for r in e["rows"] for d in (r.get("retrieval") or {}).get("docs_ranked") or []}
    c["1_same_corpus"]["raglc_hits_outside_corpus"] = len(lc_docs - corpus)
    # 2. Annotated, at the study's start.
    a = json.load(open(os.path.join(dconfig.WORK, "annotation_summary.json")))
    d = a["counts"]["document"]
    cov, stale = d["total"] / len(corpus), d["stale"] / d["total"]
    c["2_annotated"] = {"status": "pass" if cov >= 0.99 and stale <= 0.01 else "fail", "coverage": round(cov, 4),
                        "stale": round(stale, 4), "at": a.get("at"), "stale_checked": a.get("stale_checked")}
    # 3, 4. rag-lc read no annotation; the reranks ran.
    lr = [r for e in pq["rag-lc"].values() for r in e["rows"]]
    if lr:
        bad = sum(1 for r in lr if r["retrieval"].get("matched_annotation") or r["retrieval"].get("summary"))
        c["3_raglc_no_annotation"] = {"status": "fail" if bad else "pass", "searches": len(lr), "with_annotation": bad}
        rate = sum(1 for r in lr if r["retrieval"].get("reranked") is True) / len(lr)
        reasons = {}
        for r in lr:
            w = r["retrieval"].get("rerank_reason")
            if w:
                reasons[w] = reasons.get(w, 0) + 1
        c["4_rerank_raglc"] = {"status": "pass" if rate >= 0.95 else "fail", "reranked_rate": round(rate, 4), "reasons": reasons}
    xr = [r for e in pq["rag-ext"].values() for r in e["rows"]]
    if xr:
        failed = sum(1 for r in xr if r["retrieval"].get("rerank_failed"))
        c["4_rerank_ragext"] = {"status": "pass" if failed == 0 else "fail", "rerank_failed": failed, "runs": len(xr),
                                "expansion_failed": sum(1 for r in xr if r["retrieval"].get("expansion_failed"))}
    # 8. Every run answered (latest attempt per (arm, qid, rep)).
    left = {a: sum(1 for r in rows[a].values() if r["status"] != "ok") for a in ARMS}
    c["8_every_run_answered"] = {"status": "pass" if not any(left.values()) else "fail", "failed_latest": left}
    # 9. One release.
    v = [x for x in common.read_jsonl(os.path.join(out_root, "runs", "versions.jsonl")) if x.get("version")]
    if v:
        same = len({(x["version"], x.get("commit")) for x in v}) == 1
        c["9_one_release"] = {"status": "pass" if same else "fail", "versions": sorted({x["version"] for x in v})}
    return c


def judge_table(out_root, qs):
    t = {}
    for a in ARMS:
        rows = common.read_jsonl(os.path.join(out_root, "judge", a + ".jsonl"))
        for r in common.latest(rows, key=lambda r: (r["qid"], r["rep"])).values():
            if r["status"] != "ok" or r["qid"] not in qs:
                continue
            q = qs[r["qid"]]
            for key in (q["set"], "%s/%s" % (q["set"], q["kind"])):
                d = t.setdefault(key, {}).setdefault(a, {"COMPLETE": 0, "PARTIAL": 0, "WRONG": 0})
                d[r["verdict"]] += 1
    return t


def markdown(S):
    """The tables the results README quotes: counts and scores only (no ids, no titles)."""
    L = []
    f = lambda v: "—" if v is None else ("%.3f" % v if isinstance(v, float) else str(v))  # noqa: E731
    for s, e in S["sets"].items():
        L += ["", "## set `%s` — %d questions (%d document gold, %d topic gold)" % (
            s, e["questions"], e["doc_gold_questions"], e["topic_gold_questions"]), ""]
        L += ["| kind | arm | q | runs | F1 | P | R | F1 docs-only | P in-corpus | topic R | read-doc R | cited/run | outside/run | USD/run | in tok/run | out tok/run | calls/run | s/run |",
              "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|"]
        for kind, tab in [("all", e["arms"])] + sorted(e["by_kind"].items()):
            for a, m in tab.items():
                L.append("| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |" % (
                    kind, a, m["questions"], m["runs"], f(m["F1"]), f(m["P"]), f(m["R"]), f(m["F1_docs_only"]),
                    f(m["P_in_corpus"]), f(m["topic_recall"]), f(m["read_doc_recall"]), f(m["n_cited"]),
                    f(m["n_cited_outside_corpus"]), f(m["usd_per_run"]), f(m["in_tok_per_run"]), f(m["out_tok_per_run"]),
                    f(m["calls_per_run"]), f(m["wall_s_per_run"])))
        L += ["", "| test (F1, paired over questions) | n | mean diff | 98.75% CI | wins/losses/ties |", "|---|---|---|---|---|"]
        for name, t in [("all: " + k, v) for k, v in e["tests"].items()] + [
                ("%s: %s" % (kind, k), v) for kind, tt in sorted(e["tests_by_kind"].items()) for k, v in tt.items()]:
            if "ci_98.75" in t:
                L.append("| %s | %d | %+.3f | [%+.3f, %+.3f] | %d/%d/%d |" % (name, t["n"], t["diff"], t["ci_98.75"][0],
                                                                            t["ci_98.75"][1], t["wins"], t["losses"], t["ties"]))
        L += ["", "kb routes: `%s`" % json.dumps(e["routes"], sort_keys=True)]
    L += ["", "## judge", ""]
    for key, arms in sorted(S["judge"].items()):
        L.append("- %s: %s" % (key, ", ".join("%s C/P/W %d/%d/%d" % (a, d["COMPLETE"], d["PARTIAL"], d["WRONG"])
                                             for a, d in sorted(arms.items()))))
    L += ["", "## checks", ""] + ["- %s: %s" % (k, json.dumps(v, sort_keys=True)) for k, v in S["checks"].items()]
    return "\n".join(L) + "\n"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--sets", default="p7,waves,known")
    a = ap.parse_args()
    qs = questions.load([s for s in a.sets.split(",") if s])
    inv = json.load(open(dconfig.INVENTORY))
    corpus = set(inv["corpus"])
    cmap = json.load(open(dconfig.SECTIONS))["chunk_doc"]
    rows = load_runs(a.out)
    pq = per_question(qs, rows, cmap, corpus)
    S = {"sets": {}, "checks": checks(a.out, rows, pq, corpus), "judge": judge_table(a.out, qs)}
    for s in a.sets.split(","):
        ids = [q for q in qs if qs[q]["set"] == s]
        if not ids:
            continue
        doc_ids = [q for q in ids if metric(qs[q]) == "docs"]
        kinds = sorted({qs[q]["kind"] or "?" for q in ids})
        e = {"questions": len(ids), "doc_gold_questions": len(doc_ids),
             "topic_gold_questions": sum(1 for q in ids if metric(qs[q]) == "topics"),
             "arms": arm_table(qs, pq, ids),
             "by_kind": {k: arm_table(qs, pq, [q for q in ids if qs[q]["kind"] == k]) for k in kinds},
             "tests": {"kb - rag-lc": test(pq, doc_ids, "kb", "rag-lc"), "kb - rag-ext": test(pq, doc_ids, "kb", "rag-ext"),
                       "rag-ext - rag-lc": test(pq, doc_ids, "rag-ext", "rag-lc")},
             "tests_by_kind": {k: {"kb - rag-lc": test(pq, [q for q in doc_ids if qs[q]["kind"] == k], "kb", "rag-lc"),
                                   "kb - rag-ext": test(pq, [q for q in doc_ids if qs[q]["kind"] == k], "kb", "rag-ext")}
                               for k in kinds},
             "routes": routes(pq, qs, ids)}
        S["sets"][s] = e
    with open(os.path.join(a.out, "summary.json"), "w") as f:
        json.dump(S, f, indent=1, default=list)
    md = markdown(S)
    with open(os.path.join(a.out, "summary.md"), "w") as f:
        f.write(md)
    print(md)


if __name__ == "__main__":
    main()
