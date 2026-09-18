#!/usr/bin/env python3
"""L7: freed-abstention QUALITY.

Every lever in the plan lowers abstention by handing over evidence. The number
that distinguishes "braver" from "better" is what the reader produces on the
questions it previously declined — so of the store arm's NOT_FOUNDs, grade what
the oracle said.

⚠️ A reader that answers more and is wrong more is a REGRESSION on LongMemEval,
where the abstention slice scores the inverse. This is the number that catches it.
"""
import json, sys
from collections import Counter

def load(p):
    d = json.load(open(p + '/answer-report.json'))
    return {r['question']: r for r in d['results']}

s, o = load(sys.argv[1]), load(sys.argv[2])
label = sys.argv[3]

store_nf = [q for q in s if s[q].get('not_found')]
common   = [q for q in store_nf if q in o]
freed    = [q for q in common if not o[q].get('not_found')]
held     = [q for q in common if o[q].get('not_found')]

v = Counter(o[q].get('verdict') for q in freed)
tot = sum(v[k] for k in ('correct','partial','wrong'))
acc = (v['correct'] + 0.5*v['partial']) / tot if tot else 0.0

print(f"{label}")
print(f"  store abstained on {len(store_nf)} of {len(s)}; {len(common)} gradable in both")
print(f"  FREED by evidence : {len(freed)}   still abstains: {len(held)}")
print(f"    correct {v['correct']}  partial {v['partial']}  wrong {v['wrong']}"
      f"  unparsed {v['unparsed']}")
print(f"    quality of the freed answers: {acc:.4f}")
print(f"    -> {'BETTER' if acc >= 0.5 else 'BRAVER, NOT BETTER'}"
      f" (a coin would be 0.5 on a binary call; below it the abstention was right)")
