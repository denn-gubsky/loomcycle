#!/usr/bin/env python3
"""Build a Natural Questions probe corpus in the same shape prepare.py gives QASPER.

Why NQ: long Wikipedia pages with real heading structure — long enough that a whole-page
prompt stops being affordable per chunk — and two kinds of gold per question: the long
answer (a paragraph: retrieval recall with no judge) and short answers (exact match / F1,
a less saturated answer metric than QASPER's).

Input: one HF parquet shard of the NQ validation split (needs pyarrow).
Output (--out): papers.jsonl (id, title, markdown, sections[{path, text}]) and
questions.jsonl (qid, paper_id, paper_title, question, evidence[], answers[]), the field
names prepare.py uses, so every downstream script runs unchanged.

A question is kept when an annotator gave a PARAGRAPH long answer plus a short answer, and
that paragraph can be found in a converted section. NQ's tokens and the extracted text
disagree on spacing and punctuation, so the check compares lowercase word sequences, and
the evidence stored is the section's own paragraph — the text the importer will index.
"""
import argparse
import html
import json
import os
import random
import re
from html.parser import HTMLParser

import pyarrow.parquet as pq

SKIP_TAGS = {"table", "style", "script", "sup", "noscript", "math", "figure"}
SKIP_CLASS = ("navbox", "reflist", "references", "mw-editsection", "toc", "infobox",
              "thumb", "hatnote", "metadata", "vertical-navbox", "sistersitebox", "mbox")
DROP_SECTIONS = {"references", "external links", "see also", "notes", "further reading",
                 "bibliography", "sources", "citations", "footnotes", "notes and references"}
HEADING_LINE = re.compile(r"^(\s{0,3})#", re.M)
WORD = re.compile(r"[a-z0-9]+")


def words(s):
    return WORD.findall(s.lower())


class Page(HTMLParser):
    """Collect (level, heading) and paragraph/list blocks from the article body only."""

    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.in_body = 0          # depth inside #mw-content-text (0 = outside)
        self.skip = 0             # depth inside a skipped element
        self.stack = []           # open tags, to find where body / skip regions end
        self.cur = None           # ("h", level, [text]) or ("p", [text]) being built
        self.blocks = []

    def handle_starttag(self, tag, attrs):
        a = dict(attrs)
        cls = a.get("class") or ""
        entering_body = a.get("id") == "mw-content-text"
        skipping = tag in SKIP_TAGS or any(c in cls for c in SKIP_CLASS)
        if tag in ("br", "img", "hr", "meta", "link", "input", "wbr"):
            return
        self.stack.append((tag, entering_body, skipping))
        if entering_body:
            self.in_body += 1
        if skipping:
            self.skip += 1
        if not self.in_body or self.skip:
            return
        if re.fullmatch(r"h[1-6]", tag):
            self._flush()
            self.cur = ("h", int(tag[1]), [])
        elif tag in ("p", "li", "dd"):
            self._flush()
            self.cur = ("p", [])

    def handle_endtag(self, tag):
        # pop to the matching open tag (Wikipedia HTML is not always well nested)
        for i in range(len(self.stack) - 1, -1, -1):
            if self.stack[i][0] == tag:
                for t, body, sk in self.stack[i:]:
                    if body:
                        self.in_body -= 1
                    if sk:
                        self.skip -= 1
                del self.stack[i:]
                break
        if re.fullmatch(r"h[1-6]", tag) or tag in ("p", "li", "dd"):
            self._flush()

    def handle_data(self, data):
        if self.in_body and not self.skip and self.cur is not None:
            self.cur[-1].append(data)

    def _flush(self):
        if self.cur is None:
            return
        text = re.sub(r"\s+", " ", "".join(self.cur[-1])).strip()
        if text:
            if self.cur[0] == "h":
                self.blocks.append(("h", self.cur[1], re.sub(r"\[edit\]$", "", text).strip()))
            else:
                self.blocks.append(("p", text))
        self.cur = None


def page_sections(title, raw_html):
    """Sections keyed by heading path. H2 is the top level (H1 is the page title)."""
    p = Page()
    p.feed(raw_html)
    p._flush()
    sections, path, buf, dropped = [], ["Introduction"], [], False
    def close():
        if buf and not dropped:
            sections.append({"path": list(path), "text": "\n\n".join(buf)})
    for b in p.blocks:
        if b[0] == "h":
            if b[1] < 2:
                continue
            close()
            buf = []
            depth = b[1] - 2
            path = (path if path != ["Introduction"] else [])[:depth] + [b[2]]
            dropped = path[0].lower() in DROP_SECTIONS
        else:
            buf.append(HEADING_LINE.sub(r"\1\\#", b[1]))
    close()
    return sections


def render(title, sections):
    lines, opened = ["# " + title, ""], []
    for s in sections:
        for depth in range(len(s["path"])):
            if opened[: depth + 1] != s["path"][: depth + 1]:
                lines += ["#" * min(depth + 2, 6) + " " + s["path"][depth], ""]
                opened = s["path"][: depth + 1]
        lines += [x + "\n" for x in s["text"].split("\n\n")]
    return "\n".join(lines).rstrip() + "\n"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--parquet", required=True, nargs="+", help="one or more validation shards")
    ap.add_argument("--out", required=True)
    ap.add_argument("--pages", type=int, default=0, help="keep at most N pages (0 = all eligible)")
    ap.add_argument("--seed", type=int, default=7)
    ap.add_argument("--skip", type=int, default=0, help="skip the first N eligible pages (held-out)")
    a = ap.parse_args()
    rows = []
    for path in a.parquet:
        rows += pq.read_table(path, columns=["id", "document", "question", "annotations"]).to_pylist()
    random.Random(a.seed).shuffle(rows)
    os.makedirs(a.out, exist_ok=True)
    seen_titles, kept, skipped, stats = set(), 0, 0, {"no_para_short": 0, "not_found": 0, "dup_page": 0}
    with open(os.path.join(a.out, "papers.jsonl"), "w") as fp, open(os.path.join(a.out, "questions.jsonl"), "w") as fq:
        for r in rows:
            if a.pages and kept >= a.pages:
                break
            doc, ann = r["document"], r["annotations"]
            toks, is_html = doc["tokens"]["token"], doc["tokens"]["is_html"]
            golds, answers = [], set()
            for la, sa in zip(ann["long_answer"], ann["short_answers"]):
                if la["start_token"] < 0 or not sa["text"]:
                    continue
                if toks[la["start_token"]].lower() != "<p>":
                    continue  # tables and lists as long answers: dropped with the tables
                golds.append(" ".join(x for x, h in zip(toks[la["start_token"]:la["end_token"]],
                                                        is_html[la["start_token"]:la["end_token"]]) if not h))
                answers.update(html.unescape(x) for x in sa["text"])
            if not golds:
                stats["no_para_short"] += 1
                continue
            title = html.unescape(doc["title"])
            if title in seen_titles:
                stats["dup_page"] += 1  # one page, one document: a second copy would split its hits
                continue
            sections = page_sections(title, doc["html"])
            evidence = []
            for g in golds:
                gw = " ".join(words(g))
                for s in sections:
                    for para in s["text"].split("\n\n"):
                        if gw and gw in " ".join(words(para)):
                            evidence.append(para)
                            break
            if not evidence:
                stats["not_found"] += 1
                continue
            if skipped < a.skip:
                skipped += 1
                continue
            seen_titles.add(title)
            pid = "nq-" + r["id"]
            fp.write(json.dumps({"id": pid, "title": title, "markdown": render(title, sections),
                                 "sections": sections}) + "\n")
            fq.write(json.dumps({"qid": r["id"], "paper_id": pid, "paper_title": title,
                                 "question": r["question"]["text"], "evidence": sorted(set(evidence)),
                                 "answers": [{"extractive_spans": [x]} for x in sorted(answers)]}) + "\n")
            kept += 1
    print("kept", kept, "skipped(held-out offset)", skipped, stats)


if __name__ == "__main__":
    main()
