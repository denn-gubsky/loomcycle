"""RFC DM C3 — build the PolicyQA probe (privacy policies; questions written by legal
experts in their own words, each tied to the policy segment that answers it).

    python3 prepare.py --out <probe dir> [--split test]

Downloads PolicyQA (MIT, github.com/wasiahmad/PolicyQA) if absent and writes:
  policies.jsonl   one per policy: {id, site, markdown, segments}
  questions.jsonl  one per (policy, question): {qid, policy_id, question, gold}

WITHIN ONE POLICY. PolicyQA's questions are generic and repeated ("For what purpose
do you use my data?" appears 66 times in the test split), so a question means "in THIS
policy, which segment answers it". Each policy is therefore its own scope, and a
question can have several gold segments: every segment of that policy it was asked of.

A segment has no heading in the source. Each becomes a section headed by its number
alone — a heading with no letter in it is dropped from the index header, so every
chunk is indexed as "<site>\n<text>". That keeps the header arm honest: it gains the
document title and nothing invented.
"""
import argparse, json, os, urllib.request

URL = "https://raw.githubusercontent.com/wasiahmad/PolicyQA/main/data/%s.json"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--split", default="test")
    a = ap.parse_args()
    os.makedirs(a.out, exist_ok=True)
    raw = os.path.join(a.out, a.split + ".json")
    if not os.path.exists(raw):
        urllib.request.urlretrieve(URL % a.split, raw)
    data = json.load(open(raw))["data"]
    with open(os.path.join(a.out, "policies.jsonl"), "w") as pf, open(os.path.join(a.out, "questions.jsonl"), "w") as qf:
        for pi, doc in enumerate(data):
            pid = "pqa-%02d" % pi
            segs = [p["context"].strip() for p in doc["paragraphs"]]
            # Escape a segment line that would read as a heading, so the tree is the
            # one intended (the QASPER probe hit exactly this).
            body = lambda t: "\n".join(("\\" + l) if l.lstrip().startswith("#") else l for l in t.splitlines())
            md = "# %s\n\n" % doc["title"] + "\n\n".join("## %d\n\n%s" % (i + 1, body(s)) for i, s in enumerate(segs)) + "\n"
            pf.write(json.dumps({"id": pid, "site": doc["title"], "markdown": md, "segments": segs}) + "\n")
            gold = {}
            for si, p in enumerate(doc["paragraphs"]):
                for q in p["qas"]:
                    gold.setdefault(q["question"], set()).add(si)
            for qi, (question, g) in enumerate(sorted(gold.items())):
                qf.write(json.dumps({"qid": "%s-q%03d" % (pid, qi), "policy_id": pid, "question": question,
                                     "gold": sorted(g)}) + "\n")
    print("policies", len(data))


if __name__ == "__main__":
    main()
