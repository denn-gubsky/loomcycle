"""Response shapes confirmed on the live smoke (2026-11-23), pinned so the harness keeps them."""
import json, unittest

import fixture  # noqa: F401  (puts the harness on sys.path)
import common, config, prompts


class TestLiveShapes(unittest.TestCase):
    def test_code_js_run_named_loomcycle_code_js_is_not_a_model_call(self):
        u = common.Usage()
        u.add_tree({"runs": [
            {"provider": "code-js", "model": "loomcycle/code-js", "input_tokens": 0, "output_tokens": 0, "cached_tokens": 0},
            {"provider": "anthropic", "model": "claude-haiku-5-5", "input_tokens": 1000, "output_tokens": 100,
             "cached_tokens": 0}]})
        j = u.to_json()
        self.assertEqual(j["total"]["calls"], 1)
        self.assertEqual(common.main_model(j), ("anthropic", "claude-haiku-5-5"))

    def test_main_model_never_picks_code_js(self):
        u = common.Usage()
        u.add_tree({"runs": [{"provider": "code-js", "model": "loomcycle/code-js", "input_tokens": 0,
                              "output_tokens": 0, "cached_tokens": 0}]})
        self.assertIsNone(common.main_model(u.to_json()))

    def test_haiku_price_matches_the_ledger(self):
        # The ledger's own row (smoke): 2,139,140 in, 851,442 out, 7,824,626 cache read,
        # 41,376 cache write -> $0.72305.
        usd = config.price("anthropic", "claude-haiku-5-5", {"input_tokens": 2139140, "output_tokens": 851442,
                                                              "cached_tokens": 7824626, "cache_creation_tokens": 41376})
        self.assertAlmostEqual(usd, 0.723053, places=5)

    def test_search_rr_result_shape(self):
        # kbrag/search-rr's final text: {"chunks": [...], "reranked": true}; rows carry
        # chunk_id, document_id, path, rank_score, score, title -- no matched_annotation/summary.
        res = json.loads('{"chunks":[{"chunk_id":"c1","document_id":"d1","path":"/cqa/x","rank_score":0.016,'
                         '"score":0.6,"title":"t"}],"reranked":true}')
        hits = res.get("chunks")
        self.assertEqual(hits[0]["chunk_id"], "c1")
        self.assertTrue(res["reranked"])
        self.assertNotIn("rerank_reason", res)

    def test_kb_answer_citations_with_scope_query(self):
        text = ("No ([Who can apply](loom:chunk/c0ffee00c0ffee00c0ffee00c0ffee00?scope=tenant)) "
                "([guide](loom:doc/d0c0d0c0d0c0d0c0d0c0d0c0d0c0d0c0?scope=tenant)).\n\nROUTE: search | SCORE: 1")
        c = common.citations(text)
        self.assertEqual(c, {"docs": ["d0c0d0c0d0c0d0c0d0c0d0c0d0c0d0c0"], "chunks": ["c0ffee00c0ffee00c0ffee00c0ffee00"]})
        self.assertEqual(common.strip_for_judge(text), "No (Who can apply) (guide).")


if __name__ == "__main__":
    unittest.main()
