import unittest

import fixture
import common, config, prompts


class TestCitations(unittest.TestCase):
    def test_citations_parse_doc_and_chunk_links_of_any_id_shape(self):
        t = ("See [a](loom:doc/0123456789abcdef0123456789abcdef?scope=tenant) and loom:chunk/ch_9-x "
             "and loom:doc/Dabc. Again loom:doc/Dabc")
        c = common.citations(t)
        self.assertEqual(c["docs"], ["0123456789abcdef0123456789abcdef", "Dabc"])
        self.assertEqual(c["chunks"], ["ch_9-x"])

    def test_cited_pages_counts_a_chunk_for_its_page_unless_docs_only(self):
        _, data = fixture.make_work()
        c = {"docs": ["D000"], "chunks": ["C001_2", "unknown"]}
        self.assertEqual(common.cited_pages(data, c), {"cqa-000", "cqa-001"})
        self.assertEqual(common.cited_pages(data, c, docs_only=True), {"cqa-000"})

    def test_strip_trailer_drops_route_and_coverage_lines(self):
        t = "Yes, within 10 days.\nCOVERAGE: partial\nUNCOVERED: two docs\n\nROUTE: search | SCORE: 0.4 | CLASSIFIER: x | RUNS: a"
        self.assertEqual(common.strip_trailer(t), "Yes, within 10 days.")
        self.assertTrue(common.has_route(t))
        self.assertTrue(common.route_trailer(t).startswith("ROUTE: search"))
        self.assertFalse(common.has_route("no trailer"))

    def test_strip_for_judge_removes_arm_identifying_format(self):
        kb = "Within 10 days [Adoption](loom:doc/D001?scope=tenant).\n\nROUTE: search | SCORE: 1 | CLASSIFIER: c | RUNS: r"
        rd = "ANSWER: within 10 days\nSOURCES: loom:doc/D001"
        self.assertEqual(common.strip_for_judge(kb), "Within 10 days Adoption.")
        self.assertEqual(common.strip_for_judge(rd), "within 10 days")

    def test_answer_line_tolerates_bold_label(self):
        self.assertEqual(common.answer_line("**ANSWER:** within 10 days\nSOURCES: x"), "within 10 days")
        self.assertEqual(common.answer_line("thinking\nANSWER: Yes"), "Yes")
        self.assertIsNone(common.answer_line("nothing here"))


class TestScoring(unittest.TestCase):
    def test_em_f1_squad_style_max_over_gold(self):
        self.assertEqual(common.score("The 10 days", ["within 10 days", "10 days"]), (1.0, 1.0))
        em, f = common.score("within ten days", ["within 10 days"])
        self.assertEqual(em, 0.0)
        self.assertAlmostEqual(f, 2 / 3)
        self.assertEqual(common.score("NO ANSWER", ["x"]), (0.0, 0.0))
        self.assertEqual(common.score(None, ["x"]), (0.0, 0.0))

    def test_readable_sections_skip_bodyless_and_duplicates_in_rank_order(self):
        _, data = fixture.make_work()
        ranked = [("cqa-000", 0), ("cqa-000", 3), ("cqa-001", 2), ("cqa-001", 2), ("cqa-000", 1), ("nope", 1)]
        self.assertEqual(common.readable_sections(data, ranked, 10), [("cqa-001", 2), ("cqa-000", 1)])
        self.assertEqual(common.readable_sections(data, ranked, 1), [("cqa-001", 2)])

    def test_recall_and_mrr_count_the_first_gold_hit(self):
        gold = {("d", 3)}
        ranked = [("d", 1), ("d", 2), ("d", 3)]
        self.assertEqual([common.recall_at(ranked, gold, k) for k in (1, 3, 5)], [0.0, 1.0, 1.0])
        self.assertAlmostEqual(common.mrr10(ranked, gold), 1 / 3)
        self.assertEqual(common.mrr10([("x", 0)] * 10 + [("d", 3)], gold), 0.0)

    def test_chance_uses_corpus_readable_sections(self):
        _, data = fixture.make_work()
        self.assertEqual(data.n_readable, 7)
        self.assertAlmostEqual(data.chance5("q1"), 5 / 7)


class TestUsage(unittest.TestCase):
    def test_provider_guard_allows_policy_and_ignores_code_js(self):
        u = common.Usage()
        u.add_tree({"runs": [{"provider": "code-js", "model": "", "input_tokens": 0, "output_tokens": 0},
                             {"provider": "ollama-local", "model": "qwen3.6:latest", "input_tokens": 5, "output_tokens": 1},
                             {"provider": "anthropic", "model": "claude-haiku-5-5", "input_tokens": 1000000,
                              "output_tokens": 0}]})
        self.assertEqual(u.violations(), [])
        j = u.to_json()
        self.assertEqual(j["total"]["calls"], 2)
        self.assertTrue(j["calls_estimated"])
        self.assertAlmostEqual(j["total"]["usd_est"], 0.10)  # Haiku 5.5: $0.10 per million input

    def test_provider_guard_flags_anything_outside_allowed(self):
        for pair in [("anthropic", "claude-sonnet-5"), ("deepseek", "deepseek-chat"), ("ollama-spark", "qwen3.6:latest"),
                     ("openai", "gpt-5")]:
            u = common.Usage()
            u.add_chat({"provider": pair[0], "model": pair[1], "usage": {}}, config.reader("haiku"))
            self.assertEqual(u.violations(), [pair])

    def test_chat_without_serving_info_falls_back_to_requested_and_says_so(self):
        u = common.Usage()
        u.add_chat({"usage": {"input_tokens": 3}}, ("ollama-local", "qwen3.6:latest"))
        self.assertEqual(u.served(), [("ollama-local", "qwen3.6:latest")])
        self.assertEqual(u.to_json()["chat_serving_unreported"], 1)


class TestPrompts(unittest.TestCase):
    def test_first_json_list_finds_the_array_inside_prose(self):
        self.assertEqual(prompts.first_json_list('Sure: ```json\n[3, 1, 2]\n``` done'), [3, 1, 2])
        self.assertEqual(prompts.first_json_list('[bad] then ["a", "b"]'), ["a", "b"])
        self.assertIsNone(prompts.first_json_list("no list"))

    def test_verdict_reads_the_first_line_first(self):
        self.assertEqual(prompts.verdict("PARTLY\nNot WRONG entirely."), "PARTLY")
        self.assertEqual(prompts.verdict("Verdict: incorrect\nso WRONG"), "WRONG")
        self.assertIsNone(prompts.verdict(""))

    def test_ask_retries_once_on_empty_content(self):
        class LC:
            n = 0

            def chat(self, msgs, **kw):
                LC.n += 1
                if LC.n == 1:
                    return {"content": None, "stop_reason": "max_tokens", "usage": {}, "error": None,
                            "provider": "ollama-local", "model": "m"}
                self.max_tokens = kw["max_tokens"]
                return {"content": "ok", "usage": {}, "error": None, "provider": "ollama-local", "model": "m"}
        lc, u = LC(), common.Usage()
        out, err, _ = prompts.ask(lc, "p", ("ollama-local", "m"), u, max_tokens=100)
        self.assertEqual(out, "ok")
        self.assertEqual(lc.max_tokens, 200)
        self.assertEqual(u.to_json()["total"]["calls"], 2)


if __name__ == "__main__":
    unittest.main()
