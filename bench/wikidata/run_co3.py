#!/usr/bin/env python3
"""RFC CO-3 — ontology type-assignment accuracy against Wikidata P31.

The RFC BZ/CA type hierarchy has no external ground truth of any kind. Wikidata's
instance-of IS a published one, so this is the first scored number that subsystem has.

Scored per Wikidata class as well as overall: an ontology can be right about people and
wrong about organizations, and one aggregate would hide that.
"""
import json, os, re, sys, urllib.request
from collections import defaultdict

BASE = "http://truenas.local:8787"
TOK  = os.environ["WIKI_BENCH_TENANT_TOKEN"]

def ask(question):
    body = {"agent": "wiki/typer", "prompt": question, "user_id": "co3", "max_iterations": 2}
    req = urllib.request.Request(BASE + "/v1/runs", data=json.dumps(body).encode(), method="POST",
        headers={"Authorization": "Bearer " + TOK, "Content-Type": "application/json"})
    raw = urllib.request.urlopen(req, timeout=300).read().decode("utf-8", "replace")
    text = ""
    for line in raw.splitlines():
        if not line.startswith("data: "):
            continue
        try:
            ev = json.loads(line[6:])
        except Exception:
            continue
        if ev.get("type") == "text":
            text += ev.get("text", "")
    m = re.search(r"TYPE:\s*([a-zA-Z]+)", text)
    return (m.group(1).lower() if m else None), text.strip()[:120]

def main():
    items = json.load(open(sys.argv[1]))["items"]
    out, by_class = [], defaultdict(lambda: [0, 0])
    for i, it in enumerate(items, 1):
        q = f"Entity: {it['label']}\nDescription: {it['description']}"
        got, raw = ask(q)
        ok = (got == it["expected_type"])
        by_class[it["wikidata_class"]][1] += 1
        if ok:
            by_class[it["wikidata_class"]][0] += 1
        out.append({**it, "assigned": got, "correct": ok, "raw": raw})
        print(f"  {i:3}/{len(items)} {it['label'][:26]:28} want={it['expected_type']:13} "
              f"got={str(got):13} {'ok' if ok else 'MISS'}", flush=True)
        json.dump(out, open(sys.argv[2], "w"), indent=1)

    n = len(out); c = sum(1 for r in out if r["correct"])
    print(f"\noverall type accuracy: {c}/{n} = {c/n:.4f}\n")
    print(f"{'wikidata class':18}{'expected':14}{'accuracy':>10}")
    for cls, (good, tot) in sorted(by_class.items()):
        exp = next(r["expected_type"] for r in out if r["wikidata_class"] == cls)
        print(f"{cls:18}{exp:14}{good}/{tot}")
    # Under-typing: refused rather than mis-assigned. Different failure, different fix.
    none = sum(1 for r in out if r["assigned"] in (None, "none"))
    print(f"\nunder-typed (none/unparsed): {none}/{n}")

main()
