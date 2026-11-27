"""cost(), the server usage keys mapped onto its names, F1/EM, and the bootstrap on known
cases.

    python3 -I -m unittest discover -s bench/docs/kbrag/hc/tests
"""
import random, unittest

import hcfixture  # noqa: F401  (sys.path)
import client, cost, hcconfig, hcanalyze
import analyze, common, config


class TestCost(unittest.TestCase):
    def test_cost_prices_haiku_at_the_measurement_model(self):
        u = {"input_tokens": 1_000_000, "output_tokens": 1_000_000, "cache_read_input_tokens": 1_000_000,
             "cache_creation_input_tokens": 1_000_000}
        self.assertAlmostEqual(cost.cost(u), 0.10 + 0.50 + 0.01 + 0.125)
        self.assertEqual(cost.cost(None), 0.0)

    def test_server_run_row_keys_map_onto_costs_names(self):
        row = {"input_tokens": 2000, "output_tokens": 300, "cache_read_tokens": 50000, "cache_creation_tokens": 4000,
               "provider": "anthropic", "model": "claude-haiku-5-5"}
        m = cost.from_usage(row)
        self.assertEqual(m, {"input_tokens": 2000, "output_tokens": 300, "cache_read_input_tokens": 50000,
                             "cache_creation_input_tokens": 4000})
        # Unmapped, cost() would read the cache tokens as 0 and undercount.
        self.assertLess(cost.cost(row), cost.cost(m))

    def test_tree_rows_cached_tokens_map_too(self):
        tr = client.tree_row({"run_id": "r1", "agent": "kb/read", "usage": {
            "input_tokens": 10, "output_tokens": 5, "cache_read_tokens": 7, "cache_creation_tokens": 3,
            "provider": "anthropic", "model": "claude-haiku-5-5"}})
        self.assertEqual((tr["cached_tokens"], tr["cache_creation_tokens"]), (7, 3))
        self.assertEqual(cost.from_usage(tr)["cache_read_input_tokens"], 7)

    def test_local_models_and_code_js_cost_nothing(self):
        for prov in ("ollama-local", "code-js", ""):
            self.assertEqual(cost.run_cost({"provider": prov, "input_tokens": 10 ** 6, "output_tokens": 10 ** 6}), 0.0)

    def test_tree_cost_sums_root_and_children(self):
        t = {"runs": [{"provider": "code-js", "input_tokens": 0},
                      {"provider": "anthropic", "model": "claude-haiku-5-5", "input_tokens": 10 ** 6, "output_tokens": 0},
                      {"provider": "ollama-local", "input_tokens": 10 ** 6}]}
        self.assertAlmostEqual(cost.tree_cost(t), 0.10)

    def test_cost_agrees_with_the_benchmarks_row_estimate(self):
        u = {"input_tokens": 12345, "output_tokens": 678, "cached_tokens": 90000, "cache_creation_tokens": 1111}
        self.assertAlmostEqual(cost.cost(cost.from_usage(u)), config.price("anthropic", "claude-haiku-5-5", u))


class TestScoring(unittest.TestCase):
    def test_em_and_f1_on_known_spans(self):
        self.assertEqual(common.score("aged 21 or over", ["aged 21 or over"]), (1.0, 1.0))
        em, f1 = common.score("21 or over", ["aged 21 or over"])
        self.assertEqual(em, 0.0)
        self.assertAlmostEqual(f1, 2 * 1.0 * 0.75 / 1.75)
        self.assertEqual(common.score("The yes.", ["Yes"]), (1.0, 1.0))
        self.assertEqual(common.score("NO ANSWER", ["within 10 days"]), (0.0, 0.0))


class TestBootstrap(unittest.TestCase):
    def test_constant_differences_give_a_point_interval(self):
        self.assertEqual(hcanalyze.ci([0.25] * 50), (0.25, 0.25))

    def test_9833_interval_is_wider_than_95_and_contains_the_mean(self):
        rng = random.Random(7)
        d = [rng.gauss(0.05, 0.3) for _ in range(400)]
        lo, hi = hcanalyze.ci(d)
        lo95, hi95 = analyze.bootstrap_ci(d, n=hcconfig.BOOT_N, seed=hcconfig.BOOT_SEED, alpha=0.05)
        self.assertLess(lo, lo95)
        self.assertGreater(hi, hi95)
        self.assertTrue(lo < sum(d) / len(d) < hi)
        self.assertEqual(hcanalyze.ci(d), (lo, hi))  # seeded: the same every time

    def test_paired_test_on_a_known_shift(self):
        a = {"q%d" % i: 0.8 for i in range(100)}
        b = {"q%d" % i: (0.75 if i % 2 else 0.85) for i in range(100)}
        r = hcanalyze.paired(a, b, sorted(a), -0.02)
        self.assertEqual(r["diff"], 0.0)
        self.assertTrue(r["holds"])                      # lower bound above -0.02
        self.assertEqual((r["wins"], r["losses"]), (50, 50))
        self.assertEqual(r["ci_level"], round(1 - 0.05 / 3, 4))
        worse = {q: v - 0.05 for q, v in a.items()}
        self.assertFalse(hcanalyze.paired(worse, b, sorted(a), -0.02)["holds"])


if __name__ == "__main__":
    unittest.main()
