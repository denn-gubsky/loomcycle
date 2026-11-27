"""Instrument checks 1 and 2: each can fail, and the corpus filter's list is check 2's
denominator.

    python3 -I -m unittest discover -s bench/docs/kbrag/hc/tests
"""
import unittest

import hcfixture  # noqa: F401  (sys.path)
import corpus, coverage

PAGES = [{"id": "D%03d" % i, "title": "Page %d" % i, "type": None, "paths": ["/cqa/page-%d" % i]} for i in range(100)]
HOUSEKEEPING = {"id": "MEM1", "title": "Deployment context", "type": None, "paths": ["/memory/context"]}
ONTOLOGY = {"id": "ONT1", "title": "Ontology", "type": None, "paths": ["/memory/ontology"]}
STUB = {"id": "STUB1", "title": "stub", "type": "SourceStub", "paths": ["/cqa/stub"]}
TYPED_MEMORY = {"id": "MEM2", "title": "A guide filed under memory", "type": "Guide", "paths": ["/memory/guides/g"]}
EXPECTED = [p["id"] for p in PAGES]


class TestFilter(unittest.TestCase):
    def test_untyped_memory_document_ontology_and_operational_types_are_dropped(self):
        keep, dropped = corpus.filter_corpus(PAGES + [HOUSEKEEPING, ONTOLOGY, STUB])
        self.assertEqual(keep, sorted(EXPECTED))
        self.assertEqual(dropped, {"MEM1": "untyped_under_memory", "ONT1": "ontology", "STUB1": "operational:SourceStub"})

    def test_ontology_is_recognised_by_its_id_even_after_a_rename(self):
        moved = dict(ONTOLOGY, paths=["/kb/our-ontology"])
        keep, dropped = corpus.filter_corpus(PAGES + [moved], ontology_id="ONT1")
        self.assertEqual(dropped, {"ONT1": "ontology"})

    def test_typed_document_under_memory_and_untyped_page_elsewhere_stay(self):
        keep, _ = corpus.filter_corpus(PAGES[:2] + [TYPED_MEMORY])
        self.assertEqual(keep, ["D000", "D001", "MEM2"])

    def test_memory_prefix_is_a_path_segment_not_a_string_prefix(self):
        keep, _ = corpus.filter_corpus([{"id": "X", "type": None, "paths": ["/memoryless/x"]}])
        self.assertEqual(keep, ["X"])


class TestCheck1(unittest.TestCase):
    def test_passes_when_the_untyped_memory_document_is_filtered(self):
        keep, _ = corpus.filter_corpus(PAGES + [HOUSEKEEPING])
        self.assertEqual(corpus.check_same_corpus(keep, EXPECTED)["status"], "pass")

    def test_fails_on_an_extra_listed_document(self):
        extra = {"id": "EXTRA", "title": "A stray typed doc", "type": "Note", "paths": ["/notes/x"]}
        keep, _ = corpus.filter_corpus(PAGES + [HOUSEKEEPING, extra])
        r = corpus.check_same_corpus(keep, EXPECTED)
        self.assertEqual(r["status"], "fail")
        self.assertEqual(r["n_only_listed"], 1)

    def test_the_benchmarks_types_only_filter_would_have_failed(self):
        # The pre-HC filter kept every untyped document; the housekeeping one then failed check 1.
        old = sorted(d["id"] for d in PAGES + [HOUSEKEEPING] if d["type"] not in {"SourceStub", "SyncReport", "Annotation"})
        self.assertEqual(corpus.check_same_corpus(old, EXPECTED)["status"], "fail")

    def test_fails_on_a_missing_page_unless_it_has_no_readable_section(self):
        keep = sorted(EXPECTED[1:])
        self.assertEqual(corpus.check_same_corpus(keep, EXPECTED)["status"], "fail")
        self.assertEqual(corpus.check_same_corpus(keep, EXPECTED, empty={"D000"})["status"], "pass")

    def test_fails_on_a_hit_outside_the_corpus(self):
        r = corpus.check_same_corpus(EXPECTED, EXPECTED, hit_docs={"D001", "MEM1"})
        self.assertEqual((r["status"], r["hits_outside_corpus"]), ("fail", 1))

    def test_without_an_expected_set_only_the_hits_are_checked(self):
        self.assertEqual(corpus.check_same_corpus(EXPECTED, None, hit_docs={"D001"})["status"], "pass")


class TestSnapshotListing(unittest.TestCase):
    def test_columnar_pages_are_read_and_followed_to_the_end(self):
        pages = [
            {"page": {"documents": 2, "next_cursor": "c1"}, "codes": {"types": ["SourceStub"]},
             "documents": {"ids": ["A", "B"], "titles": ["a", "b"], "type_code": [-1, 0], "paths": [["/memory/x"], ["/s"]]}},
            {"page": {"documents": 1}, "codes": {"types": ["Guide"]},
             "documents": {"ids": ["C"], "titles": ["c"], "type_code": [0], "paths": [["/g"]]}},
        ]
        calls = []

        class LC:
            def doc(self, op, **kw):
                calls.append(kw.get("after"))
                return pages[len(calls) - 1]

        docs = corpus.list_documents(LC())
        self.assertEqual(calls, [None, "c1"])
        self.assertEqual([(d["id"], d["type"], d["paths"]) for d in docs],
                         [("A", None, ["/memory/x"]), ("B", "SourceStub", ["/s"]), ("C", "Guide", ["/g"])])
        self.assertEqual(corpus.filter_corpus(docs)[0], ["C"])


def anns(ids, stale=()):
    return [{"document_id": i, "stale": i in stale} for i in ids]


class TestCheck2(unittest.TestCase):
    def corpus_of(self, n):
        return ["D%04d" % i for i in range(n)]

    def test_coverage_of_98_9_percent_fails_and_99_passes(self):
        c = self.corpus_of(1000)
        self.assertEqual(coverage.coverage(c, anns(c[:989]))["status"], "fail")
        self.assertEqual(coverage.coverage(c, anns(c[:990]))["status"], "pass")

    def test_stale_share_of_1_1_percent_fails_and_1_0_passes(self):
        c = self.corpus_of(1000)
        self.assertEqual(coverage.coverage(c, anns(c, stale=set(c[:11])))["status"], "fail")
        self.assertEqual(coverage.coverage(c, anns(c, stale=set(c[:10])))["status"], "pass")

    def test_the_filtered_housekeeping_document_is_not_in_the_denominator(self):
        # 100 pages, all annotated; the untyped /memory document has no annotation. Counted, it
        # would bring coverage to 100/101 = 0.990..., and with 2 such documents below 0.99.
        docs = PAGES + [HOUSEKEEPING, dict(HOUSEKEEPING, id="MEM3", paths=["/memory/other"])]
        keep, _ = corpus.filter_corpus(docs)
        r = coverage.coverage(keep, anns(EXPECTED))
        self.assertEqual((r["documents"], r["coverage"], r["status"]), (100, 1.0, "pass"))
        unfiltered = coverage.coverage([d["id"] for d in docs], anns(EXPECTED))
        self.assertEqual(unfiltered["status"], "fail")

    def test_an_annotation_outside_the_corpus_counts_for_nothing(self):
        c = self.corpus_of(100)
        r = coverage.coverage(c, anns(c[:98] + ["MEM1", "ONT1"]))
        self.assertEqual((r["covered"], r["status"]), (98, "fail"))

    def test_the_servers_sample_size_is_recorded(self):
        c = self.corpus_of(10)
        r = coverage.coverage(c, anns(c), {"counts": {"document": {"total": 10, "stale": 0}}, "stale_checked": 2000})
        self.assertEqual(r["server_summary"]["sample_size"], 2000)

    def test_annotation_listing_is_paged_at_document_level(self):
        seen = []
        pages = [{"annotations": [{"document_id": "A", "stale": False}], "next_cursor": "x/document"},
                 {"annotations": [{"document_id": "B", "stale": True}]}]

        class LC:
            def doc(self, op, **kw):
                seen.append((op, kw["level"], kw.get("after")))
                return pages[len(seen) - 1]

        self.assertEqual(coverage.list_document_annotations(LC()), anns(["A", "B"], stale={"B"}))
        self.assertEqual(seen, [("list_annotations", "document", None), ("list_annotations", "document", "x/document")])


if __name__ == "__main__":
    unittest.main()
