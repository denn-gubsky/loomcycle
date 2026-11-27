"""The fixed model calls of Study C: reader, condense, query rewrite, listwise rerank, judge.

Every call goes through `ask`, which records what served it into a row's Usage and retries
once when the reply is empty (a thinking model on the local profile can spend its whole
budget thinking; the retry doubles max_tokens for that case).
"""
import json, re

READER = """Answer the question using only the sections below from UK government guidance.

{sections}

{question}

Reply in exactly this form:
ANSWER: <the answer as briefly as possible: a short phrase, preferably copied from the sections; for a yes/no question only "Yes" or "No"; "Unanswerable" if the sections do not contain the answer>
SOURCES: <the documents you used, each written as loom:doc/<id> with the id in square brackets before a section, separated by spaces>

The first line must start with ANSWER:. Write nothing else."""

CONDENSE = """Below are a question and an answer someone wrote to it.

{question}

Answer:
{answer}

Return the shortest span of the answer that states its answer to the question, copied from it. If the question is a yes/no question and the answer gives yes or no, return only "Yes" or "No". If the answer does not answer the question, return NO ANSWER. Output only the span, nothing else."""

REWRITE = """Rewrite the search request below three different ways for searching UK government guidance pages: vary the wording, name the likely topic, keep every condition that matters.

{question}

Return only a JSON list of three strings."""

RERANK = """A user asked:

{question}

Below are {n} numbered passages from UK government guidance. Choose the {keep} passages most useful for answering, most useful first.

{passages}

Return only a JSON list of {keep} passage numbers, e.g. [3, 1, 7]."""

JUDGE = """You grade an answer to a question about UK government guidance against the reference answers.

{question}

Reference answers (any one is correct): {gold}

Answer to grade:
{answer}

Reply with exactly one word on the first line: CORRECT (it gives a reference answer or an equivalent), PARTLY (partly right, or right with a wrong condition or extra wrong claim), or WRONG. Then one sentence saying why."""


def question_text(q):
    return "Situation: %s\nQuestion: %s" % (q["scenario"], q["question"])


def ask(lc, prompt, pair, usage, *, max_tokens=4096, timeout=600):
    """One user-turn chat at temperature 0. Returns (content or None, error, served pair)."""
    provider, model = pair
    msgs = [{"role": "user", "content": prompt}]
    served, err = pair, None
    for attempt in range(2):
        r = lc.chat(msgs, model=model, provider=provider, temperature=0, max_tokens=max_tokens, timeout=timeout)
        served = usage.add_chat(r, pair)
        content = (r.get("content") or "").strip()
        if content and not r.get("error"):
            return content, None, served
        err = r.get("error") or "empty content (stop_reason %s)" % r.get("stop_reason")
        if r.get("stop_reason") in ("max_tokens", "length"):
            max_tokens *= 2
    return None, err, served


def first_json_list(text):
    """The first parseable JSON array in a reply (models wrap them in prose or fences)."""
    for m in re.finditer(r"\[", text or ""):
        depth = 0
        for j in range(m.start(), len(text)):
            if text[j] == "[":
                depth += 1
            elif text[j] == "]":
                depth -= 1
                if depth == 0:
                    try:
                        v = json.loads(text[m.start():j + 1])
                    except ValueError:
                        break
                    if isinstance(v, list):
                        return v
                    break
    return None


VERDICT = re.compile(r"\b(CORRECT|PARTLY|WRONG)\b")


def verdict(text):
    # The first line is the verdict; the sentence after it may quote another label.
    for chunk in ((text or "").strip().split("\n", 1)[0], text or ""):
        m = VERDICT.search(chunk.upper())
        if m:
            return m.group(1)
    return None
