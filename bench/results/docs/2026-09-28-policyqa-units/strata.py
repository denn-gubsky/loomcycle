import json, os, sys
sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "../../../docs/units"))  # analyze.py
# usage: python3 THIS.py <probe dir>
from analyze import load, mcnemar
P = sys.argv[1]
qs = {q["qid"]: q for q in map(json.loads, open(os.path.join(P, "questions.jsonl")))}
chunks = json.load(open(os.path.join(P, "chunks.json")))
gold = {qid: {chunks[q["policy_id"]][s] for s in q["gold"]} for qid, q in qs.items()}
h, u = load(P, "header-p1"), load(P, "units-p1")
size = {p: len(c) for p, c in chunks.items()}
for lo, hi in ((0, 10), (10, 30), (30, 999)):
    qq = [q for q in qs if lo <= size[qs[q]["policy_id"]] < hi]
    if not qq: continue
    hit = lambda res: [any(c in gold[q] for c in res[q]["ranked"][:5]) for q in qq]
    ch = sum(min(1.0, 5 * len(gold[q]) / size[qs[q]["policy_id"]]) for q in qq) / len(qq)
    pols = sorted({qs[q]["policy_id"] for q in qq})
    print("segments %d-%d: %d policies, %d pairs, chance %.3f, header %.3f, units %.3f, mcnemar %s" % (
        lo, hi - 1, len(pols), len(qq), ch, sum(hit(h)) / len(qq), sum(hit(u)) / len(qq), mcnemar(hit(u), hit(h))))
