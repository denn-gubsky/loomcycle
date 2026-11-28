"""Score the RFC HC gate study against PREREG-HC.md: per-arm measures, T1-T3, the cost checks,
the secondaries, the nine instrument checks, and the gate verdict.

    python3 -I bench/docs/kbrag/hc/hcanalyze.py --out <work>/study   -> <out>/summary.json, <out>/summary.md

Works on partial data. A test is computed only on a COMPLETE set (every arm it compares holds an
ok row for every question and run it needs, condensed where it is scored on F1; no spend-cap
stop; no provider violation); anything less is reported as not tested, never scored on the part
that ran. Bootstrap: paired over questions, 10,000 resamples, seed 1, the 98.33% CI
(../analyze.bootstrap_ci with alpha = 0.05/3).

Amendment 1 (post hoc, 2026-11-28) replaces checks 1 and 7 for a labelled exploratory reading:
`amendment_1` per set and `gate_amendment_1` in the summary. The registered verdict is unchanged.
"""
import argparse, json, os, sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(1, os.path.dirname(HERE))
import corpus as corpus_mod, hcconfig  # noqa: E402
import common  # noqa: E402
from analyze import bootstrap_ci  # noqa: E402  (../analyze.py: the pre-registered bootstrap)
from docs.danalyze import prf  # noqa: E402  (Study D's cited-document P/R/F1)

T_ARMS = {"cqa": ("kb-today", "kb-first", "rag-lc", "oracle"), "p7": ("kb-today", "kb-first")}


def mean(xs):
    xs = [x for x in xs if x is not None]
    return round(sum(xs) / len(xs), 4) if xs else None


def ci(diffs):
    lo, hi = bootstrap_ci(diffs, n=hcconfig.BOOT_N, seed=hcconfig.BOOT_SEED, alpha=hcconfig.ALPHA)
    return round(lo, 4), round(hi, 4)


def paired(a, b, qids, bound):
    """a, b: {qid: score}. The paired test of mean(a - b) with its 98.33% CI against `bound`."""
    both = [q for q in qids if a.get(q) is not None and b.get(q) is not None]
    if len(both) < 2:
        return {"n": len(both), "verdict": "not tested (too few questions)"}
    d = [a[q] - b[q] for q in both]
    lo, hi = ci(d)
    return {"n": len(both), "diff": round(sum(d) / len(d), 4), "ci": [lo, hi], "ci_level": round(1 - hcconfig.ALPHA, 4),
            "bound": bound, "holds": lo > bound, "wins": sum(1 for x in d if x > 0), "losses": sum(1 for x in d if x < 0)}


def load(out, set_name):
    d = os.path.join(out, set_name)
    rd = lambda sub, arm: common.read_jsonl(os.path.join(d, sub, arm + ".jsonl"))  # noqa: E731
    return {"runs": {a: common.latest(rd("runs", a)) for a in hcconfig.ARMS_FOR[set_name]},
            "runs_all": {a: rd("runs", a) for a in hcconfig.ARMS_FOR[set_name]},
            "condensed": {a: {(r["qid"], r["rep"], r["attempt"]): r for r in rd("condensed", a) if r["status"] == "ok"}
                          for a in hcconfig.ARMS_FOR[set_name]},
            "versions": common.read_jsonl(os.path.join(d, "runs", "versions.jsonl")),
            "events": common.read_jsonl(os.path.join(d, "runs", "events.jsonl"))}


def jload(path):
    return common.jload(path) if os.path.exists(path) else None


# --- ConditionalQA -----------------------------------------------------------------

def is_yes_no(answers):
    return all(common.normalize(a) in ("yes", "no") for a in answers)


def chance_page(data, q):
    """The chance that K random readable sections include one of the gold page's."""
    n_page = sum(1 for s in data.docs[q["doc_id"]]["sections"] if s["text"].strip())
    return min(1.0, hcconfig.K * n_page / data.n_readable)


def cqa_scores(data, L, arm, qids, rep=0):
    """{qid: (EM, F1)} on condensed rep-`rep` rows, and the rows themselves."""
    rows, sc = {}, {}
    for q in qids:
        r = L["runs"][arm].get((q, rep))
        if not r or r["status"] != "ok":
            continue
        rows[q] = r
        c = L["condensed"][arm].get((q, rep, r["attempt"]))
        if c:
            sc[q] = common.score(c["condensed"], data.qs[q]["answers"])
    return sc, rows


def cited_page(data, row):
    cites = row.get("citations") or {"docs": [], "chunks": []}
    return data.qs[row["qid"]]["doc_id"] in common.cited_pages(data, cites)


def cqa_arm_measures(data, L, arm, qids):
    sc, rows = cqa_scores(data, L, arm, qids)
    m = {"rows_ok": len(rows), "condensed": len(sc), "EM": mean(v[0] for v in sc.values()),
         "F1": mean(v[1] for v in sc.values()),
         "cited_page_recall": mean(float(cited_page(data, r)) for r in rows.values()),
         "usd_per_question": mean(r.get("usd") for r in rows.values()),
         "usd_est_per_question": mean(r["usage"]["total"]["usd_est"] for r in rows.values()),
         "wall_s": mean(r.get("wall_s") for r in rows.values())}
    for shape, test in (("yes_no", True), ("span", False)):
        s = [v[1] for q, v in sc.items() if is_yes_no(data.qs[q]["answers"]) == test]
        m["F1_" + shape] = mean(s)
        m["n_" + shape] = len(s)
    if arm == "oracle":
        m["F1_direct_on_condensed"] = mean(common.score(rows[q].get("answer_line") or "", data.qs[q]["answers"])[1]
                                           for q in sc)
    if arm in hcconfig.KB_ARMS or arm == "rag-lc":
        routes, first, esc = {}, {}, 0
        for r in rows.values():
            rt = (r.get("route") or "(none)") if arm != "rag-lc" else "answer"
            routes[rt] = routes.get(rt, 0) + 1
            f = (r.get("trailer") or {}).get("FIRST")
            if f:
                first[f] = first.get(f, 0) + 1
            if r.get("first_call_ran") and "->" in (r.get("route") or ""):
                esc += 1
        m["routes"], m["first"] = routes, first
        ran = sum(1 for r in rows.values() if r.get("first_call_ran"))
        gold_chunks = lambda q: {data.chunkmap[data.qs[q]["doc_id"]][i] for i in data.qs[q]["gold"]}  # noqa: E731
        m["not_found_with_gold_read"] = sum(
            1 for q, r in rows.items() if (r.get("trailer") or {}).get("FIRST") == "not_found"
            and gold_chunks(q) & set((r.get("first_call") or {}).get("read_chunks") or []))
        m["first_call_rows"] = ran
        if arm == "kb-first":
            m["escalation_rate"] = round(esc / ran, 4) if ran else None
    return m, sc, rows


def spread(data, L, arm, sample):
    f = {}
    for (q, rep), r in L["runs"][arm].items():
        c = L["condensed"][arm].get((q, rep, r["attempt"])) if r["status"] == "ok" else None
        if q in sample and c:
            f.setdefault(q, {})[rep] = common.score(c["condensed"], data.qs[q]["answers"])[1]
    full = {q: v for q, v in f.items() if len(v) == hcconfig.SPREAD_REPS}
    if not full:
        return {"questions_with_all_reps": 0}
    return {"questions_with_all_reps": len(full),
            "F1_by_rep": [mean(v[r] for v in full.values()) for r in range(hcconfig.SPREAD_REPS)],
            "mean_range_F1": mean(max(v.values()) - min(v.values()) for v in full.values())}


# --- the walker set ------------------------------------------------------------------

def p7_scores(qs, L, arm):
    """{qid: mean cited-document F1 over the arm's ok runs} on document-gold questions, the
    runs per question, and the mean USD per run."""
    per, usd = {}, []
    for (q, rep), r in L["runs"][arm].items():
        if q not in qs or r["status"] != "ok" or not qs[q]["gold"] or qs[q]["gold_topics"]:
            continue
        cites = r.get("citations") or {"docs": [], "chunks": []}
        cmap = r.get("chunk_docs") or {}
        docs = set(cites["docs"]) | {cmap[c] for c in cites["chunks"] if cmap.get(c)}
        per.setdefault(q, []).append(prf(docs, qs[q]["gold"], qs[q]["also"])[2])
        usd.append(r.get("usd"))
    return {q: sum(v) / len(v) for q, v in per.items()}, {q: len(v) for q, v in per.items()}, mean(usd)


# --- instrument checks -----------------------------------------------------------------

def put(c, n, name, status, **kw):
    c["%d_%s" % (n, name)] = dict(kw, status=status)


def common_checks(c, work, set_name, L, ok_rows, hit_docs, expected=None, empty=()):
    cj = jload(os.path.join(work, set_name, "corpus.json"))
    if cj:
        exp = expected if expected is not None else cj.get("expected")
        res = corpus_mod.check_same_corpus(cj["corpus"], exp, empty, hit_docs)
        put(c, 1, "same_corpus", res.pop("status"), dropped=len(cj.get("dropped") or {}), **res)
    else:
        put(c, 1, "same_corpus", "not-run", note="needs <work>/%s/corpus.json (precheck.py)" % set_name)
    cov = jload(os.path.join(work, set_name, "coverage.json"))
    if cov:
        put(c, 2, "annotated", cov["status"], coverage=cov["coverage"], stale=cov["stale_share"], documents=cov["documents"],
            at=cov.get("at"), server_sample_size=(cov.get("server_summary") or {}).get("sample_size"))
    else:
        put(c, 2, "annotated", "not-run", note="needs <work>/%s/coverage.json (sweep.py, then precheck.py)" % set_name)
    first = [r for r in ok_rows if r.get("first_call_ran")]
    if first:
        unread = sum(1 for r in first if "searches" not in (r.get("first_call") or {}))
        bad = sum(1 for r in first if (r.get("first_call") or {}).get("with_annotation"))
        status = "fail" if bad else ("not-run" if unread else "pass")
        put(c, 3, "first_call_no_annotation", status, rows=len(first), with_annotation=bad, unread=unread)
        rate = sum(1 for r in first if r.get("reranked") is True) / len(first)
        reasons = {}
        for r in first:
            if r.get("reranked") is not True:
                w = r.get("rerank_reason") or ("no RERANKED field" if r.get("reranked") is None else "?")
                reasons[w] = reasons.get(w, 0) + 1
        put(c, 4, "first_call_reranked", "pass" if rate >= hcconfig.RERANKED_MIN else "fail", rows=len(first),
            reranked_rate=round(rate, 4), not_reranked=reasons)
    else:
        put(c, 3, "first_call_no_annotation", "not-run")
        put(c, 4, "first_call_reranked", "not-run")


def every_run_answered(c, L, needed):
    """needed: {arm: [(qid, rep)]}."""
    missing, failed, twice = {}, {}, {}
    for arm, keys in needed.items():
        rows = L["runs"].get(arm) or {}
        missing[arm] = sum(1 for k in keys if k not in rows)
        failed[arm] = sum(1 for k in keys if k in rows and rows[k]["status"] != "ok")
        twice[arm] = sum(1 for k in keys if k in rows and rows[k]["status"] != "ok" and rows[k]["attempt"] >= 2)
    if any(twice.values()):
        put(c, 8, "every_run_answered", "fail", failed_after_retry=twice)
    elif any(missing.values()) or any(failed.values()):
        put(c, 8, "every_run_answered", "not-run", note="incomplete: rows missing or awaiting their retry",
            missing=missing, failed=failed)
    else:
        put(c, 8, "every_run_answered", "pass")


def one_release(c, L):
    v = [x for x in L["versions"] if x.get("version")]
    if len(v) >= 2:
        same = (v[0]["version"], v[0].get("commit")) == (v[-1]["version"], v[-1].get("commit"))
        put(c, 9, "one_release", "pass" if same else "fail", first=v[0]["version"], last=v[-1]["version"])
    else:
        put(c, 9, "one_release", "not-run")


VOIDS = {"cqa": {"1_same_corpus": ("T1", "T2"), "2_annotated": ("T1", "T2"), "3_first_call_no_annotation": ("T1", "T2"),
                 "4_first_call_reranked": ("T1", "T2"), "5_retrieval_rag-lc": ("T1",), "6_oracle_above_raglc": ("T1", "T2"),
                 "7_condense_faithful": ("T1", "T2"), "8_every_run_answered": ("T1", "T2"), "9_one_release": ("T1", "T2")},
         "p7": {"1_same_corpus": ("T3",), "2_annotated": ("T3",), "3_first_call_no_annotation": ("T3",),
                "4_first_call_reranked": ("T3",), "8_every_run_answered": ("T3",), "9_one_release": ("T3",)}}


def problems_of(L):
    p = []
    if any(e.get("kind") == "stop" and str(e.get("reason", "")).startswith("spend cap") for e in L["events"]):
        p.append("stopped by the spend cap")
    if any(e.get("kind") == "provider_violation" for e in L["events"]):
        p.append("provider violation recorded")
    states = {r.get("state") for rows in L["runs"].values() for r in rows.values() if r["status"] == "ok"}
    if len(states) > 1:
        p.append("rows ran under %d tenant states" % len(states))
    return p


def verdict(res, voided, not_run):
    if voided:
        return "void (failed check: %s)" % ", ".join(voided)
    return ("holds" if res["holds"] else "does not hold") + (
        " (provisional: checks not run: %s)" % ", ".join(not_run) if not_run else "")


def analyze_cqa(work, out, data, test_qids):
    L = load(out, "cqa")
    S, scores, rows = {}, {}, {}
    for a in hcconfig.ARMS_FOR["cqa"]:
        S[a], scores[a], rows[a] = cqa_arm_measures(data, L, a, test_qids)
        S[a]["spread"] = spread(data, L, a, common.spread_sample(test_qids))
    f1 = {a: {q: v[1] for q, v in scores[a].items()} for a in scores}
    res = {"arms": S, "checks": {}, "tests": {}, "secondary": {}, "cost_check": None, "problems": problems_of(L)}
    c = res["checks"]
    ok_rows = [r for a in rows for r in rows[a].values()]
    # As on the walker set: kb/ask cannot be restricted, so check 1's hit test reads the first
    # call's own rows (rag-lc); a kb citation outside the corpus is a wrong citation.
    hit_docs = set()
    for r in rows["rag-lc"].values():
        cites = r.get("citations") or {"docs": [], "chunks": []}
        hit_docs |= set(cites["docs"]) | {d for d in (r.get("chunk_docs") or {}).values() if d}
        hit_docs |= {data.docmap.get(data.section_of_chunk[ch][0]) for ch in cites["chunks"] if ch in data.section_of_chunk}
    empty = {data.docmap.get(d) for d, doc in data.docs.items() if not any(s["text"].strip() for s in doc["sections"])}
    common_checks(c, work, "cqa", L, ok_rows, hit_docs, expected=sorted(data.docmap.values()) or None, empty=empty)
    rl = rows["rag-lc"]
    if rl:
        rec = mean(float(cited_page(data, r)) for r in rl.values())
        ch = mean(chance_page(data, data.qs[q]) for q in rl)
        put(c, 5, "retrieval_rag-lc", "pass" if rec > hcconfig.RETRIEVAL_X_CHANCE * ch else "fail",
            cited_page_recall=rec, ten_x_chance=round(hcconfig.RETRIEVAL_X_CHANCE * ch, 6))
    else:
        put(c, 5, "retrieval_rag-lc", "not-run")
    o, r_ = S["oracle"], S["rag-lc"]
    if o["F1"] is not None and r_["F1"] is not None:
        put(c, 6, "oracle_above_raglc", "pass" if o["F1"] > r_["F1"] else "fail", oracle=o["F1"], rag_lc=r_["F1"])
    else:
        put(c, 6, "oracle_above_raglc", "not-run")
    if o["F1"] is not None and o.get("F1_direct_on_condensed") is not None:
        d = abs(o["F1"] - o["F1_direct_on_condensed"])
        put(c, 7, "condense_faithful", "pass" if d <= hcconfig.CONDENSE_MAX_DIFF else "fail", condensed=o["F1"],
            direct=o["F1_direct_on_condensed"], diff=round(d, 4), bound=hcconfig.CONDENSE_MAX_DIFF)
    else:
        put(c, 7, "condense_faithful", "not-run")
    a1 = {}
    a1_same_corpus(a1, work, "cqa", hit_docs)
    a1_condense_extractive(a1, rows["oracle"], L["condensed"]["oracle"])
    every_run_answered(c, L, {a: [(q, 0) for q in test_qids] for a in T_ARMS["cqa"]})
    one_release(c, L)

    complete = all(len(scores[a]) == len(test_qids) for a in T_ARMS["cqa"]) and not res["problems"]
    res["complete"] = complete
    voided = {}
    for n, x in c.items():
        if x["status"] == "fail":
            for t in VOIDS["cqa"].get(n, ()):
                voided.setdefault(t, []).append(n)
    not_run = [n for n, x in c.items() if x["status"] == "not-run"]
    for t, a, b, bound in (("T1", "kb-first", "rag-lc", hcconfig.T1_BOUND), ("T2", "kb-first", "kb-today", hcconfig.T2_BOUND)):
        if not complete:
            res["tests"][t] = {"comparison": "%s - %s, condensed F1" % (a, b), "verdict": "not tested (set incomplete)"}
            continue
        x = paired(f1[a], f1[b], test_qids, bound)
        x["comparison"] = "%s - %s, condensed F1" % (a, b)
        x["verdict"] = verdict(x, voided.get(t), not_run)
        res["tests"][t] = x
    for a, b in (("kb-today+answer-first", "kb-today"), ("kb-today+answer-first", "kb-first")):
        x = paired(f1[a], f1[b], test_qids, 0.0) if len(f1[a]) == len(test_qids) else {"verdict": "not computed (incomplete)"}
        x["comparison"] = "%s - %s, condensed F1 (secondary, reported)" % (a, b)
        res["secondary"][a + " vs " + b] = x
    res["amendment_1"] = amended(res, "cqa", a1)
    kt, kf = S["kb-today"]["usd_per_question"], S["kb-first"]["usd_per_question"]
    if kt and kf is not None:
        res["cost_check"] = {"kb_first": kf, "kb_today": kt, "max_ratio": hcconfig.COST_CQA_MAX_RATIO,
                             "holds": kf <= hcconfig.COST_CQA_MAX_RATIO * kt}
    return res


def analyze_p7(work, out, qs):
    L = load(out, "p7")
    doc_q = sorted(q for q in qs if qs[q]["gold"] and not qs[q]["gold_topics"])
    S, f1, nruns = {}, {}, {}
    for a in hcconfig.ARMS_FOR["p7"]:
        f1[a], nruns[a], usd = p7_scores(qs, L, a)
        S[a] = {"questions": len(f1[a]), "F1": mean(f1[a].values()), "usd_per_run": usd}
    res = {"arms": S, "checks": {}, "tests": {}, "secondary": {}, "cost_check": None, "problems": problems_of(L)}
    c = res["checks"]
    ok_rows = [r for a in L["runs"] for r in L["runs"][a].values() if r["status"] == "ok"]
    # kb/ask cannot be restricted to the corpus, so a kb citation outside it is a wrong citation
    # (precision), not a corpus failure: check 1's hit test reads the first call's rows only.
    first_hits = set()
    for r in ok_rows:
        if r["arm"] == "rag-lc":
            cites = r.get("citations") or {"docs": [], "chunks": []}
            first_hits |= set(cites["docs"]) | {d for d in (r.get("chunk_docs") or {}).values() if d}
    common_checks(c, work, "p7", L, ok_rows, first_hits)
    a1 = {}
    a1_same_corpus(a1, work, "p7", first_hits)
    every_run_answered(c, L, {a: [(q, rep) for q in doc_q for rep in range(hcconfig.P7_REPS)] for a in T_ARMS["p7"]})
    one_release(c, L)
    complete = all(nruns[a].get(q) == hcconfig.P7_REPS for a in T_ARMS["p7"] for q in doc_q) and not res["problems"]
    res["complete"] = complete
    voided = [n for n, x in c.items() if x["status"] == "fail" and "T3" in VOIDS["p7"].get(n, ())]
    not_run = [n for n, x in c.items() if x["status"] == "not-run"]
    if not complete:
        res["tests"]["T3"] = {"comparison": "kb-first - kb-today, cited-document F1", "verdict": "not tested (set incomplete)"}
    else:
        x = paired(f1["kb-first"], f1["kb-today"], doc_q, hcconfig.T3_BOUND)
        x["comparison"] = "kb-first - kb-today, cited-document F1"
        x["verdict"] = verdict(x, voided, not_run)
        x["fails_below_stop_point"] = (not x["holds"]) and x["diff"] < hcconfig.T3_STOP_POINT
        res["tests"]["T3"] = x
    for a, b in (("kb-today+answer-first", "kb-today"), ("rag-lc", "kb-today")):
        x = paired(f1[a], f1[b], doc_q, 0.0)
        x["comparison"] = "%s - %s, cited-document F1 (secondary, reported)" % (a, b)
        res["secondary"][a + " vs " + b] = x
    res["amendment_1"] = amended(res, "p7", a1)
    kt, kf = S["kb-today"]["usd_per_run"], S["kb-first"]["usd_per_run"]
    if kt and kf is not None:
        res["cost_check"] = {"kb_first": kf, "kb_today": kt, "max_ratio": hcconfig.COST_P7_MAX_RATIO,
                             "holds": kf <= hcconfig.COST_P7_MAX_RATIO * kt}
    return res


# --- Amendment 1 (post hoc, owner ruling 2026-11-28): exploratory, never the headline ------
# Two registered checks could not fail for the reason they guard. Check 7 scored the reader's
# ANSWER: line against short gold spans, but the bundle's kb/read writes a sentence there, so
# its direct F1 sits far below any condensed F1 whatever the condenser does. Check 1's hit test
# failed on two documents that were in the tenant at the corpus snapshot and were left out by
# the filter (the ontology, one untyped memory document): a wrong citation, not a corpus change.
# The registered verdict stays the result; these replace those two checks for a labelled
# exploratory reading only. Each replacement is shown to fail on a corrupted input in the tests.

def _empty_answer(s):
    return common.normalize(s or "") in ("", "not found", "no answer")


def condense_extractive(condensed, direct):
    """The condensed answer is drawn from the reader's own ANSWER: line (every word of it occurs
    there), or it is empty (NO ANSWER / NOT FOUND) exactly when that line is."""
    if _empty_answer(condensed) or _empty_answer(direct):
        return _empty_answer(condensed) and _empty_answer(direct)
    return set(common.normalize(condensed).split()) <= set(common.normalize(direct).split())


def a1_condense_extractive(c, oracle_rows, condensed):
    """A1 check 7: the share of the oracle's scored rows whose condense is extractive is at least
    1 - CONDENSE_MAX_DIFF (the registered 0.03 bound, read as a share)."""
    got = [condense_extractive(x["condensed"], x.get("direct"))
           for x in (condensed.get((q, 0, r["attempt"])) for q, r in oracle_rows.items()) if x]
    if not got:
        put(c, 7, "condense_faithful", "not-run")
        return
    share, need = round(sum(got) / len(got), 4), round(1 - hcconfig.CONDENSE_MAX_DIFF, 4)
    put(c, 7, "condense_faithful", "pass" if share >= need else "fail", extractive_share=share, min_share=need,
        rows=len(got), not_extractive=len(got) - sum(got))


def a1_same_corpus(c, work, set_name, hit_docs):
    """A1 check 1: a cited document fails the set only when it was not in the tenant at the corpus
    snapshot (neither kept nor dropped by the filter), i.e. the corpus changed under the runs.
    Citations of snapshot documents the filter left out are reported, not failed."""
    cj = jload(os.path.join(work, set_name, "corpus.json"))
    if not cj:
        put(c, 1, "same_corpus", "not-run")
        return
    snap = set(cj["corpus"]) | set(cj.get("dropped") or {})
    hits = set(hit_docs)
    new = sorted(hits - snap)
    put(c, 1, "same_corpus", "fail" if new else "pass", not_in_snapshot=new[:20],
        cited_left_out={d: cj["dropped"][d] for d in sorted(hits & set(cj.get("dropped") or {}))})


def amended(res, set_name, a1_checks):
    """The registered tests re-labelled with the A1 checks in place of the registered ones."""
    checks = dict(res["checks"], **a1_checks)
    out = {"checks": a1_checks, "tests": {}}
    not_run = [n for n, x in checks.items() if x["status"] == "not-run"]
    for t, x in res["tests"].items():
        if "diff" not in x:
            out["tests"][t] = dict(x)
            continue
        voided = [n for n, k in checks.items() if k["status"] == "fail" and t in VOIDS[set_name].get(n, ())]
        out["tests"][t] = dict(x, verdict=verdict(x, voided, not_run))
    return out


def gate(cqa, p7):
    """Plan §5: P2 opens iff T1 and T2 hold, T3 does not fail with a point estimate below
    -0.05, and both cost checks hold. Anything untested or void leaves the gate closed."""
    t1, t2, t3 = cqa["tests"].get("T1", {}), cqa["tests"].get("T2", {}), p7["tests"].get("T3", {})
    reasons = []
    for name, t in (("T1", t1), ("T2", t2)):
        if not str(t.get("verdict", "")).startswith("holds"):
            reasons.append("%s: %s" % (name, t.get("verdict")))
    if "diff" not in t3 or str(t3.get("verdict", "")).startswith("void"):
        reasons.append("T3: %s" % t3.get("verdict"))
    elif t3.get("fails_below_stop_point"):
        reasons.append("T3 fails with point estimate %s below %s" % (t3["diff"], hcconfig.T3_STOP_POINT))
    for name, cc in (("ConditionalQA cost", cqa.get("cost_check")), ("walker-set cost", p7.get("cost_check"))):
        if not cc or not cc.get("holds"):
            reasons.append("%s check: %s" % (name, "not computed" if not cc else "fails"))
    return {"P2_opens": not reasons, "reasons": reasons}


def markdown(s):
    L = ["# RFC HC gate study", "", "**Gate:** %s" % ("P2 opens" if s["gate"]["P2_opens"] else "closed"), ""]
    L += ["- %s" % r for r in s["gate"]["reasons"]]
    for set_name in ("cqa", "p7"):
        x = s[set_name]
        L += ["", "## %s (%s)" % (set_name, "complete" if x["complete"] else "INCOMPLETE"), ""]
        L += ["- problem: %s" % p for p in x["problems"]]
        for a, m in x["arms"].items():
            L.append("- `%s`: %s" % (a, json.dumps({k: v for k, v in m.items() if not isinstance(v, dict)})))
        for t, r in x["tests"].items():
            L.append("- **%s** (%s): %s%s" % (t, r.get("comparison"), r.get("verdict"),
                                              "" if "ci" not in r else ": diff %s, %.2f%% CI [%s, %s] vs bound %s" % (
                                                  r["diff"], 100 * r["ci_level"], r["ci"][0], r["ci"][1], r["bound"])))
        for t, r in x["secondary"].items():
            L.append("- %s: %s" % (t, json.dumps(r)))
        L.append("- cost check: %s" % json.dumps(x["cost_check"]))
        for n, c in x["checks"].items():
            rest = {k: v for k, v in c.items() if k != "status"}
            L.append("- check %s: **%s** %s" % (n, c["status"], json.dumps(rest, default=list) if rest else ""))
    if "gate_amendment_1" in s:
        g = s["gate_amendment_1"]
        L += ["", "## Amendment 1 (post hoc, exploratory; not the registered result)", "",
              "**Gate under Amendment 1:** %s" % ("P2 would open" if g["P2_opens"] else "closed")]
        L += ["- %s" % r for r in g["reasons"]]
        for set_name in ("cqa", "p7"):
            a1 = s[set_name]["amendment_1"]
            for n, c in a1["checks"].items():
                rest = {k: v for k, v in c.items() if k != "status"}
                L.append("- %s A1 check %s: **%s** %s" % (set_name, n, c["status"], json.dumps(rest, default=list)))
            for t, r in a1["tests"].items():
                L.append("- %s **%s** under A1: %s" % (set_name, t, r.get("verdict")))
    return "\n".join(L) + "\n"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    a = ap.parse_args()
    import hcarms, splits
    work = hcconfig.work()
    cqa_data = hcarms.load_set("cqa", work).cqa
    test = splits.load()["test"]
    p7 = hcarms.load_set("p7", work).qs
    s = {"cqa": analyze_cqa(work, a.out, cqa_data, test), "p7": analyze_p7(work, a.out, p7)}
    s["gate"] = gate(s["cqa"], s["p7"])
    s["gate_amendment_1"] = gate(dict(s["cqa"], tests=s["cqa"]["amendment_1"]["tests"]),
                                 dict(s["p7"], tests=s["p7"]["amendment_1"]["tests"]))
    common.jdump(s, os.path.join(a.out, "summary.json"), indent=2, default=list)
    md = markdown(s)
    with open(os.path.join(a.out, "summary.md"), "w", encoding="utf-8") as f:
        f.write(md)
    print(md)


if __name__ == "__main__":
    main()
