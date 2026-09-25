#!/usr/bin/env python3
"""Build the QASPER probe corpus: sampled papers as Markdown + gold evidence per question.

Why QASPER: the papers have real section structure, so a heading-split import produces the
structural chunks this probe keeps fixed, and every answer is annotated with the evidence
PARAGRAPHS it came from — retrieval recall is scored against those with no judge at all.

Output (in --out):
  papers.jsonl     one row per paper: id, title, markdown, sections[{path, text}]
  questions.jsonl  one row per scoreable question: qid, paper_id, question, evidence[],
                   answers[] (for a later Answer-F1 pass)

A question is scoreable when at least one annotator answered it AND cited evidence. The
unanswerable ones are kept out of retrieval scoring: there is nothing to retrieve.
"""
import argparse
import json
import os
import random

FLOAT_PREFIX = "FLOAT SELECTED: "


def heading_levels(section_name):
    # QASPER flattens nesting into "Parent ::: Child ::: Grandchild".
    return [p.strip() for p in section_name.split(":::") if p.strip()]


def paper_markdown(p):
    """Render one paper. Returns (markdown, sections) where each section is the text that
    will land in ONE chunk body, keyed by its heading path — the evidence matcher needs to
    know which chunk a paragraph ended up in, and it must agree with the importer.
    """
    lines = ["# " + p["title"], ""]
    sections = []
    if p.get("abstract"):
        lines += ["## Abstract", "", p["abstract"].strip(), ""]
        sections.append({"path": ["Abstract"], "text": p["abstract"].strip()})
    opened = []  # heading path already emitted
    for s in p["full_text"]:
        path = heading_levels(s.get("section_name") or "") or ["Untitled"]
        # Emit any ancestor heading that has not been opened yet, so "A ::: B" under a
        # fresh "A" still nests. Depth caps at ###### (Markdown's last level).
        for depth in range(len(path)):
            if opened[: depth + 1] != path[: depth + 1]:
                lvl = min(depth + 2, 6)
                lines += ["#" * lvl + " " + path[depth], ""]
                opened = path[: depth + 1]
        paras = [x.strip() for x in s.get("paragraphs") or [] if x and x.strip()]
        lines += [x + "\n" for x in paras]
        if paras:
            sections.append({"path": path, "text": "\n\n".join(paras)})
    floats = [f for f in p.get("figures_and_tables") or [] if f.get("caption")]
    if floats:
        lines += ["## Figures and Tables", ""]
        caps = [f["caption"].strip() for f in floats]
        lines += [c + "\n" for c in caps]
        sections.append({"path": ["Figures and Tables"], "text": "\n\n".join(caps)})
    return "\n".join(lines).rstrip() + "\n", sections


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--data", required=True, help="qasper-dev-v0.3.json")
    ap.add_argument("--out", required=True)
    ap.add_argument("--papers", type=int, default=40)
    ap.add_argument("--seed", type=int, default=7)
    # A held-out set is the NEXT papers of the same shuffle: disjoint from the first set by
    # construction, and drawn from the same distribution.
    ap.add_argument("--skip", type=int, default=0, help="skip the first N usable papers")
    a = ap.parse_args()

    d = json.load(open(a.data))
    ids = sorted(d)
    random.Random(a.seed).shuffle(ids)
    os.makedirs(a.out, exist_ok=True)
    npq = 0
    with open(os.path.join(a.out, "papers.jsonl"), "w") as fp, \
         open(os.path.join(a.out, "questions.jsonl"), "w") as fq:
        taken = skipped = 0
        for pid in ids:
            if taken >= a.papers:
                break
            p = d[pid]
            md, sections = paper_markdown(p)
            qs = []
            for q in p["qas"]:
                ev, answers = set(), []
                for ans in q["answers"]:
                    x = ans["answer"]
                    if x.get("unanswerable"):
                        continue
                    for e in x.get("evidence") or []:
                        e = e.strip()
                        if e.startswith(FLOAT_PREFIX):
                            e = e[len(FLOAT_PREFIX):].strip()
                        if e:
                            ev.add(e)
                    answers.append({k: x.get(k) for k in ("extractive_spans", "yes_no", "free_form_answer")})
                if ev:
                    qs.append({"qid": q["question_id"], "paper_id": pid, "paper_title": p["title"],
                               "question": q["question"], "evidence": sorted(ev), "answers": answers})
            if not qs:
                continue  # a paper with nothing to score adds only distractors; keep the count honest
            if skipped < a.skip:
                skipped += 1
                continue
            taken += 1
            fp.write(json.dumps({"id": pid, "title": p["title"], "markdown": md, "sections": sections}) + "\n")
            for q in qs:
                fq.write(json.dumps(q) + "\n")
            npq += len(qs)
    print("papers", taken, "scoreable questions", npq)


if __name__ == "__main__":
    main()
