"""Offline tests of Study D's parsing and scoring (no network, synthetic ids).

    LOOMCYCLE_DEV_TOKEN=x python3 -I -m unittest discover -s bench/docs/kbrag/docs/tests
"""
import os, sys, unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))
os.environ.setdefault("LOOMCYCLE_DEV_TOKEN", "test-not-a-token")

import export  # noqa: E402
import danalyze  # noqa: E402

MD = """# Title
<!-- loom: {"id":"root"} -->

Intro text.

## One
<!-- loom: {"id":"a"} -->

Body of one.
### Not a chunk heading
still one

```
## inside a fence
<!-- loom: {"id":"fenced"} -->
```

### One.one
<!-- loom: {"id":"b"} -->

Deep.

## Two
<!-- loom: {"id":"c"} -->

<!-- loom-edges:
a -> b [cites]
-->
"""


class TestExport(unittest.TestCase):
    def test_sections_follow_loom_headings_and_paths(self):
        s = export.sections(MD)
        self.assertEqual([x["chunk_id"] for x in s], ["root", "a", "b", "c"])
        self.assertEqual(s[2]["path"], ["One", "One.one"])
        self.assertEqual(s[3]["path"], ["Two"])
        self.assertIn("still one", s[1]["text"])
        self.assertIn("inside a fence", s[1]["text"])
        self.assertEqual(s[3]["text"], "")


class TestScore(unittest.TestCase):
    def test_prf_counts_also_for_precision_only(self):
        p, r, f = danalyze.prf({"g1", "a1", "x"}, ["g1", "g2"], ["a1"])
        self.assertAlmostEqual(p, 2 / 3)
        self.assertAlmostEqual(r, 0.5)
        self.assertAlmostEqual(f, 2 * p * r / (p + r))

    def test_prf_nothing_cited_scores_zero(self):
        self.assertEqual(danalyze.prf(set(), ["g"], []), (0.0, 0.0, 0.0))

    def test_prf_in_corpus_variant_ignores_outside_citations(self):
        p, _, _ = danalyze.prf({"g", "out"}, ["g"], [], corpus={"g"})
        self.assertEqual(p, 1.0)

    def test_chunk_citation_counts_for_its_document(self):
        q = {"gold": ["D1"], "also": [], "gold_topics": [], "text": "q"}
        row = {"arm": "kb", "citations": {"docs": [], "chunks": ["c1"]}}
        s = danalyze.score_run(q, row, {"c1": "D1"}, {"D1"})
        self.assertEqual((s["R"], s["R_docs_only"]), (1.0, 0.0))


if __name__ == "__main__":
    unittest.main()
