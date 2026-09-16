#!/usr/bin/env python3
"""Negative control on the DB-1 judge.

A judge that says CORRECT for everything would report a perfect oracle arm on a
broken answerer. Feed it each question's answer paired with ANOTHER question's
gold value and confirm it says WRONG.
"""
import json, os, urllib.request, concurrent.futures as cf

# run_arms runs the whole benchmark at import time, so the one call it shares is
# re-stated here rather than imported.
HERE = os.path.dirname(os.path.abspath(__file__))
BASE = "http://127.0.0.1:8873"
TOKEN = os.environ["LOOMCYCLE_AUTH_TOKEN"]
CORPUS = json.load(open(os.path.join(HERE, "corpus.json")))
ROWS = {(r["qid"], r["arm"]): r for r in json.load(open(os.path.join(HERE, "results.json")))}


def run(agent, prompt):
    body = json.dumps({"agent": agent, "prompt": prompt}).encode()
    req = urllib.request.Request(BASE + "/v1/runs", data=body, headers={
        "Authorization": "Bearer " + TOKEN, "Content-Type": "application/json",
        "Accept": "text/event-stream"})
    out = []
    with urllib.request.urlopen(req, timeout=300) as r:
        ev = None
        for raw in r:
            line = raw.decode("utf-8", "replace").rstrip("\n")
            if line.startswith("event: "):
                ev = line[7:]
            elif line.startswith("data: ") and ev == "text":
                out.append(json.loads(line[6:]).get("text", ""))
    return "".join(out).strip()


qs = CORPUS["questions"]
jobs = []
for i, q in enumerate(qs):
    decoy = qs[(i + 1) % len(qs)]          # the NEXT question's gold, a real but wrong value
    ans = ROWS[(q["id"], "oracle")]["answer"]
    jobs.append((q, decoy, ans))


def one(q, decoy, ans):
    # Grade the TRUE answer against a DECOY gold: the judge must say WRONG.
    v = run("db1/judge", "QUESTION: %s\nGOLD: %s\nANSWER: %s" % (q["q"], decoy["gold"], ans))
    first = v.strip().splitlines()[0].strip().upper() if v.strip() else "EMPTY"
    return q["id"], decoy["gold"], first.split()[0] if first else "EMPTY", ans


res = []
with cf.ThreadPoolExecutor(max_workers=4) as ex:
    for r in ex.map(lambda j: one(*j), jobs):
        res.append(r)
        print(".", end="", flush=True)
print()
wrong = sum(1 for _, _, v, _ in res if v.startswith("WRONG"))
print("judge said WRONG on %d/%d decoy-gold pairs" % (wrong, len(res)))
for qid, g, v, a in sorted(res):
    if not v.startswith("WRONG"):
        print("  NOT-WRONG %s decoy_gold=%r verdict=%s answer=%r" % (qid, g, v, a[:90]))
