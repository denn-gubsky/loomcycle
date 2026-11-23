"""Study D question sets, loaded from gold files kept OUTSIDE the repo. Never pooled.

- `p7`: the 24 questions of scripts/measure/walker/questions.yaml with the operator's gold
  (loomcycle-internal gm-p7/p7_gold.yaml), merged by the walker driver's own parser.
- `waves`: the operator's 12 question waves (loomcycle-internal doc-internal), normalised
  to <work>/gold/waves.json.
- `known`: the mechanical known-item set (<work>/gold/known_items.json).

Each question: {id, set, kind, text, gold, also, gold_topics, expected_answer}.
"""
import hashlib, json, os, sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import dconfig  # noqa: E402

P7_GOLD = os.environ.get("KBRAG_P7_GOLD", "/home/denn/workspace/loomcycle-internal/gm-p7/p7_gold.yaml")
WAVES = os.path.join(dconfig.WORK, "gold", "waves.json")
WALKER_DIR = os.path.normpath(os.path.join(HERE, "..", "..", "..", "..", "scripts", "measure", "walker"))


def sha256(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def p7():
    sys.path.insert(0, WALKER_DIR)
    import drive  # scripts/measure/walker/drive.py: its parser and merge rules, unchanged
    qs = drive.load_questions(os.path.join(WALKER_DIR, "questions.yaml"))
    drive.merge_gold(qs, open(P7_GOLD, encoding="utf-8").read())
    return [{"id": "p7-" + q["id"], "set": "p7", "kind": q["kind"], "text": q["text"], "gold": q.get("gold") or [],
             "also": q.get("also") or [], "gold_topics": q.get("gold_topics") or [], "expected_answer": None}
            for q in qs]


def waves():
    if not os.path.exists(WAVES):
        return []
    out = []
    for q in json.load(open(WAVES, encoding="utf-8"))["questions"]:
        out.append({"id": q["id"], "set": "waves", "wave": q.get("wave"), "kind": q.get("class"), "text": q["text"],
                    "gold": q.get("core") or [], "also": q.get("borderline") or [], "gold_topics": [],
                    "expected_answer": q.get("expected_answer")})
    return out


def known():
    if not os.path.exists(dconfig.KNOWN):
        return []
    return [{"id": q["id"], "set": "known", "kind": "known-item", "text": q["text"], "gold": q["gold"], "also": [],
             "gold_topics": [], "expected_answer": None} for q in json.load(open(dconfig.KNOWN))["questions"]]


def load(sets=("p7", "waves", "known")):
    out = []
    for s in sets:
        out += {"p7": p7, "waves": waves, "known": known}[s]()
    return {q["id"]: q for q in out}
