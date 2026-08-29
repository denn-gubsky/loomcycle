#!/usr/bin/env python3
"""RFC CO-4 fixture: the same entity's label in several languages.

The cross-lingual test Wikidata makes free: one item carries labels for the SAME
identity in many languages, so a fact written in one language and queried in another
has a known-correct answer with no translation work and no answer-string matching —
the expected key is the row we wrote.

Languages: en, uk, ru, de, fr, ja, ar — the research doc's proposal, chosen for script
diversity (Latin / Cyrillic / CJK / Arabic) plus the ones actually in use here.
"""
import json, sys, time, urllib.parse, urllib.request

UA = "loomcycle-memory-benchmark/1.0 (RFC CO-4; research)"
LANGS = ["en", "uk", "ru", "de", "fr", "ja", "ar"]

def get(url):
    req = urllib.request.Request(url, headers={"User-Agent": UA, "Accept": "application/json"})
    for a in range(4):
        try:
            return json.loads(urllib.request.urlopen(req, timeout=60).read())
        except Exception:
            if a == 3:
                raise
            time.sleep(2 ** a)

def main():
    # Reuse the CO-3 entities: already filtered to well-described items, and reusing
    # them keeps the two phases talking about the same corpus.
    items = json.load(open(sys.argv[1]))["items"]
    qids = [it["qid"] for it in items]
    out = []
    for i in range(0, len(qids), 50):          # 50 ids per call, the documented cap
        d = get("https://www.wikidata.org/w/api.php?" + urllib.parse.urlencode({
            "action": "wbgetentities", "ids": "|".join(qids[i:i+50]),
            "props": "labels|descriptions", "languages": "|".join(LANGS),
            "format": "json"}))
        for qid, ent in (d.get("entities") or {}).items():
            labs = {l: v["value"] for l, v in (ent.get("labels") or {}).items() if l in LANGS}
            desc = (ent.get("descriptions") or {}).get("en", {}).get("value", "")
            # An item missing the English label cannot seed the corpus row, and one
            # with only English has nothing to query cross-lingually. Both are dropped
            # rather than half-measured.
            if "en" not in labs or len(labs) < 3:
                continue
            out.append({"qid": qid, "labels": labs, "description": desc})
        time.sleep(0.4)
    json.dump({"items": out}, open(sys.argv[2], "w"), indent=1)
    covered = {l: sum(1 for r in out if l in r["labels"]) for l in LANGS}
    print(f"entities with >=3 languages: {len(out)}", file=sys.stderr)
    print(f"per-language coverage: {covered}", file=sys.stderr)

main()
