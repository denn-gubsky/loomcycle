#!/usr/bin/env python3
"""Build a Wikidata fact corpus for loomcycle memory (5k-15k rows).

Facts are rendered as ONE self-contained English sentence each, because that is what
gets embedded and what a model reads back. "Marie Curie was born on 1867-11-07", not a
triple — a bag of QIDs embeds badly and reads worse.

Deterministic from the property list + LIMIT, so a rebuild produces the same corpus and
a later score is comparable. CC0.
"""
import json, re, sys, time, urllib.parse, urllib.request

UA = "loomcycle-memory-benchmark/1.0 (fact corpus; research)"
QLEVER = "https://qlever.dev/api/wikidata"

# (property, object-is-literal, sentence template). Chosen because each renders into a
# natural sentence without a subordinate clause — a template that needs one produces
# text no human would write, and the embedding then measures the template.
PROPS = [
    ("P569", True,  "{s} was born on {o}."),
    ("P570", True,  "{s} died on {o}."),
    ("P571", True,  "{s} was founded on {o}."),
    ("P27",  False, "{s} is a citizen of {o}."),
    ("P106", False, "{s} works as a {o}."),
    ("P19",  False, "{s} was born in {o}."),
    ("P17",  False, "{s} is located in {o}."),
    ("P36",  False, "The capital of {s} is {o}."),
    ("P112", False, "{s} was founded by {o}."),
    ("P159", False, "{s} is headquartered in {o}."),
    ("P50",  False, "{s} was written by {o}."),
    ("P495", False, "{s} originates from {o}."),
]
PREFIXES = ("PREFIX wdt: <http://www.wikidata.org/prop/direct/> "
            "PREFIX rdfs: <http://www.w3.org/2000/01/rdf-schema#> ")

def get(url):
    req = urllib.request.Request(url, headers={
        "User-Agent": UA, "Accept": "application/sparql-results+json"})
    for a in range(4):
        try:
            return json.loads(urllib.request.urlopen(req, timeout=180).read())
        except Exception as e:
            if a == 3:
                raise
            time.sleep(2 ** a)

# Offsets spread across the index. A single LIMIT-N query returns one CONTIGUOUS block,
# and QLever's index order is grouped by object — a first attempt produced 809 of 1000
# "is located in Gabon" and 810 of 996 "headquartered in Boston". That corpus would make
# any retrieval test meaningless, because most queries would have hundreds of equally
# good answers.
#
# Geometric rather than uniform, and short-circuiting on an empty chunk, because the
# properties differ in size by orders of magnitude: P17 has millions of triples, P112
# far fewer, and a fixed large offset returns nothing for the small ones.
OFFSETS = [0, 3000, 12000, 40000, 90000, 180000, 320000, 550000, 900000,
           1400000, 2100000, 3000000, 4200000, 5800000, 7800000]

def query(prop, literal, limit, offset):
    if literal:
        q = (PREFIXES + f"SELECT ?s ?sl ?o WHERE {{ ?s wdt:{prop} ?o ; rdfs:label ?sl . "
             f"FILTER(LANG(?sl)='en') }} LIMIT {limit} OFFSET {offset}")
    else:
        q = (PREFIXES + f"SELECT ?s ?sl ?ol WHERE {{ ?s wdt:{prop} ?o ; rdfs:label ?sl . "
             f"?o rdfs:label ?ol . FILTER(LANG(?sl)='en') FILTER(LANG(?ol)='en') }} "
             f"LIMIT {limit} OFFSET {offset}")
    return get(QLEVER + "?" + urllib.parse.urlencode({"query": q}))

def clean_date(v):
    """A Wikidata time literal is an xsd:dateTime; the day is the useful part. A
    year-only value arrives as -01-01 and is rendered as the year alone rather than
    asserting a January 1st that the source never claimed."""
    m = re.match(r"^([+-]?\d{4})-(\d{2})-(\d{2})", v)
    if not m:
        return None
    y, mo, d = m.groups()
    if y.startswith("-"):
        return None                      # BCE: the sentence templates read wrong
    y = y.lstrip("+")
    return y if (mo, d) == ("01", "01") else f"{y}-{mo}-{d}"

def main():
    per = int(sys.argv[2]) if len(sys.argv) > 2 else 1000
    facts, seen = [], set()
    for prop, literal, tmpl in PROPS:
        kept = 0
        chunk = max(20, per // len(OFFSETS))
        for off in OFFSETS:
            if kept >= per:
                break
            try:
                res = query(prop, literal, chunk, off)
            except Exception as e:
                print(f"  {prop}@{off}: FAILED {e}", file=sys.stderr)
                continue
            rows = res.get("results", {}).get("bindings", [])
            if not rows:
                break                      # past the end of this property
            for b in rows:
                qid = b["s"]["value"].rsplit("/", 1)[-1]
                key = f"wiki:{qid}:{prop}"
                if key in seen:            # one value per (entity, property)
                    continue
                subj = (b.get("sl") or {}).get("value", "")
                if not subj or subj.startswith("Q"):
                    continue
                if literal:
                    obj = clean_date((b.get("o") or {}).get("value", ""))
                else:
                    obj = (b.get("ol") or {}).get("value", "")
                    if obj.startswith("Q"):
                        obj = ""
                if not obj:
                    continue
                seen.add(key)
                facts.append({"key": key, "qid": qid, "property": prop,
                              "text": tmpl.format(s=subj, o=obj)})
                kept += 1
            time.sleep(0.4)                # polite; QLever is a shared service
        print(f"  {prop}: {kept} facts", file=sys.stderr)
    json.dump({"facts": facts}, open(sys.argv[1], "w"), indent=1)
    print(f"\nwrote {len(facts)} facts", file=sys.stderr)

main()
