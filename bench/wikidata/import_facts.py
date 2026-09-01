#!/usr/bin/env python3
"""Bulk-import a Wikidata fact corpus into loomcycle memory.

NOT through `Memory op=add`: that is one LLM call per span and a five-figure token bill
at this size. Bulk goes through the off-run PUT with embed=false, then
/v1/_memory/backfill_embeddings vectorizes in batches.

The backfill bounds by work DONE, not rows seen, so it is driven in a loop until it
reports nothing left — a single call silently leaves most of the corpus unembedded, and
an unembedded row is invisible to recall.
"""
import json, os, sys, time, urllib.parse, urllib.request
from concurrent.futures import ThreadPoolExecutor

BASE = "http://truenas.local:8787"
TOK  = os.environ["WIKI_BENCH_TENANT_TOKEN"]

def api(method, path, body=None, timeout=300):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method,
        headers={"Authorization": "Bearer " + TOK, "Content-Type": "application/json"})
    return urllib.request.urlopen(req, timeout=timeout).read()

def main():
    facts = json.load(open(sys.argv[1]))["facts"]
    scope = json.loads(api("GET", "/v1/_me"))["subject"]
    print(f"scope: user/{scope}   facts: {len(facts)}", flush=True)

    # Write unembedded and fast. Concurrency is fine here — this is our own server,
    # not a public API — but kept modest so the box stays responsive.
    done = [0]
    def put(f):
        api("PUT", f"/v1/_memory/scopes/user/{scope}/keys/{urllib.parse.quote(f['key'], safe='')}",
            {"value": f["text"], "embed": False})
        done[0] += 1
        if done[0] % 500 == 0:
            print(f"  written {done[0]}/{len(facts)}", flush=True)

    t0 = time.time()
    with ThreadPoolExecutor(max_workers=8) as ex:
        list(ex.map(put, facts))
    print(f"wrote {done[0]} rows in {time.time()-t0:.0f}s\n", flush=True)

    # Vectorize. dry_run defaults TRUE on this endpoint, so it must be said explicitly
    # or the loop spins forever reporting work it never does.
    total, rounds = 0, 0
    while True:
        rounds += 1
        q = urllib.parse.urlencode({"scope": "user", "scope_id": scope,
                                    "dry_run": "false", "limit": 500})
        out = json.loads(api("POST", "/v1/_memory/backfill_embeddings?" + q, {}, timeout=900))
        n = out.get("embedded") or out.get("updated") or 0
        total += n
        print(f"  round {rounds:3}: embedded {n:4}  stop_reason={out.get('stop_reason')}  total={total}",
              flush=True)
        if n == 0:
            break
        if rounds > 200:
            print("  ABORT: too many rounds, something is not converging", flush=True)
            break
    print(f"\nembedded {total} rows over {rounds} rounds")

main()
