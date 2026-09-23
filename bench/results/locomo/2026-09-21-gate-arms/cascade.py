#!/usr/bin/env python3
"""The cascade: cosine decides who gets asked, the verifier decides the rest.

Arm 0 (a threshold on top1) and arm 2 (a model asked whether the material answers the
question) each lost to the other somewhere. The cascade runs the verifier ONLY below a
cosine threshold, and beats both — but not for the reason the saving suggests.

  cosine protects the verifier from its own false positives. The verifier is far too
  eager (it withholds 34 of 96 answerable questions), and that eagerness costs most on
  high-confidence questions, which are exactly the ones a threshold can hand it without
  asking. 16 of 29 unanswerable questions sit in the bottom two cosine deciles, so
  confining the verifier there keeps its recall where the problem actually is.

  the verifier RESCUES rather than confirms. Below the threshold it says "attach" for 12
  of 39, and 9 of those are answerable questions the reader got right — a pure threshold
  would have thrown all 9 away.

Each decider fixes the other's characteristic error, and those cases are enumerable
rather than statistical, which is the part of this that survives a configuration change.

⚠️ What the numbers do NOT establish. On this configuration L2's `_abs` cost is ONE
question of 30, so every strategy here lands inside the noise: the cascade beats L2 on
the answerable slice by +3/-1 (p = 0.63) and on `_abs` by 26 -> 27 correct refusals.
Establishing an effect size needs a configuration where the cost is real.

⚠️ And one deviation, identical for both deciders: each judges the retrieval for the RAW
question, while the reader saw the retrieval for its own mid-run query. Ranking the
deciders is fair; the absolute withhold rates are not what an in-runtime gate produces.

Usage: cascade.py [results-dir]
"""
import json, os, sys
from math import comb

D = sys.argv[1] if len(sys.argv) > 1 else os.path.dirname(os.path.abspath(__file__))
VAL = {"correct": 1.0, "partial": 0.5, "wrong": 0.0}


def load():
    # Two naming generations: bench/results ignores everything but README.md,
    # summary-*.json and *.py, so a results file that is not named summary-* is simply
    # not in the repo. Accept both rather than silently failing on the committed set.
    def pick(*names):
        for n in names:
            if os.path.exists(f"{D}/{n}"):
                return json.load(open(f"{D}/{n}"))
        raise SystemExit(f"none of {names} under {D}")

    shape = {r["question"]: r for r in pick("summary-retrieval-shape.json",
                                            "retrieval-shape.json")["rows"]}
    verd = pick("summary-verifier-verdicts.json", "verifier-verdicts.json")
    ctl = {r["question"]: r for r in pick("summary-arm-control.json",
                                          "arm-control.json")["results"]}
    l2 = {r["question"]: r for r in pick("summary-arm-l2.json", "arm-l2.json")["results"]}
    # only questions present everywhere: a verdict the model never produced is not a
    # "yes", and folding it into either class would invent data.
    qs = [q for q in shape if q in verd and q in ctl and q in l2]
    return shape, verd, ctl, l2, qs


def score(qs, shape, pick):
    """The two slices, never pooled: on `_abs` refusing IS correct, so one slice rewards
    answering and the other punishes it."""
    ans = [q for q in qs if not shape[q]["abstain"]]
    ab = [q for q in qs if shape[q]["abstain"]]
    g = [q for q in ans if pick(q).get("verdict") in VAL]
    return (sum(VAL[pick(q)["verdict"]] for q in g) / len(g) if g else 0.0,
            sum(1 for q in ab if pick(q).get("not_found")) / len(ab) if ab else 0.0)


def paired(qs, shape, ref, pick):
    """Discordant pairs on the answerable slice. The arms share their answerer draws, so
    a difference cannot come from the answerer having a different day."""
    up = dn = 0
    for q in qs:
        if shape[q]["abstain"]:
            continue
        a, b = VAL.get(ref(q).get("verdict"), 0.0), VAL.get(pick(q).get("verdict"), 0.0)
        up += b > a
        dn += b < a
    return up, dn


def sign_p(up, dn):
    """Exact two-sided sign test — the honest p for a handful of discordant pairs."""
    n = up + dn
    if n == 0:
        return 1.0
    k = min(up, dn)
    tail = sum(comb(n, i) for i in range(k + 1)) / 2 ** n
    return min(1.0, 2 * tail)


def main():
    shape, verd, ctl, l2, qs = load()
    n_ab = sum(1 for q in qs if shape[q]["abstain"])
    print(f"{len(qs)} questions ({n_ab} unanswerable, {len(qs)-n_ab} answerable)\n")

    L2, C = (lambda q: l2[q]), (lambda q: ctl[q])

    print("where the unanswerable questions sit in the cosine range:")
    tops = sorted(shape[q]["top1"] for q in qs)
    dec = lambda x: min(9, sum(1 for t in tops if t <= x) * 10 // len(tops))
    row = [sum(1 for q in qs if shape[q]["abstain"] and dec(shape[q]["top1"]) == d)
           for d in range(10)]
    print("  decile 0..9 (low->high top1):", row)
    print(f"  {sum(row[:2])} of {n_ab} in the bottom two deciles; "
          f"{sum(row[5:])} in the top half\n")

    # ⚠️ NO AUTO-SELECTED WINNER. Picking the threshold that maximises one slice lands
    # on a point where the cascade degenerates into the pure threshold (the verifier is
    # asked about so few questions that it rescues nobody), and picking after seeing
    # both slices is in-sample either way. The sweep is the result; the four-way below
    # runs at a threshold NAMED on the command line, and 0.595 is reported in the README
    # because it is where `_abs` recovers fully while the answerable slice stays above
    # L2 — a choice made after seeing this table, not a fitted optimum.
    T = float(os.environ.get("CASCADE_T", "0.595"))
    print("cascade sweep — top1 >= T attaches WITHOUT asking; below T the verifier decides")
    print(f"{'T':>7}{'asked':>8}{'answerable':>12}{'_abs':>8}{'vs L2':>10}{'p':>8}")
    for f in (0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8):
        t = tops[min(len(tops) - 1, int(f * len(tops)))]
        pick = lambda q, t=t: l2[q] if (shape[q]["top1"] >= t or verd[q]) else ctl[q]
        a, b = score(qs, shape, pick)
        u, d = paired(qs, shape, L2, pick)
        asked = sum(1 for q in qs if shape[q]["top1"] < t)
        print(f"{t:>7.3f}{asked:>8}{a:>12.4f}{b:>8.4f}{f'+{u}/-{d}':>10}{sign_p(u,d):>8.3f}")

    pick = lambda q: l2[q] if (shape[q]["top1"] >= T or verd[q]) else ctl[q]
    print(f"\nall four strategies on the SAME questions, threshold {T:.3f} "
          f"(set CASCADE_T to move it):")
    for name, p in (("L2 — always attach", L2),
                    ("pure threshold", lambda q: l2[q] if shape[q]["top1"] >= T else ctl[q]),
                    ("verifier everywhere", lambda q: l2[q] if verd[q] else ctl[q]),
                    ("CASCADE", pick)):
        a, b = score(qs, shape, p)
        print(f"  {name:<24} answerable {a:.4f}   _abs {b:.4f}")

    low = [q for q in qs if shape[q]["top1"] < T]
    resc = [q for q in low if verd[q]]
    ok = [q for q in resc if not shape[q]["abstain"] and VAL.get(l2[q].get("verdict"), 0) > 0]
    print(f"\nthe verifier RESCUES: {len(low)} fall below {T:.3f}, it says attach for "
          f"{len(resc)}, and {len(ok)} of those are answerable questions the reader got "
          f"right — a pure threshold discards all {len(ok)}")

    skipped = [q for q in qs if shape[q]["top1"] >= T]
    ab_sk = [q for q in skipped if shape[q]["abstain"]]
    harmed = [q for q in ab_sk if not l2[q].get("not_found")]
    print(f"\n⚠️ the exposure: {len(ab_sk)} unanswerable questions bypass the verifier; the "
          f"reader refuses {len(ab_sk)-len(harmed)} of them anyway, and {len(harmed)} are "
          f"answered — the verifier would have caught all {sum(1 for q in ab_sk if verd[q] is False)}")


if __name__ == "__main__":
    main()
