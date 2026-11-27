import os, tempfile, time, unittest

import fixture
import common, ragext


class TestChunking(unittest.TestCase):
    def test_short_section_is_one_window(self):
        self.assertEqual(ragext.windows("a b c"), ["a b c"])

    def test_long_section_cuts_400_token_windows_overlapping_50(self):
        text = " ".join("w%d" % i for i in range(900))
        w = ragext.windows(text)
        toks = [ragext.TOKEN.findall(x) for x in w]
        self.assertEqual([len(t) for t in toks], [400, 400, 200])
        self.assertEqual(toks[0][-50:], toks[1][:50])
        self.assertEqual(toks[1][-50:], toks[2][:50])
        self.assertEqual(toks[2][-1], "w899")

    def test_window_keeps_original_punctuation_and_spacing(self):
        text = "Hello, world! " * 300
        w = ragext.windows(text)
        self.assertTrue(w[0].startswith("Hello, world!"))
        self.assertEqual(len(ragext.TOKEN.findall(w[0])), 400)

    def test_build_chunks_skips_empty_sections_and_prefixes_the_header(self):
        chunks = ragext.build_chunks({d["id"]: d for d in fixture.DOCS})
        ids = [c["id"] for c in chunks]
        self.assertNotIn("cqa-000#0#0", ids)
        self.assertNotIn("cqa-000#3#0", ids)
        self.assertEqual(len(chunks), 7)
        c = next(c for c in chunks if c["id"] == "cqa-000#2#0")
        self.assertTrue(c["text"].startswith("Child Benefit > Overview > Eligibility\nYou can claim"))
        root = next(c for c in chunks if c["id"] == "cqa-002#0#0")
        self.assertTrue(root["text"].startswith("Pension Credit\n"))


class TestBM25(unittest.TestCase):
    def test_toy_corpus_ranks_the_matching_document_first(self):
        b = ragext.BM25(["the cat sat on the mat", "dogs chase cats", "a court reply within ten days",
                         "the the the the the"])
        self.assertEqual(b.search("court reply", 10)[0], 2)
        self.assertEqual(b.search("cat mat", 10)[0], 0)
        self.assertEqual(b.search("zebra", 10), [])

    def test_idf_and_length_normalisation(self):
        b = ragext.BM25(["apple banana", "apple banana cherry date elder fig grape"])
        # Same tf, the shorter document scores higher (b = 0.75).
        self.assertEqual(b.search("banana", 10), [0, 1])
        self.assertGreater(b.idf["cherry"], b.idf["apple"])

    def test_lowercases_tokens(self):
        b = ragext.BM25(["Pension Credit"])
        self.assertEqual(b.search("PENSION", 5), [0])


class TestFusion(unittest.TestCase):
    def test_rrf_sums_reciprocal_ranks_with_k60(self):
        out = ragext.rrf([["a", "b", "c"], ["b", "c", "d"]])
        self.assertEqual(out[0], "b")
        self.assertEqual(set(out), {"a", "b", "c", "d"})
        self.assertEqual(ragext.rrf([["a", "b"], ["b", "a"]]), ["a", "b"])  # tie: first seen rank, then id
        self.assertEqual(ragext.rrf([["x", "y", "z"]], n=2), ["x", "y"])

    def test_parents_dedupe_windows_of_one_section_in_rank_order(self):
        chunks = [{"doc": "d", "sec": 1}, {"doc": "d", "sec": 1}, {"doc": "e", "sec": 0}, {"doc": "d", "sec": 2}]
        self.assertEqual(ragext.parents(chunks, [1, 2, 0, 3]), [("d", 1), ("e", 0), ("d", 2)])


class TestDense(unittest.TestCase):
    def build(self, lc, chunks, d, **kw):
        return ragext.build_dense(lc, chunks, d, pace=0, log=lambda s: None, sleep=lambda s: None, **kw)

    def test_build_is_resumable_after_a_failure_and_load_searches_by_cosine(self):
        chunks = ragext.build_chunks({d["id"]: d for d in fixture.DOCS}) * 10  # 70 chunks, 3 batches
        for i, c in enumerate(chunks):
            c = dict(c, id="%s~%d" % (c["id"], i))
            chunks[i] = c
        d = tempfile.mkdtemp()
        lc = fixture.FakeLC()
        calls = {"n": 0}
        real = lc.embed

        def flaky(texts):
            calls["n"] += 1
            if calls["n"] >= 2:
                raise RuntimeError("gpu busy")
            return real(texts)
        lc.embed = flaky
        self.assertFalse(self.build(lc, chunks, d, max_fail=2))
        m = common.jload(os.path.join(d, "manifest.json"))
        self.assertEqual(m["done"], 32)
        self.assertEqual(m["errors"], 2)
        lc.embed = real
        self.assertTrue(self.build(lc, chunks, d))
        m = common.jload(os.path.join(d, "manifest.json"))
        self.assertEqual((m["done"], m["calls"], m["dim"]), (70, 3, 26))
        self.assertEqual(os.path.getsize(os.path.join(d, "vectors.f32")), 70 * 26 * 4)
        dense = ragext.Dense.load(d, chunks)
        q = lc.embed([chunks[5]["text"]])[0]
        self.assertEqual(chunks[dense.search(q, 1)[0]]["text"], chunks[5]["text"])

    def test_load_refuses_a_stale_index(self):
        chunks = ragext.build_chunks({d["id"]: d for d in fixture.DOCS})
        d = tempfile.mkdtemp()
        self.build(fixture.FakeLC(), chunks, d)
        changed = [dict(c) for c in chunks]
        changed[0]["text"] += " edited"
        with self.assertRaises(SystemExit):
            ragext.Dense.load(d, changed)


class TestPipeline(unittest.TestCase):
    def setUp(self):
        self.docs = {d["id"]: d for d in fixture.DOCS}
        self.chunks = ragext.build_chunks(self.docs)
        self.dir = tempfile.mkdtemp()
        ragext.build_dense(fixture.FakeLC(), self.chunks, self.dir, pace=0, log=lambda s: None)
        self.r = ragext.RagExt(self.docs, ragext.Dense.load(self.dir, self.chunks), self.chunks)

    def test_retrieve_expands_reranks_and_returns_whole_sections(self):
        lc, u = fixture.FakeLC(), common.Usage()
        rec = self.r.retrieve(lc, fixture.QS[1], ("anthropic", "claude-haiku-5-5"), u, strategy="listwise")
        self.assertEqual(len(rec["queries"]), 4)
        self.assertFalse(rec["expansion_failed"])
        self.assertFalse(rec["rerank_failed"])
        self.assertFalse(rec["rerank_short"])  # the pool holds 7 chunks, so 7 is the whole ask
        self.assertEqual(len(rec["sections"]), 7)
        self.assertEqual(u.to_json()["total"]["calls"], 2)  # rewrite + rerank
        self.assertIn(["cqa-001", 2], rec["sections"][:5])

    def test_rerank_failure_keeps_fused_order_and_flags_it(self):
        lc, u = fixture.FakeLC(), common.Usage()
        lc_chat = lc.chat

        def chat(messages, **kw):
            if "numbered passages" in messages[0]["content"]:
                return {"content": "I cannot rank these.", "usage": {}, "provider": "anthropic",
                        "model": "claude-haiku-5-5", "error": None}
            return lc_chat(messages, **kw)
        lc.chat = chat
        rec = self.r.retrieve(lc, fixture.QS[0], ("anthropic", "claude-haiku-5-5"), u, strategy="listwise")
        self.assertTrue(rec["rerank_failed"])
        self.assertEqual(rec["reranked_ids"], rec["fused"][:20])

    def test_strategy_none_makes_no_rerank_call(self):
        lc, u = fixture.FakeLC(), common.Usage()
        rec = self.r.retrieve(lc, fixture.QS[0], ("anthropic", "claude-haiku-5-5"), u, strategy="none")
        self.assertEqual(u.to_json()["total"]["calls"], 1)
        self.assertEqual(rec["rerank_strategy"], "none")

    def test_per_query_time_on_toy_index_is_small(self):
        t0 = time.time()
        for q in fixture.QS:
            self.r.leg_fuse(q["query"], fixture.FakeLC().embed([q["query"]])[0])
        self.assertLess(time.time() - t0, 1.0)


if __name__ == "__main__":
    unittest.main()
