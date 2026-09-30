"""Build the ConditionalQA probe: one Document per gov.uk page, twice, and the questions.

    python3 prepare.py --src <dir with documents.json, dev.json, train.json> --out <probe dir>
      -> docs.jsonl       {id, url, title, header_md, plain_md, sections: [{path, text}]}
      -> questions.jsonl  {qid, doc_id, query, scenario, question, answers, gold}

A page's sections are its heading tree in document order: section 0 is the page itself
(any text before the first heading), then one per heading. `gold` indexes the sections
holding the question's evidence.

The two variants carry the SAME text under different titles. `header_md` keeps the real
page title and headings, so the shipped index header ("<title> — <heading path>") is
built from them. `plain_md` numbers the page and its headings instead: a title with no
letter in it is dropped from the header (by the feature's own rule), so every chunk is
indexed under its content alone — the control the header is measured against.
"""
import argparse, html, json, os, re

HEADING = re.compile(r"^<h([1-6])>(.*)</h\1>$", re.S)
TAG = re.compile(r"<[^>]+>")


def text_of(el):
    """One content element as a Markdown line."""
    m = re.match(r"^<(p|li|tr|td|th|div|span)>(.*)</\1>$", el, re.S)
    kind, inner = (m.group(1), m.group(2)) if m else ("p", el)
    t = html.unescape(TAG.sub("", inner)).strip()
    if not t:
        return ""
    if t.startswith("#"):
        t = "\\" + t  # a line that reads as a heading must stay content
    return ("- " + t) if kind == "li" else t


def sections(page):
    """The page's heading tree in document order."""
    out = [{"path": [], "lines": [], "level": 0, "els": []}]
    stack = []  # (level, title)
    for el in page["contents"]:
        m = HEADING.match(el)
        if m:
            lvl, title = int(m.group(1)), html.unescape(TAG.sub("", m.group(2))).strip()
            stack = [s for s in stack if s[0] < lvl] + [(lvl, title)]
            out.append({"path": [s[1] for s in stack], "lines": [], "level": len(stack), "els": []})
            continue
        line = text_of(el)
        if line:
            out[-1]["lines"].append(line)
        out[-1]["els"].append(el)
    return out


def render(title, secs, numbered, number=""):
    # A numbered page is titled by its index: letterless, and unique, because import_md
    # names a document after its title.
    md = ["# " + (title if not numbered else number), ""]
    if secs[0]["lines"]:
        md += ["\n".join(secs[0]["lines"]), ""]
    for i, s in enumerate(secs[1:], 1):
        # One Markdown level per tree depth, so import_md builds the tree prepare.py
        # computed, whatever levels the page skipped.
        head = s["path"][-1] if not numbered else str(i)
        md += ["#" * (s["level"] + 1) + " " + head, ""]
        if s["lines"]:
            md += ["\n".join(s["lines"]), ""]
    return "\n".join(md)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--src", required=True)
    ap.add_argument("--out", required=True)
    a = ap.parse_args()
    os.makedirs(a.out, exist_ok=True)
    pages = json.load(open(os.path.join(a.src, "documents.json")))
    by_url, docs = {}, []
    for i, p in enumerate(pages):
        secs = sections(p)
        d = {"id": "cqa-%03d" % i, "url": p["url"], "title": p["title"],
             "header_md": render(p["title"], secs, False), "plain_md": render(p["title"], secs, True, "%03d" % i),
             "sections": [{"path": s["path"], "text": "\n".join(s["lines"])} for s in secs]}
        docs.append(d)
        by_url[p["url"]] = (d, secs)
    qs, dropped = [], {"not_answerable": 0, "no_answer": 0, "no_gold": 0, "unknown_page": 0}
    for split in ("dev", "train"):
        for q in json.load(open(os.path.join(a.src, split + ".json"))):
            if q["not_answerable"]:
                dropped["not_answerable"] += 1
                continue
            if not q["answers"]:
                dropped["no_answer"] += 1
                continue
            if q["url"] not in by_url:
                dropped["unknown_page"] += 1
                continue
            d, secs = by_url[q["url"]]
            gold = sorted({i for e in q["evidences"] for i, s in enumerate(secs) if e in s["els"] and s["lines"]})
            if not gold:
                dropped["no_gold"] += 1
                continue
            qs.append({"qid": q["id"], "doc_id": d["id"], "scenario": q["scenario"], "question": q["question"],
                       "query": (q["scenario"].strip() + " " + q["question"].strip()).strip(),
                       "answers": sorted({x[0] for x in q["answers"]}), "gold": gold})
    with open(os.path.join(a.out, "docs.jsonl"), "w") as f:
        for d in docs:
            f.write(json.dumps(d) + "\n")
    with open(os.path.join(a.out, "questions.jsonl"), "w") as f:
        for q in qs:
            f.write(json.dumps(q) + "\n")
    n_sec = sum(len(d["sections"]) for d in docs)
    print("docs", len(docs), "sections", n_sec, "questions", len(qs), "dropped", dropped)


if __name__ == "__main__":
    main()
