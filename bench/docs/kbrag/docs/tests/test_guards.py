"""Offline tests of the Amendment 3 fixes: keys, the kb-on-Haiku guard, the rag-lc stop, the
preflight, and the driver's order and restore-on-exit. No network; synthetic ids.

    python3 -I -m unittest discover -s bench/docs/kbrag/docs/tests
"""
import os, re, sys, unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))
os.environ.setdefault("LOOMCYCLE_DEV_TOKEN", "test-not-a-token")

import darms, preflight, study  # noqa: E402

KEY = re.compile(r"^[A-Za-z0-9:._-]{1,200}$")


class TestKeys(unittest.TestCase):
    def test_key_fits_server_charset_and_twins_differ(self):
        a = darms.key({"prefix": "kbrag-docs-study2"}, "kb", {"id": "w12-T1'"}, 0, 1)
        b = darms.key({"prefix": "kbrag-docs-study2"}, "kb", {"id": "w12-T1"}, 0, 1)
        self.assertRegex(a, KEY)
        self.assertNotEqual(a, b)

    def test_key_prefix_is_the_given_namespace(self):
        self.assertTrue(darms.key({"prefix": "kbrag-docs-study2"}, "kb", {"id": "p7-q01"}, 2, 2)
                        .startswith("kbrag-docs-study2:kb:p7-q01-"))


class TestKbGuard(unittest.TestCase):
    def test_walker_on_static_tier_is_a_violation(self):
        tree = [{"agent": "kb/ask", "provider": "code-js", "model": "loomcycle/code-js"},
                {"agent": "kb/screen", "provider": "anthropic", "model": "claude-haiku-5-5"},
                {"agent": "kb/walker", "provider": "ollama-local", "model": "qwen3.6:latest"}]
        self.assertEqual(darms.kb_tree_violations(tree), [("kb/walker", "ollama-local", "qwen3.6:latest")])

    def test_all_haiku_tree_passes(self):
        tree = [{"agent": "kb/ask", "provider": "code-js", "model": "loomcycle/code-js"},
                {"agent": "kb/walker", "provider": "anthropic", "model": "claude-haiku-5-5"}]
        self.assertEqual(darms.kb_tree_violations(tree), [])


class FakeRunner(darms.Runner):
    def __init__(self, rows):
        self._rows = list(rows)
        self.lock = __import__("threading").RLock()
        self.raglc_seen = self.raglc_streak = 0
        self.rows = {"rag-lc": []}
        self.qs = {"q": {"set": "p7"}}
        self.events = []

    def event(self, kind, **kw):
        self.events.append(kind)


class TestRaglcStop(unittest.TestCase):
    def setUp(self):
        import tempfile
        self.tmp = tempfile.mkdtemp()

    def tearDown(self):
        import shutil
        shutil.rmtree(self.tmp, ignore_errors=True)

    def _feed(self, rows):
        r = FakeRunner(rows)
        stopped = None
        for i, row in enumerate(rows):
            def arm(lc, corpus, q, rep, attempt, usage, row_, ctx, _row=row):
                row_.update(_row)
            darms.ARM_FN["rag-lc"], saved = arm, darms.ARM_FN["rag-lc"]
            try:
                r.path = lambda a: os.path.join(self.tmp, a + ".jsonl")
                r.prefix, r.corpus, r.lc = "x", None, None
                r.one("rag-lc", "q", i, 1)
            except darms.Stop as e:
                stopped = (i, e.code)
                break
            finally:
                darms.ARM_FN["rag-lc"] = saved
        return stopped

    def test_an_early_unreranked_row_stops_the_runner(self):
        ok = {"status": "ok", "retrieval": {"reranked": True}}
        self.assertEqual(self._feed([ok, {"status": "ok", "retrieval": {"reranked": False}}]), (1, 3))

    def test_later_three_bad_in_a_row_stop_but_one_does_not(self):
        ok = {"status": "ok", "retrieval": {"reranked": True}}
        bad = {"status": "failed", "error": "search run failed"}
        self.assertIsNone(self._feed([ok] * 6 + [bad, ok]))
        self.assertEqual(self._feed([ok] * 6 + [bad] * 3), (8, 3))


class TestPreflight(unittest.TestCase):
    def setUp(self):
        self.defs = {"d1": {"model": "haiku"}, "d2": {"provider": "anthropic", "model": "claude-haiku-5-5"},
                     "h": {"provider": "code-js"}, "q": {"tier": "middle"}}
        self.aliases = {"haiku": {"provider": "anthropic", "model": "claude-haiku-5-5"}}

    def lib(self, walker="d2", helper="h"):
        return {"kb/classify": {"active_def_id": "d1"}, "kb/search": {"active_def_id": "d1"},
                "kb/screen": {"active_def_id": "d2"}, "kb/walker": {"active_def_id": walker},
                "kbrag/search-rr": {"active_def_id": helper}}

    def test_ready_tenant_has_no_problems(self):
        self.assertEqual(preflight.problems(self.lib(), self.defs.get, self.aliases), [])

    def test_retired_walker_fork_or_helper_is_refused(self):
        p = preflight.problems(self.lib(walker=None, helper=None), self.defs.get, self.aliases)
        self.assertEqual(len(p), 2)

    def test_tier_routed_walker_is_refused(self):
        self.assertEqual(len(preflight.problems(self.lib(walker="q"), self.defs.get, self.aliases)), 1)


class TestDriver(unittest.TestCase):
    def make(self, rcs, boom_at=None):
        calls = []

        def run(cmd):
            name = os.path.basename(cmd[2]) + " " + " ".join(cmd[3:5])
            calls.append(name)
            if boom_at and boom_at in name:
                raise KeyboardInterrupt
            for k, v in rcs.items():
                if k in name:
                    return v
            return 0
        d = study.Driver(os.path.join(HERE, "_tmp_out"), "p", "2026-11-24T06:00:00Z",
                         run=run, log=lambda s: None)
        return d, calls

    def tearDown(self):
        import shutil
        shutil.rmtree(os.path.join(HERE, "_tmp_out"), ignore_errors=True)

    def test_steps_run_in_order_and_restore_precedes_judge(self):
        d, calls = self.make({})
        self.assertEqual(d.run(), 0)
        names = [c.split()[0] for c in calls]
        self.assertLess(names.index("register.py", 2), names.index("djudge.py"))
        self.assertLess(max(i for i, n in enumerate(names) if n == "darms.py"), calls.index("forks.py --restore"))

    def test_a_crash_mid_run_still_restores(self):
        d, calls = self.make({}, boom_at="darms.py")
        with self.assertRaises(KeyboardInterrupt):
            d.run()
        self.assertIn("forks.py --restore", calls)
        self.assertIn("forks.py --verify-restored", calls)
        self.assertNotIn("djudge.py --out", [c for c in calls if c.startswith("djudge")])

    def test_a_violation_skips_later_scored_steps(self):
        d, calls = self.make({"darms.py": 3})
        d.run()
        self.assertEqual(sum(1 for c in calls if c.startswith("darms.py")), 1)
        self.assertIn("forks.py --restore", calls)


if __name__ == "__main__":
    unittest.main()
