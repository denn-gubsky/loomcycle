#!/usr/bin/env python3
"""RFC DB-1 feasibility runner: oracle vs no-memory arm over the synthesis corpus.

No store and no memory by design -- the oracle arm puts the supporting facts in
the prompt and the control arm gives nothing, so the two arms can only differ by
the facts.
"""
import argparse, json, os, sys, urllib.request, concurrent.futures as cf

HERE = os.path.dirname(os.path.abspath(__file__))
ap = argparse.ArgumentParser(description=__doc__)
ap.add_argument("--corpus", default=os.path.join(HERE, "corpus.json"))
ap.add_argument("--out", default=None, help="default: <corpus>-results.json")
ap.add_argument("--workers", type=int, default=4)
ARGS = ap.parse_args()

BASE = os.environ.get("DB_BASE", "http://127.0.0.1:8873")
TOKEN = os.environ["LOOMCYCLE_AUTH_TOKEN"]          # never printed
CORPUS = json.load(open(ARGS.corpus))
FACTS = {f["id"]: f for f in CORPUS["facts"]}


def run_once(agent, prompt):
    body = json.dumps({"agent": agent, "prompt": prompt}).encode()
    req = urllib.request.Request(
        BASE + "/v1/runs", data=body,
        headers={"Authorization": "Bearer " + TOKEN,
                 "Content-Type": "application/json",
                 "Accept": "text/event-stream"})
    out, stop = [], ""
    with urllib.request.urlopen(req, timeout=300) as r:
        ev = None
        for raw in r:
            line = raw.decode("utf-8", "replace").rstrip("\n")
            if line.startswith("event: "):
                ev = line[7:]
            elif line.startswith("data: "):
                if ev == "text":
                    out.append(json.loads(line[6:]).get("text", ""))
                elif ev == "done":
                    stop = json.loads(line[6:]).get("stop_reason", "")
                elif ev == "error":
                    raise RuntimeError("run error: " + line[6:][:300])
    return "".join(out).strip(), stop


def run(agent, prompt):
    """An EMPTY reply is an instrument fault, not a verdict.

    deepseek-v4-flash is a hybrid thinking model: the reasoning trace is spent
    before the first visible token, so a tight max_tokens yields an empty string
    rather than a short answer. Surface it instead of bucketing it as 'other'.
    """
    last = ""
    for _ in range(2):
        text, stop = run_once(agent, prompt)
        if text:
            return text, stop
        last = stop
    raise RuntimeError("empty reply after 2 attempts (stop_reason=%s)" % last)


def ask(q, arm):
    if arm == "oracle":
        facts = "\n".join("- " + FACTS[fid]["text"] for fid in q["supports"])
        prompt = "FACTS:\n%s\n\nQUESTION: %s" % (facts, q["q"])
    else:
        prompt = "QUESTION: %s" % q["q"]
    text, stop = run("db1/answerer", prompt)
    return prompt, text, stop


def judge(q, answer):
    """Grade one answer. An ABSTENTION is classified in code, not by the model.

    Whether the answerer refused is a fact about the string, and DB-3 reports
    abstention as its own signal -- so asking a model to re-derive it only adds a
    way to get it wrong. Measured: the judge misfiled 4 of 120 literal NOT_FOUND
    replies as WRONG, which would have understated abstention by 3pp.
    """
    if answer.strip().upper().rstrip(".") == "NOT_FOUND":
        return "NOT_FOUND", "(classified in code: the answer is literally NOT_FOUND)"
    v, _ = run("db1/judge", "QUESTION: %s\nGOLD: %s\nANSWER: %s" % (q["q"], q["gold"], answer))
    first = v.strip().splitlines()[0].strip().upper()
    for tag in ("NOT_FOUND", "CORRECT", "WRONG"):
        if first.startswith(tag):
            return tag, v
    return "UNPARSED", v


def one(q, arm):
    prompt, ans, stop = ask(q, arm)
    verdict, raw = judge(q, ans)
    return {"qid": q["id"], "hops": q["hops"], "arm": arm, "q": q["q"],
            "gold": q["gold"], "answer": ans, "stop_reason": stop,
            "verdict": verdict, "judge_raw": raw}


jobs = [(q, arm) for q in CORPUS["questions"] for arm in ("oracle", "nomem")]
rows = []
with cf.ThreadPoolExecutor(max_workers=ARGS.workers) as ex:
    futs = {ex.submit(one, q, arm): (q["id"], arm) for q, arm in jobs}
    for f in cf.as_completed(futs):
        qid, arm = futs[f]
        try:
            rows.append(f.result())
        except Exception as e:
            rows.append({"qid": qid, "arm": arm, "hops": 0, "verdict": "ERROR",
                         "answer": "", "judge_raw": repr(e)[:300]})
        print(".", end="", flush=True)
print()

rows.sort(key=lambda r: (int(r["qid"][1:]), r["arm"]))
out = ARGS.out or (os.path.splitext(ARGS.corpus)[0] + "-results.json")
json.dump(rows, open(out, "w"), indent=1)

bad = [r for r in rows if r["verdict"] in ("UNPARSED", "ERROR")]
for arm in ("oracle", "nomem"):
    a = [r for r in rows if r["arm"] == arm]
    n = len(a)
    c = sum(1 for r in a if r["verdict"] == "CORRECT")
    w = sum(1 for r in a if r["verdict"] == "WRONG")
    nf = sum(1 for r in a if r["verdict"] == "NOT_FOUND")
    print("%-7s n=%2d  CORRECT=%2d (%.0f%%)  WRONG=%2d  NOT_FOUND=%2d  faults=%d"
          % (arm, n, c, 100.0 * c / n if n else 0, w, nf, n - c - w - nf))
for h in sorted({r.get("hops", 0) for r in rows} - {0}):
    for arm in ("oracle", "nomem"):
        a = [r for r in rows if r["arm"] == arm and r.get("hops") == h]
        if a:
            print("  %d-hop %-7s %d/%d" % (h, arm, sum(1 for r in a if r["verdict"] == "CORRECT"), len(a)))
if bad:
    print("INSTRUMENT FAULTS: %d" % len(bad))
    for r in bad:
        print("  %s/%s %s %s" % (r["qid"], r["arm"], r["verdict"], repr(r["judge_raw"])[:160]))
print("wrote", out)
