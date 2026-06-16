#!/usr/bin/env python3
# exp6 independent re-derivation. Reads the generation ledger JSON (the `entries` array from
# GET /v1/_memory/scopes/user/exp6/keys?prefix=gen:) from a FILE (argv[1]) — never trusts the
# breeder's self-report. Recomputes per-generation mean/max score + mean gene vector, checks the
# improvement trend and lineage integrity. argv[2]=result:summary JSON (or "" if none).
import sys, json

def load(path):
    try:
        with open(path) as f:
            return json.load(f).get("entries", [])
    except Exception:
        return []

def val(v):
    if isinstance(v, str):
        try: return json.loads(v)
        except Exception: return {}
    return v or {}

entries = load(sys.argv[1])
result = sys.argv[2] if len(sys.argv) > 2 else ""

gens = {}  # g -> {i -> {genes, score, def_id, parent}}
for e in entries:
    k = e.get("key", ""); v = val(e.get("value"))
    p = k.split(":")                       # gen:<g>:var:<i>[:result|:eval]
    if len(p) < 4 or p[0] != "gen" or p[2] != "var":
        continue
    try: g, i = int(p[1]), int(p[3])
    except ValueError: continue
    slot = gens.setdefault(g, {}).setdefault(i, {})
    if len(p) == 4:                        # the genotype record
        slot["def_id"] = v.get("def_id"); slot["genes"] = v.get("genes"); slot["parent"] = v.get("parent")
    elif p[4] == "eval":
        slot["score"] = v.get("score")
    elif p[4] == "result":
        slot.setdefault("genes", v.get("genes")); slot["run_id"] = v.get("run_id")

print("gen | n | mean_score | max_score | mean_genes {creativity,courage,caution}")
first_mean = last_mean = None
for g in sorted(gens):
    vs = list(gens[g].values())
    scores = [x["score"] for x in vs if isinstance(x.get("score"), (int, float))]
    genes = [x["genes"] for x in vs if isinstance(x.get("genes"), dict)]
    mean = sum(scores) / len(scores) if scores else None
    mx = max(scores) if scores else None
    def avg(key):
        gg = [gd[key] for gd in genes if isinstance(gd.get(key), (int, float))]
        return round(sum(gg) / len(gg), 1) if gg else None
    mg = {k: avg(k) for k in ("creativity", "courage", "caution")}
    ms = f"{mean:8.3f}" if mean is not None else "   n/a  "
    xs = f"{mx:7.3f}" if mx is not None else "  n/a  "
    print(f"{g:3d} | {len(vs):d} | {ms} | {xs} | {mg}")
    if mean is not None:
        if first_mean is None: first_mean = mean
        last_mean = mean

print()
if first_mean is not None and last_mean is not None:
    verdict = "PASS" if last_mean >= first_mean else "REVIEW (declined)"
    print(f"[check] mean(score) last gen ({last_mean:.3f}) >= first gen ({first_mean:.3f}) ? {verdict}")
else:
    print("[check] improvement: n/a (no scored generations)")

# lineage integrity: every gen>0 variant's parent must resolve to a known def_id
defids = {x["def_id"] for g in gens for x in gens[g].values() if x.get("def_id")}
broken = [(g, i) for g in gens if g > 0 for i, x in gens[g].items()
          if x.get("parent") and x["parent"] not in defids]
if any(g > 0 for g in gens):
    print(f"[check] lineage parents all resolve to a known def_id ? {'PASS' if not broken else 'FAIL ' + str(broken)}")
else:
    print("[check] lineage: only gen 0 present (no mutation generations to check)")

if result:
    try:
        r = json.loads(result) if isinstance(result, str) else result
        print(f"[winner] generations={r.get('generations')} best_score={r.get('best_score')} "
              f"winner_def_id={r.get('winner_def_id')} stopped={r.get('stopped')}")
    except Exception:
        print(f"[winner] result:summary = {result[:200]}")
