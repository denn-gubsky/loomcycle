"""Unit tests for drive.py: stdlib only, no network.

  python3 -I -m unittest scripts/measure/walker/test_drive.py
"""

import contextlib
import importlib.util
import io
import json
import os
import tempfile
import unittest

_HERE = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location("walker_drive", os.path.join(_HERE, "drive.py"))
drive = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(drive)

QUESTIONS = """\
questions:
  - id: q01
    kind: overview
    text: What do we have on routing?
    gold: []
  - id: q02
    kind: area
    text: What is in our material on security, grouped by subject?
    gold: []
  - id: q03
    kind: overview
    text: A question nobody filled.
    gold: []
"""

# Synthetic ids: never real documents.
DOC_A = "a" * 32
DOC_B = "b" * 32
DOC_C = "c" * 32

GOLD = f"""\
# a comment line
questions:
  - id: q01
    gold: [{DOC_A}, "{DOC_B}"]  # trailing comment
    also:
      - {DOC_C}
    confidence: high
    gold_notes: |
      Filled from a listing; # is text here.
      Second line.
  - id: q02
    gold_topics:
      - name: security and access control
        aliases: [access control, authz]
      - name: sandboxing
      - secrets
"""


def _questions(tmp):
    p = os.path.join(tmp, "questions.yaml")
    with open(p, "w", encoding="utf-8") as f:
        f.write(QUESTIONS)
    return drive.load_questions(p)


class GoldMergeTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.qs = _questions(self.tmp)

    def test_merge_gold_fills_docs_also_topics_by_id(self):
        warnings = []
        drive.merge_gold(self.qs, GOLD, warn=warnings.append)
        q1, q2, q3 = self.qs
        self.assertEqual(q1["gold"], [DOC_A, DOC_B])
        self.assertEqual(q1["also"], [DOC_C])
        self.assertEqual(q1["confidence"], "high")
        self.assertEqual(q1["gold_notes"], "Filled from a listing; # is text here.\nSecond line.")
        self.assertEqual(q2["gold"], [])
        self.assertEqual(q2["gold_topics"], [
            {"name": "security and access control", "aliases": ["access control", "authz"]},
            {"name": "sandboxing", "aliases": []},
            {"name": "secrets", "aliases": []},
        ])
        # The question text stays questions.yaml's.
        self.assertEqual(q2["text"], "What is in our material on security, grouped by subject?")
        self.assertEqual(q3["gold"], [])

    def test_merge_gold_warns_on_questions_without_gold(self):
        warnings = []
        drive.merge_gold(self.qs, GOLD, warn=warnings.append)
        self.assertEqual(len(warnings), 1)
        self.assertIn("q03", warnings[0])

    def test_merge_gold_refuses_unknown_id(self):
        with self.assertRaisesRegex(ValueError, "unknown question id q99"):
            drive.merge_gold(self.qs, "questions:\n  - id: q99\n    gold: [x]\n")

    def test_merge_gold_refuses_duplicate_id_and_unknown_key(self):
        with self.assertRaisesRegex(ValueError, "duplicate question id q01"):
            drive.merge_gold(self.qs, "questions:\n  - id: q01\n  - id: q01\n")
        with self.assertRaisesRegex(ValueError, "unknown keys"):
            drive.merge_gold(self.qs, "questions:\n  - id: q01\n    gould: [x]\n")

    def test_topic_named_in_question_text_is_warned_and_unscorable(self):
        warnings = []
        gold = ("questions:\n  - id: q02\n    gold_topics:\n"
                "      - name: access control\n        aliases: [Security]\n"
                "      - name: sandboxing\n")
        drive.merge_gold(self.qs, gold, warn=warnings.append)
        topics = self.qs[1]["gold_topics"]
        self.assertTrue(topics[0].get("unscorable"))
        self.assertFalse(topics[1].get("unscorable"))
        self.assertTrue(any("q02" in w and "access control" in w for w in warnings), warnings)
        # The unscorable topic leaves the denominator: the answer restating
        # the question earns nothing for it.
        s = drive.score(self.qs[1], {"text": "On security: sandboxing.", "tools": {}})
        self.assertEqual(s["recall"], 1.0)
        self.assertEqual(s["topics_named"], ["sandboxing"])
        self.assertEqual(s["topics_unscorable"], ["access control"])

    def test_main_refuses_gold_file_with_unknown_id(self):
        gold = os.path.join(self.tmp, "gold.yaml")
        with open(gold, "w", encoding="utf-8") as f:
            f.write("questions:\n  - id: q99\n    gold: [x]\n")
        with self.assertRaises(SystemExit) as cm, contextlib.redirect_stderr(io.StringIO()):
            drive.main(["--questions", os.path.join(self.tmp, "questions.yaml"), "--gold", gold,
                        "--arm", "tax=kb/walker", "--dry-run"])
        self.assertIn("unknown question id q99", str(cm.exception.code))


class YamlSubsetTest(unittest.TestCase):
    def test_flow_list_apostrophe_inside_item_keeps_items_apart(self):
        self.assertEqual(drive._scalar("[owner's view, access control]"), ["owner's view", "access control"])
        self.assertEqual(drive._scalar("['a, b', it's]"), ["a, b", "it's"])

    def test_comment_after_plain_scalar_with_apostrophe_is_stripped(self):
        self.assertEqual(drive._strip_comment("confidence: owner's call  # a note"), "confidence: owner's call")
        self.assertEqual(drive._strip_comment("- it's here # note"), "- it's here")
        self.assertEqual(drive._strip_comment("aliases: [owner's view, x] # note"), "aliases: [owner's view, x]")
        # A quoted scalar still keeps its #.
        self.assertEqual(drive._strip_comment("gold_notes: 'a # b' # c"), "gold_notes: 'a # b'")
        self.assertEqual(drive._strip_comment("x: 'it''s # in' # out"), "x: 'it''s # in'")
        self.assertEqual(drive._strip_comment('- "a # b"  # c'), '- "a # b"')


class TopicMatchTest(unittest.TestCase):
    TOPIC = {"name": "security and access control", "aliases": ["security", "access control", "authz"]}

    def test_topic_named_by_name_case_insensitive_across_whitespace(self):
        self.assertTrue(drive.topic_named("On Security And\nAccess Control: ...", self.TOPIC))

    def test_topic_named_by_alias(self):
        self.assertTrue(drive.topic_named("The authz rules decide this.", self.TOPIC))
        self.assertTrue(drive.topic_named("See (access control).", self.TOPIC))

    def test_topic_alias_matches_only_on_word_boundary(self):
        self.assertFalse(drive.topic_named("the authzx module and xauthz", self.TOPIC))
        self.assertFalse(drive.topic_named("insecurity", {"name": "security", "aliases": []}))

    def test_shorter_topic_inside_longer_topic_match_counts_only_the_longer(self):
        q = {"id": "q", "gold": [], "gold_topics": [
            {"name": "access control", "aliases": []},
            {"name": "access", "aliases": ["control"]},
        ]}
        s = drive.score(q, {"text": "We hold notes on access control.", "tools": {}})
        self.assertEqual(s["topics_named"], ["access control"])
        self.assertEqual(s["recall"], 0.5)
        # A separate mention of the shorter term still counts.
        s = drive.score(q, {"text": "Access control, and access logs.", "tools": {}})
        self.assertEqual(s["topics_named"], ["access control", "access"])

    def test_topic_name_with_regex_characters_is_literal(self):
        t = {"name": "c++ (native)", "aliases": []}
        self.assertTrue(drive.topic_named("we cover C++ (native) code", t))
        self.assertFalse(drive.topic_named("we cover c native code", t))


def _run(arm, question, kind, text):
    return {"arm": arm, "agent": "kb/" + arm, "question": question, "kind": kind, "rep": 0,
            "text": text, "tool_calls": 2, "tools": {}, "input": 0, "output": 0, "cache_read": 0,
            "cache_write": 0, "stop": "end_turn", "error": None, "run_id": None, "cost_usd": 0.01,
            "score": {}}


class ScoreOnlyTest(unittest.TestCase):
    def test_score_only_reports_docs_and_topics_separately_per_arm(self):
        tmp = tempfile.mkdtemp()
        qpath = os.path.join(tmp, "questions.yaml")
        with open(qpath, "w", encoding="utf-8") as f:
            f.write(QUESTIONS)
        gold = os.path.join(tmp, "gold.yaml")
        with open(gold, "w", encoding="utf-8") as f:
            f.write(GOLD)
        out = os.path.join(tmp, "out")
        os.makedirs(out)
        runs = [
            # q01 docs: tax cites A and C (also) -> recall 0.5, precision 1.0.
            _run("tax", "q01", "overview", f"loom:doc/{DOC_A} and loom:doc/{DOC_C}\nCOVERAGE: complete"),
            _run("notax", "q01", "overview", "nothing cited"),
            # q02 topics: tax names 2 of 3, notax 1 of 3 (authzx does not count).
            _run("tax", "q02", "area", "Groups: authz; Sandboxing."),
            _run("notax", "q02", "area", "Groups: authzx, secrets."),
            _run("tax", "q03", "overview", "anything"),
        ]
        with open(os.path.join(out, "runs.jsonl"), "w", encoding="utf-8") as f:
            for r in runs:
                f.write(json.dumps(r) + "\n")

        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            drive.main(["--questions", qpath, "--gold", gold, "--out", out, "--score-only"])

        with open(os.path.join(out, "runs.scored.jsonl"), encoding="utf-8") as f:
            scored = {(r["arm"], r["question"]): r["score"] for r in map(json.loads, f)}
        self.assertEqual(scored[("tax", "q01")]["metric"], "docs")
        self.assertEqual(scored[("tax", "q01")]["recall"], 0.5)
        self.assertEqual(scored[("tax", "q01")]["precision"], 1.0)
        self.assertEqual(scored[("tax", "q02")]["metric"], "topics")
        self.assertEqual(scored[("tax", "q02")]["topics_named"], ["security and access control", "sandboxing"])
        self.assertEqual(scored[("notax", "q02")]["topics_named"], ["secrets"])
        self.assertIsNone(scored[("tax", "q03")]["metric"])

        with open(os.path.join(out, "summary.json"), encoding="utf-8") as f:
            summary = json.load(f)
        by_metric = {m["metric"]: m["arms"] for m in summary["by_metric"]}
        self.assertEqual(set(by_metric), {"docs", "topics"})
        self.assertEqual(by_metric["docs"]["tax"]["recall_mean"], 0.5)
        self.assertEqual(by_metric["docs"]["notax"]["recall_mean"], 0.0)
        self.assertEqual(by_metric["topics"]["tax"]["recall_mean"], 0.667)
        self.assertEqual(by_metric["topics"]["notax"]["recall_mean"], 0.333)
        metrics = {(r["arm"], r["kind"], r["metric"]) for r in summary["rows"]}
        self.assertIn(("tax", "overview", "none"), metrics)
        self.assertIn(("tax", "area", "topics"), metrics)


    def test_score_only_without_gold_refuses_and_keeps_summary(self):
        tmp = tempfile.mkdtemp()
        qpath = os.path.join(tmp, "questions.yaml")
        with open(qpath, "w", encoding="utf-8") as f:
            f.write(QUESTIONS)
        out = os.path.join(tmp, "out")
        os.makedirs(out)
        with open(os.path.join(out, "runs.jsonl"), "w", encoding="utf-8") as f:
            f.write(json.dumps(_run("tax", "q01", "overview", "x")) + "\n")
        with open(os.path.join(out, "summary.json"), "w", encoding="utf-8") as f:
            f.write("previous")
        with self.assertRaises(SystemExit) as cm, contextlib.redirect_stdout(io.StringIO()), \
                contextlib.redirect_stderr(io.StringIO()):
            drive.main(["--questions", qpath, "--out", out, "--score-only"])
        self.assertNotIn(cm.exception.code, (0, None))
        with open(os.path.join(out, "summary.json"), encoding="utf-8") as f:
            self.assertEqual(f.read(), "previous")


class DryRunTest(unittest.TestCase):
    def test_dry_run_counts_metrics_and_sends_nothing(self):
        tmp = tempfile.mkdtemp()
        qpath = os.path.join(tmp, "questions.yaml")
        with open(qpath, "w", encoding="utf-8") as f:
            f.write(QUESTIONS)
        gold = os.path.join(tmp, "gold.yaml")
        with open(gold, "w", encoding="utf-8") as f:
            f.write(GOLD)
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf), contextlib.redirect_stderr(io.StringIO()):
            drive.main(["--questions", qpath, "--gold", gold, "--arm", "tax=kb/walker",
                        "--arm", "notax=kb/walker-notax", "--runs", "2", "--dry-run",
                        "--base-url", "http://127.0.0.1:1"])
        self.assertIn("3 questions (1 scored on docs, 1 on topics) x 2 arms x 2 runs = 12 runs", buf.getvalue())
        self.assertIn("ceiling $0.48", buf.getvalue())


if __name__ == "__main__":
    unittest.main()
