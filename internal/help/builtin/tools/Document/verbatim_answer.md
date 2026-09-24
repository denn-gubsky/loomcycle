---
name: Document/verbatim_answer
description: "Document op=verbatim_answer — answer a lookup question with one verified stored fact, quoted exactly with its source, or decline and say why."
---
`verbatim_answer` tries to answer a LOOKUP question ("what is my GitHub
username?") with a stored fact returned word for word, together with the source
quote it was verified against — no generated text. It answers only when the
closest matching fact is clearly the closest, close enough, and judged
`supported`; otherwise it declines and says why. **When `answered` is `false`,
fall back to ordinary recall (`search`, `list_facts`, Memory) — do not present
the declined fact as the answer.**

## Arguments

- `query` (required) — the lookup question.
- `min_score` — how close the best match must be, 0 to 1 (default 0.6). The
  scale depends on the embedding model; the response always reports the real
  score, so tune against it.
- `scope` — `user` (default), `agent` or `tenant`. One scope per call.

It looks only at facts (plain document prose is ignored) and needs an embedder.

## Returns

Answered: `{answered: true, answer, source, chunk_id, score, confidence,
judged_at}` — `answer` is the fact's body, `source` its verified quote.

Declined: `{answered: false, reason, chunk_id?, score?}`. The reasons:

| reason says | what to do |
|---|---|
| no stored fact matches the question | nothing is stored; recall elsewhere or ask |
| not close enough to be quoted (with `min_score`) | the match is weak; use ordinary recall |
| has not been verified | the fact exists but is unjudged; `judge_fact` it, or answer from recall |
| checked and not affirmed outright | its verdict is below `supported` |
| carries no source span | there is nothing to cite |
| two stored facts match ... about equally well (with `runner_up`) | ask a sharper question |

## Errors

- `verbatim_answer: missing required field: query ...`.
- `verbatim_answer: requires a configured embedder / vector memory` — this
  server cannot answer; retrying is pointless.
- `verbatim_answer: embed: ...` — the embedder failed; retrying later is
  reasonable.

## Examples

Answer a lookup question from the user's facts:

```json
{"op": "verbatim_answer", "query": "What is my GitHub username?"}
```

```json result
{"answered": true, "answer": "The user's GitHub username is ada-l.", "source": "my github handle is ada-l", "chunk_id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00", "score": 0.83, "confidence": 0.9, "judged_at": 1772409600000000000}
```

A stricter match threshold against the tenant's shared facts:

```json
{"op": "verbatim_answer", "query": "Which port does the staging database listen on?", "scope": "tenant", "min_score": 0.75}
```

```json result
{"answered": false, "chunk_id": "7a6b5c4d3e2f10a9b8c7d6e5f4a3b2c1", "score": 0.71, "reason": "the closest stored fact is not close enough to be quoted as an answer", "min_score": 0.75}
```
