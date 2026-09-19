#!/usr/bin/env python3
"""The residue: questions the best arm still gets wrong, and WHY.

Four buckets, because they have different fixes and lumping them hides that:

  UNREACHABLE  the reading ceiling (oracle) also fails it. No retrieval arrangement
               helps — this is the reader, or the question, or the gold.
               ⚠️ On LoCoMo the oracle renders only the ANNOTATED evidence, which is
               off by one, so this bucket over-counts: some of it is reachable by
               real retrieval and the oracle simply never saw the turn.
  RETRIEVAL    the oracle gets it, the arm answers something WRONG. The evidence
               exists and was not found. This is what more retrieval work can win.
  VAGUE        the oracle gets it, the arm answers PARTIALLY — right area, missing
               the specific ("from her home country" for "Sweden"). The evidence was
               found; the answer dropped the detail. A prompt/answering fix, not a
               retrieval one, and cheap if it is a real share of the residue.
  ABSTAINED    the arm declined. Separated from `wrong` because declining and
               confabulating are different failures with different costs.
  REGRESSED    the control got it and the arm lost it.
"""
import json, sys
from collections import Counter

S = {'correct': 1.0, 'partial': 0.5, 'wrong': 0.0}
CAT = {1: 'multi-hop', 2: 'temporal', 3: 'open-domain', 4: 'single-hop'}

def load(p):
    d = json.load(open(p + '/answer-report.json'))
    return {r['question']: r for r in d['results']}

ctl, arm, orc = load(sys.argv[1]), load(sys.argv[2]), load(sys.argv[3])
label = sys.argv[4]

buckets = {'UNREACHABLE': [], 'RETRIEVAL': [], 'VAGUE': [], 'ABSTAINED': [], 'REGRESSED': []}
graded = 0
for q, r in arm.items():
    v = r.get('verdict')
    if v not in S:
        continue
    graded += 1
    if S[v] == 1.0:
        continue                      # fully correct, not residue
    o = orc.get(q, {}).get('verdict')
    c = ctl.get(q, {}).get('verdict')
    row = (q, r.get('gold'), r.get('answer'), CAT.get(r.get('category'), '?'), v, o)
    if c in S and S[c] > S[v]:
        buckets['REGRESSED'].append(row)
    elif r.get('not_found'):
        buckets['ABSTAINED'].append(row)
    elif o in S and S[o] == 1.0:
        # ⚠️ PARTIAL AND WRONG ARE DIFFERENT FAILURES WITH DIFFERENT FIXES.
        # "Where did Caroline move from?" answered "from her home country" when the
        # gold is "Sweden" is not a retrieval miss — the evidence was found and the
        # SPECIFIC was dropped. That is an answering/prompt problem. A wrong answer
        # on a question the oracle gets is the genuine retrieval miss.
        buckets['VAGUE' if v == 'partial' else 'RETRIEVAL'].append(row)
    else:
        buckets['UNREACHABLE'].append(row)

tot = sum(len(v) for v in buckets.values())
print(f"{label}: {tot} of {graded} graded are residue ({tot/graded*100:.0f}%)\n")
for name in ('RETRIEVAL', 'VAGUE', 'ABSTAINED', 'UNREACHABLE', 'REGRESSED'):
    rows = buckets[name]
    cats = Counter(r[3] for r in rows)
    print(f"  {name:12s} {len(rows):3d}  {dict(cats)}")
print()
for name in ('RETRIEVAL', 'VAGUE', 'ABSTAINED'):
    if not buckets[name]:
        continue
    print(f"--- {name} (the actionable ones) ---")
    for q, gold, ans, cat, v, o in buckets[name][:6]:
        print(f"  [{cat}] {q[:72]}")
        print(f"      gold: {str(gold)[:66]}")
        print(f"      got : {str(ans)[:66]}  ({v}; oracle={o})")
    print()
