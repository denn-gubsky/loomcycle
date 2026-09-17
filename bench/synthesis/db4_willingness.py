#!/usr/bin/env python3
"""RFC DB-4 — will an agent handed a traversal tool actually use it?

DB-3 measured the MECHANISM: the harness retrieved, so an equal content budget
was enforceable and "the agent declined to call the tool" could not confound the
result. That left the other half untested, and this project has been caught by
that gap before — a diagnosis is not a demonstration.

So: give one agent BOTH tools (Document for graph_recall, Memory for search),
tell it nothing about which to prefer, and record what it reaches for.
"""
import argparse, collections, json, os, subprocess, urllib.request
import concurrent.futures as cf

HERE = os.path.dirname(os.path.abspath(__file__))
ap = argparse.ArgumentParser()
ap.add_argument("--base", default="http://127.0.0.1:8878")
ap.add_argument("--agent", default="db4/agent")
ap.add_argument("--judge", default="locomo/judge")
ap.add_argument("--questions", type=int, default=20)
ap.add_argument("--workers", type=int, default=3)
ap.add_argument("--pg", default="loomcycle_db2")
ap.add_argument("--out", default=os.path.join(HERE, "db4-results.json"))
A = ap.parse_args()
TOK = open("/tmp/db2-token").read().strip()
C = json.load(open(os.path.join(HERE, "corpus-db2.json")))
QS = C["questions"][: A.questions]


def run(agent, prompt):
    body = json.dumps({"agent": agent, "prompt": prompt}).encode()
    req = urllib.request.Request(A.base + "/v1/runs", data=body, headers={
        "Authorization": "Bearer " + TOK, "Content-Type": "application/json",
        "Accept": "text/event-stream"})
    out, run_id = [], ""
    with urllib.request.urlopen(req, timeout=1800) as r:
        ev = None
        for raw in r:
            l = raw.decode("utf-8", "replace").rstrip("\n")
            if l.startswith("event: "):
                ev = l[7:]
            elif l.startswith("data: "):
                if ev == "text":
                    out.append(json.loads(l[6:]).get("text", ""))
                elif ev == "agent":
                    run_id = json.loads(l[6:]).get("run_id", "")
    return "".join(out).strip(), run_id


def tools_used(run_id):
    """Read the tool calls from the store — the SSE frame nests them under
    tool_use, and a wrong field name here would report 'no tools' for an agent
    that used them, which is the exact claim this phase is making."""
    q = ("select convert_from(payload,'UTF8') from events "
         f"where run_id='{run_id}' and type='tool_call' order by seq")
    txt = subprocess.run(["psql", "-d", A.pg, "-tAc", q],
                         capture_output=True, text=True).stdout
    calls = []
    for line in txt.splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            d = json.loads(line).get("tool_use", {})
        except Exception:
            continue
        name = d.get("name", "?")
        op = (d.get("input") or {}).get("op", "")
        calls.append(f"{name}.{op}" if op else name)
    return calls


def judge(q, answer):
    if answer.strip().upper().rstrip(".") == "NOT_FOUND":
        return "NOT_FOUND"
    v, _ = run(A.judge, "QUESTION: %s\nGOLD: %s\nANSWER: %s" % (q["q"], q["gold"], answer))
    first = v.strip().splitlines()[0].strip().upper() if v.strip() else ""
    for t in ("NOT_FOUND", "CORRECT", "WRONG"):
        if first.startswith(t):
            return t
    return "UNPARSED"


def one(q):
    ans, rid = run(A.agent, q["q"])
    calls = tools_used(rid)
    return {"qid": q["id"], "hops": q["hops"], "q": q["q"], "gold": q["gold"],
            "answer": ans, "verdict": judge(q, ans), "tools": calls,
            "used_graph": any("graph_recall" in c for c in calls)}


rows = []
with cf.ThreadPoolExecutor(max_workers=A.workers) as ex:
    for r in ex.map(one, QS):
        rows.append(r)
        print(".", end="", flush=True)
print()
json.dump(rows, open(A.out, "w"), indent=1)

n = len(rows)
graph = sum(1 for r in rows if r["used_graph"])
correct = sum(1 for r in rows if r["verdict"] == "CORRECT")
print(f"\n  questions            {n}")
print(f"  used graph_recall    {graph}/{n}  ({100*graph//max(n,1)}%)   <- DB-4's question")
print(f"  correct              {correct}/{n}  ({100*correct//max(n,1)}%)")
print(f"  mean tool calls/run  {sum(len(r['tools']) for r in rows)/max(n,1):.1f}")
print("\n  what it reached for:")
for name, c in collections.Counter(t for r in rows for t in r["tools"]).most_common(8):
    print(f"    {c:>3}x  {name}")
print("wrote", A.out)
