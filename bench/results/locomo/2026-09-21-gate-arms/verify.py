#!/usr/bin/env python3
"""Arm 2's decider: ask a LOCAL model whether the retrieved material answers the question.

Same withhold decision as the threshold gate, different decider — that is the whole
point of the comparison, so nothing else may differ. Reads the material straight out of
the retrieval dump, so the verifier judges EXACTLY the set the threshold judged.
"""
import json, sys, urllib.request, concurrent.futures as cf

S = "/private/tmp/claude-503/-Users-denn-work-loomcycle/e34a60ba-4e4b-4a01-be79-101f463f1a05/scratchpad"
TOK = open(S + "/lme/.tok-gate").read().strip()
BASE = "http://127.0.0.1:8801"


def turn_text(t):
    try:
        return json.loads(t).get("text", "")
    except Exception:
        return t


def final_text(sse):
    """Accumulate `text` deltas; ignore `thinking` (ornith is a reasoning model and its
    trace is not the answer)."""
    out = []
    for line in sse.split("\n"):
        if not line.startswith("data: "):
            continue
        try:
            d = json.loads(line[6:])
        except Exception:
            continue
        if d.get("type") == "text" and d.get("text"):
            out.append(d["text"])
    return "".join(out)


def verdict(text):
    """true / false / None when the model did not produce a parseable verdict —
    None is NOT silently folded into either class; it is reported."""
    i, j = text.find("{"), text.rfind("}")
    if i >= 0 and j > i:
        try:
            return bool(json.loads(text[i:j + 1]).get("contains_answer"))
        except Exception:
            pass
    return None


def one(r):
    mat = "\n".join("- " + turn_text(t) for t in r.get("texts", []))
    prompt = "QUESTION: %s\n\nMATERIAL:\n%s" % (r["question"], mat)
    body = json.dumps({"agent": "locomo/sufficiency", "prompt": prompt,
                       "user_id": "bench",
                       # ⚠️ 2000, not the def's 300: ornith is a reasoning model and its
                       # trace alone spent the whole 300-token budget, so every run
                       # stopped at max_tokens with ZERO text — 69 of 130 unparsed.
                       "max_tokens": 2000}).encode()
    req = urllib.request.Request(BASE + "/v1/runs", data=body,
                                 headers={"Authorization": "Bearer " + TOK,
                                          "Content-Type": "application/json"})
    for attempt in range(2):
        try:
            with urllib.request.urlopen(req, timeout=900) as resp:
                return r["question"], verdict(final_text(resp.read().decode()))
        except Exception as e:
            if attempt:
                return r["question"], None
    return r["question"], None


rows = [json.loads(l) for l in open(S + "/lme/dump-texts.jsonl") if l.strip()]
res = {}
with cf.ThreadPoolExecutor(max_workers=4) as ex:
    for i, (q, v) in enumerate(ex.map(one, rows), 1):
        res[q] = v
        if i % 20 == 0:
            print("  %d/%d" % (i, len(rows)), flush=True)
unparsed = sum(1 for v in res.values() if v is None)
says_no = sum(1 for v in res.values() if v is False)
print("verdicts: %d | says-NO (would withhold) %d | unparsed %d"
      % (len(res), says_no, unparsed))
json.dump({q: v for q, v in res.items() if v is not None},
          open(S + "/lme/verifier-verdicts.json", "w"))
