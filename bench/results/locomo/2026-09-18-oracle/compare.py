#!/usr/bin/env python3
"""Per-question comparison of a store arm against its reading-ceiling oracle.

The headline accuracies say how far apart the arms are; they do not say whether
the gap is reachable. This splits it:

  WON   store wrong -> oracle right   = retrieval headroom. What L2/L3 can win.
  LOST  store right -> oracle wrong   = distraction the wider prompt introduced.
  FLOOR store wrong -> oracle wrong   = the reading floor. NO retrieval arrangement
                                        reaches these, because the reader fails with
                                        the answer already in front of it.

⚠️ Joined on QUESTION TEXT, not list position. Two arms can drop different
questions (an unparsed verdict, a dropped category), and a positional join would
silently pair question i of one arm with question j of the other.
"""
import json, sys
from collections import Counter

SCORE = {'correct': 1.0, 'partial': 0.5, 'wrong': 0.0}

def load(p):
    d = json.load(open(p + '/answer-report.json'))
    out = {}
    for r in d['results']:
        v = r.get('verdict')
        if v not in SCORE:      # unparsed: the judge malfunctioned, not the reader
            continue
        out[r['question']] = (SCORE[v], r.get('not_found'), r.get('category'), v)
    return d, out

sd, s = load(sys.argv[1])
od, o = load(sys.argv[2])
label_s, label_o = sys.argv[3], sys.argv[4]

common = [q for q in s if q in o]
won   = [q for q in common if s[q][0] < o[q][0]]
lost  = [q for q in common if s[q][0] > o[q][0]]
floor = [q for q in common if s[q][0] == 0.0 and o[q][0] == 0.0]

print(f"{label_s}  vs  {label_o}")
print(f"  graded in common: {len(common)}")
print(f"  store acc {sum(s[q][0] for q in common)/len(common):.4f}   "
      f"oracle acc {sum(o[q][0] for q in common)/len(common):.4f}")
print(f"  WON  (retrieval headroom) : {len(won)}")
print(f"  LOST (wider prompt hurt)  : {len(lost)}")
print(f"  FLOOR(reading floor)      : {len(floor)}")

# McNemar exact on the discordant pairs, treating partial as its own half-step is
# not binomial-safe, so binarise on "scored anything" the way the arms are compared.
b, c = len(won), len(lost)
if b + c:
    from math import comb
    n = b + c
    k = min(b, c)
    p = sum(comb(n, i) for i in range(k + 1)) / 2 ** n * 2
    print(f"  McNemar exact: +{b}/-{c}, p = {min(p,1.0):.3g}")

print("  by category (won/lost/floor):")
for cat, name in ((1,'multi-hop'),(2,'temporal'),(3,'open-domain'),(4,'single-hop')):
    w=sum(1 for q in won if s[q][2]==cat); l=sum(1 for q in lost if s[q][2]==cat)
    f=sum(1 for q in floor if s[q][2]==cat)
    tot=sum(1 for q in common if s[q][2]==cat)
    print(f"    {name:12s} n={tot:3d}  won={w:3d} lost={l:3d} floor={f:3d}")

nf = [q for q in common if o[q][1]]
print(f"  oracle still abstains on {len(nf)} of {len(common)}"
      f"  {Counter(s[q][2] for q in nf)}")
