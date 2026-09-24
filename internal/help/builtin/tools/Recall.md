---
name: Recall
description: "Recall tool — fetch, verbatim, the original turns of THIS conversation that were summarised away, by a plain-language query; also searches your durable memory. Read-only."
---
The `Recall` tool gets back details from earlier in the conversation you are
in that are no longer in front of you. As a conversation grows, older turns
are condensed into a summary and their specifics — exact values, names,
numbers, ids, file paths, quoted lines — drop out. Recall searches the
ORIGINAL turns by meaning and returns the closest ones word for word. It also
searches your durable memory in the same call, so a fact learned in an earlier
session can come back too.

Describe what you need in plain language, the way you would ask a colleague:
"the deployment token the user gave", "the revenue figure from the Q2 report".
**You do not need the exact wording**, and there is no id to pass. If you are
about to state a specific detail from earlier and are not sure it is still in
your context, recall it rather than guess.

## When to use it — and when not

- **Use Recall** for a detail from earlier in THIS conversation, or a fact you
  may have remembered before.
- **Not for another conversation.** Finding and reading a past chat is
  `History` (`search`, then `get`).
- **Not to store anything.** Recall is read-only. Keeping a fact is
  `Memory op=set` or `Memory op=add`.
- **Not for a targeted memory search.** Recall looks in every memory scope you
  are granted at once and returns only text. When you need one scope, the
  fact's id, its kind or its source span, use `Memory op=recall`; to reach the
  turns a fact came from, follow it with `History op=window`.

## Arguments

- `query` (required) — a plain-language description of the detail you need.
- `top_k` — how many results to return (default 3, at most 10; larger values
  are cut to 10).

No other argument is accepted.

## What it searches

1. The turns of this run that have been summarised away. This part is on only
   when the agent's context settings enable it and an embedder is configured;
   before anything has been summarised it holds nothing.
2. Your durable memory, in each of the `user`, `agent` and `tenant` scopes the
   agent is granted. Scopes you are not granted are skipped silently.

Both are merged and ranked by similarity to the query, best match first — not
by time.

## Returns

`{recalled: [{text, score, source}]}`, best match first. `source` is `run` for
an original turn of this conversation and `memory` for a durable fact. `text`
is the original wording.

When nothing matches, the result is the plain text `No earlier details matched
that query.` — that is not an error. Rephrase once with different words; if it
still finds nothing, the detail was never said or stored, so say so rather
than guess.

## Errors

- `Recall: missing required field: query` — pass a non-empty `query`.
- `Recall: invalid input: ...` — the arguments were not valid JSON, or a field
  had the wrong type (for example `top_k` as a string).

## Examples

Recover a value the user gave earlier in this conversation:

```json
{"query": "the staging database hostname the user gave"}
```

```json result
{"recalled": [
  {"text": "Staging DB is at pg-staging-02.internal, port 6432.", "score": 0.81, "source": "run"},
  {"text": "The user's team deploys to staging every Thursday.", "score": 0.44, "source": "memory"}]}
```

Ask for more candidates when the detail could have come up several times:

```json
{"query": "the budget figures discussed for Q3", "top_k": 6}
```
