#!/usr/bin/env python3
"""Wide-sample analysis: is a SECOND score worth anything beside the first?

Two corpora, two ways of being unanswerable:

  longmemeval  label = the `_abs` flag           30 negatives (the whole oracle corpus)
  locomo       label = category 5 (adversarial)  444 negatives, minimal-pair against
                                                 the answerable ones ("what did CAROLINE
                                                 realize after HER race" against "what did
                                                 MELANIE realize after the race")

Three questions, in order of what they decide:

  1. Does any signal separate the classes at all, and with what confidence?
  2. Are top1 and perplexity ORTHOGONAL? (asserted, so measure it)
  3. Does a two-feature model beat top1 alone OUT OF FOLD? That is the only form of
     "two scores are better than one" that cannot be manufactured by overfitting.

Usage: analyze_wide.py <dump.jsonl> [longmemeval|locomo]
"""
import json, math, sys
import numpy as np

SIGNALS = ["top1", "mean_k", "std_k", "nqc", "gap_top1_mean",
           "pplx_t05", "pplx_t01", "pplx_raw"]


def auc(pos, neg):
    """P(random positive > random negative), ties at half."""
    p, n = np.asarray(pos, float), np.asarray(neg, float)
    allv = np.concatenate([p, n])
    order = allv.argsort()
    ranks = np.empty(len(allv), float)
    ranks[order] = np.arange(1, len(allv) + 1)
    # average ranks over ties
    uniq, inv, cnt = np.unique(allv, return_inverse=True, return_counts=True)
    sums = np.zeros(len(uniq))
    np.add.at(sums, inv, ranks)
    ranks = (sums / cnt)[inv]
    return (ranks[:len(p)].sum() - len(p) * (len(p) + 1) / 2) / (len(p) * len(n))


def se_auc(a, n1, n2):
    """Hanley-McNeil standard error — the honest width on an unbalanced sample."""
    q1 = a / (2 - a)
    q2 = 2 * a * a / (1 + a)
    return math.sqrt((a * (1 - a) + (n1 - 1) * (q1 - a * a) + (n2 - 1) * (q2 - a * a)) / (n1 * n2))


def logit_fit(X, y, iters=600, lr=0.5, l2=1e-3):
    """Plain logistic regression. Standardised inputs, so the L2 is comparable
    across features and a feature cannot win by having a bigger unit."""
    X = np.column_stack([np.ones(len(X)), X])
    w = np.zeros(X.shape[1])
    for _ in range(iters):
        p = 1 / (1 + np.exp(-X @ w))
        g = X.T @ (p - y) / len(y) + l2 * np.r_[0, w[1:]]
        w -= lr * g * 10
    return w


def logit_score(X, w):
    return np.column_stack([np.ones(len(X)), X]) @ w


def cv_auc(feats, y, folds=5, seed=0):
    """Out-of-fold AUC. In-sample AUC of a fitted model is not evidence."""
    rng = np.random.default_rng(seed)
    idx = rng.permutation(len(y))
    oof = np.zeros(len(y))
    for f in range(folds):
        te = idx[f::folds]
        tr = np.setdiff1d(idx, te)
        mu, sd = feats[tr].mean(0), feats[tr].std(0) + 1e-9
        w = logit_fit((feats[tr] - mu) / sd, y[tr])
        oof[te] = logit_score((feats[te] - mu) / sd, w)
    return auc(oof[y == 1], oof[y == 0])


def main(path, kind):
    rows = [json.loads(l) for l in open(path) if l.strip()]
    rows = [r for r in rows if r["found"] > 0 and not r.get("error")]
    if kind == "locomo":
        neg = lambda r: r["category"] == 5
    else:
        neg = lambda r: r["abstain"]
    ans = [r for r in rows if not neg(r)]
    abst = [r for r in rows if neg(r)]
    print(f"{kind}: answerable={len(ans)}  unanswerable={len(abst)}\n")
    if len(abst) < 5 or len(ans) < 5:
        print("one class too small")
        return

    print(f"{'signal':<15}{'AUC':>7}{'95% CI':>18}{'mean ans':>12}{'mean neg':>12}")
    print("-" * 64)
    res = []
    for nm in SIGNALS:
        p_ = [r[nm] for r in ans]
        n_ = [r[nm] for r in abst]
        a = auc(p_, n_)
        s = se_auc(max(a, 1 - a), len(p_), len(n_))
        res.append((abs(a - .5), nm, a, s))
        lo, hi = a - 1.96 * s, a + 1.96 * s
        sig = "" if lo <= .5 <= hi else "  *"
        print(f"{nm:<15}{a:>7.3f}{f'{lo:.3f}..{hi:.3f}':>18}"
              f"{np.mean(p_):>12.4f}{np.mean(n_):>12.4f}{sig}")
    print("  * = CI excludes 0.5")

    t = np.array([r["top1"] for r in rows])
    q = np.array([r["pplx_t05"] for r in rows])
    y = np.array([0 if neg(r) else 1 for r in rows])
    print(f"\nORTHOGONALITY — correlation top1 <-> perplexity")
    print(f"  overall r = {np.corrcoef(t, q)[0,1]:+.3f}   "
          f"answerable {np.corrcoef(t[y==1], q[y==1])[0,1]:+.3f}   "
          f"unanswerable {np.corrcoef(t[y==0], q[y==0])[0,1]:+.3f}")
    print("  (orthogonal would be ~0.00; shared variance = r^2 = "
          f"{np.corrcoef(t,q)[0,1]**2:.0%})")

    print("\nDOES A SECOND SCORE HELP? — out-of-fold AUC, 5-fold, 5 seeds")
    combos = {"top1 alone": np.column_stack([t]),
              "perplexity alone": np.column_stack([q]),
              "top1 + perplexity": np.column_stack([t, q]),
              "top1 + pplx + std_k": np.column_stack(
                  [t, q, np.array([r["std_k"] for r in rows])])}
    base = None
    for name, F in combos.items():
        vals = [cv_auc(F, y, seed=s) for s in range(5)]
        m, sd = float(np.mean(vals)), float(np.std(vals))
        if name == "top1 alone":
            base = m
        delta = "" if base is None or name == "top1 alone" else f"   {m-base:+.3f} vs top1"
        print(f"  {name:<22} AUC {m:.3f} ± {sd:.3f}{delta}")


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2] if len(sys.argv) > 2 else "longmemeval")
