#!/usr/bin/env python3
"""Do the retrieved-set statistics separate answerable from unanswerable?

POSITIVE = question searched against its OWN conversation's store.
NEGATIVE = same question against the OTHER conversation's store.
"""
import json, math, sys
from statistics import mean, pstdev

rows = []
for path, store in ((sys.argv[1], "conv26store"), (sys.argv[2], "conv30store")):
    for line in open(path):
        r = json.loads(line)
        own = (r["qset"] == "conv26" and store == "conv26store") or \
              (r["qset"] == "conv30" and store == "conv30store")
        r["label"] = 1 if own else 0
        rows.append(r)

def entropy_pplx(v, temp=None):
    if not v: return 0.0
    if temp:                                  # softmax with temperature
        m = max(v); e = [math.exp((x - m) / temp) for x in v]
    else:                                     # normalise the raw positive scores
        e = [max(x, 1e-12) for x in v]
    t = sum(e); p = [x / t for x in e]
    h = -sum(x * math.log(x) for x in p if x > 0)
    return math.exp(h)

def signals(r):
    s, k = r["scores"], r["ranks"]
    if not s: return None
    out = {
        "top1":        max(s),
        "mean_k":      mean(s),
        "std_k":       pstdev(s),
        "nqc":         pstdev(s) / mean(s) if mean(s) else 0,
        "gap_top1_mean": max(s) - mean(s),
        "gap_top1_top5": max(s) - mean(sorted(s, reverse=True)[:5]),
        "pplx_raw":    entropy_pplx(s),
        "pplx_T0.01":  entropy_pplx(s, 0.01),
        "pplx_T0.05":  entropy_pplx(s, 0.05),
        "rank_top1":   max(k),
        "rank_nqc":    pstdev(k) / mean(k) if mean(k) else 0,
        "rank_pplx":   entropy_pplx(k),
    }
    return out

for r in rows:
    r["sig"] = signals(r)
rows = [r for r in rows if r["sig"]]
pos = [r for r in rows if r["label"] == 1]
neg = [r for r in rows if r["label"] == 0]
print(f"positives={len(pos)}  negatives={len(neg)}\n")

def auc(p, n):
    """Mann-Whitney U / (|p||n|) — P(random positive ranks above random negative)."""
    allv = sorted([(v, 1) for v in p] + [(v, 0) for v in n])
    ranks, i = {}, 0
    rk = [0.0] * len(allv)
    while i < len(allv):
        j = i
        while j + 1 < len(allv) and allv[j + 1][0] == allv[i][0]: j += 1
        avg = (i + j) / 2 + 1
        for t in range(i, j + 1): rk[t] = avg
        i = j + 1
    rsum = sum(rk[t] for t in range(len(allv)) if allv[t][1] == 1)
    u = rsum - len(p) * (len(p) + 1) / 2
    return u / (len(p) * len(n))

names = list(pos[0]["sig"].keys())
print(f"{'signal':<16}{'AUC':>7}{'|AUC-.5|':>10}   {'mean pos':>10}{'mean neg':>10}")
print("-" * 56)
res = []
for nm in names:
    p = [r["sig"][nm] for r in pos]; n = [r["sig"][nm] for r in neg]
    a = auc(p, n)
    res.append((abs(a - .5), nm, a, mean(p), mean(n)))
for d, nm, a, mp, mn in sorted(res, reverse=True):
    print(f"{nm:<16}{a:>7.3f}{d:>10.3f}   {mp:>10.4f}{mn:>10.4f}")

# The decision is the operating point, not the AUC: a gate that eats the
# answerable slice is useless however well it ranks.
print("\noperating point — threshold keeping >=95% of POSITIVES, what % of NEGATIVES blocked:")
best = sorted(res, reverse=True)[:4]
for d, nm, a, mp, mn in best:
    p = sorted(r["sig"][nm] for r in pos); n = [r["sig"][nm] for r in neg]
    hi = a > .5                     # higher means answerable?
    thr = p[int(0.05 * len(p))] if hi else p[int(0.95 * len(p))]
    blocked = sum(1 for v in n if (v < thr) if hi) if hi else sum(1 for v in n if v > thr)
    kept = sum(1 for v in p if (v >= thr) if hi) if hi else sum(1 for v in p if v <= thr)
    print(f"  {nm:<16} thr={thr:.4f}  keeps {kept/len(p):6.1%} of positives, blocks {blocked/len(n):6.1%} of negatives")
