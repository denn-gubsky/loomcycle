import json, os, sys, collections
sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "../../../docs/units"))  # analyze.py
# usage: python3 THIS.py <probe dir>
from analyze import load
P = sys.argv[1]
qs = {q["qid"]: q for q in map(json.loads, open(os.path.join(P, "questions.jsonl")))}
chunks = json.load(open(os.path.join(P, "chunks.json")))
gold = {qid: {chunks[q["policy_id"]][s] for s in q["gold"]} for qid, q in qs.items()}
h, u = load(P, "header-p1"), load(P, "units-p1")
hit = lambda res, q: any(c in gold[q] for c in res[q]["ranked"][:5])
win = [q for q in qs if hit(u, q) and not hit(h, q)]
loss = [q for q in qs if hit(h, q) and not hit(u, q)]
def gold_kind(q):  # how the gold reached the units top 5
    ks = [k for k, c in zip(u[q]["matched"][:5], u[q]["ranked"][:5]) if c in gold[q]]
    return ks[0] if ks else "-"
def wrong_units(q):  # non-gold top-5 chunks that got there through a unit
    return sum(1 for k, c in zip(u[q]["matched"][:5], u[q]["ranked"][:5]) if k and c not in gold[q])
print("wins by gold route:", collections.Counter(gold_kind(q) for q in win))
print("losses: mean wrong-via-unit in top5 = %.2f" % (sum(map(wrong_units, loss)) / len(loss)))
print("all: mean top5 slots via unit = %.2f" % (sum(sum(1 for k in u[q]["matched"][:5] if k) for q in qs) / len(qs)))
pol = collections.Counter(); 
for q in win: pol[qs[q]["policy_id"]] += 1
for q in loss: pol[qs[q]["policy_id"]] -= 1
print("net by policy:", sorted(pol.items()))
