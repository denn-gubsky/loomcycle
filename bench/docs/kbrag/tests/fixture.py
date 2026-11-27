"""A tiny synthetic Study C on disk and a fake `lc`, so every test runs offline."""
import json, os, sys, tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))
import common  # noqa: E402

DOCS = [
    {"id": "cqa-000", "title": "Child Benefit", "sections": [
        {"path": [], "text": ""},
        {"path": ["Overview"], "text": "Child Benefit is paid every 4 weeks to parents."},
        {"path": ["Overview", "Eligibility"], "text": "You can claim if you are responsible for a child under 16."},
        {"path": ["How to claim"], "text": ""},
        {"path": ["How to claim", "Online"], "text": "Claim online with your Government Gateway account."}]},
    {"id": "cqa-001", "title": "Adoption", "sections": [
        {"path": [], "text": ""},
        {"path": ["Overview"], "text": "To adopt you must be aged 21 or over."},
        {"path": ["Court"], "text": "The court will reply within 10 days of the application."}]},
    {"id": "cqa-002", "title": "Pension Credit", "sections": [
        {"path": [], "text": "Pension Credit tops up your weekly income."},
        {"path": ["Eligibility"], "text": "You must have reached State Pension age."}]},
]
QS = [
    {"qid": "q1", "doc_id": "cqa-001", "scenario": "I want to adopt.", "question": "How old must I be?",
     "answers": ["aged 21 or over"], "gold": [1]},
    {"qid": "q2", "doc_id": "cqa-001", "scenario": "I applied to court.", "question": "When will the court reply?",
     "answers": ["within 10 days"], "gold": [2]},
    {"qid": "q3", "doc_id": "cqa-000", "scenario": "I have a child.", "question": "How often is it paid?",
     "answers": ["every 4 weeks"], "gold": [1]},
    {"qid": "q4", "doc_id": "cqa-002", "scenario": "I am 70.", "question": "What age must I have reached?",
     "answers": ["State Pension age"], "gold": [1]},
]
for _q in QS:
    _q["query"] = _q["scenario"] + " " + _q["question"]
DOCMAP = {d["id"]: "D" + d["id"][-3:] for d in DOCS}
CHUNKS = {d["id"]: ["C%s_%d" % (d["id"][-3:], i) for i in range(len(d["sections"]))] for d in DOCS}


def make_work():
    """A temp work dir laid out like /home/denn/workspace/kbrag-work/cqa; returns (dir, Data)."""
    w = tempfile.mkdtemp(prefix="kbrag-test-")
    os.makedirs(os.path.join(w, "prepared"))
    with open(os.path.join(w, "prepared", "docs.jsonl"), "w") as f:
        f.writelines(json.dumps(d) + "\n" for d in DOCS)
    with open(os.path.join(w, "prepared", "questions.jsonl"), "w") as f:
        f.writelines(json.dumps(q) + "\n" for q in QS)
    common.jdump([q["qid"] for q in QS], os.path.join(w, "subset.json"))
    common.jdump(DOCMAP, os.path.join(w, "docmap.json"))
    common.jdump(CHUNKS, os.path.join(w, "chunks.json"))
    data = common.Data(prepared=os.path.join(w, "prepared"), docmap=os.path.join(w, "docmap.json"),
                       chunkmap=os.path.join(w, "chunks.json"), subset=os.path.join(w, "subset.json"))
    return w, data


class FakeLC:
    """Answers from the gold so arms score; serves every call from `served` (default Haiku)."""

    def __init__(self, served=("anthropic", "claude-haiku-5-5"), kb_text=None, spent=0.0, fail_kb=()):
        self.served, self.kb_text, self.spent, self.fail_kb = served, kb_text, spent, set(fail_kb)
        self.calls = {"run": 0, "chat": 0, "embed": 0, "tree": 0}
        self.runs = {}

    def version(self):
        return {"version": "1.165.0", "commit": "abc"}

    def spend(self):
        return self.spent

    def run(self, agent, prompt, *, metadata=None, user_tier="free", idempotency_key=None, timeout=1800,
            provider=None, model=None):
        self.calls["run"] += 1
        rid = "r%d" % self.calls["run"]
        self.runs[rid] = agent
        if agent == "kb/ask":
            q = next(q for q in QS if q["question"] in prompt)
            if q["qid"] in self.fail_kb:
                return {"run_id": rid, "text": "partial", "status": "failed", "error": "boom", "wall_s": 1.0}
            text = self.kb_text or ("The answer is %s [p](loom:doc/%s?scope=tenant) [s](loom:chunk/%s?scope=tenant)\n\n"
                                    "ROUTE: search | SCORE: 0.9 | CLASSIFIER: decision | RUNS: kb/search:completed"
                                    % (q["answers"][0], DOCMAP[q["doc_id"]], CHUNKS[q["doc_id"]][q["gold"][0]]))
            return {"run_id": rid, "text": text, "status": "completed", "error": None, "wall_s": 2.0}
        q = next(q for q in QS if q["query"] == prompt)
        hits = [{"chunk_id": CHUNKS[q["doc_id"]][q["gold"][0]], "score": 1.0},
                {"chunk_id": CHUNKS["cqa-000"][0], "score": 0.5},   # bodyless: skipped
                {"chunk_id": CHUNKS["cqa-002"][1], "score": 0.4}]
        return {"run_id": rid, "text": json.dumps({"chunks": hits, "reranked": True}), "status": "completed",
                "error": None, "wall_s": 0.5}

    def tree_usage(self, run_id):
        self.calls["tree"] += 1
        runs = [{"run_id": run_id, "agent": self.runs[run_id], "provider": "code-js", "model": "",
                 "input_tokens": 0, "output_tokens": 0, "cached_tokens": 0}]
        if self.runs[run_id] == "kb/ask":
            runs.append({"run_id": run_id + "c", "agent": "kb/search", "provider": self.served[0], "model": self.served[1],
                         "input_tokens": 1000, "output_tokens": 100, "cached_tokens": 0})
        return {"runs": runs, "totals": {}, "providers": []}

    def chat(self, messages, *, model, provider="anthropic", temperature=0, max_tokens=1024, timeout=600):
        self.calls["chat"] += 1
        p = messages[0]["content"]
        q = next((q for q in QS if q["question"] in p), QS[0])
        if p.startswith("Rewrite"):
            content = json.dumps(["a " + q["question"], "b " + q["question"], "c " + q["question"]])
        elif "numbered passages" in p:
            content = json.dumps(list(range(1, 21)))
        elif p.startswith("Answer the question"):
            content = "ANSWER: %s\nSOURCES: loom:doc/%s" % (q["answers"][0], DOCMAP[q["doc_id"]])
        elif p.startswith("Below are a question"):
            content = q["answers"][0]
        elif p.startswith("You grade"):
            content = "CORRECT\nIt matches."
        else:
            content = "?"
        return {"content": content, "usage": {"input_tokens": 100, "output_tokens": 10}, "stop_reason": "end_turn",
                "provider": self.served[0], "model": self.served[1], "wall_s": 0.1, "error": None}

    def embed(self, texts):
        self.calls["embed"] += 1
        # A deterministic bag-of-letters vector: texts sharing words point the same way.
        out = []
        for t in texts:
            v = [0.0] * 26
            for ch in t.lower():
                if "a" <= ch <= "z":
                    v[ord(ch) - 97] += 1.0
            out.append(v)
        return out
