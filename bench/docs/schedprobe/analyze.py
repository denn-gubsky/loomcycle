#!/usr/bin/env python3
"""Tables for the RFC EI P0 host measurements (see PREREG.md).

    analyze.py --cold runs/E1-cold.jsonl --warm runs/E1-warm.jsonl [runs/E2-*.jsonl ...]

--cold is the E1 cold arm: it calibrates each model and is the subject of
instrument check 1. --warm is the E1 warm arm (check 2). Every other file is
summarised against that calibration. Standard library only.
"""
import argparse
import json
import statistics
import sys
from collections import defaultdict

LOAD_MS = 1000  # a call that spent longer loading reloaded the model


def read(path):
    header, calls = None, []
    with open(path) as f:
        for line in f:
            r = json.loads(line)
            if r["type"] == "run":
                header = r  # the last header carries `finished`
            else:
                calls.append(r)
    if header is None:
        sys.exit(f"{path}: no run header")
    return header, calls


def ok_chat(calls):
    return [c for c in calls if c["kind"] == "chat" and not c.get("error") and c.get("prefill_ms")]


def pct(xs, p):
    if not xs:
        return None
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(round(p / 100 * (len(xs) - 1))))]


def fit_line(xs, ys):
    mx, my = statistics.fmean(xs), statistics.fmean(ys)
    var = sum((x - mx) ** 2 for x in xs)
    b = sum((x - mx) * (y - my) for x, y in zip(xs, ys)) / var if var else 0.0
    return my - b * mx, b


def calibrate(cold_calls):
    by_model = defaultdict(list)
    for c in ok_chat(cold_calls):
        by_model[c["model"]].append(c)
    cal = {}
    for model, cs in by_model.items():
        if len(cs) < 3:
            sys.exit(f"calibration: {model} has {len(cs)} usable cold calls, need >= 3")
        a, b = fit_line([c["prompt_chars"] for c in cs], [c["prefill_ms"] for c in cs])
        tpc = statistics.median(c["prompt_eval_count"] / c["prompt_chars"] for c in cs)
        cal[model] = {"a": a, "b": b, "tok_per_char": tpc}
    return cal


def reuse(c, cal):
    m = cal.get(c["model"])
    if not m:
        return None
    expected = m["a"] + m["b"] * c["prompt_chars"]
    return 1 - c["prefill_ms"] / expected if expected > 0 else None


def count_ratio(c, cal):
    m = cal.get(c["model"])
    if not m or not c.get("prompt_eval_count"):
        return None
    return c["prompt_eval_count"] / (c["prompt_chars"] * m["tok_per_char"])


def turns(calls):
    """{turn: call} for a single-agent arm."""
    agents = {c["agent"] for c in calls}
    if len(agents) != 1:
        sys.exit(f"an E1 arm must have one agent, found {sorted(agents)}")
    return {c["turn"]: c for c in ok_chat(calls)}


def instrument_checks(cold_calls, warm_calls):
    cold, warm = turns(cold_calls), turns(warm_calls)
    first, last = min(cold), max(cold)
    size_ratio = cold[last]["prompt_chars"] / cold[first]["prompt_chars"]
    time_ratio = cold[last]["prefill_ms"] / cold[first]["prefill_ms"]
    c1 = time_ratio >= 0.8 * size_ratio
    t0 = min(warm)
    rel = warm[t0]["prefill_ms"] / cold[first]["prefill_ms"]
    c2 = 0.7 <= rel <= 1.3
    print("## Instrument checks (E1)\n")
    print("| check | measured | rule | result |\n|---|---|---|---|")
    print(f"| 1 nonce busts the cache | prefill x{time_ratio:.2f} for prompt x{size_ratio:.2f} | >= 0.8 x size ratio | {'PASS' if c1 else 'FAIL'} |")
    print(f"| 2 warm arm starts cold | turn-1 prefill {rel:.2f} x cold | within ±30 % | {'PASS' if c2 else 'FAIL'} |")
    print()
    return c1 and c2


def summarise(path, header, calls, cal):
    chat = ok_chat(calls)
    errors = sum(1 for c in calls if c.get("error"))
    starts = [c["start_unix_ms"] for c in calls]
    ends = [c["start_unix_ms"] + c["wall_ms"] for c in calls]
    makespan = (max(ends) - min(starts)) / 1000 if calls else 0
    out_tok = sum(c.get("eval_count", 0) for c in chat)
    agent_end = defaultdict(int)
    for c, e in zip(calls, ends):
        agent_end[c["agent"]] = max(agent_end[c["agent"]], e - min(starts))
    fin = list(agent_end.values())
    spread = statistics.pstdev(fin) / statistics.fmean(fin) if len(fin) > 1 and statistics.fmean(fin) else 0
    later = [c for c in chat if c["turn"] >= 1]
    ru = [r for r in (reuse(c, cal) for c in later) if r is not None]
    cr = [r for r in (count_ratio(c, cal) for c in later) if r is not None]
    seen, reloads = set(), 0
    for c in sorted(calls, key=lambda c: c["start_unix_ms"]):
        key = (c["model"], c.get("num_ctx", 0))
        if key in seen and c.get("load_ms", 0) > LOAD_MS:
            reloads += 1
        seen.add(key)
    lat = defaultdict(list)
    for c in calls:
        if not c.get("error"):
            lat[c["kind"]].append(c["wall_ms"])
    fmt = lambda v, f="{:.0f}": "–" if v is None else f.format(v)
    return [
        header["label"], header["policy"], str(header["quantum"]), str(header["hold_ms"]),
        str(len(calls)), str(errors), f"{makespan:.0f}", f"{out_tok / makespan:.1f}" if makespan else "–",
        fmt(pct([c["ttft_ms"] for c in chat if c.get("ttft_ms")], 50)),
        fmt(pct([c["ttft_ms"] for c in chat if c.get("ttft_ms")], 95)),
        fmt(pct([c["wait_ms"] for c in calls], 95)),
        f"{spread:.2f}",
        fmt(statistics.median(ru) if ru else None, "{:.2f}"),
        fmt(statistics.median(cr) if cr else None, "{:.2f}"),
        str(reloads),
        fmt(pct(lat.get("embed", []), 95)), fmt(pct(lat.get("decision", []), 95)),
    ]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--cold", required=True)
    ap.add_argument("--warm", required=True)
    ap.add_argument("runs", nargs="*")
    args = ap.parse_args()

    _, cold_calls = read(args.cold)
    _, warm_calls = read(args.warm)
    cal = calibrate(cold_calls)
    print("## Calibration (E1 cold arm)\n")
    print("| model | prefill ms = a + b·chars | tokens/char |\n|---|---|---|")
    for m, v in cal.items():
        print(f"| {m} | {v['a']:.1f} + {v['b']:.4f}·chars | {v['tok_per_char']:.3f} |")
    print()
    if not instrument_checks(cold_calls, warm_calls):
        print("**Instrument check failed: nothing after E1 is scored (PREREG).**")
        return 1

    cols = ["label", "policy", "q", "hold", "calls", "err", "makespan s", "out tok/s",
            "TTFT p50", "TTFT p95", "wait p95", "finish spread", "reuse (turn 2+)", "count ratio",
            "reloads", "embed p95", "decision p95"]
    print("## Runs\n")
    print("| " + " | ".join(cols) + " |\n|" + "---|" * len(cols))
    for path in [args.warm] + args.runs:
        header, calls = read(path)
        print("| " + " | ".join(summarise(path, header, calls, cal)) + " |")
    return 0


if __name__ == "__main__":
    sys.exit(main())
