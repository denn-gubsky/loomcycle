#!/usr/bin/env python3
"""Stage 2: does the retrieved set's SHAPE predict a REAL `_abs` question?

Stage 1 (LoCoMo cross-store) could not answer this: its negatives were off-topic, so
raw cosine separated the classes perfectly and the instrument saturated. LongMemEval's
`_abs` instances are the real test — on-topic by construction, the answer simply absent.

Criterion, fixed BEFORE the run:
  * a signal is worth building on at AUC >= 0.75
  * AUC 0.5-0.65 is a dead instrument
  * but the DECISION is the operating point: at a threshold keeping >=95% of the
    answerable slice (that is where the +24.5pp lives), what fraction of `_abs`
    attachments does it block?

Usage: analyze_abs.py dump-full.jsonl
"""
import json, math, sys
from statistics import mean

SIGNALS = ["top1", "mean_k", "std_k", "nqc", "gap_top1_mean",
           "pplx_t05", "pplx_t01", "pplx_raw"]


def auc_and_p(pos, neg):
    """AUC = P(random positive > random negative), plus the Mann-Whitney normal-approx p."""
    allv = sorted([(v, 1) for v in pos] + [(v, 0) for v in neg])
    rk, i = [0.0] * len(allv), 0
    ties = []
    while i < len(allv):
        j = i
        while j + 1 < len(allv) and allv[j + 1][0] == allv[i][0]:
            j += 1
        avg = (i + j) / 2 + 1
        for t in range(i, j + 1):
            rk[t] = avg
        ties.append(j - i + 1)
        i = j + 1
    n1, n2 = len(pos), len(neg)
    rsum = sum(rk[t] for t in range(len(allv)) if allv[t][1] == 1)
    u = rsum - n1 * (n1 + 1) / 2
    a = u / (n1 * n2)
    n = n1 + n2
    tie_corr = sum(t ** 3 - t for t in ties)
    var = n1 * n2 / 12 * ((n + 1) - tie_corr / (n * (n - 1)))
    z = (u - n1 * n2 / 2) / math.sqrt(var) if var > 0 else 0.0
    p = math.erfc(abs(z) / math.sqrt(2))
    return a, p


def main(path):
    rows = [json.loads(l) for l in open(path) if l.strip()]
    empty = [r for r in rows if r["found"] == 0]
    errs = [r for r in rows if r.get("error")]
    rows = [r for r in rows if r["found"] > 0 and not r.get("error")]
    ans = [r for r in rows if not r["abstain"]]
    abst = [r for r in rows if r["abstain"]]
    print(f"answerable={len(ans)}  _abs={len(abst)}  "
          f"(dropped {len(empty)} empty-index, {len(errs)} errored)\n")
    if not abst or not ans:
        print("one class is empty — nothing to test")
        return

    print(f"retrieved-set size: answerable mean {mean(r['found'] for r in ans):.1f}, "
          f"_abs mean {mean(r['found'] for r in abst):.1f}\n")

    print(f"{'signal':<15}{'AUC':>7}{'p':>10}{'mean answerable':>18}{'mean _abs':>12}")
    print("-" * 62)
    res = []
    for nm in SIGNALS:
        p_, n_ = [r[nm] for r in ans], [r[nm] for r in abst]
        a, pv = auc_and_p(p_, n_)
        res.append((abs(a - .5), nm, a, pv, mean(p_), mean(n_)))
    for _, nm, a, pv, mp, mn in sorted(res, reverse=True):
        flag = "  <-- over gate" if abs(a - .5) >= .25 else ""
        print(f"{nm:<15}{a:>7.3f}{pv:>10.4f}{mp:>18.4f}{mn:>12.4f}{flag}")

    print("\nOPERATING POINT — keep >=95% of the answerable slice, block how much `_abs`?")
    print("(a gate that eats the answerable slice is useless however well it ranks)")
    for _, nm, a, pv, mp, mn in sorted(res, reverse=True)[:4]:
        p_ = sorted(r[nm] for r in ans)
        n_ = [r[nm] for r in abst]
        higher_is_answerable = a > .5
        if higher_is_answerable:
            thr = p_[max(0, int(0.05 * len(p_)) - 1)]
            kept = sum(1 for v in p_ if v >= thr)
            blocked = sum(1 for v in n_ if v < thr)
        else:
            thr = p_[min(len(p_) - 1, int(0.95 * len(p_)))]
            kept = sum(1 for v in p_ if v <= thr)
            blocked = sum(1 for v in n_ if v > thr)
        print(f"  {nm:<15} thr={thr:>9.4f}  keeps {kept/len(p_):6.1%} answerable, "
              f"blocks {blocked/len(n_):6.1%} of _abs ({blocked}/{len(n_)})")


if __name__ == "__main__":
    main(sys.argv[1])
