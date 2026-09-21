#!/usr/bin/env python3
"""Assemble a gated arm from the two states a gate chooses between.

A gate on the trace grant only ever decides one thing per question: attach the
retrieved turns, or do not. Both outcomes were measured on the SAME question, the same
store and the same judge — the control arm is "withheld", the L2 arm is "attached" — so
a gated arm is not an estimate of those runs, it IS those runs, selected per question by
whatever decider you name. That buys every threshold at once instead of one arm per
threshold, and it makes the comparison PAIRED: the arms share their answerer draws, so a
difference cannot come from the answerer having a different day.

What it is NOT: a fresh measurement. Each per-question outcome is a single draw, shared
across arms. The comparison is paired and sound; the absolute numbers carry single-draw
noise, exactly as the arms they are built from do.

⚠️ AND ONE DEVIATION, TRUE OF EVERY DECIDER HERE. The gate decides from the retrieval
for the RAW question, while the L2 answerer saw the retrieval for its OWN mid-run query,
which the model composes and which differs (L2 beat the tool-free L3 by 9.3pp largely
for that reason). The handicap is identical across deciders, so ranking them is fair;
the absolute block rates are not what an in-runtime gate would produce.

Usage:
  assemble_gate.py <control/answer-report.json> <l2/answer-report.json> <retrieval-dump.jsonl>
                   [--verdicts verifier.json]
"""
import json, sys, argparse
from statistics import mean


def load_arm(path):
    """question -> outcome row. The question text is the join key: it is what both the
    arm and the retrieval dump record, and instance ids are not carried on every row."""
    rows = json.load(open(path))["results"]
    out = {}
    for r in rows:
        out[r["question"]] = r
    return out


def score(rows, abstain_qs):
    """LoCoMo convention, with the `_abs` inversion the harness applies: on an
    abstention question refusing IS correct and answering is wrong, so those are scored
    without the judge and reported as their own slice. Pooling the two is meaningless —
    one slice rewards answering, the other punishes it."""
    ans = [r for q, r in rows.items() if q not in abstain_qs]
    abst = [r for q, r in rows.items() if q in abstain_qs]

    def acc(rs):
        graded = [r for r in rs if r.get("verdict") in ("correct", "partial", "wrong")]
        if not graded:
            return 0.0, 0
        v = sum(1.0 if r["verdict"] == "correct" else 0.5 if r["verdict"] == "partial" else 0.0
                for r in graded)
        return v / len(graded), len(graded)

    a_acc, a_n = acc(ans)
    # on `_abs`, correct == refused
    ab_correct = sum(1 for r in abst if r.get("not_found"))
    ab_acc = ab_correct / len(abst) if abst else 0.0
    return {
        "answerable_acc": a_acc, "answerable_n": a_n,
        "answerable_nf": mean([1.0 if r.get("not_found") else 0.0 for r in ans]) if ans else 0.0,
        "abs_acc": ab_acc, "abs_n": len(abst),
    }


def mcnemar(a, b, abstain_qs, slice_abs):
    """Paired counts between two assembled arms on one slice."""
    up = dn = 0
    for q in a:
        if (q in abstain_qs) != slice_abs:
            continue
        ra, rb = a[q], b.get(q)
        if rb is None:
            continue
        if slice_abs:
            ga, gb = bool(ra.get("not_found")), bool(rb.get("not_found"))
        else:
            val = {"correct": 1.0, "partial": 0.5, "wrong": 0.0}
            ga, gb = val.get(ra.get("verdict"), 0.0), val.get(rb.get("verdict"), 0.0)
        if gb > ga:
            up += 1
        elif gb < ga:
            dn += 1
    return up, dn


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("control"); ap.add_argument("l2"); ap.add_argument("dump")
    ap.add_argument("--verdicts", default=None,
                    help="JSON {question: bool} from the sufficiency verifier")
    a = ap.parse_args()

    ctrl, l2 = load_arm(a.control), load_arm(a.l2)
    dump = {}
    for line in open(a.dump):
        if line.strip():
            r = json.loads(line)
            dump[r["question"]] = r

    shared = set(ctrl) & set(l2) & set(dump)
    abstain_qs = {q for q in shared if dump[q]["abstain"]}
    print(f"joined {len(shared)} questions "
          f"(control {len(ctrl)}, L2 {len(l2)}, dump {len(dump)}) — "
          f"{len(abstain_qs)} `_abs`\n")
    if len(shared) < 0.9 * min(len(ctrl), len(l2)):
        print("⚠️ the join dropped >10% — question text is not matching; check before reading on\n")

    base = {q: ctrl[q] for q in shared}
    full = {q: l2[q] for q in shared}

    def show(name, arm, ref=None):
        s = score(arm, abstain_qs)
        line = (f"{name:<28} answerable {s['answerable_acc']:.4f} (nf {s['answerable_nf']:.3f})"
                f"   _abs {s['abs_acc']:.4f}")
        if ref is not None:
            ua, da = mcnemar(ref, arm, abstain_qs, False)
            ub, db = mcnemar(ref, arm, abstain_qs, True)
            line += f"   vs L2: ans +{ua}/-{da}, _abs +{ub}/-{db}"
        print(line)

    show("control (never attach)", base)
    show("L2 (always attach)", full)

    print("\nARM 0 — threshold gate on top1, every threshold at once:")
    print(f"{'threshold':>10}{'withheld':>10}{'answerable':>13}{'_abs':>9}"
          f"{'vs L2 ans':>14}{'vs L2 _abs':>13}")
    cands = sorted({round(dump[q]["top1"], 3) for q in shared})
    step = max(1, len(cands) // 14)
    for thr in cands[::step]:
        arm = {q: (full[q] if dump[q]["top1"] >= thr else base[q]) for q in shared}
        s = score(arm, abstain_qs)
        w = sum(1 for q in shared if dump[q]["top1"] < thr)
        ua, da = mcnemar(full, arm, abstain_qs, False)
        ub, db = mcnemar(full, arm, abstain_qs, True)
        print(f"{thr:>10.3f}{w:>10}{s['answerable_acc']:>13.4f}{s['abs_acc']:>9.4f}"
              f"{f'+{ua}/-{da}':>14}{f'+{ub}/-{db}':>13}")

    if a.verdicts:
        v = json.load(open(a.verdicts))
        arm = {q: (full[q] if v.get(q, True) else base[q]) for q in shared}
        w = sum(1 for q in shared if not v.get(q, True))
        print(f"\nARM 2 — sufficiency verifier ({w} of {len(shared)} withheld):")
        show("  verifier gate", arm, ref=full)


if __name__ == "__main__":
    main()
