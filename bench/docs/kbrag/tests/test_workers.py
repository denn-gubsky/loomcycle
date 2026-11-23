"""--workers, --no-spread / --spread-only, split arms and the output root."""
import json, os, threading, time, unittest

import fixture
import arms, common, condense, judge, passes
from test_arms import ALL, ragext_factory

QIDS = [q["qid"] for q in fixture.QS]


class SlowLC(fixture.FakeLC):
    """FakeLC whose calls take a little time and record how many overlap."""

    def __init__(self, delay=0.02, **kw):
        super().__init__(**kw)
        self.delay, self.active, self.peak = delay, 0, 0
        self._l = threading.Lock()

    def _enter(self):
        with self._l:
            self.active += 1
            self.peak = max(self.peak, self.active)
        time.sleep(self.delay)
        with self._l:
            self.active -= 1

    def chat(self, messages, **kw):
        self._enter()
        return super().chat(messages, **kw)

    def run(self, agent, prompt, **kw):
        self._enter()
        return super().run(agent, prompt, **kw)


def lines_parse(path):
    with open(path) as f:
        for line in f:
            json.loads(line)  # raises on a torn or interleaved line


class TestRunJobs(unittest.TestCase):
    def test_jobs_start_in_list_order_and_all_run(self):
        started, lock = [], threading.Lock()

        def fn(j):
            with lock:
                started.append(j)
            time.sleep(0.005)
        self.assertEqual(common.run_jobs(list(range(40)), fn, workers=4), 40)
        self.assertEqual(started, list(range(40)))

    def test_an_exception_stops_new_starts_lets_in_flight_finish_and_is_raised(self):
        finished, lock = [], threading.Lock()

        def fn(j):
            if j == 3:
                raise arms.Stop("guard")
            time.sleep(0.02)
            with lock:
                finished.append(j)
        with self.assertRaises(arms.Stop):
            common.run_jobs(list(range(100)), fn, workers=4)
        # Jobs 0-2 and whatever was already in flight finished; nothing near the end started.
        self.assertTrue({0, 1, 2} <= set(finished))
        self.assertLess(max(finished), 10)

    def test_one_worker_is_a_sequential_loop_stopping_at_the_first_error(self):
        seen = []

        def fn(j):
            seen.append(j)
            if j == 2:
                raise ValueError("x")
        with self.assertRaises(ValueError):
            common.run_jobs([0, 1, 2, 3, 4], fn, workers=1)
        self.assertEqual(seen, [0, 1, 2])


class TestRunnerWorkers(unittest.TestCase):
    def setUp(self):
        self.work, self.data = fixture.make_work()
        self.out = os.path.join(self.work, "runs")

    def runner(self, lc, **kw):
        kw.setdefault("arms", ALL)
        kw.setdefault("profile", "haiku")
        return arms.Runner(lc, self.data, out_dir=self.out, ragext_factory=ragext_factory, log=lambda s: None, **kw)

    def test_workers_write_the_same_rows_as_one_worker_with_whole_lines(self):
        lc = SlowLC()
        n = self.runner(lc, workers=4).run(QIDS, reps=2)
        self.assertEqual(n, 4 * 4 * 2)
        self.assertGreater(lc.peak, 1)  # runs really overlapped
        for a in ALL:
            lines_parse(os.path.join(self.out, a + ".jsonl"))
            rows = common.read_jsonl(os.path.join(self.out, a + ".jsonl"))
            self.assertEqual(sorted((r["qid"], r["rep"]) for r in rows), sorted((q, r) for q in QIDS for r in (0, 1)))
            self.assertTrue(all(r["status"] == "ok" for r in rows))
        # Resumable as before: nothing left to do.
        self.assertEqual(self.runner(lc, workers=4).run(QIDS, reps=2), 0)

    def test_submission_order_is_the_seeded_order(self):
        r = self.runner(fixture.FakeLC(), workers=4)
        started, lock = [], threading.Lock()
        real = r.one

        def one(arm, qid, rep, attempt):
            with lock:
                started.append((arm, qid, rep, attempt))
            return real(arm, qid, rep, attempt)
        expected = r.jobs(QIDS, reps=1)
        r.one = one
        r.run(QIDS, reps=1)
        # Workers take jobs off the list in order; the moment each enters one() can only
        # swap neighbours that started together, never move a run past a later batch.
        self.assertEqual(sorted(started), sorted(expected))
        for i, j in enumerate(started):
            self.assertLess(abs(expected.index(j) - i), 4)

    def test_provider_violation_under_workers_stops_all_with_whole_lines(self):
        lc = SlowLC(served=("deepseek", "deepseek-chat"))
        with self.assertRaises(arms.Stop):
            self.runner(lc, workers=4).run(QIDS, reps=2)
        rows = []
        for a in ALL:
            p = os.path.join(self.out, a + ".jsonl")
            if os.path.exists(p):
                lines_parse(p)
                rows += common.read_jsonl(p)
        self.assertGreaterEqual(len(rows), 1)
        self.assertLessEqual(len(rows), 4)  # at most the runs already in flight
        ev = [e["kind"] for e in common.read_jsonl(os.path.join(self.out, "events.jsonl"))]
        self.assertEqual(ev[-1], "stop")

    def test_spend_guard_counts_runs_in_flight(self):
        r = self.runner(fixture.FakeLC(spent=18.975), arms=["oracle"], budget_usd=19.0, workers=4)
        r.guard_before("oracle")          # 18.975 + 0.01 <= 19
        r.guard_before("oracle")          # 18.975 + 0.01 + 0.01 <= 19
        with self.assertRaises(arms.Stop) as cm:
            r.guard_before("oracle")      # 18.975 + 2 x 0.01 in flight + 0.01 > 19
        self.assertIn("in flight", str(cm.exception))
        r.release("oracle")
        r.guard_before("oracle")          # one finished: room again

    def test_no_spread_and_spread_only_partition_the_plan(self):
        r = self.runner(fixture.FakeLC())
        full = r.plan(QIDS)
        prim, spr = r.plan(QIDS, spread="none"), r.plan(QIDS, spread="only")
        self.assertEqual(prim, [(q, 0) for q in QIDS])
        self.assertTrue(all(rep in (1, 2) for _, rep in spr))
        self.assertEqual(prim + spr, full)
        with self.assertRaises(SystemExit):
            r.plan(QIDS, reps=1, spread="none")

    def test_primary_then_spread_as_two_invocations_equals_one(self):
        lc = fixture.FakeLC()
        self.assertEqual(self.runner(lc, arms=["oracle"]).run(QIDS, spread="none"), 4)
        self.assertEqual(self.runner(lc, arms=["oracle"]).run(QIDS, spread="only"), 8)
        self.assertEqual(self.runner(lc, arms=["oracle"]).run(QIDS), 0)

    def test_arms_can_run_in_separate_invocations_into_one_directory(self):
        lc = fixture.FakeLC()
        self.assertEqual(self.runner(lc, arms=["rag-lc", "rag-ext", "oracle"], workers=2).run(QIDS, reps=1), 12)
        self.assertEqual(self.runner(lc, arms=["kb"]).run(QIDS, reps=1), 4)
        self.assertEqual(self.runner(lc).run(QIDS, reps=1), 0)


class TestKeyPrefix(unittest.TestCase):
    def test_study_and_other_roots_never_share_idempotency_keys(self):
        import config
        study = arms.key_prefix(config.WORK)
        smoke = arms.key_prefix(os.path.join(config.WORK, "smoke"))
        pilot = arms.key_prefix(os.path.join(config.WORK, "pilot"))
        self.assertEqual(study, "kbrag-cqa-study")
        self.assertEqual(len({study, smoke, pilot, "kbrag-cqa"}), 4)

    def test_runner_uses_its_prefix_on_kb_and_raglc_keys(self):
        work, data = fixture.make_work()
        seen = []
        lc = fixture.FakeLC()
        real = lc.run

        def run(agent, prompt, **kw):
            seen.append(kw.get("idempotency_key"))
            return real(agent, prompt, **kw)
        lc.run = run
        arms.Runner(lc, data, profile="haiku", arms=["kb", "rag-lc"], out_dir=os.path.join(work, "runs"),
                    log=lambda s: None, key_prefix="kbrag-cqa-smoke").run(["q1"], reps=1)
        self.assertEqual(sorted(seen), ["kbrag-cqa-smoke:kb:q1:0", "kbrag-cqa-smoke:rag-lc:q1:0"])


class TestPassWorkers(unittest.TestCase):
    def setUp(self):
        self.work, self.data = fixture.make_work()
        arms.Runner(fixture.FakeLC(), self.data, profile="haiku", arms=ALL, out_dir=os.path.join(self.work, "runs"),
                    ragext_factory=ragext_factory, log=lambda s: None).run(QIDS, reps=2)

    def test_condense_with_workers_writes_every_row_once_and_resumes(self):
        lc = SlowLC()
        runs = passes.latest_ok_runs("oracle", os.path.join(self.work, "runs"))
        items = [((r["qid"], r["rep"], r["attempt"]), r) for _, r in sorted(runs.items())]
        out = os.path.join(self.work, "condensed", "oracle.jsonl")
        call = condense.condense_call(lc, self.data, ("anthropic", "claude-haiku-5-5"))
        self.assertEqual(passes.run_pass(lc, items, call, out, budget_usd=19.0, log=lambda s: None, workers=4), 8)
        self.assertGreater(lc.peak, 1)
        lines_parse(out)
        self.assertEqual(len(common.read_jsonl(out)), 8)
        self.assertEqual(passes.run_pass(lc, items, call, out, budget_usd=19.0, log=lambda s: None, workers=4), 0)

    def test_judge_items_carry_their_order_and_go_to_one_file_per_arm(self):
        lc = fixture.FakeLC()
        by_arm = {a: passes.latest_ok_runs(a, os.path.join(self.work, "runs")) for a in ALL}
        items, order = judge.items_and_order(by_arm)
        self.assertEqual(len(items), 4 * 4 * 2)
        call = judge.judge_call(lc, self.data)

        def with_pos(src, usage):
            return dict(call(src, usage), order=order[(src["arm"], src["qid"], src["rep"], src["attempt"])])
        path = lambda src: os.path.join(self.work, "judge", src["arm"] + ".jsonl")  # noqa: E731
        self.assertEqual(passes.run_pass(lc, items, with_pos, path, budget_usd=19.0, log=lambda s: None, workers=3), 32)
        for a in ALL:
            rows = common.read_jsonl(os.path.join(self.work, "judge", a + ".jsonl"))
            self.assertEqual(len(rows), 8)
            self.assertTrue(all(r["verdict"] == "CORRECT" and r["order"] in range(4) for r in rows))
        self.assertEqual(passes.run_pass(lc, items, with_pos, path, budget_usd=19.0, log=lambda s: None, workers=3), 0)

    def test_pass_spend_guard_counts_calls_in_flight(self):
        lc = SlowLC(delay=0.05, spent=18.975)  # room for two calls at the $0.01 floor, not three at once
        runs = passes.latest_ok_runs("oracle", os.path.join(self.work, "runs"))
        items = [((r["qid"], r["rep"], r["attempt"]), r) for _, r in sorted(runs.items())]
        out = os.path.join(self.work, "condensed", "oracle.jsonl")
        call = condense.condense_call(lc, self.data, ("anthropic", "claude-haiku-5-5"))
        with self.assertRaises(passes.Stop):
            passes.run_pass(lc, items, call, out, budget_usd=19.0, log=lambda s: None, workers=4)
        self.assertLessEqual(lc.peak, 2)
        lines_parse(out)


if __name__ == "__main__":
    unittest.main()
