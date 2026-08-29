#!/usr/bin/env python3
"""RFC CO-3 fixture: entities whose Wikidata P31 maps unambiguously onto one of
loomcycle's base ontology types.

Only UNAMBIGUOUS mappings are used. A film is arguably an `object` and arguably not;
scoring the store on a call a careful human would hesitate over measures the mapping,
not the ontology. So the set is restricted to P31 values where the base type is not in
question, and the ambiguous middle is left out rather than guessed at.
"""
import json, sys, time, urllib.parse, urllib.request

UA = "loomcycle-memory-benchmark/1.0 (RFC CO-3; research)"

# Wikidata class -> loomcycle base type. Each entry is a case where the base ontology
# has exactly one defensible answer.
CLASSES = [
    ("Q5",       "person",       "human"),
    ("Q3624078", "location",     "sovereign state"),
    ("Q515",     "location",     "city"),
    ("Q3918",    "organization", "university"),
    ("Q4830453", "organization", "business"),
    ("Q7278",    "organization", "political party"),
    ("Q198",     "event",        "war"),
    ("Q1656682", "event",        "event"),
]
PER_CLASS = 8

def get(url):
    req = urllib.request.Request(url, headers={"User-Agent": UA, "Accept": "application/sparql-results+json"})
    for a in range(4):
        try:
            return json.loads(urllib.request.urlopen(req, timeout=60).read())
        except Exception:
            if a == 3:
                raise
            time.sleep(2 ** a)

def sparql(q):
    """QLever, not WDQS.

    WDQS began returning 429 "aggressively rate-limiting to 1 req / min - this rule was
    created during active wdqs outage" partway through this work. The research doc
    already recommended QLever for set-selection queries precisely because WDQS is
    unreliable at this shape; this is that recommendation taken.
    """
    # NOTE: qlever.cs.uni-freiburg.de now 308-redirects to qlever.dev. The research
    # doc still cites the old host; this is the live one.
    return get("https://qlever.dev/api/wikidata?" + urllib.parse.urlencode({"query": q}))

def sample(cls, n):
    """Entities of one Wikidata class, with an English label and description.

    No popularity ordering: combining wikibase:sitelinks with label and description in
    one pattern returned zero rows on QLever, and ranking by fame is a nicety here —
    what the fixture needs is entities that HAVE a description to build a sentence
    from, which the FILTERs already guarantee.
    """
    q = ("PREFIX wdt: <http://www.wikidata.org/prop/direct/> "
         "PREFIX wd: <http://www.wikidata.org/entity/> "
         "PREFIX rdfs: <http://www.w3.org/2000/01/rdf-schema#> "
         "PREFIX schema: <http://schema.org/> "
         f"SELECT ?e ?eLabel ?d WHERE {{ ?e wdt:P31 wd:{cls} ; rdfs:label ?eLabel ; "
         "schema:description ?d . FILTER(LANG(?eLabel)='en') FILTER(LANG(?d)='en') } "
         f"LIMIT {n}")
    out = []
    for b in sparql(q).get("results", {}).get("bindings", []):
        qid = b["e"]["value"].rsplit("/", 1)[-1]
        label = (b.get("eLabel") or {}).get("value", "")
        desc = (b.get("d") or {}).get("value", "")
        if not label or label == qid or not desc:
            continue
        out.append({"qid": qid, "label": label, "description": desc})
    return out

def main():
    items = []
    for cls, base, human in CLASSES:
        try:
            rows = sample(cls, PER_CLASS)
        except Exception as e:
            print(f"  {human}: FAILED {e}", file=sys.stderr); continue
        for r in rows:
            r["expected_type"] = base
            r["wikidata_class"] = human
        items.extend(rows)
        print(f"  {human:16} -> {base:13} {len(rows)} items", file=sys.stderr)
        time.sleep(0.5)
    json.dump({"items": items}, open(sys.argv[1], "w"), indent=1)
    print(f"\nwrote {len(items)} items", file=sys.stderr)

main()
