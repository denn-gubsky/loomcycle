"""Offline fixtures for the HC harness: the benchmark's synthetic ConditionalQA corpus
(../../tests/fixture.py), a fake client that plays kb/ask, kb/answer and kb/read, and a fake
spend meter. No network; synthetic ids only."""
import json, os, sys, tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
HC = os.path.dirname(HERE)
KBRAG = os.path.dirname(HC)
for p in (os.path.join(KBRAG, "tests"), KBRAG, HC):
    if p not in sys.path:
        sys.path.insert(0, p)

import fixture  # noqa: E402  (../../tests/fixture.py)
from fixture import CHUNKS, DOCMAP, QS  # noqa: E402

HAIKU = ("anthropic", "claude-haiku-5-5")
# Invented kb/answer constants, so a runner test with the oracle needs no kb/answer in the bundle.
CONTRACT = {"head": "H", "ask": "Read.", "section_chars": 1000, "context_chars": 100000, "min_text": 10}


def question_of(text):
    return next(q for q in QS if q["question"] in (text or ""))


class FakeSpend:
    def __init__(self, value=0.0):
        self.value = value

    def increase(self):
        return self.value


class FakeClient:
    """Answers from the gold. Per arm: kb/ask writes prose + a ROUTE trailer (with the first
    call when the run metadata asks for it), kb/answer the answer + FIRST fields, kb/read the
    ANSWER/SOURCES lines. `reranked` decides each first call's RERANKED field by question id;
    `served` is what serves every model run; `search_inputs` are kb/answer's search calls."""

    def __init__(self, served=HAIKU, reranked=None, search_inputs=None, version="1.170.0", fail=(), wrong=()):
        self.served, self.reranked = served, reranked or (lambda qid: True)
        self.search_inputs = search_inputs or [{"op": "search", "scope": "tenant", "query": "q", "limit": 20}]
        self.ver, self.fail, self.wrong = version, set(fail), set(wrong)  # wrong: {(arm, qid)}
        self.n, self.runs, self.bodies, self.first = 0, {}, [], {}

    def version(self):
        return {"version": self.ver, "commit": "c0ffee"}

    def disallowed(self, pairs):
        return []

    def ledger_pairs(self, since=None):
        return []

    def doc(self, op, **kw):
        if op == "get_chunk":
            return {"document_id": "D-" + kw["id"]}
        raise AssertionError("unexpected doc op " + op)

    def run(self, agent, prompt=None, *, segments=None, metadata=None, user_tier="free", idempotency_key=None,
            timeout=1800):
        self.n += 1
        rid = "r%d" % self.n
        self.runs[rid] = agent
        self.first[rid] = bool((metadata or {}).get("first_call"))
        self.bodies.append({"agent": agent, "prompt": prompt, "segments": segments, "metadata": metadata,
                            "idempotency_key": idempotency_key})
        text_in = prompt if segments is None else segments[0]["content"][0]["text"]
        q = question_of(text_in)
        if q["qid"] in self.fail:
            return {"run_id": rid, "text": "", "status": "failed", "error": "boom", "wall_s": 1.0}
        chunk = CHUNKS[q["doc_id"]][q["gold"][0]]
        arm = (metadata or {}).get("arm")
        gold = "something else entirely" if (arm, q["qid"]) in self.wrong else q["answers"][0]
        rr = "true (ok)" if self.reranked(q["qid"]) else "false (timeout)"
        first = "FIRST: found | SECTIONS: 5 | RERANKED: %s" % rr
        if agent == "kb/read":
            text = "ANSWER: %s\nSOURCES: S1" % gold
        elif agent == "kb/answer":
            text = "%s\n\nSources:\n- [p](loom:chunk/%s?scope=tenant)\n\n%s" % (gold, chunk, first)
        else:
            if (metadata or {}).get("first_call"):
                route = "ROUTE: answer | SCORE: 0.05 | CLASSIFIER: decision | RUNS: kb/answer:completed | " + first
            else:
                route = "ROUTE: search | SCORE: 0.05 | CLASSIFIER: decision | RUNS: kb/search:completed"
            text = "The answer is %s [p](loom:doc/%s?scope=tenant)\n\n%s | DEPTH: standard" % (
                gold, DOCMAP[q["doc_id"]], route)
        return {"run_id": rid, "text": text, "status": "completed", "error": None, "wall_s": 1.0}

    def tree_usage(self, run_id):
        agent = self.runs[run_id]
        runs = [{"run_id": run_id, "agent": agent, "provider": "code-js", "model": "", "input_tokens": 0,
                 "output_tokens": 0, "cached_tokens": 0, "cache_creation_tokens": 0}]
        child = {"kb/ask": "kb/search", "kbhc/ask-af": "kbhc/search-af", "kb/answer": "kb/read"}.get(agent)
        if agent == "kb/read":
            runs[0].update(provider=self.served[0], model=self.served[1], input_tokens=500, output_tokens=20)
        if child:
            runs.append({"run_id": run_id + "c", "agent": child, "provider": self.served[0], "model": self.served[1],
                         "input_tokens": 1000, "output_tokens": 100, "cached_tokens": 0, "cache_creation_tokens": 0})
        if self.first.get(run_id):  # the first call ran inside kb/ask: kb/answer, then kb/read
            runs[1:] = [{"run_id": run_id + "a", "agent": "kb/answer", "provider": "code-js", "model": "",
                         "input_tokens": 0, "output_tokens": 0, "cached_tokens": 0, "cache_creation_tokens": 0},
                        {"run_id": run_id + "r", "agent": "kb/read", "provider": self.served[0], "model": self.served[1],
                         "input_tokens": 400, "output_tokens": 20, "cached_tokens": 0, "cache_creation_tokens": 0}]
        return {"runs": runs, "totals": {}, "providers": []}

    def run_events(self, run_id):
        return [{"type": "tool_call", "data": {"tool_use": {"name": "Document", "input": inp}}}
                for inp in self.search_inputs]

    def chat(self, messages, *, model, provider="anthropic", temperature=0, max_tokens=1024, timeout=600):
        p = messages[0]["content"]
        q = next((q for q in QS if q["question"] in p), QS[0])
        ans = p.split("Answer:\n", 1)[1].strip().splitlines()[0] if "Answer:\n" in p else "?"
        ans = ans.replace("ANSWER:", "").replace("The answer is", "").split("[")[0].strip() or q["answers"][0]
        return {"content": ans, "usage": {"input_tokens": 100, "output_tokens": 10}, "stop_reason": "end_turn",
                "provider": self.served[0], "model": self.served[1], "wall_s": 0.1, "error": None}


def make_work():
    """(work dir laid out as <work>/cqa/..., the benchmark fixture's Data)."""
    w, data = fixture.make_work()
    hc = tempfile.mkdtemp(prefix="kbrag-hc-test-")
    os.makedirs(os.path.join(hc, "cqa"))
    for name in ("docmap.json", "chunks.json"):
        with open(os.path.join(w, name)) as f, open(os.path.join(hc, "cqa", name), "w") as g:
            g.write(f.read())
    os.symlink(os.path.join(w, "prepared"), os.path.join(hc, "cqa", "prepared"))
    return hc, data


def write_json(path, obj):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        json.dump(obj, f)
