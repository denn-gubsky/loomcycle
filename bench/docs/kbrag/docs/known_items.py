"""The known-item question set of Study D: mechanical, annotation-free gold.

    python3 -I bench/docs/kbrag/docs/known_items.py [--n 30]

Documents are drawn from the corpus with a fixed seed; from each, one section (readable,
600+ characters) drawn with the same RNG. Haiku writes one question that section answers
(dprompts.KI_WRITE); a second, separate Haiku call sees only the question and the section
and says whether the section is THE source for it (KI_CHECK). A question that fails the
check is dropped and the next draw is taken, until --n pass. Gold = the section's document.

Nothing here reads an annotation, a brief, the taxonomy or any arm's output: the sample is
the seed, the question is written from the section text alone.

-> <work>/gold/known_items.json  {questions: [{id, kind: known-item, text, gold: [doc], section}], draws, usage}
"""
import argparse, json, os, random, sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(1, os.path.dirname(HERE))
import dconfig, dprompts  # noqa: E402
import common  # noqa: E402  (../common.py: Usage)

SEED = 20261123
MIN_CHARS = 600
MAX_CHARS = 3000


def draws(docs, rng):
    """An endless seeded stream of (document id, section index)."""
    ids = sorted(docs)
    while True:
        did = rng.choice(ids)
        secs = [i for i, s in enumerate(docs[did]["sections"]) if len(s["text"].strip()) >= MIN_CHARS]
        if secs:
            yield did, rng.choice(secs)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--n", type=int, default=30)
    ap.add_argument("--max-draws", type=int, default=80)
    a = ap.parse_args()
    import dlc
    dlc.assert_tenant()
    docs = json.load(open(dconfig.SECTIONS, encoding="utf-8"))["docs"]
    rng = random.Random(SEED)
    usage = common.Usage()
    out, log, seen = [], [], set()
    for n, (did, i) in enumerate(draws(docs, rng)):
        if len(out) >= a.n or n >= a.max_draws:
            break
        if did in seen:  # one question per document, so no document is gold twice
            log.append({"draw": n, "doc": did, "sec": i, "skipped": "document already used"})
            continue
        s = docs[did]["sections"][i]
        path = " > ".join(s["path"]) or "(document start)"
        text = s["text"].strip()[:MAX_CHARS]
        q, err, _ = dprompts.ask(dlc, dprompts.KI_WRITE.format(title=docs[did]["title"], path=path, text=text),
                                 dconfig.READER, usage, max_tokens=2048)
        q = (q or "").strip().strip('"').splitlines()[0].strip() if q else None
        if not q:
            log.append({"draw": n, "doc": did, "sec": i, "failed": err})
            continue
        chk, err2, _ = dprompts.ask(dlc, dprompts.KI_CHECK.format(question=q, title=docs[did]["title"], path=path,
                                                                  text=text), dconfig.READER, usage, max_tokens=2048)
        ok = dprompts.yes(chk)
        log.append({"draw": n, "doc": did, "sec": i, "question": q, "check": (chk or err2 or "")[:300], "kept": ok})
        if ok:
            seen.add(did)
            out.append({"id": "k%02d" % (len(out) + 1), "kind": "known-item", "text": q, "gold": [did],
                        "section": {"chunk_id": s["chunk_id"], "index": i}})
    res = {"seed": SEED, "n": len(out), "draws": log, "questions": out, "usage": usage.to_json(),
           "served": [list(p) for p in usage.served()], "violations": [list(p) for p in usage.violations()]}
    os.makedirs(os.path.dirname(dconfig.KNOWN), exist_ok=True)
    with open(dconfig.KNOWN, "w", encoding="utf-8") as f:
        json.dump(res, f, ensure_ascii=False, indent=1)
    print(json.dumps({"kept": len(out), "draws": len(log), "rejected": sum(1 for l in log if l.get("kept") is False),
                      "usd_est": res["usage"]["total"]["usd_est"], "served": res["served"],
                      "violations": res["violations"]}, indent=1))
    if res["violations"]:
        sys.exit(2)


if __name__ == "__main__":
    main()
