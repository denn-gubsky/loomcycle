"""Score Study C against PREREG.md: per-arm measures, H3/H4, instrument checks 1-9.

    python3 -I bench/docs/kbrag/analyze.py   -> <work>/summary.json and <work>/summary.md

Works on partial data. The hypotheses are tested only on a COMPLETE study: kb, rag-lc and
rag-ext each hold an ok, condensed rep-0 row for every question of the reader subset, the
oracle too (checks 6 and 7), no spend-cap stop, one profile and one reader model across arms.
Anything less is reported as incomplete and its hypotheses as not tested — never scored on
the part that ran.
"""
import argparse, json, os, random, sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import common, config  # noqa: E402

ALPHA = 0.0125            # per co-primary test, two-sided -> the 98.75% CI
BOUND = -0.02             # the non-inferiority margin of H3/H4
RAG = ("rag-lc", "rag-ext")
MAIN = ("kb", "rag-lc", "rag-ext")


def bootstrap_ci(diffs, n=10000, seed=1, alpha=ALPHA):
    """Percentile CI of the mean of paired differences, resampling questions."""
    rng = random.Random(seed)
    m = len(diffs)
    means = sorted(sum(rng.choices(diffs, k=m)) / m for _ in range(n))
    return means[int(alpha / 2 * n)], means[int((1 - alpha / 2) * n) - 1]


def load(work):
    def rows(sub, arm):
        return common.read_jsonl(os.path.join(work, sub, arm + ".jsonl"))
    return {"runs": {a: rows("runs", a) for a in config.ARMS},
            "condensed": {a: rows("condensed", a) for a in config.ARMS},
            "judge": {a: rows("judge", a) for a in config.ARMS},
            "versions": common.read_jsonl(os.path.join(work, "runs", "versions.jsonl")),
            "events": common.read_jsonl(os.path.join(work, "runs", "events.jsonl"))}


def _mean(xs):
    xs = list(xs)
    return round(sum(xs) / len(xs), 4) if xs else None


def arm_view(data, arm, L, qids):
    """Per-question records of one arm's scored rows (rep 0), joined with condense and judge."""
    runs = common.latest(L["runs"][arm])
    cond = {(r["qid"], r["rep"], r["attempt"]): r for r in L["condensed"][arm] if r["status"] == "ok"}
    judge = {(r["qid"], r["rep"], r["attempt"]): r for r in L["judge"][arm] if r["status"] == "ok"}
    per, missing, failed, uncondensed = {}, [], [], []
    for qid in qids:
        r = runs.get((qid, 0))
        if r is None:
            missing.append(qid)
            continue
        if r["status"] != "ok":
            failed.append(qid)
            continue
        key = (qid, 0, r["attempt"])
        c, j = cond.get(key), judge.get(key)
        if c is None:
            uncondensed.append(qid)
        per[qid] = {"run": r, "cond": c, "judge": j}
    return per, missing, failed, uncondensed


def measures(data, arm, per):
    s = {}
    scored = {q: p for q, p in per.items() if p["cond"]}
    if scored:
        sc = {q: common.score(p["cond"]["condensed"], data.qs[q]["answers"]) for q, p in scored.items()}
        s["EM"] = _mean(v[0] for v in sc.values())
        s["F1"] = _mean(v[1] for v in sc.values())
        s["n_condensed"] = len(sc)
    if arm != "kb" and per:
        d = [common.score(p["run"].get("answer_line") or "", data.qs[q]["answers"]) for q, p in per.items()]
        s["EM_direct"], s["F1_direct"] = _mean(x[0] for x in d), _mean(x[1] for x in d)
        if scored:  # check 7 compares the two on the same questions
            s["F1_direct_on_condensed"] = _mean(common.score(per[q]["run"].get("answer_line") or "",
                                                             data.qs[q]["answers"])[1] for q in scored)
    if per:
        for docs_only, name in ((False, "cited_page_recall"), (True, "cited_page_recall_docs_only")):
            s[name] = _mean(float(data.qs[q]["doc_id"] in common.cited_pages(data, p["run"].get("citations") or
                                                                               {"docs": [], "chunks": []}, docs_only))
                            for q, p in per.items())
        judged = [p["judge"]["verdict"] for p in per.values() if p["judge"]]
        s["judge"] = {v: judged.count(v) for v in ("CORRECT", "PARTLY", "WRONG")}
        s["judge"]["n"] = len(judged)
        tot = [p["run"]["usage"]["total"] for p in per.values()]
        s["per_question"] = {"input_tokens": _mean(t["input_tokens"] for t in tot),
                             "output_tokens": _mean(t["output_tokens"] for t in tot),
                             "cached_tokens": _mean(t["cached_tokens"] for t in tot),
                             "model_calls": _mean(t["calls"] for t in tot),
                             "usd_est": _mean(t["usd_est"] for t in tot),
                             "wall_s": _mean(p["run"]["wall_s"] for p in per.values())}
        s["calls_are_floor"] = any(p["run"]["usage"].get("calls_estimated") for p in per.values())
        all_tok = sum(t["input_tokens"] + t["output_tokens"] for t in tot)
        all_usd = sum(t["usd_est"] for t in tot)
        correct_j = s["judge"]["CORRECT"]
        correct_em = sum(1 for q, p in scored.items()
                         if common.score(p["cond"]["condensed"], data.qs[q]["answers"])[0] == 1.0)
        s["cost_per_correct"] = {
            "judge_tokens": round(all_tok / correct_j, 1) if correct_j else None,
            "judge_usd_est": round(all_usd / correct_j, 6) if correct_j else None,
            "em_tokens": round(all_tok / correct_em, 1) if correct_em else None,
            "em_usd_est": round(all_usd / correct_em, 6) if correct_em else None}
        s["served"] = sorted({tuple(x) for p in per.values() for x in p["run"].get("served") or []})
    if arm in RAG and per:
        rk = {q: [tuple(x) for x in (p["run"].get("retrieval") or {}).get("sections") or []] for q, p in per.items()}
        for k in (1, 3, 5, 10):
            s["R@%d" % k] = _mean(common.recall_at(rk[q], data.gold(q), k) for q in rk)
        s["MRR@10"] = _mean(common.mrr10(rk[q], data.gold(q)) for q in rk)
        s["chance_R@5"] = round(sum(data.chance5(q) for q in rk) / len(rk), 6)
    if arm == "kb" and per:
        routes = {}
        for p in per.values():
            r = (p["run"].get("route") or "").replace("ROUTE:", "").split("|")[0].strip() or "(none)"
            routes[r] = routes.get(r, 0) + 1
        s["routes"] = routes
    return s


def spread(data, arm, L, sample):
    """Run-to-run spread on the spread sample: condensed F1 per rep, and the mean
    per-question range. Reported only; enters no test."""
    runs = common.latest(L["runs"][arm])
    cond = {(r["qid"], r["rep"], r["attempt"]): r for r in L["condensed"][arm] if r["status"] == "ok"}
    f = {}
    for (qid, rep), r in runs.items():
        if qid in sample and r["status"] == "ok" and (qid, rep, r["attempt"]) in cond:
            f.setdefault(qid, {})[rep] = common.score(cond[(qid, rep, r["attempt"])]["condensed"], data.qs[qid]["answers"])[1]
    full = {q: v for q, v in f.items() if len(v) == config.SPREAD_REPS}
    if not full:
        return {"questions_with_all_reps": 0}
    return {"questions_with_all_reps": len(full),
            "F1_by_rep": [_mean(v[r] for v in full.values()) for r in range(config.SPREAD_REPS)],
            "mean_range_F1": _mean(max(v.values()) - min(v.values()) for v in full.values())}


def reader_pairs(per, arm):
    out = set()
    for p in per.values():
        r = p["run"]
        pair = tuple(r["reader_served"]) if r.get("reader_served") else common.main_model(r.get("usage"))
        if pair:
            out.add(tuple(pair))
    return out


def checks(data, work, L, views, S):
    c = {}

    def put(n, name, status, **kw):
        c["%d_%s" % (n, name)] = dict(kw, status=status)

    # 1. Same corpus: the pages rag-ext indexed, as tenant ids, against the tenant's listing.
    mpath = os.path.join(work, "ragext", "manifest.json")
    tpath = os.path.join(work, "tenant_documents.json")
    if os.path.exists(mpath) and os.path.exists(tpath) and data.docmap:
        m = common.jload(mpath)
        indexed = {data.docmap.get(cid.split("#")[0]) for cid in m["order"]}
        tenant = set(common.jload(tpath))
        # A page with no readable section has no chunk; it cannot be indexed and is not a gap.
        empty = {data.docmap.get(d) for d, doc in data.docs.items() if not any(s["text"].strip() for s in doc["sections"])}
        put(1, "same_corpus", "pass" if indexed == tenant - empty else "fail",
            indexed=len(indexed), tenant=len(tenant), only_indexed=len(indexed - tenant), only_tenant=len(tenant - indexed - empty))
    else:
        put(1, "same_corpus", "not-run", note="needs ragext/manifest.json, docmap.json and tenant_documents.json "
                                               "(the tenant's query_documents ids less the operational types)")
    # 2. The knowledge base is annotated.
    apath = os.path.join(work, "annotation_summary.json")
    n_docs = len(common.jload(tpath)) if os.path.exists(tpath) else len(data.docmap)
    if os.path.exists(apath) and n_docs:
        a = common.jload(apath)
        d = (a.get("counts") or {}).get("document") or {}
        # One document-level annotation per document, so the level's total is the count
        # of annotated documents.
        cov = d.get("total", 0) / n_docs
        stale = d.get("stale", 0) / d["total"] if d.get("total") else 1.0
        kw = {"coverage": round(cov, 4), "stale": round(stale, 4), "documents": n_docs, "at": a.get("at")}
        if "stale_checked" in a:
            kw["note"] = "the server checked only %s annotations for staleness" % a["stale_checked"]
        put(2, "annotated", "pass" if cov >= 0.99 and stale <= 0.01 else "fail", **kw)
    else:
        put(2, "annotated", "not-run", note="needs annotation_summary.json (precheck.py) at the study's start")
    # 3. rag-lc used no annotation.
    rl = views["rag-lc"][0]
    if rl:
        bad = sum(1 for p in rl.values() if (p["run"].get("retrieval") or {}).get("matched_annotation")
                  or (p["run"].get("retrieval") or {}).get("summary"))
        put(3, "raglc_no_annotation", "fail" if bad else "pass", searches=len(rl), with_annotation=bad)
    else:
        put(3, "raglc_no_annotation", "not-run")
    # 4. The rerank ran.
    if rl:
        rate = sum(1 for p in rl.values() if (p["run"].get("retrieval") or {}).get("reranked") is True) / len(rl)
        reasons = {}
        for p in rl.values():
            why = (p["run"].get("retrieval") or {}).get("rerank_reason")
            if why:
                reasons[why] = reasons.get(why, 0) + 1
        put(4, "rerank_raglc", "pass" if rate >= 0.95 else "fail", reranked_rate=round(rate, 4), reasons=reasons)
    else:
        put(4, "rerank_raglc", "not-run")
    rx = views["rag-ext"][0]
    if rx:
        strat = {(p["run"].get("retrieval") or {}).get("rerank_strategy") for p in rx.values()}
        failed = sum(1 for p in rx.values() if (p["run"].get("retrieval") or {}).get("rerank_failed"))
        ok = strat == {"listwise"} and failed == 0
        put(4, "rerank_ragext", "pass" if ok else "fail", strategies=sorted(map(str, strat)), rerank_failed=failed,
            note="listwise reader-model rerank stands in for the pre-registered cross-encoder (amendment)")
    else:
        put(4, "rerank_ragext", "not-run")
    # 5. Retrieval works.
    for arm in RAG:
        s = S.get(arm, {})
        if "R@5" in s:
            put(5, "retrieval_" + arm, "pass" if s["R@5"] > 10 * s["chance_R@5"] else "fail",
                R5=s["R@5"], ten_x_chance=round(10 * s["chance_R@5"], 6))
        else:
            put(5, "retrieval_" + arm, "not-run")
    # 6. The reader reads.
    o = S.get("oracle", {})
    if o.get("F1") is not None and all(S.get(a, {}).get("F1") is not None for a in RAG):
        put(6, "oracle_above_rag", "pass" if all(o["F1"] > S[a]["F1"] for a in RAG) else "fail",
            oracle=o["F1"], **{a: S[a]["F1"] for a in RAG})
    else:
        put(6, "oracle_above_rag", "not-run")
    # 7. The condense step is faithful.
    if o.get("F1") is not None and o.get("F1_direct_on_condensed") is not None:
        d = abs(o["F1"] - o["F1_direct_on_condensed"])
        put(7, "condense_faithful", "pass" if d <= 0.02 else "fail", condensed=o["F1"], direct=o["F1_direct_on_condensed"],
            diff=round(d, 4))
    else:
        put(7, "condense_faithful", "not-run")
    # 8. Every run answered (latest row; a row may have been retried once).
    fails = {a: len(views[a][2]) for a in config.ARMS if views[a][0] or views[a][2]}
    missing = {a: len(views[a][1]) for a in config.ARMS}
    twice = {a: sum(1 for (q, rep), r in common.latest(L["runs"][a]).items()
                    if r["status"] != "ok" and r["attempt"] >= 2) for a in config.ARMS}
    if any(twice.values()):
        put(8, "every_run_answered", "fail", failed_after_retry=twice, failed=fails)
    elif any(missing[a] for a in MAIN + ("oracle",)) or any(fails.values()):
        put(8, "every_run_answered", "not-run", note="incomplete: rows missing or awaiting their retry",
            missing=missing, failed=fails)
    else:
        put(8, "every_run_answered", "pass")
    # 9. One release.
    v = [x for x in L["versions"] if x.get("version")]
    if len(v) >= 2:
        same = (v[0]["version"], v[0].get("commit")) == (v[-1]["version"], v[-1].get("commit"))
        put(9, "one_release", "pass" if same else "fail", first=v[0]["version"], last=v[-1]["version"])
    else:
        put(9, "one_release", "not-run")
    return c


# Which hypotheses a failed check voids (PREREG "Instrument checks").
VOIDS = {"1_same_corpus": ("H3", "H4"), "2_annotated": ("H3", "H4"), "3_raglc_no_annotation": ("H3",),
         "4_rerank_raglc": ("H3",), "4_rerank_ragext": ("H4",), "5_retrieval_rag-lc": ("H3",),
         "5_retrieval_rag-ext": ("H4",), "6_oracle_above_rag": ("H3", "H4"), "7_condense_faithful": ("H3", "H4"),
         "8_every_run_answered": ("H3", "H4"), "9_one_release": ("H3", "H4")}


def analyze(work, data, out=None):
    """work: the study inputs (ragext/, tenant_documents.json, annotation_summary.json);
    out: the root holding runs/, condensed/ and judge/ (default: work)."""
    L = load(out or work)
    qids = list(data.subset)
    views = {a: arm_view(data, a, L, qids) for a in config.ARMS}
    S = {a: measures(data, a, views[a][0]) for a in config.ARMS}
    sample = common.spread_sample(data.subset)
    for a in config.ARMS:
        S[a]["rows"] = {"ok": len(views[a][0]), "missing": len(views[a][1]), "failed": len(views[a][2]),
                        "uncondensed": len(views[a][3])}
        S[a]["spread"] = spread(data, a, L, sample)
    out = {"study": "C (ConditionalQA)", "questions": len(qids), "arms": S, "hypotheses": {}, "problems": []}

    profiles = {r.get("profile") for a in config.ARMS for p in views[a][0].values() for r in [p["run"]]}
    readers = {a: sorted(reader_pairs(views[a][0], a)) for a in config.ARMS if views[a][0]}
    out["profiles"], out["reader_models"] = sorted(map(str, profiles)), readers
    mixed = len(profiles) > 1 or len({tuple(p) for ps in readers.values() for p in ps}) > 1
    if mixed:
        out["problems"].append("mixed profile: the arms' scored rows used different profiles or reader models")
    cap = [e for e in L["events"] if e.get("kind") == "stop" and str(e.get("reason", "")).startswith("spend cap")]
    if cap:
        out["problems"].append("stopped by the spend cap at %s" % cap[0]["ts"])
    viol = [e for e in L["events"] if e.get("kind") == "provider_violation"]
    if viol:
        out["problems"].append("provider violation recorded (%d)" % len(viol))
    incomplete = [a for a in MAIN + ("oracle",) if views[a][1] or views[a][2] or views[a][3]]
    if incomplete:
        out["problems"].append("incomplete: %s lack an ok, condensed row for some of the %d questions"
                               % (", ".join(incomplete), len(qids)))
    out["complete"] = not (incomplete or cap or mixed or viol)
    out["checks"] = checks(data, work, L, views, S)

    voided = {}
    for name, c in out["checks"].items():
        if c["status"] == "fail":
            for h in VOIDS.get(name, ()):
                voided.setdefault(h, []).append(name)
    not_run = [n for n, c in out["checks"].items() if c["status"] == "not-run"]
    for h, rag in (("H3", "rag-lc"), ("H4", "rag-ext")):
        res = {"comparison": "kb - %s, mean condensed F1" % rag, "bound": BOUND, "ci_level": 1 - ALPHA}
        if not out["complete"]:
            res["verdict"] = "not tested (study incomplete)"
        else:
            kb, other = views["kb"][0], views[rag][0]
            d = [common.score(kb[q]["cond"]["condensed"], data.qs[q]["answers"])[1]
                 - common.score(other[q]["cond"]["condensed"], data.qs[q]["answers"])[1] for q in qids]
            lo, hi = bootstrap_ci(d)
            res.update(diff=round(sum(d) / len(d), 4), ci=[round(lo, 4), round(hi, 4)], n=len(d),
                       non_inferior=lo > BOUND, superior=lo > 0)
            if voided.get(h):
                res["verdict"] = "void (failed check: %s)" % ", ".join(voided[h])
            else:
                res["verdict"] = ("holds" if lo > BOUND else "does not hold") + (
                    "" if not not_run else " (provisional: checks not run: %s)" % ", ".join(not_run))
        out["hypotheses"][h] = res
    return out


def markdown(s):
    L = ["# Study C (ConditionalQA): kb vs RAG", "",
         "**%s.** Profiles: %s." % ("Complete" if s["complete"] else "INCOMPLETE — hypotheses not tested",
                                    ", ".join(s["profiles"]) or "none"), ""]
    for p in s["problems"]:
        L.append("- %s" % p)
    L += ["", "| arm | rows ok | EM | F1 | page recall | page recall (docs) | judge C/P/W | R@1 | R@3 | R@5 | R@10 | MRR@10 "
          "| in tok/q | out tok/q | calls/q | USD/q (est) | wall s/q | tokens per correct | USD per correct |",
          "|---|" + "---|" * 18]
    for a in config.ARMS:
        x = s["arms"][a]
        j = x.get("judge") or {}
        pq = x.get("per_question") or {}
        cc = x.get("cost_per_correct") or {}

        def f(v):
            return "—" if v is None else str(v)
        L.append("| %s | %d | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |" % (
            a, x["rows"]["ok"], f(x.get("EM")), f(x.get("F1")), f(x.get("cited_page_recall")),
            f(x.get("cited_page_recall_docs_only")),
            "%s/%s/%s" % (j.get("CORRECT", "—"), j.get("PARTLY", "—"), j.get("WRONG", "—")) if j else "—",
            f(x.get("R@1")), f(x.get("R@3")), f(x.get("R@5")), f(x.get("R@10")), f(x.get("MRR@10")),
            f(pq.get("input_tokens")), f(pq.get("output_tokens")), f(pq.get("model_calls")), f(pq.get("usd_est")),
            f(pq.get("wall_s")), f(cc.get("judge_tokens")), f(cc.get("judge_usd_est"))))
    L += ["", "## Hypotheses", ""]
    for h, r in s["hypotheses"].items():
        extra = "" if "ci" not in r else ": diff %s, %.2f%% CI [%s, %s] vs bound %s; superior (CI above 0): %s" % (
            r["diff"], 100 * r["ci_level"], r["ci"][0], r["ci"][1], r["bound"], r["superior"])
        L.append("- **%s** (%s): %s%s" % (h, r["comparison"], r["verdict"], extra))
    L += ["", "## Instrument checks", ""]
    for n, c in s["checks"].items():
        rest = {k: v for k, v in c.items() if k != "status"}
        L.append("- %s: **%s** %s" % (n, c["status"], json.dumps(rest) if rest else ""))
    return "\n".join(L) + "\n"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--work", default=config.WORK)
    ap.add_argument("--out", help="root holding runs/, condensed/, judge/; summary.* written there (default: --work)")
    a = ap.parse_args()
    out = a.out or a.work
    data = common.Data(prepared=os.path.join(a.work, "prepared"), docmap=os.path.join(a.work, "docmap.json"),
                       chunkmap=os.path.join(a.work, "chunks.json"))
    s = analyze(a.work, data, out)
    common.jdump(s, os.path.join(out, "summary.json"), indent=2, default=list)
    md = markdown(s)
    with open(os.path.join(out, "summary.md"), "w", encoding="utf-8") as f:
        f.write(md)
    print(md)


if __name__ == "__main__":
    main()
