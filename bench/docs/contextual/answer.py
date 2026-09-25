#!/usr/bin/env python3
"""Answer every question from its retrieved chunks with a local reader; score Answer-F1.

Retrieval is re-run here (the hybrid agent path, pooled layout), and the reader sees the
chunk BODIES exactly as stored — so the header/context arms also hand the reader their
prefix, as an agent reading these chunks would get them. Bodyless chunks (document roots,
empty parent headings) are skipped: they carry nothing to read.

`oracle` hands the reader the gold evidence paragraphs instead of retrieved chunks. It is
the reader's ceiling on this corpus, so an arm's gap to it is what retrieval still costs.

Scoring is QASPER's official Answer-F1 — SQuAD-style token F1 after normalisation, the max
over annotators — so no judge is involved. Direct Ollama calls: no path to a paid model.
"""
import argparse
import collections
import json
import os
import re
import string
import sys
import time
import urllib.request

PROMPT = """Answer the question about {doc_kind} using only the excerpts below.

{excerpts}

Question: {question}

Answer as briefly as possible: a short phrase, preferably copied from the excerpts. If the question is a yes/no question, answer only "Yes" or "No". If the excerpts do not contain the answer, answer "Unanswerable". Give only the answer."""


def normalize(s):
    s = s.lower()
    s = "".join(ch for ch in s if ch not in set(string.punctuation))
    s = re.sub(r"\b(a|an|the)\b", " ", s)
    return " ".join(s.split())


def em(pred, gold):
    return float(normalize(pred) == normalize(gold))


def f1(pred, gold):
    p, g = normalize(pred).split(), normalize(gold).split()
    common = collections.Counter(p) & collections.Counter(g)
    same = sum(common.values())
    if not p or not g:
        return float(p == g)
    if same == 0:
        return 0.0
    pr, rc = same / len(p), same / len(g)
    return 2 * pr * rc / (pr + rc)


def gold_strings(q):
    # QASPER's reference rendering of each annotation type.
    out = []
    for a in q["answers"]:
        if a.get("yes_no") is not None:
            out.append("Yes" if a["yes_no"] else "No")
        elif a.get("extractive_spans"):
            out.append(", ".join(a["extractive_spans"]))
        elif a.get("free_form_answer"):
            out.append(a["free_form_answer"])
    return out


def post(base, tok, path, body):
    r = urllib.request.Request(base + path, data=json.dumps(body).encode(), method="POST",
                               headers={"Authorization": "Bearer " + tok, "Content-Type": "application/json"})
    with urllib.request.urlopen(r, timeout=300) as x:
        return json.load(x)


def ollama(host, model, prompt):
    body = {"model": model, "stream": False, "think": False,
            "options": {"num_ctx": 16384, "temperature": 0, "num_predict": 128},
            "messages": [{"role": "user", "content": prompt}]}
    r = urllib.request.Request(host + "/api/chat", data=json.dumps(body).encode(),
                               headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(r, timeout=900) as x:
        return json.load(x)["message"]["content"].strip()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--probe", required=True)
    ap.add_argument("--base", default="http://127.0.0.1:8801")
    ap.add_argument("--host", default="http://100.112.7.68:11434")
    ap.add_argument("--model", default="ornith-1.5:35b")
    ap.add_argument("--arms", default="oracle,plain,header,context,both")
    ap.add_argument("--k", type=int, default=5)
    ap.add_argument("--doc-kind", default="a scientific paper", help='"a Wikipedia article" for NQ')
    ap.add_argument("--hide-context", action="store_true",
                    help="retrieve with the arm's index but strip the LLM context from what the "
                         "reader sees — Anthropic uses the context for indexing only")
    a = ap.parse_args()
    contexts = set()
    if a.hide_context:
        contexts = {json.loads(l)["context"] for l in open(os.path.join(a.probe, "contexts.jsonl"))}
    tok = open(os.path.join(a.probe, "..", ".tok")).read().strip()
    qs = [json.loads(l) for l in open(os.path.join(a.probe, "questions.jsonl"))]
    out_path = os.path.join(a.probe, "answers.jsonl")
    done = set()
    if os.path.exists(out_path):
        done = {(r["arm"], r["qid"]) for r in map(json.loads, open(out_path))}
    with open(out_path, "a") as f:
        for arm in a.arms.split(","):
            t0, n = time.time(), 0
            for q in qs:
                if (arm + ("/hidden-ctx" if a.hide_context else ""), q["qid"]) in done:
                    continue
                if arm == "oracle":
                    bodies, ids = q["evidence"], []
                else:
                    res = post(a.base, tok, "/v1/_memory/search",
                               {"query": 'In the paper "%s": %s' % (q["paper_title"], q["question"]),
                                "scope": "user", "scope_id": arm + "-pool", "top_k": a.k * 3,
                                "sources": ["documents"]})
                    bodies, ids = [], []
                    for e in res.get("entries") or []:
                        b = (e.get("value") or {}).get("body", "") if isinstance(e.get("value"), dict) else ""
                        if b.strip():
                            if a.hide_context:
                                head, sep, rest = b.partition("\n\n")
                                for c in contexts:
                                    if c and head.endswith(c):
                                        head = head[: -len(c)].rstrip()
                                        break
                                b = (head + sep + rest) if head else rest
                            bodies.append(b)
                            ids.append(e.get("chunk_id"))
                        if len(bodies) == a.k:
                            break
                excerpts = "\n\n".join("[%d] %s" % (i + 1, b) for i, b in enumerate(bodies))
                for attempt in range(4):
                    try:
                        pred = ollama(a.host, a.model, PROMPT.format(doc_kind=a.doc_kind, excerpts=excerpts, question=q["question"]))
                        break
                    except Exception as e:
                        # Keep Ollama's own error body: a CUDA fault and a model reload are
                        # both a bare 500 otherwise, and they need different fixes.
                        err = str(e) + " " + (e.read()[:300].decode("utf-8", "replace") if hasattr(e, "read") else "")
                        time.sleep(10 * (attempt + 1))
                else:
                    print("FAIL", arm, q["qid"], err[:200], file=sys.stderr, flush=True)
                    continue
                golds = gold_strings(q)
                score = max((f1(pred, g) for g in golds), default=0.0)
                exact = max((em(pred, g) for g in golds), default=0.0)
                f.write(json.dumps({"arm": arm + ("/hidden-ctx" if a.hide_context else ""), "qid": q["qid"], "pred": pred, "f1": score, "em": exact,
                                    "chunk_ids": ids}) + "\n")
                f.flush()
                n += 1
            print("answered", arm, n, "in %.0fs" % (time.time() - t0), flush=True)


if __name__ == "__main__":
    main()
