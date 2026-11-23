"""The fixed model calls of Study D: reader, query rewrite, listwise rerank, judge, and the
two known-item generation passes.

Same shapes as ../prompts.py (Study C), reworded for this corpus: the loomcycle project's own
documentation, and answers that name many documents (overview questions), so the reader
lists every document that belongs in the answer rather than one short span. `ask` and
`first_json_list` are reused from ../prompts.py unchanged.
"""
import os, re, sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
from prompts import ask, first_json_list  # noqa: E402,F401  (re-exported)

READER = """Answer the question using only the sections below from the loomcycle project's documentation (RFCs, guides, plans, research notes). Each section starts with [<document id>] and the document title and heading path.

{sections}

Question: {question}

Answer from the sections only. If the question asks what material exists on a subject, name each document that belongs in the answer with one line on what it covers. If the sections do not answer the question, say so.

Then end with one line:
SOURCES: <every document that belongs in the answer, each written as loom:doc/<id> with the id in square brackets before its sections, separated by spaces>"""

REWRITE = """Rewrite the search request below three different ways for searching a software project's documentation (design RFCs, guides, plans, research notes): vary the wording, name the likely technical topic, keep every condition that matters.

{question}

Return only a JSON list of three strings."""

RERANK = """A user asked:

{question}

Below are {n} numbered passages from a software project's documentation. Choose the {keep} passages most useful for answering, most useful first.

{passages}

Return only a JSON list of {keep} passage numbers, e.g. [3, 1, 7]."""

JUDGE_DOCS = """You grade an answer to a question about a software project's documentation against a reference.

Question: {question}

Reference: a complete answer draws on these documents (title: what it is about):
{gold}

Answer to grade:
{answer}

Reply with exactly one word on the first line: COMPLETE (it covers the reference material, or nearly all of it, and says nothing materially wrong), PARTIAL (it covers some of the reference material, or covers it with wrong claims), or WRONG (it misses the reference material or answers something else). Then one sentence saying why."""

JUDGE_TOPICS = """You grade an answer to a question about a software project's documentation against a reference.

Question: {question}

Reference: a complete answer covers these subjects:
{gold}

Answer to grade:
{answer}

Reply with exactly one word on the first line: COMPLETE (it covers these subjects, or nearly all of them, and says nothing materially wrong), PARTIAL (it covers some of them), or WRONG (it covers none of them or answers something else). Then one sentence saying why."""

# Known-item set: one section in, one question out; then a blind answerability check.
KI_WRITE = """Below is one section of a software project's documentation (document title, heading path, then the text).

{title} > {path}

{text}

Write ONE question that a developer on the project might ask and that this section answers specifically: about a concrete fact, decision, rule, number or behaviour stated in it. The question must be answerable from this section alone, must not be answerable from general knowledge, must not quote the document title or section heading verbatim, and must not say "this document" or "this section". Keep it under 30 words.

Return only the question."""

KI_CHECK = """Below are a question and one section of a software project's documentation.

Question: {question}

Section ({title} > {path}):
{text}

Does the section state the answer to the question, specifically enough that a reader would take this section as THE source for it (not a generic statement that many documents would make)? Reply with exactly YES or NO on the first line, then the answer in one short sentence if YES."""

VERDICT = re.compile(r"\b(COMPLETE|PARTIAL|WRONG)\b")


def verdict(text):
    # The first line is the verdict; the sentence after it may quote another label.
    for chunk in ((text or "").strip().split("\n", 1)[0], text or ""):
        m = VERDICT.search(chunk.upper())
        if m:
            return m.group(1)
    return None


def yes(text):
    first = (text or "").strip().split("\n", 1)[0].strip().upper()
    return first.startswith("YES")
