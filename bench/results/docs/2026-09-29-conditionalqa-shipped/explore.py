"""Post hoc: where does the header's R@5 gain come from? Usage: python3 THIS.py <probe dir>"""
import collections, json, os, sys
sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "../../../docs/measure"))  # analyze.py
from analyze import mcnemar
P = sys.argv[1]
docs = {d["id"]: d for d in map(json.loads, open(os.path.join(P, "docs.jsonl")))}
qs = {q["qid"]: q for q in map(json.loads, open(os.path.join(P, "questions.jsonl")))}
maps = {s: json.load(open(os.path.join(P, "chunks-%s.json" % s))) for s in ("plain", "header")}
has = {s: {c for d, ids in maps[s].items() for i, c in enumerate(ids) if docs[d]["sections"][i]["text"].strip()} for s in maps}
res = {a: {r["qid"]: r for r in map(json.loads, open(os.path.join(P, "results", a + ".jsonl")))} for a in ("plain", "header")}
def hit(a, q):
    g = {maps[a][qs[q]["doc_id"]][i] for i in qs[q]["gold"]}
    return any(c in g for c in [c for c in res[a][q]["ranked"] if c in has[a]][:5])
def page_hit(a, q):  # right PAGE among the top 5
    where = {c: d for d, ids in maps[a].items() for c in ids}
    return any(where.get(c) == qs[q]["doc_id"] for c in [c for c in res[a][q]["ranked"] if c in has[a]][:5])
own = collections.Counter(s["path"][-1] for d in docs.values() for s in d["sections"][1:])
common = {h for h, n in own.items() if n >= 20}
def generic(q):
    d = docs[qs[q]["doc_id"]]
    return any(i > 0 and d["sections"][i]["path"][-1] in common for i in qs[q]["gold"])
for name, sel in (("gold under a heading used on >=20 pages", True), ("gold under a rarer heading", False)):
    qq = [q for q in qs if generic(q) == sel]
    hp, hh = [hit("plain", q) for q in qq], [hit("header", q) for q in qq]
    print("%-42s n=%4d plain %.3f header %.3f  %s" % (name, len(qq), sum(hp) / len(qq), sum(hh) / len(qq), mcnemar(hh, hp)))
allq = sorted(qs)
print("right page in top 5: plain %.3f header %.3f" % (sum(page_hit("plain", q) for q in allq) / len(allq), sum(page_hit("header", q) for q in allq) / len(allq)))
