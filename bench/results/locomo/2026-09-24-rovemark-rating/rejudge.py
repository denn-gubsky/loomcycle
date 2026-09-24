#!/usr/bin/env python3
"""Re-judge the questions the judge left UNPARSED, with a larger output budget.

WHY. The judge is deepseek-v4-flash, a hybrid reasoning model, and ingest.yaml caps it at
max_tokens 1000. On some questions it spends the whole budget reasoning and stops at
max_tokens with EMPTY text, which ParseVerdict records as "unparsed" — a question the
memory answered but the instrument never graded. Same judge, same system prompt, same
user prompt (answer.go's exact format), only the budget raised to 4000: a verdict that
fit in 1000 would not change, so re-grading only the truncated ones leaves the rest of the
instrument untouched. Each re-graded row keeps verdict_orig and rejudged=true.

Usage: rejudge.py <answer-report.json> [...]
"""
import json, re, sys, urllib.request

URL = "http://127.0.0.1:8801/v1/runs"
TOKEN = "gate-probe-local"  # the bench server's local legacy bearer, not a secret
OBJ = re.compile(r"\{[^{}]*\}")


def judge(q, gold, ans, budget):
    body = {"agent": "locomo/judge", "user_id": "rejudge", "max_tokens": budget,
            "prompt": "Question: %s\nGold answer: %s\nModel answer: %s" % (q, gold, ans)}
    req = urllib.request.Request(URL, json.dumps(body).encode(),
                                 {"Content-Type": "application/json", "Authorization": "Bearer " + TOKEN})
    text, stop = "", None
    with urllib.request.urlopen(req, timeout=300) as r:
        for line in r:
            line = line.decode().strip()
            if not line.startswith("data:"):
                continue
            d = json.loads(line[5:])
            if d.get("type") == "text":
                text += d.get("text", "")
            elif d.get("type") == "done":
                stop = d.get("stop_reason")
    # mirror ParseVerdict: first JSON object whose verdict is one of the three
    for cand in OBJ.findall(text)[:4]:
        try:
            v = json.loads(cand)
        except Exception:
            continue
        verdict = str(v.get("verdict", "")).strip().lower()
        if verdict in ("correct", "partial", "wrong"):
            return verdict, v.get("why", ""), stop
    return "unparsed", text[:160], stop


for path in sys.argv[1:]:
    d = json.load(open(path))
    n = fixed = 0
    for x in d["results"]:
        if x.get("verdict") != "unparsed":
            continue
        x.setdefault("verdict_orig", "unparsed")
        n += 1
        # 4000 first; a reasoning trace that still hits the cap gets one try at 8000
        for budget in (4000, 8000):
            v, why, stop = judge(x["question"], x["gold"], x["answer"], budget)
            if v != "unparsed" or stop != "max_tokens":
                break
        x["rejudged"], x["rejudge_stop"], x["rejudge_budget"] = True, stop, budget
        x["verdict"], x["why"] = v, why
        fixed += v != "unparsed"
    json.dump(d, open(path, "w"), indent=1)
    print(f"{path.split('/')[-2]}: rejudged {n}, now graded {fixed}")
