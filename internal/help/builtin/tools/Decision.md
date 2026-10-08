---
name: Decision
description: "Decision tool — ask a decision model typed questions (choice, noul, score) about text you supply and get each answer with probabilities; input and output formats with a worked example per question type."
---
The `Decision` tool asks a **decision model** typed questions about a piece of
text and returns each answer with its probabilities. A decision model does not
write: it reads what you give it and answers a choice, a yes/no, or a score.
There is no reply to parse and the call costs a few output tokens.

## When to use it — and when not

- **Use it** to route, gate, rank or grade text you already have: which team
  takes this ticket, is this message urgent, how complete is this report.
- **Not for anything that needs reasoning or written output.** It cannot
  explain, summarise, extract a value or follow steps. Do that yourself.
- **Not for lookup.** The model reads only `state`. It knows nothing about the
  conversation, your memory or the world beyond what you put there.
- **Not as proof.** The numbers are a judgement, not a measurement — see
  "Reading the numbers".

## Arguments

- `state` (required) — a JSON object holding what the questions are about. Any
  shape: strings, numbers, nested objects, arrays. Every question in the call
  is answered about this same state.
- `questions` (required) — an object of 1 to 64 questions. Each key is a name
  you choose; the answer comes back under the same name. Each question is
  `{"type", "instructions", "criteria"}`.
- `model` — which decision model answers. Omit it for the default.

No other argument is accepted, and there is no `op`.

## The three question types

`instructions` is required for all three: the question, in plain words.

- `choice` — pick one of 2 to 26 options. `criteria` (required) is an object:
  each key is an option, each value describes it, or is `null` when the key
  explains itself. The key is what comes back, so use your own words.
- `noul` — yes or no. `criteria` is optional: an object describing `"true"`
  and/or `"false"` when the plain question is not enough.
- `score` — a position on a scale of 2 to 26 levels. `criteria` (required) is
  an ARRAY of level descriptions, lowest first.

## Returns

`{model, provider, served_model, answers, usage}`:

- `model` — the name that answered, as this deployment lists it (the default's
  name when you omitted `model`). `provider` and `served_model` say what that
  name resolved to.
- `answers` — one answer per question, under the question's name (in name
  order), exactly as the model gave it. Each has `type`, then:
  - choice: `choice` (the option picked), `probabilities` (one per option,
    summing to 1), `confidence`.
  - noul: `noul` — the probability that the answer is YES, from 0 to 1. `0` is
    a definite no, not a missing answer. There is no separate true/false field:
    above 0.5 leans yes.
  - score: `score` (the expected position, counted from 0 — `1.7` is between
    level 1 and level 2), `legend` (position → your description),
    `probabilities` (one per position), `confidence`.
  - `confidence` — how concentrated the probabilities are: near 1 when one
    option holds almost all the weight, near 0 when they are spread evenly.
- `usage` — `input_tokens` and `output_tokens` of this call. They are charged
  to your run.

## Reading the numbers

**The probabilities are not calibrated.** 0.9 does not mean "right nine times
in ten".

- Compare options **within one answer**: which is highest, and by how much.
- Do not treat a fixed threshold as a guarantee. If you gate on one, treat the
  middle of the range as "unsure" and handle it another way.
- The same question can come back differently when asked alongside others or
  with different wording. Below, `urgent` alone is 0.381 and 0.316 next to two
  other questions. Keep the wording and the grouping stable for answers you
  compare.

## Limits

- 1 to 64 questions per call; 2 to 26 options for a choice, 2 to 26 levels for
  a score.
- The whole request — state, instructions and criteria — must fit the model's
  context. **It is never shortened for you**: an over-long request is refused
  with `prompt_too_large`, and you shorten it.

## Picking a model

Omit `model`. The default is the one chosen for this deployment and this agent.
Name one only when you were told to. The names are the ones this deployment
lists, which need not be a model's public name; a refused name comes back with
the list you may use.

## Errors

A failed call's `error` starts `Decision: <code>: `, and the code tells you what
to do.

| Code | What happened | What to do |
|---|---|---|
| `invalid_input` | `state` is missing or not an object, or the arguments are not the shape above. | Send `state` and `questions` as shown below. |
| `bad_question` | A question has no `instructions`, an unknown `type`, or `criteria` of the wrong shape for its type. | Fix that question; the message names it. |
| `bad_options` | A choice or score has fewer than 2 or more than 26 options. | Add or remove options, or split the question. |
| `too_many_questions` | More than 64 questions. | Split them across calls. |
| `prompt_too_large` | The request does not fit the model's context. | Shorten the state or the criteria, or ask fewer questions per call. |
| `model_not_allowed` | The `model` you named is not one you may ask. | Omit `model`, or use a name from the list in the message. |
| `model_not_found` | The provider does not serve that model. | Use another listed name; the same call will fail again. |
| `timeout` | The model did not answer in time. | Send the same call again; if it repeats, make it smaller. |
| `call_failed` | The call did not complete. | Try once more, then continue without the decision and say so. |
| `operator_key_restricted` | This run may not use the operator's provider key and has none of its own. | Do not retry. Decide another way and report it. |
| `decision_not_configured` | This deployment has no decision models. | Do not retry. Decide another way. |

## Examples

Each request is followed by the complete result it returned.

A `noul` — one yes/no question:

```json
{"state": {"ticket": "My invoice for March was charged twice and I want my money back.", "customer_tier": "gold"},
 "questions": {
   "urgent": {"type": "noul", "instructions": "Does this ticket need a reply within the hour?"}}}
```

```json result
{"model": "decide", "provider": "ollama-local", "served_model": "nimble",
 "answers": {
   "urgent": {"type": "noul", "noul": 0.381}},
 "usage": {"input_tokens": 187, "output_tokens": 1}}
```

A `choice` — the options are the keys of `criteria`:

```json
{"state": {"ticket": "My invoice for March was charged twice and I want my money back.", "customer_tier": "gold"},
 "questions": {
   "route": {"type": "choice", "instructions": "Which team should handle this ticket?",
             "criteria": {"billing": "invoices, refunds, charges", "support": "bugs, outages, login problems", "sales": "upgrades, quotes"}}}}
```

```json result
{"model": "decide", "provider": "ollama-local", "served_model": "nimble",
 "answers": {
   "route": {"type": "choice", "choice": "billing",
             "probabilities": {"billing": 0.996, "support": 0.001, "sales": 0.002}, "confidence": 0.975}},
 "usage": {"input_tokens": 217, "output_tokens": 1}}
```

A `score` — `criteria` is an array, lowest level first:

```json
{"state": {"ticket": "My invoice for March was charged twice and I want my money back.", "customer_tier": "gold"},
 "questions": {
   "detail": {"type": "score", "instructions": "How complete is the problem report?",
              "criteria": ["no detail", "some detail", "everything needed"]}}}
```

```json result
{"model": "decide", "provider": "ollama-local", "served_model": "nimble",
 "answers": {
   "detail": {"type": "score", "score": 1.704,
              "legend": {"0": "no detail", "1": "some detail", "2": "everything needed"},
              "probabilities": {"0": 0.03, "1": 0.236, "2": 0.734}, "confidence": 0.388}},
 "usage": {"input_tokens": 207, "output_tokens": 1}}
```

All three in one call, about the same state:

```json
{"state": {"ticket": "My invoice for March was charged twice and I want my money back.", "customer_tier": "gold"},
 "questions": {
   "route": {"type": "choice", "instructions": "Which team should handle this ticket?",
             "criteria": {"billing": "invoices, refunds, charges", "support": "bugs, outages, login problems", "sales": "upgrades, quotes"}},
   "urgent": {"type": "noul", "instructions": "Does this ticket need a reply within the hour?"},
   "detail": {"type": "score", "instructions": "How complete is the problem report?",
              "criteria": ["no detail", "some detail", "everything needed"]}}}
```

```json result
{"model": "decide", "provider": "ollama-local", "served_model": "nimble",
 "answers": {
   "detail": {"type": "score", "score": 1.874,
              "legend": {"0": "no detail", "1": "some detail", "2": "everything needed"},
              "probabilities": {"0": 0.039, "1": 0.048, "2": 0.913}, "confidence": 0.677},
   "route": {"type": "choice", "choice": "billing",
             "probabilities": {"billing": 0.983, "support": 0.007, "sales": 0.011}, "confidence": 0.911},
   "urgent": {"type": "noul", "noul": 0.316}},
 "usage": {"input_tokens": 1059, "output_tokens": 4}}
```

The optional parts: a named `model`, a nested `state`, options that explain
themselves (`null`), and a `noul` with both sides described:

```json
{"model": "decide-deep",
 "state": {"message": {"subject": "Re: contract", "body": "Please send the signed copy by Friday."},
           "thread": ["Can you sign this?", "Signed, attached."]},
 "questions": {
   "language": {"type": "choice", "instructions": "What language is the message written in?",
                "criteria": {"english": null, "german": null, "other": "any other language"}},
   "needs_action": {"type": "noul", "instructions": "Does the sender expect us to do something?",
                    "criteria": {"true": "a request, a deadline or a question for us", "false": "information only"}}}}
```

From a code agent the result is a string: `JSON.parse(Decision({state, questions}))`.
