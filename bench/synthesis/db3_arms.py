#!/usr/bin/env python3
"""RFC DB-3 — the five arms, at one content budget, paired.

  oracle     the question's true supports          — the ceiling
  nomem      nothing                               — the parametric control
  single     vector search in rank order           — today's retrieval, the baseline P5 must beat
  traversal  a few seeds, expanded along relations — P5's mechanism
  shuffled   the same expansion over rewired edges — the control that decides what a win MEANS

Every retrieval arm is capped on CONTENT handed to the answerer, never on rows
retrieved (section 6): traversal fetches more rows than single-hop by
construction, and an arm that wins on volume is the trap this measures around.
The cap and the actual fill are reported, and an arm that cannot fill its budget
says so.

Grading: abstention is classified in code, never by the judge — whether the
answerer refused is a fact about the string, and DB-3 reports abstention as its
own signal. An EMPTY reply is an instrument fault, not a verdict: the answerer
is a hybrid thinking model and a tight max_tokens truncates to "" rather than to
something short.
"""
import argparse, collections, json, os, re, sys, urllib.request
import concurrent.futures as cf
from math import comb

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import db3_retrieval as R

ap = argparse.ArgumentParser()
ap.add_argument("--base", default=os.environ.get("DB_BASE", "http://127.0.0.1:8874"))
ap.add_argument("--corpus", default=os.path.join(HERE, "corpus-db2.json"))
ap.add_argument("--token-file", default="/tmp/db2-token")
ap.add_argument("--arms", default="oracle,nomem,single,traversal,shuffled")
ap.add_argument("--budget-chars", type=int, default=1200)
ap.add_argument("--seeds", type=int, default=5, help="seed facts the traversal arms expand from")
ap.add_argument("--depth", type=int, default=3)
ap.add_argument("--shuffle-seed", type=int, default=11)
ap.add_argument("--replicates", type=int, default=2)
ap.add_argument("--questions", type=int, default=0, help="0 = all")
ap.add_argument("--workers", type=int, default=6)
ap.add_argument("--answerer", default="locomo/answerer")
ap.add_argument("--judge", default="locomo/judge")
ap.add_argument("--out", default=os.path.join(HERE, "db3-results.json"))
A = ap.parse_args()

TOKEN = open(A.token_file).read().strip()
CORPUS = json.load(open(A.corpus))
FACT = {f["id"]: f for f in CORPUS["facts"]}
QS = CORPUS["questions"][: A.questions] if A.questions else CORPUS["questions"]
ARMS = [a.strip() for a in A.arms.split(",") if a.strip()]


def run(agent, prompt):
    body = json.dumps({"agent": agent, "prompt": prompt}).encode()
    req = urllib.request.Request(A.base + "/v1/runs", data=body, headers={
        "Authorization": "Bearer " + TOKEN, "Content-Type": "application/json",
        "Accept": "text/event-stream"})
    out, stop = [], ""
    with urllib.request.urlopen(req, timeout=600) as r:
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
                    raise RuntimeError("run error: " + line[6:][:200])
    return "".join(out).strip(), stop


def ask(agent, prompt):
    last = ""
    for _ in range(2):
        text, stop = run(agent, prompt)
        if text:
            return text
        last = stop
    raise RuntimeError("empty reply after 2 attempts (stop_reason=%s)" % last)


def judge(q, answer):
    if answer.strip().upper().rstrip(".") == "NOT_FOUND":
        return "NOT_FOUND", "(classified in code)"
    v = ask(A.judge, "QUESTION: %s\nGOLD: %s\nANSWER: %s" % (q["q"], q["gold"], answer))
    first = v.strip().splitlines()[0].strip().upper()
    for tag in ("NOT_FOUND", "CORRECT", "WRONG"):
        if first.startswith(tag):
            return tag, v
    return "UNPARSED", v


# ---------------------------------------------------------------- retrieval
GRAPH = R.Graph(R.busiest_scope_schema())
SHUF = GRAPH.shuffled(A.shuffle_seed)
# One search per question, shared by every arm and replicate: it is identical
# across them, and re-running it would add latency and a chance of drift.
SEEDS = {}


def seeds_for(q):
    if q["id"] not in SEEDS:
        SEEDS[q["id"]] = R.search(A.base, TOKEN, q["q"], top_k=50)
    return SEEDS[q["id"]]


def facts_for(arm, q):
    if arm == "nomem":
        return [], 0
    if arm == "oracle":
        return [FACT[s]["text"] for s in q["supports"]], -1
    s = seeds_for(q)
    if arm == "single":
        return R.fill(GRAPH, s, A.budget_chars)
    if arm == "traversal":
        return R.fill(GRAPH, GRAPH.expand(s[: A.seeds], A.depth), A.budget_chars)
    if arm == "shuffled":
        return R.fill(SHUF, SHUF.expand(s[: A.seeds], A.depth), A.budget_chars)
    raise SystemExit("unknown arm %r" % arm)


def one(arm, q, rep):
    facts, used = facts_for(arm, q)
    if facts:
        prompt = "FACTS:\n%s\n\nQUESTION: %s" % ("\n".join("- " + f for f in facts), q["q"])
    else:
        prompt = "QUESTION: %s" % q["q"]
    ans = ask(A.answerer, prompt)
    verdict, raw = judge(q, ans)
    want = {FACT[s]["text"].rstrip(".").lower() for s in q["supports"]}
    got = {f.rstrip(".").lower() for f in facts}
    return {"arm": arm, "qid": q["id"], "hops": q["hops"], "rep": rep,
            "q": q["q"], "gold": q["gold"], "answer": ans, "verdict": verdict,
            "n_facts": len(facts), "chars": used,
            "support_coverage": len(want & got) / len(want),
            "judge_raw": raw}


jobs = [(arm, q, rep) for arm in ARMS for q in QS for rep in range(A.replicates)]
# Warm the search cache serially: the workers share it and a race would issue the
# same query many times over.
for q in QS:
    if any(a in ARMS for a in ("single", "traversal", "shuffled")):
        seeds_for(q)

rows = []
with cf.ThreadPoolExecutor(max_workers=A.workers) as ex:
    futs = {ex.submit(one, *j): j for j in jobs}
    for f in cf.as_completed(futs):
        arm, q, rep = futs[f]
        try:
            rows.append(f.result())
        except Exception as e:
            rows.append({"arm": arm, "qid": q["id"], "hops": q["hops"], "rep": rep,
                         "verdict": "ERROR", "answer": "", "n_facts": 0, "chars": 0,
                         "support_coverage": 0.0, "judge_raw": repr(e)[:200]})
        print(".", end="", flush=True)
print()

json.dump({"config": vars(A), "rows": rows}, open(A.out, "w"), indent=1)


def acc(rs):
    graded = [r for r in rs if r["verdict"] in ("CORRECT", "WRONG", "NOT_FOUND")]
    if not graded:
        return 0.0, 0, 0
    c = sum(1 for r in graded if r["verdict"] == "CORRECT")
    return c / len(graded), c, len(graded)


print("\nbudget %d chars | seeds %d | depth %d | %d questions x %d replicates"
      % (A.budget_chars, A.seeds, A.depth, len(QS), A.replicates))
print("\n%-10s %8s %7s %8s %9s %7s %7s" %
      ("arm", "accuracy", "correct", "abstain", "coverage", "facts", "chars"))
for arm in ARMS:
    rs = [r for r in rows if r["arm"] == arm]
    if not rs:
        continue
    a, c, n = acc(rs)
    ab = sum(1 for r in rs if r["verdict"] == "NOT_FOUND") / len(rs)
    cov = sum(r["support_coverage"] for r in rs) / len(rs)
    nf = sum(r["n_facts"] for r in rs) / len(rs)
    ch = sum(max(r["chars"], 0) for r in rs) / len(rs)
    flag = "" if arm in ("oracle", "nomem") or ch >= 0.9 * A.budget_chars else "  <- cannot fill its budget"
    print("%-10s %7.1f%% %4d/%-4d %7.1f%% %8.2f %7.1f %7.0f%s" %
          (arm, 100 * a, c, n, 100 * ab, cov, nf, ch, flag))

for h in sorted({r["hops"] for r in rows}):
    line = "  %d-hop " % h
    for arm in ARMS:
        rs = [r for r in rows if r["arm"] == arm and r["hops"] == h]
        if rs:
            line += " %s %.0f%%" % (arm[:4], 100 * acc(rs)[0])
    print(line)

# McNemar against single-hop, paired on (question, replicate).
faults = [r for r in rows if r["verdict"] in ("ERROR", "UNPARSED")]
if "single" in ARMS:
    print()
    base = {(r["qid"], r["rep"]): r["verdict"] == "CORRECT"
            for r in rows if r["arm"] == "single"}
    for arm in ARMS:
        if arm == "single":
            continue
        other = {(r["qid"], r["rep"]): r["verdict"] == "CORRECT"
                 for r in rows if r["arm"] == arm}
        keys = set(base) & set(other)
        b = sum(1 for k in keys if other[k] and not base[k])
        c = sum(1 for k in keys if base[k] and not other[k])
        n = b + c
        p = min(1.0, sum(comb(n, k) for k in range(0, min(b, c) + 1)) / 2 ** n * 2) if n else 1.0
        print("%-10s vs single: +%d / -%d discordant -> two-sided exact p = %.6f" % (arm, b, c, p))
if faults:
    print("\nINSTRUMENT FAULTS: %d" % len(faults))
print("wrote", A.out)
