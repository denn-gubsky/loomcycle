"""The analysis: every instrument check fails where it should, T1-T3 and the cost checks
are computed on a complete set only, and the gate reads them.

    python3 -I -m unittest discover -s bench/docs/kbrag/hc/tests
"""
import json, os, tempfile, unittest
from unittest import mock

import hcfixture
from hcfixture import DOCMAP, FakeClient, FakeSpend, QS, write_json
import corpus, coverage, hcanalyze, hcarms, hccondense, hcconfig
import common, passes

QIDS = [q["qid"] for q in QS]
LISTED = [{"id": d, "title": d, "type": None, "paths": ["/cqa/" + d]} for d in DOCMAP.values()]
HOUSEKEEPING = {"id": "MEM1", "title": "context", "type": None, "paths": ["/memory/context"]}
# rag-lc is wrong on q4 (the oracle then reads better: check 6) and kb-today on q3 (kb-first beats it: T2).
WRONG = {("rag-lc", "q4"), ("kb-today", "q3")}


class Study:
    """A complete synthetic ConditionalQA set on disk: runs, condensed answers, check inputs."""

    def __init__(self, lc=None, listed=None, annotated=None):
        self.work, self.cqa = hcfixture.make_work()
        self.out = tempfile.mkdtemp(prefix="kbrag-hc-out-")
        self.lc = lc or FakeClient(wrong=WRONG)
        data = hcarms.SetData("cqa", self.cqa.qs, self.cqa)
        hcarms.Runner(self.lc, data, arms=hcconfig.ARMS, out=self.out, split="test", spend=FakeSpend(),
                      log=lambda s: None, contract=hcfixture.CONTRACT).run(hcarms.plan(QIDS, 1))
        call = hccondense.hc_condense_call(self.lc, self.cqa)
        for arm in hcconfig.ARMS:
            runs = passes.latest_ok_runs(arm, os.path.join(self.out, "cqa", "runs"))
            items = [((r["qid"], r["rep"], r["attempt"]), r) for _, r in sorted(runs.items())]
            passes.run_pass(self.lc, items, call, os.path.join(self.out, "cqa", "condensed", arm + ".jsonl"),
                            budget_usd=None, log=lambda s: None)
        docs = listed if listed is not None else LISTED + [HOUSEKEEPING]
        keep, dropped = corpus.filter_corpus(docs)
        write_json(os.path.join(self.work, "cqa", "corpus.json"), {"corpus": keep, "dropped": dropped, "expected": None})
        anns = [{"document_id": d, "stale": False} for d in (annotated if annotated is not None else keep)]
        write_json(os.path.join(self.work, "cqa", "coverage.json"), dict(coverage.coverage(keep, anns), at="t"))

    def analyze(self):
        # The fixture's corpus is 7 readable sections, so chance is 1 and check 5 cannot pass at
        # the pre-registered 10x; the tests that need it lower the factor, the check test keeps it.
        return hcanalyze.analyze_cqa(self.work, self.out, self.cqa, QIDS)


class TestCQA(unittest.TestCase):
    @mock.patch.object(hcconfig, "RETRIEVAL_X_CHANCE", 0.5)
    def test_complete_study_passes_every_check_and_tests_t1_t2(self):
        s = Study().analyze()
        bad = {n: c for n, c in s["checks"].items() if c["status"] != "pass"}
        self.assertEqual(bad, {})
        self.assertTrue(s["complete"])
        self.assertEqual(s["arms"]["kb-first"]["F1"], 1.0)
        self.assertEqual(s["arms"]["rag-lc"]["F1"], 0.75)
        self.assertTrue(s["tests"]["T1"]["verdict"].startswith("holds"), s["tests"]["T1"])
        self.assertEqual(s["tests"]["T2"]["diff"], 0.25)
        self.assertEqual(s["tests"]["T1"]["ci_level"], round(1 - 0.05 / 3, 4))
        self.assertIsNotNone(s["cost_check"])
        self.assertEqual(s["arms"]["kb-first"]["first_call_rows"], 4)
        self.assertEqual(s["arms"]["kb-first"]["first"], {"found": 4})

    def test_check_1_fails_on_an_extra_listed_document(self):
        st = Study(listed=LISTED + [HOUSEKEEPING, {"id": "EXTRA", "type": "Note", "paths": ["/n"]}])
        self.assertEqual(st.analyze()["checks"]["1_same_corpus"]["status"], "fail")

    def test_check_1_passes_with_the_untyped_memory_document_listed(self):
        self.assertEqual(Study().analyze()["checks"]["1_same_corpus"]["status"], "pass")

    def test_check_2_fails_when_a_corpus_document_lacks_its_annotation(self):
        st = Study(annotated=list(DOCMAP.values())[:2])
        self.assertEqual(st.analyze()["checks"]["2_annotated"]["status"], "fail")

    def test_check_3_fails_when_the_first_call_searched_with_annotations(self):
        lc = FakeClient(wrong=WRONG, search_inputs=[{"op": "search", "annotations": "include"}])
        self.assertEqual(Study(lc=lc).analyze()["checks"]["3_first_call_no_annotation"]["status"], "fail")

    def test_check_4_fails_below_95_percent_reranked(self):
        lc = FakeClient(wrong=WRONG, reranked=lambda qid: qid != "q1")
        c = Study(lc=lc).analyze()["checks"]["4_first_call_reranked"]
        self.assertEqual((c["status"], c["rows"], c["reranked_rate"]), ("fail", 8, 0.75))
        self.assertEqual(c["not_reranked"], {"timeout": 2})

    def test_check_4_counts_95_percent_as_a_pass(self):
        rows = [{"first_call_ran": True, "reranked": i != 0, "first_call": {"searches": 1, "with_annotation": 0}}
                for i in range(20)]
        c = {}
        hcanalyze.common_checks(c, tempfile.mkdtemp(), "cqa", None, rows, set())
        self.assertEqual(c["4_first_call_reranked"]["status"], "pass")
        rows[1]["reranked"] = False
        c = {}
        hcanalyze.common_checks(c, tempfile.mkdtemp(), "cqa", None, rows, set())
        self.assertEqual(c["4_first_call_reranked"]["status"], "fail")

    def test_check_5_fails_at_ten_times_a_chance_of_one(self):
        self.assertEqual(Study().analyze()["checks"]["5_retrieval_rag-lc"]["status"], "fail")

    def test_check_6_fails_when_the_oracle_reads_no_better_than_rag_lc(self):
        lc = FakeClient(wrong={("oracle", "q4"), ("rag-lc", "q4")})
        self.assertEqual(Study(lc=lc).analyze()["checks"]["6_oracle_above_raglc"]["status"], "fail")

    def test_check_7_fails_when_condensing_moves_the_oracles_f1(self):
        st = Study()
        p = os.path.join(st.out, "cqa", "condensed", "oracle.jsonl")
        rows = common.read_jsonl(p)
        rows[0]["condensed"] = "NO ANSWER"
        with open(p, "w") as f:
            f.writelines(json.dumps(r) + "\n" for r in rows)
        c = st.analyze()["checks"]["7_condense_faithful"]
        self.assertEqual((c["status"], c["bound"]), ("fail", 0.03))

    def test_check_8_fails_on_a_run_that_failed_twice(self):
        st = Study()
        common.append_jsonl(os.path.join(st.out, "cqa", "runs", "kb-first.jsonl"),
                            {"arm": "kb-first", "qid": "q2", "rep": 0, "attempt": 2, "status": "failed", "error": "x"})
        s = st.analyze()
        self.assertEqual(s["checks"]["8_every_run_answered"]["status"], "fail")
        self.assertFalse(s["complete"])
        self.assertTrue(s["tests"]["T1"]["verdict"].startswith("not tested"))

    def test_check_9_fails_when_the_release_changed(self):
        st = Study()
        common.append_jsonl(os.path.join(st.out, "cqa", "runs", "versions.jsonl"), {"version": "1.171.0", "commit": "x"})
        self.assertEqual(st.analyze()["checks"]["9_one_release"]["status"], "fail")

    @mock.patch.object(hcconfig, "RETRIEVAL_X_CHANCE", 0.5)
    def test_a_failed_check_voids_the_tests_it_guards(self):
        lc = FakeClient(wrong=WRONG, reranked=lambda qid: qid != "q1")
        s = Study(lc=lc).analyze()
        self.assertTrue(s["tests"]["T1"]["verdict"].startswith("void"))
        self.assertIn("4_first_call_reranked", s["tests"]["T2"]["verdict"])

    def test_a_spend_cap_stop_leaves_the_set_incomplete(self):
        st = Study()
        common.append_jsonl(os.path.join(st.out, "cqa", "runs", "events.jsonl"), {"kind": "stop", "reason": "spend cap: x"})
        s = st.analyze()
        self.assertFalse(s["complete"])
        self.assertIn("stopped by the spend cap", s["problems"])


def p7_rows(out, arm, docs_by_q, reps=3, usd=0.02):
    for q, docs in docs_by_q.items():
        for rep in range(reps):
            common.append_jsonl(os.path.join(out, "p7", "runs", arm + ".jsonl"), {
                "arm": arm, "qid": q, "rep": rep, "attempt": 1, "status": "ok", "state": "s",
                "citations": {"docs": docs, "chunks": []}, "usd": usd, "first_call_ran": arm == "rag-lc",
                "reranked": True, "first_call": {"searches": 1, "with_annotation": 0}})
    common.append_jsonl(os.path.join(out, "p7", "runs", "versions.jsonl"), {"version": "1.170.0", "commit": "c"})


P7_QS = {"p7-q%02d" % i: {"qid": "p7-q%02d" % i, "gold": ["g%d" % i, "h%d" % i], "also": ["a%d" % i], "gold_topics": [],
                          "text": "t"} for i in range(1, 9)}


class TestP7(unittest.TestCase):
    def setUp(self):
        self.work, self.out = tempfile.mkdtemp(), tempfile.mkdtemp()
        all_docs = sorted({d for q in P7_QS.values() for d in q["gold"] + q["also"]})
        write_json(os.path.join(self.work, "p7", "corpus.json"), {"corpus": all_docs, "expected": None})
        write_json(os.path.join(self.work, "p7", "coverage.json"), dict(
            coverage.coverage(all_docs, [{"document_id": d, "stale": False} for d in all_docs]), at="t"))

    def run_arms(self, first_docs, first_usd=0.02):
        both = {q: v["gold"] for q, v in P7_QS.items()}
        for arm in ("kb-today", "kb-today+answer-first", "rag-lc"):
            p7_rows(self.out, arm, both)
        p7_rows(self.out, "kb-first", first_docs, usd=first_usd)
        return hcanalyze.analyze_p7(self.work, self.out, P7_QS)

    def test_t3_holds_when_kb_first_keeps_the_documents(self):
        s = self.run_arms({q: v["gold"] for q, v in P7_QS.items()})
        self.assertTrue(s["complete"])
        self.assertTrue(s["tests"]["T3"]["verdict"].startswith("holds"), s["tests"]["T3"])
        self.assertTrue(s["cost_check"]["holds"])

    def test_t3_fails_below_the_stop_point_when_kb_first_loses_documents(self):
        s = self.run_arms({q: v["gold"][:1] for q, v in P7_QS.items()})
        t = s["tests"]["T3"]
        self.assertFalse(t["holds"])
        self.assertTrue(t["fails_below_stop_point"])

    def test_p7_cost_check_fails_above_kb_today_plus_ten_percent(self):
        s = self.run_arms({q: v["gold"] for q, v in P7_QS.items()}, first_usd=0.0225)
        self.assertFalse(s["cost_check"]["holds"])

    def test_a_missing_run_leaves_t3_untested(self):
        both = {q: v["gold"] for q, v in P7_QS.items()}
        p7_rows(self.out, "kb-today", both)
        p7_rows(self.out, "kb-first", both, reps=2)
        s = hcanalyze.analyze_p7(self.work, self.out, P7_QS)
        self.assertTrue(s["tests"]["T3"]["verdict"].startswith("not tested"))


class TestGate(unittest.TestCase):
    def test_gate_opens_only_when_t1_t2_hold_t3_is_not_below_the_stop_point_and_costs_hold(self):
        ok = lambda: {"tests": {"T1": {"verdict": "holds"}, "T2": {"verdict": "holds"}},  # noqa: E731
                      "cost_check": {"holds": True}}
        p7 = lambda d, f: {"tests": {"T3": {"verdict": "holds" if d > -0.1 else "does not hold", "diff": d,  # noqa: E731
                                            "fails_below_stop_point": f}}, "cost_check": {"holds": True}}
        self.assertTrue(hcanalyze.gate(ok(), p7(0.0, False))["P2_opens"])
        # T3 not shown but its point estimate above -0.05: the gate still opens (plan §5).
        self.assertTrue(hcanalyze.gate(ok(), p7(-0.03, False))["P2_opens"])
        self.assertFalse(hcanalyze.gate(ok(), p7(-0.08, True))["P2_opens"])
        c = ok(); c["tests"]["T2"]["verdict"] = "does not hold"
        self.assertFalse(hcanalyze.gate(c, p7(0.0, False))["P2_opens"])
        c = ok(); c["cost_check"] = {"holds": False}
        self.assertFalse(hcanalyze.gate(c, p7(0.0, False))["P2_opens"])
        c = ok(); c["tests"]["T1"]["verdict"] = "not tested (set incomplete)"
        self.assertFalse(hcanalyze.gate(c, p7(0.0, False))["P2_opens"])


if __name__ == "__main__":
    unittest.main()


class TestAmendment1(unittest.TestCase):
    """The post-hoc replacements for checks 1 and 7: each passes the study's real shape and fails
    on a corrupted input, and they only re-label the registered tests (exploratory)."""

    def test_condense_extractive_accepts_a_span_of_the_readers_sentence(self):
        ok = hcanalyze.condense_extractive
        self.assertTrue(ok("No", "No, student accommodation is exempt from right to rent checks."))
        self.assertTrue(ok("halls of residence", "Yes: halls of residence are exempt."))
        self.assertTrue(ok("NO ANSWER", "NOT FOUND"))
        self.assertTrue(ok("NOT FOUND", "NOT FOUND"))

    def test_condense_extractive_rejects_words_the_reader_did_not_write_and_dropped_answers(self):
        ok = hcanalyze.condense_extractive
        self.assertFalse(ok("Yes", "No, it is exempt."))
        self.assertFalse(ok("NO ANSWER", "Send a Self Assessment tax return to HMRC."))
        self.assertFalse(ok("No", "NOT FOUND"))

    def test_a1_check_7_fails_when_condensed_answers_are_swapped_between_questions(self):
        direct = {"q%d" % i: "Yes, item %d qualifies." % i for i in range(40)}
        rows = {q: {"attempt": 1} for q in direct}
        good = {(q, 0, 1): {"condensed": "item %s" % q[1:], "direct": d} for q, d in direct.items()}
        c = {}
        hcanalyze.a1_condense_extractive(c, rows, good)
        self.assertEqual((c["7_condense_faithful"]["status"], c["7_condense_faithful"]["extractive_share"]), ("pass", 1.0))
        qs = sorted(direct)
        swapped = {(q, 0, 1): {"condensed": "item %s" % qs[(i + 1) % len(qs)][1:], "direct": direct[q]}
                   for i, q in enumerate(qs)}
        c = {}
        hcanalyze.a1_condense_extractive(c, rows, swapped)
        self.assertEqual(c["7_condense_faithful"]["status"], "fail")

    def test_a1_check_7_fails_on_the_study_when_the_condenser_drops_an_answer(self):
        st = Study()
        p = os.path.join(st.out, "cqa", "condensed", "oracle.jsonl")
        rows = common.read_jsonl(p)
        rows[0]["condensed"] = "NO ANSWER"
        with open(p, "w") as f:
            f.writelines(json.dumps(r) + "\n" for r in rows)
        self.assertEqual(st.analyze()["amendment_1"]["checks"]["7_condense_faithful"]["status"], "fail")

    def test_a1_check_1_passes_a_cited_snapshot_document_and_fails_a_new_one(self):
        work = tempfile.mkdtemp()
        write_json(os.path.join(work, "p7", "corpus.json"),
                   {"corpus": ["D1", "D2"], "dropped": {"ONT": "ontology"}, "expected": None})
        c = {}
        hcanalyze.a1_same_corpus(c, work, "p7", {"D1", "ONT"})
        self.assertEqual(c["1_same_corpus"]["status"], "pass")
        self.assertEqual(c["1_same_corpus"]["cited_left_out"], {"ONT": "ontology"})
        c = {}
        hcanalyze.a1_same_corpus(c, work, "p7", {"D1", "NEW"})
        self.assertEqual((c["1_same_corpus"]["status"], c["1_same_corpus"]["not_in_snapshot"]), ("fail", ["NEW"]))

    def test_amended_relabels_a_test_voided_only_by_a_replaced_check(self):
        res = {"checks": {"7_condense_faithful": {"status": "fail"}, "6_oracle_above_raglc": {"status": "pass"}},
               "tests": {"T1": {"diff": 0.01, "ci": [-0.01, 0.03], "holds": True, "verdict": "void (failed check: 7)"},
                         "T2": {"verdict": "not tested (set incomplete)"}}}
        a = hcanalyze.amended(res, "cqa", {"7_condense_faithful": {"status": "pass"}})
        self.assertEqual(a["tests"]["T1"]["verdict"], "holds")
        self.assertTrue(a["tests"]["T2"]["verdict"].startswith("not tested"))
        self.assertTrue(res["tests"]["T1"]["verdict"].startswith("void"))  # the registered result is untouched
        res["checks"]["6_oracle_above_raglc"]["status"] = "fail"
        a = hcanalyze.amended(res, "cqa", {"7_condense_faithful": {"status": "pass"}})
        self.assertIn("6_oracle_above_raglc", a["tests"]["T1"]["verdict"])
