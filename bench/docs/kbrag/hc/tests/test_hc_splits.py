"""The frozen dev/test split: disjoint, complete, stable, and the committed file is the one
the inputs give.

    python3 -I -m unittest discover -s bench/docs/kbrag/hc/tests
"""
import json, unittest

import hcfixture  # noqa: F401  (sys.path)
import hcconfig, splits


class TestMakeSplits(unittest.TestCase):
    ANSWERABLE = ["dev-%d" % i for i in range(10)] + ["train-%d" % i for i in range(20)]
    TEST = ["train-3", "dev-7", "train-11", "dev-0"]

    def test_split_is_disjoint_and_complete(self):
        s = splits.make_splits(self.ANSWERABLE, self.TEST)
        self.assertFalse(set(s["dev"]) & set(s["test"]))
        self.assertEqual(set(s["dev"]) | set(s["test"]), set(self.ANSWERABLE))
        self.assertEqual(set(s["test"]), set(self.TEST))
        self.assertEqual(splits.problems(s, self.ANSWERABLE, self.TEST), [])

    def test_split_is_stable_whatever_the_input_order(self):
        a = splits.make_splits(self.ANSWERABLE, self.TEST)
        b = splits.make_splits(list(reversed(self.ANSWERABLE)), list(reversed(self.TEST)))
        self.assertEqual(json.dumps(a, sort_keys=True), json.dumps(b, sort_keys=True))

    def test_a_test_id_outside_the_answerable_set_is_refused(self):
        with self.assertRaises(splits.SplitError):
            splits.make_splits(self.ANSWERABLE, self.TEST + ["train-999"])

    def test_problems_catch_overlap_a_missing_id_and_a_changed_test_set(self):
        s = splits.make_splits(self.ANSWERABLE, self.TEST)
        overlap = dict(s, dev=sorted(s["dev"] + ["dev-7"]))
        self.assertTrue(any("share" in p for p in splits.problems(overlap, self.ANSWERABLE, self.TEST)))
        short = dict(s, dev=s["dev"][1:])
        self.assertTrue(any("not the answerable set" in p for p in splits.problems(short, self.ANSWERABLE, self.TEST)))
        self.assertTrue(any("reader_subset" in p for p in splits.problems(s, self.ANSWERABLE, self.TEST[:-1])))

    def test_which_names_the_side_of_a_question(self):
        s = splits.make_splits(self.ANSWERABLE, self.TEST)
        self.assertEqual(splits.which(s, "dev-7"), "test")
        self.assertEqual(splits.which(s, "dev-1"), "dev")
        self.assertIsNone(splits.which(s, "nope"))


class TestCommittedSplit(unittest.TestCase):
    def setUp(self):
        self.s = splits.load()
        with open(hcconfig.SUBSET) as f:
            self.subset = json.load(f)

    def test_test_side_is_exactly_reader_subset_and_counts_hold(self):
        self.assertEqual(set(self.s["test"]), set(self.subset))
        self.assertEqual((self.s["n_test"], self.s["n_dev"], self.s["n_answerable"]), (800, 1715, 2515))
        self.assertEqual(len(self.s["test"]), 800)
        self.assertEqual(len(self.s["dev"]), 1715)
        self.assertEqual(self.s["reader_subset_sha256"], splits.sha256(hcconfig.SUBSET))

    def test_committed_split_is_disjoint_sorted_and_complete_over_its_own_ids(self):
        answerable = self.s["dev"] + self.s["test"]
        self.assertEqual(len(set(answerable)), self.s["n_answerable"])
        self.assertEqual(splits.problems(self.s, answerable, self.subset), [])


if __name__ == "__main__":
    unittest.main()
