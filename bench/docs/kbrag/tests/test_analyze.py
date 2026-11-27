import json, os, random, unittest

import fixture
import analyze, arms, common, condense, judge, passes, ragext
from test_arms import ALL, ragext_factory

QIDS = [q["qid"] for q in fixture.QS]


def build_study(work, data, lc=None, arms_=ALL):
    lc = lc or fixture.FakeLC()
    arms.Runner(lc, data, profile="haiku", arms=arms_, out_dir=os.path.join(work, "runs"),
                ragext_factory=ragext_factory, log=lambda s: None).run(QIDS, reps=1)
    for arm in arms_:
        runs = passes.latest_ok_runs(arm, os.path.join(work, "runs"))
        items = [((r["qid"], r["rep"], r["attempt"]), r) for _, r in sorted(runs.items())]
        passes.run_pass(lc, items, condense.condense_call(lc, data, ("anthropic", "claude-haiku-5-5")),
                        os.path.join(work, "condensed", arm + ".jsonl"), budget_usd=19.0, log=lambda s: None)
    by_arm = {arm: passes.latest_ok_runs(arm, os.path.join(work, "runs")) for arm in arms_}
    call = judge.judge_call(lc, data)
    for arm, pos, r in judge.schedule(by_arm):
        passes.run_pass(lc, [((r["qid"], r["rep"], r["attempt"]), r)], lambda s, u, pos=pos: dict(call(s, u), order=pos),
                        os.path.join(work, "judge", arm + ".jsonl"), budget_usd=19.0, log=lambda s: None)
    return lc


class TestAnalyze(unittest.TestCase):
    def setUp(self):
        self.work, self.data = fixture.make_work()

    def wrong_condensed(self, arm, qid):
        p = os.path.join(self.work, "condensed", arm + ".jsonl")
        rows = common.read_jsonl(p)
        for r in rows:
            if r["qid"] == qid:
                r["condensed"] = "NO ANSWER"
        with open(p, "w") as f:
            f.writelines(json.dumps(r) + "\n" for r in rows)

    def test_complete_synthetic_study_is_scored_with_checks(self):
        build_study(self.work, self.data)
        for arm in ("rag-lc", "rag-ext"):  # the RAG arms miss one question the oracle and kb get
            self.wrong_condensed(arm, "q2")
        # The toy corpus has 7 readable sections, so 10x chance would exceed 1; the real
        # one has 12,800. Check 5 is scored here as on a corpus of that size.
        self.data.n_readable = 12800
        s = analyze.analyze(self.work, self.data)
        self.assertTrue(s["complete"], s["problems"])
        self.assertEqual(s["arms"]["kb"]["F1"], 1.0)
        self.assertEqual(s["arms"]["rag-lc"]["F1"], 0.75)
        for a in ALL:
            self.assertEqual(s["arms"][a]["cited_page_recall"], 1.0)
            self.assertEqual(s["arms"][a]["judge"]["CORRECT"], 4)
        self.assertEqual(s["arms"]["kb"]["cost_per_correct"]["judge_tokens"], 1100.0)
        self.assertEqual(s["arms"]["rag-lc"]["R@1"], 1.0)
        self.assertNotIn("R@1", s["arms"]["kb"])
        self.assertEqual(s["arms"]["kb"]["routes"], {"search": 4})
        h3 = s["hypotheses"]["H3"]
        self.assertEqual(h3["diff"], 0.25)
        self.assertGreater(h3["ci"][0], -0.02)
        self.assertTrue(h3["non_inferior"])
        self.assertTrue(h3["verdict"].startswith("holds (provisional"), h3["verdict"])
        c = s["checks"]
        for name in ("3_raglc_no_annotation", "4_rerank_raglc", "4_rerank_ragext", "6_oracle_above_rag",
                     "7_condense_faithful", "8_every_run_answered", "9_one_release"):
            self.assertEqual(c[name]["status"], "pass", name)
        self.assertEqual(c["1_same_corpus"]["status"], "not-run")
        self.assertEqual(c["2_annotated"]["status"], "not-run")
        self.assertIn("| kb |", analyze.markdown(s))

    def test_a_failed_check_voids_its_hypotheses(self):
        build_study(self.work, self.data)  # oracle == RAG arms: check 6 fails
        s = analyze.analyze(self.work, self.data)
        self.assertEqual(s["checks"]["6_oracle_above_rag"]["status"], "fail")
        self.assertTrue(s["hypotheses"]["H3"]["verdict"].startswith("void"))
        self.assertTrue(s["hypotheses"]["H4"]["verdict"].startswith("void"))

    def test_partial_study_is_incomplete_and_not_tested(self):
        build_study(self.work, self.data, arms_=["kb", "rag-lc"])
        s = analyze.analyze(self.work, self.data)
        self.assertFalse(s["complete"])
        self.assertEqual(s["hypotheses"]["H3"]["verdict"], "not tested (study incomplete)")
        self.assertNotIn("ci", s["hypotheses"]["H3"])
        self.assertEqual(s["arms"]["kb"]["F1"], 1.0)  # partial measures are still reported

    def test_mixed_profiles_are_refused(self):
        build_study(self.work, self.data)
        p = os.path.join(self.work, "runs", "oracle.jsonl")
        rows = common.read_jsonl(p)
        rows[0]["reader_served"] = ["ollama-local", "qwen3.6:latest"]
        with open(p, "w") as f:
            f.writelines(json.dumps(r) + "\n" for r in rows)
        s = analyze.analyze(self.work, self.data)
        self.assertFalse(s["complete"])
        self.assertTrue(any("mixed profile" in x for x in s["problems"]))

    def test_spend_cap_stop_makes_the_study_incomplete(self):
        build_study(self.work, self.data)
        common.append_jsonl(os.path.join(self.work, "runs", "events.jsonl"),
                            {"kind": "stop", "reason": "spend cap: ...", "ts": "2026-11-23T00:00:00Z"})
        s = analyze.analyze(self.work, self.data)
        self.assertFalse(s["complete"])

    def test_same_corpus_check_runs_when_the_tenant_listing_exists(self):
        build_study(self.work, self.data)
        chunks = ragext.build_chunks(self.data.docs)
        os.makedirs(os.path.join(self.work, "ragext"), exist_ok=True)
        common.jdump({"order": [c["id"] for c in chunks]}, os.path.join(self.work, "ragext", "manifest.json"))
        common.jdump(sorted(fixture.DOCMAP.values()), os.path.join(self.work, "tenant_documents.json"))
        s = analyze.analyze(self.work, self.data)
        self.assertEqual(s["checks"]["1_same_corpus"]["status"], "pass")
        common.jdump(sorted(fixture.DOCMAP.values()) + ["Dextra"], os.path.join(self.work, "tenant_documents.json"))
        self.assertEqual(analyze.analyze(self.work, self.data)["checks"]["1_same_corpus"]["status"], "fail")


    def test_annotated_check_reads_the_summarize_output(self):
        build_study(self.work, self.data)
        common.jdump({"counts": {"document": {"total": 3, "stale": 0, "manual": 0}}},
                     os.path.join(self.work, "annotation_summary.json"))
        self.assertEqual(analyze.analyze(self.work, self.data)["checks"]["2_annotated"]["status"], "pass")
        common.jdump({"counts": {"document": {"total": 2, "stale": 0, "manual": 0}}},
                     os.path.join(self.work, "annotation_summary.json"))
        c = analyze.analyze(self.work, self.data)["checks"]["2_annotated"]
        self.assertEqual((c["status"], c["coverage"]), ("fail", 0.6667))


class TestBootstrap(unittest.TestCase):
    def test_bootstrap_is_deterministic_under_seed_1(self):
        rng = random.Random(7)
        d = [rng.uniform(-1, 1) for _ in range(200)]
        a = analyze.bootstrap_ci(d, n=2000)
        self.assertEqual(a, analyze.bootstrap_ci(d, n=2000))
        self.assertNotEqual(a, analyze.bootstrap_ci(d, n=2000, seed=2))
        lo, hi = a
        self.assertLess(lo, sum(d) / len(d))
        self.assertGreater(hi, sum(d) / len(d))

    def test_ci_is_the_9875_percent_interval(self):
        # A wider level than 95%: the 98.75% interval contains the 95% one.
        d = [x / 10 for x in range(-10, 11)] * 5
        lo, hi = analyze.bootstrap_ci(d, n=4000)
        lo95, hi95 = analyze.bootstrap_ci(d, n=4000, alpha=0.05)
        self.assertLessEqual(lo, lo95)
        self.assertGreaterEqual(hi, hi95)


class TestJudgeSchedule(unittest.TestCase):
    def test_judge_order_is_a_seeded_shuffle_per_question(self):
        by_arm = {a: {(q, 0): {"qid": q, "rep": 0, "attempt": 1, "arm": a} for q in QIDS} for a in ALL}
        s1, s2 = judge.schedule(by_arm), judge.schedule(by_arm)
        self.assertEqual([(a, p) for a, p, _ in s1], [(a, p) for a, p, _ in s2])
        orders = {}
        for a, p, r in s1:
            orders.setdefault(r["qid"], []).append(a)
        self.assertGreater(len({tuple(v) for v in orders.values()}), 1)


if __name__ == "__main__":
    unittest.main()
