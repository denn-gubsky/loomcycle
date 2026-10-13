"""Exit 0 when a run is clean: no failed call, and its chat agents' first turns were
cold (PREREG amendment 2: median first-turn prefill >= 0.8 x the E1 calibration at
that prompt size). Prints the reason otherwise.

    coldcheck.py RUN.jsonl E1-cold.jsonl
"""
import json, statistics, sys

def calls(path):
    return [r for r in map(json.loads, open(path)) if r["type"] == "call"]

run, cold = calls(sys.argv[1]), calls(sys.argv[2])
errs = sum(1 for c in run if c.get("error"))
if errs:
    print(f"{errs} failed calls")
    sys.exit(1)
pts = [(c["prompt_chars"], c["prefill_ms"]) for c in cold if c["kind"] == "chat" and c.get("prefill_ms")]
mx, my = statistics.fmean(p[0] for p in pts), statistics.fmean(p[1] for p in pts)
b = sum((x - mx) * (y - my) for x, y in pts) / sum((x - mx) ** 2 for x, _ in pts)
a = my - b * mx
first = [c["prefill_ms"] / (a + b * c["prompt_chars"]) for c in run
         if c["kind"] == "chat" and c["turn"] == 0 and c.get("prefill_ms")]
if first and statistics.median(first) < 0.8:
    print(f"first turns not cold: median prefill {statistics.median(first):.2f} x calibration")
    sys.exit(1)
sys.exit(0)
