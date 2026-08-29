#!/usr/bin/env python3
"""RFC CO-1 fixture builder: Wikidata officeholder chains.

Deterministic from a fixed office list so the corpus hashes stably. CC0, committable.
Ground truth is a QID, never a name string.

WIKIDATA IS NOT CLEAN and this file is mostly the cleaning. A first pass produced
"Phil Baker, President of the United States (2018-)", the same person three times from
separate re-election statements, and a generic `president` item holding unrelated
people. A benchmark built on that measures the corpus, not the store.
"""
import json, re, sys, time, urllib.parse, urllib.request

UA = "loomcycle-memory-benchmark/1.0 (RFC CO; research; https://github.com/denn-gubsky/loomcycle)"

def candidate_offices():
    """Discover office items rather than guessing QIDs.

    Hand-picking twenty QIDs produced seven chains, several of them wrong items (a
    generic `president` holding unrelated people). Sovereign states declare their
    head-of-government and head-of-state offices as properties, so the candidate set
    can be derived instead of recalled — and the per-office cleaner then rejects the
    messy ones on structure rather than on my memory of what a QID means.
    """
    q = ("SELECT DISTINCT ?office WHERE { ?c wdt:P31 wd:Q3624078 . "
         "{ ?c wdt:P1313 ?office } UNION { ?c wdt:P1906 ?office } }")
    return [b["office"]["value"].rsplit("/", 1)[-1]
            for b in sparql(q)["results"]["bindings"]]


def get(url):
    req = urllib.request.Request(url, headers={"User-Agent": UA, "Accept": "application/json"})
    for a in range(4):
        try:
            return json.loads(urllib.request.urlopen(req, timeout=60).read())
        except Exception:
            if a == 3:
                raise
            time.sleep(2 ** a)          # serial + backoff, per API etiquette

def sparql(q):
    return get("https://query.wikidata.org/sparql?" + urllib.parse.urlencode({"query": q, "format": "json"}))

def holders(office):
    q = ("SELECT ?holder ?start ?end WHERE { ?holder p:P39 ?st . "
         f"?st ps:P39 wd:{office} ; pq:P580 ?start . "
         "OPTIONAL { ?st pq:P582 ?end } } ORDER BY ?start")
    out = []
    for b in sparql(q)["results"]["bindings"]:
        out.append({"qid": b["holder"]["value"].rsplit("/", 1)[-1],
                    "start": b["start"]["value"][:10],
                    "end": b["end"]["value"][:10] if "end" in b else None})
    return out

def labels(qids):
    out = {}
    for i in range(0, len(qids), 50):
        d = get("https://www.wikidata.org/w/api.php?" + urllib.parse.urlencode({
            "action": "wbgetentities", "ids": "|".join(qids[i:i+50]),
            "props": "labels", "languages": "en", "format": "json"}))
        for qid, ent in (d.get("entities") or {}).items():
            v = (ent.get("labels") or {}).get("en", {}).get("value")
            if v:
                out[qid] = v
        time.sleep(0.4)
    return out

def clean(rows):
    """Reduce a raw statement list to the one transition CO-1 needs: the CURRENT holder
    and the one immediately before.

    A first attempt validated the whole succession and rejected all 18 offices. Real
    Wikidata has interim holders, concurrent statements and historical terms with no
    end date — a chain that is clean end-to-end is the exception. But the benchmark
    only needs ONE change to be a knowledge-update case, so validating the rest was
    strictness bought at the cost of having any data at all.
    """
    rows = [r for r in rows if r["start"] >= "1990-01-01"]
    rows.sort(key=lambda r: r["start"])
    merged = []
    for r in rows:
        if merged and merged[-1]["qid"] == r["qid"]:   # re-election, not a change
            merged[-1]["end"] = r["end"]
            continue
        merged.append(dict(r))
    if len(merged) < 2:
        return None, "fewer than two distinct holders since 1990"
    # Exactly one open-ended term, and it must be the LAST one. More than one means the
    # office is not single-holder here (or the data is wrong), and either way "who
    # holds it now" has no single answer to grade against.
    open_terms = [i for i, r in enumerate(merged) if r["end"] is None]
    if open_terms != [len(merged) - 1]:
        return None, f"{len(open_terms)} open-ended terms, current holder ambiguous"
    prev, cur = merged[-2], merged[-1]
    if prev["end"] is None or prev["end"] > cur["start"]:
        return None, "previous term does not close before the current one begins"
    return [prev, cur], None

def main():
    chains, need, rejected = [], set(), []
    offices = candidate_offices()
    print(f'discovered {len(offices)} candidate offices', file=sys.stderr)
    for off in offices:
        try:
            raw = holders(off)
        except Exception as e:
            rejected.append((off, f"query failed: {e}")); continue
        rows, why = clean(raw)
        if rows is None or len(rows) < 2:
            rejected.append((off, why or f"only {len(rows or [])} states")); continue
        chains.append({"office": off, "holders": rows})
        need.add(off); need.update(r["qid"] for r in rows)
        time.sleep(0.5)
    lab = labels(sorted(need))
    kept = []
    for c in chains:
        # A chain with an unresolved label cannot be graded by name in the report, and
        # more importantly signals an item that is not what we think it is.
        if c["office"] not in lab or any(h["qid"] not in lab for h in c["holders"]):
            rejected.append((c["office"], "unresolved label")); continue
        c["office_label"] = lab[c["office"]]
        for h in c["holders"]:
            h["label"] = lab[h["qid"]]
        kept.append(c)
    json.dump({"chains": kept}, open(sys.argv[1], "w"), indent=1)
    from collections import Counter
    for why, n in Counter(w for _, w in rejected).most_common():
        print(f"  rejected {n:4}: {why}", file=sys.stderr)
    print(f"\nkept {len(kept)} chains, rejected {len(rejected)}", file=sys.stderr)

main()
