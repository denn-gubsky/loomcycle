import datetime, os, tempfile, unittest

import fixture
import arms, common, ragext

ALL = ["kb", "rag-lc", "rag-ext", "oracle"]


def ragext_factory():
    docs = {d["id"]: d for d in fixture.DOCS}
    chunks = ragext.build_chunks(docs)
    d = tempfile.mkdtemp()
    ragext.build_dense(fixture.FakeLC(), chunks, d, pace=0, log=lambda s: None)
    return ragext.RagExt(docs, ragext.Dense.load(d, chunks), chunks)


class TestRunner(unittest.TestCase):
    def setUp(self):
        self.work, self.data = fixture.make_work()
        self.out = os.path.join(self.work, "runs")
        self.logs = []

    def runner(self, lc, **kw):
        kw.setdefault("arms", ALL)
        kw.setdefault("profile", "haiku")
        return arms.Runner(lc, self.data, out_dir=self.out, ragext_factory=ragext_factory,
                           log=self.logs.append, **kw)

    def rows(self, arm):
        return common.read_jsonl(os.path.join(self.out, arm + ".jsonl"))

    def test_every_arm_writes_an_ok_row_with_usage_citations_and_retrieval(self):
        n = self.runner(fixture.FakeLC()).run(["q1", "q2"], reps=1)
        self.assertEqual(n, 8)
        kb = self.rows("kb")[0]
        self.assertEqual(kb["status"], "ok")
        self.assertTrue(kb["route"].startswith("ROUTE:"))
        self.assertEqual(kb["citations"]["docs"], ["D001"])
        self.assertEqual(kb["served"], [["anthropic", "claude-haiku-5-5"]])
        rl = self.rows("rag-lc")[0]
        self.assertEqual(rl["retrieval"]["sections"][0], ["cqa-001", 1])
        self.assertNotIn(["cqa-000", 0], rl["retrieval"]["sections"])  # bodyless skipped
        self.assertEqual(rl["answer_line"], "aged 21 or over")
        self.assertTrue(rl["retrieval"]["reranked"])
        rx = self.rows("rag-ext")[0]
        self.assertEqual(rx["usage"]["total"]["calls"], 3)  # rewrite, rerank, reader
        self.assertEqual(rx["retrieval"]["rerank_strategy"], "listwise")
        orc = self.rows("oracle")[0]
        self.assertEqual(orc["read_sections"], [["cqa-001", 1]])
        self.assertEqual(orc["profile"], "haiku")
        self.assertEqual([v["when"] for v in common.read_jsonl(os.path.join(self.out, "versions.jsonl"))],
                         ["start", "end"])

    def test_rerun_skips_ok_rows_and_retries_failed_only_on_request(self):
        lc = fixture.FakeLC(fail_kb=["q1"])
        self.runner(lc).run(["q1", "q2"], reps=1)
        self.assertEqual([r["status"] for r in self.rows("kb")], ["failed", "ok"] if self.rows("kb")[0]["qid"] == "q1"
                         else ["ok", "failed"])
        before = dict(lc.calls)
        self.assertEqual(self.runner(lc).run(["q1", "q2"], reps=1), 0)
        self.assertEqual(lc.calls, before)
        lc.fail_kb = set()
        self.assertEqual(self.runner(lc, retry_failed=True).run(["q1", "q2"], reps=1), 1)
        last = common.latest(self.rows("kb"))[("q1", 0)]
        self.assertEqual((last["status"], last["attempt"]), ("ok", 2))
        # A third attempt is never made: PREREG retries a failed run once.
        lc.fail_kb = {"q2"}
        self.assertEqual(self.runner(lc, retry_failed=True).run(["q1", "q2"], reps=1), 0)

    def test_retry_uses_a_fresh_idempotency_key(self):
        seen = []
        lc = fixture.FakeLC(fail_kb=["q1"])
        real = lc.run

        def run(agent, prompt, **kw):
            seen.append(kw.get("idempotency_key"))
            return real(agent, prompt, **kw)
        lc.run = run
        self.runner(lc, arms=["kb"]).run(["q1"], reps=1)
        self.runner(lc, arms=["kb"], retry_failed=True).run(["q1"], reps=1)
        self.assertEqual(seen, ["kbrag-cqa:kb:q1:0", "kbrag-cqa:kb:q1:0:a2"])

    def test_kb_without_route_trailer_is_failed(self):
        self.runner(fixture.FakeLC(kb_text="An answer with no trailer."), arms=["kb"]).run(["q1"], reps=1)
        r = self.rows("kb")[0]
        self.assertEqual(r["status"], "failed")
        self.assertIn("ROUTE", r["error"])

    def test_provider_outside_policy_marks_the_row_and_stops_everything(self):
        lc = fixture.FakeLC(served=("deepseek", "deepseek-chat"))
        with self.assertRaises(arms.Stop) as cm:
            self.runner(lc).run(["q1", "q2"], reps=1)
        self.assertIn("PROVIDER VIOLATION", str(cm.exception))
        rows = [r for a in ALL for r in self.rows(a)]
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["status"], "provider_violation")
        ev = [e["kind"] for e in common.read_jsonl(os.path.join(self.out, "events.jsonl"))]
        self.assertIn("provider_violation", ev)

    def test_usage_ledger_violation_stops_the_runner(self):
        lc = fixture.FakeLC()
        lc.ledger_pairs = lambda since: [("anthropic", "claude-haiku-5-5"), ("gemini", "gemini-3-pro")]
        lc.disallowed = lambda pairs: [p for p in pairs if p[0] == "gemini"]
        with self.assertRaises(arms.Stop) as cm:
            self.runner(lc, arms=["oracle"]).run(["q1"], reps=1)
        self.assertIn("usage ledger", str(cm.exception))
        ev = [e["kind"] for e in common.read_jsonl(os.path.join(self.out, "events.jsonl"))]
        self.assertEqual(ev, ["start", "provider_violation", "stop"])

    def test_spend_cap_stops_before_a_run(self):
        lc = fixture.FakeLC(spent=18.96)
        with self.assertRaises(arms.Stop) as cm:
            self.runner(lc, arms=["kb"], budget_usd=19.0).run(["q1"], reps=1)
        self.assertIn("spend cap", str(cm.exception))
        self.assertEqual(lc.calls["run"], 0)
        lc.spent = 18.0
        self.runner(lc, arms=["kb"], budget_usd=19.0).run(["q1"], reps=1)
        self.assertEqual(lc.calls["run"], 1)

    def test_deadline_stops_when_p90_duration_would_pass_it(self):
        lc = fixture.FakeLC()
        r = self.runner(lc, arms=["oracle"])
        r.run(["q1"], reps=1)
        r.rows["oracle"][0]["wall_s"] = 600  # as if the last run took ten minutes
        r.deadline = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=60)
        with self.assertRaises(arms.Stop) as cm:
            r.run(["q2"], reps=1)
        self.assertIn("deadline", str(cm.exception))
        self.assertEqual(len(self.rows("oracle")), 1)

    def test_profiles_never_mix_in_one_run_directory(self):
        self.runner(fixture.FakeLC(), arms=["oracle"]).run(["q1"], reps=1)
        with self.assertRaises(arms.Stop):
            self.runner(fixture.FakeLC(served=("ollama-local", "qwen3.6:latest")), arms=["oracle"],
                        profile="local").run(["q2"], reps=1)

    def test_default_plan_is_rep0_then_spread_reps(self):
        r = self.runner(fixture.FakeLC())
        plan = r.plan(["q1", "q2", "q3", "q4"])
        self.assertEqual(plan[:4], [("q1", 0), ("q2", 0), ("q3", 0), ("q4", 0)])
        self.assertEqual(len(plan), 12)  # the toy subset (4) is smaller than the 100-question sample
        self.assertEqual(sorted(set(rep for _, rep in plan)), [0, 1, 2])

    def test_arm_order_is_rotated_per_question_by_a_seed(self):
        r = self.runner(fixture.FakeLC())
        seen = []
        real = r.one

        def one(arm, qid, rep, attempt):
            seen.append((qid, arm))
            return real(arm, qid, rep, attempt)
        r.one = one
        qids = ["q1", "q2", "q3", "q4"]
        r.run(qids, reps=1)
        firsts = [next(a for q, a in seen if q == qid) for qid in qids]
        self.assertGreater(len(set(firsts)), 1)
        # Seeded: the first arm is the registered rotation of the arm list.
        import random
        self.assertEqual(firsts, [ALL[random.Random("order:%s:0" % q).randrange(4)] for q in qids])


class TestSpreadSample(unittest.TestCase):
    def test_spread_sample_is_the_registered_draw(self):
        import random, config
        subset = common.jload(config.SUBSET)
        self.assertEqual(common.spread_sample(subset), set(random.Random(20261122).sample(sorted(subset), 100)))
        self.assertEqual(len(common.spread_sample(subset)), 100)


if __name__ == "__main__":
    unittest.main()
