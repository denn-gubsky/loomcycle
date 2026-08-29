#!/usr/bin/env python3
"""RFC CO-4 — cross-lingual retrieval.

Write one row per entity in ENGLISH, keyed by its QID. Then query with the SAME
entity's label in another language and check whether that row comes back.

No LLM anywhere: the expected key is the row we wrote, so this scores the EMBEDDER and
the ranker directly. That is the point — a judge here would measure the judge.

bge-m3 claims 100+ languages and has never been verified on this stack.
"""
import json, os, sys, urllib.request
from collections import defaultdict

BASE = "http://truenas.local:8787"
TOK  = os.environ["WIKI_BENCH_TENANT_TOKEN"]
TOP_K = 10

def api(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method,
        headers={"Authorization": "Bearer " + TOK, "Content-Type": "application/json"})
    return urllib.request.urlopen(req, timeout=180).read()

def main():
    items = json.load(open(sys.argv[1]))["items"]
    scope = json.loads(api("GET", "/v1/_me"))["subject"]
    print(f"scope: {scope}   entities: {len(items)}", flush=True)

    keys = [f"co4:{it['qid']}" for it in items]
    for k in keys:                                    # start from a known-empty corpus
        try: api("DELETE", f"/v1/_memory/scopes/user/{scope}/keys/{k}")
        except Exception: pass

    # Seed in ENGLISH only.
    for it in items:
        body = {"value": f"{it['labels']['en']} — {it['description']}", "embed": True}
        out = json.loads(api("PUT", f"/v1/_memory/scopes/user/{scope}/keys/co4:{it['qid']}", body))
        if not out.get("embedded"):
            raise SystemExit(f"{it['qid']} did not embed: {out.get('embed_warning')}")
    print(f"seeded {len(items)} english rows", flush=True)

    hits = defaultdict(lambda: [0, 0])
    rows = []
    for it in items:
        want = f"co4:{it['qid']}"
        for lang, label in it["labels"].items():
            res = json.loads(api("POST", "/v1/_memory/search", {
                "query": label, "scope": "user", "scope_id": scope, "top_k": TOP_K}))
            got = [e["key"] for e in (res.get("entries") or [])]
            ok = want in got
            rank = got.index(want) + 1 if ok else 0
            hits[lang][1] += 1
            if ok: hits[lang][0] += 1
            rows.append({"qid": it["qid"], "lang": lang, "query": label,
                         "hit": ok, "rank": rank})
        json.dump(rows, open(sys.argv[2], "w"), indent=1)

    print(f"\nEnglish corpus, query language varied — recall@{TOP_K}\n")
    print(f"{'query lang':12}{'hit@10':>10}{'rate':>9}{'MRR':>8}")
    for lang in ["en", "de", "fr", "uk", "ru", "ja", "ar"]:
        good, tot = hits[lang]
        if not tot: continue
        mrr = sum(1.0/r["rank"] for r in rows if r["lang"] == lang and r["rank"]) / tot
        star = "  <- same language as the corpus" if lang == "en" else ""
        print(f"{lang:12}{good}/{tot:<8}{good/tot:>8.3f}{mrr:>8.3f}{star}")

main()
