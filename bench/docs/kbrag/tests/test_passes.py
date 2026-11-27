import os, unittest

import fixture
import common, condense, passes


class TestPasses(unittest.TestCase):
    def setUp(self):
        self.work, self.data = fixture.make_work()
        self.out = os.path.join(self.work, "condensed", "kb.jsonl")

    def src(self, qid="q2", raw=None):
        return {"arm": "kb", "qid": qid, "rep": 0, "attempt": 1, "profile": "haiku", "status": "ok",
                "raw_answer": raw or "Within 10 days.\nCOVERAGE: partial\n\nROUTE: search | SCORE: 1 | CLASSIFIER: c | RUNS: r"}

    def test_condense_sees_kb_answer_without_trailer_and_is_resumable(self):
        lc = fixture.FakeLC()
        seen = []
        real = lc.chat

        def chat(messages, **kw):
            seen.append(messages[0]["content"])
            return real(messages, **kw)
        lc.chat = chat
        call = condense.condense_call(lc, self.data, ("anthropic", "claude-haiku-5-5"))
        items = [(("q2", 0, 1), self.src())]
        self.assertEqual(passes.run_pass(lc, items, call, self.out, budget_usd=19.0, log=lambda s: None), 1)
        self.assertNotIn("ROUTE", seen[0])
        self.assertNotIn("COVERAGE", seen[0])
        self.assertIn("Within 10 days.", seen[0])
        row = common.read_jsonl(self.out)[0]
        self.assertEqual(row["condensed"], "within 10 days")
        self.assertEqual(passes.run_pass(lc, items, call, self.out, budget_usd=19.0, log=lambda s: None), 0)
        self.assertEqual(len(seen), 1)

    def test_pass_stops_on_a_provider_outside_policy(self):
        lc = fixture.FakeLC(served=("openai", "gpt-5"))
        call = condense.condense_call(lc, self.data, ("anthropic", "claude-haiku-5-5"))
        with self.assertRaises(passes.Stop):
            passes.run_pass(lc, [(("q2", 0, 1), self.src())], call, self.out, budget_usd=19.0, log=lambda s: None)
        self.assertEqual(common.read_jsonl(self.out)[0]["status"], "provider_violation")

    def test_pass_stops_before_a_call_at_the_spend_cap(self):
        lc = fixture.FakeLC(spent=18.995)
        call = condense.condense_call(lc, self.data, ("anthropic", "claude-haiku-5-5"))
        with self.assertRaises(passes.Stop):
            passes.run_pass(lc, [(("q2", 0, 1), self.src())], call, self.out, budget_usd=19.0, log=lambda s: None)
        self.assertEqual(lc.calls["chat"], 0)


if __name__ == "__main__":
    unittest.main()
