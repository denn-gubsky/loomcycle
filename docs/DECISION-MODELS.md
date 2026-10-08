# Decision models — typed questions, typed answers

A **decision model** reads a piece of state and answers typed questions about it: pick one option, yes or no, or a position on a scale. Each answer comes back with probabilities. The model writes no text, so there is no reply to parse.

loomcycle exposes decision models four ways: the `Decision` tool inside a run, `POST /v1/_decide`, the gRPC `Decide` RPC, and the MCP `decision` tool. All four take the same request and return the same answer.

## Why this, and not a chat model

- **Cost and speed.** A decision is one short call: a few hundred input tokens and a handful of output tokens. The same judgement from a chat model costs a full reply.
- **No parsing.** The answer is a field, not prose. It cannot be malformed and cannot name an option that was not offered.
- **What it is not for.** A decision model does not reason, look things up, summarise or write. It reads only the state it is given. Use a chat agent for anything that needs those.
- **The probabilities are not calibrated.** Measured on `nimble`, scores above 0.9 were right 42% of the time. Compare options within one answer; do not treat a fixed threshold as a guarantee without measuring it on your own data.

## Setting it up

Decision models are declared like any other model, as `models:` aliases tagged with their kind, and listed in a `decision:` block.

```yaml
models:
  decide:      { provider: ollama-local, model: nimble, kind: decision }
  decide-deep: { provider: ollama-local, model: clef,   kind: decision }

decision:
  default: decide                 # the model a call gets when it names none
  models: [decide, decide-deep]   # optional; omitted = every alias tagged kind: decision
  timeout_ms: 30000               # default 30000; must cover a model reload on a shared GPU
  max_concurrent: 4               # default 4, per provider
```

- **Providers.** Today the only provider that serves decision models is Ollama, version 0.35 or later (`nimble`, `clef`, `clef-flash`, `tev1`). Config load fails if the block names a provider with no decision support.
- **No block, no capability.** Without a `decision:` block nothing changes: the tool is not offered and the endpoints answer `decision_not_configured`.
- **A plain model name** (not an alias) is allowed in the list when `decision.provider` is set.
- **A change needs a restart**, as the memory reranker's block does.
- **`max_concurrent` is per provider:** all the block's models on one provider share that bound.

### Model kinds

A `models:` alias can declare what the model is for: `kind: chat` (the default), `kind: decision` or `kind: embedder`.

| where an alias is used | kind it must have |
|---|---|
| an agent's `model:`, a tier candidate, the listwise memory reranker, the unit generator | `chat` |
| the `decision:` block, a memory reranker with `kind: decision` | `decision` |
| `memory.embedder.model` | `embedder` |

- **A tagged alias in the wrong place fails config load.** The error names the alias, its kind and the place.
- **An untagged alias used as a decision model or an embedder still loads, with a warning** at boot and in `loomcycle validate` that names the alias and the tag to add. A later release will make this a failure, so tag them now.
- An untagged alias used as a chat model is the normal case and says nothing.
- `GET /v1/_models` reports each tagged alias's kind.
- The kind is a check on where an alias may be written. It is never a routing input.

### Letting an agent use it

- List `Decision` in the agent's `tools:`.
- Optionally narrow which models the agent may ask:

```yaml
agents:
  triage:
    tools: [Decision]
    decision:
      default: decide
      models: [decide]
```

- The agent's block can only narrow the operator's list. A name outside it fails config load, or is refused when the agent is created or forked at runtime.
- With no `model` in a call, the agent's default is used; failing that, the operator's default if the agent may name it; failing that, the first of the agent's list.
- The block is part of the agent's content hash when set. A sub-agent uses its own definition's block, not its parent's.
- An agent that lists `Decision` on a deployment with no `decision:` block is simply not offered the tool, and `loomcycle validate` says so.

## The request

```json
{
  "model": "decide",
  "state": {"ticket": "My invoice for March was charged twice and I want my money back.", "customer_tier": "gold"},
  "questions": {
    "route":  {"type": "choice", "instructions": "Which team should handle this ticket?",
               "criteria": {"billing": "invoices, refunds, charges", "support": "bugs, outages, login problems", "sales": "upgrades, quotes"}},
    "urgent": {"type": "noul", "instructions": "Does this ticket need a reply within the hour?"},
    "detail": {"type": "score", "instructions": "How complete is the problem report?",
               "criteria": ["no detail", "some detail", "everything needed"]}
  }
}
```

- `model` is optional. Omit it for the default.
- `state` is any JSON object, nested values included. Every question is answered about the same state.
- `questions` holds 1 to 64 named questions. `instructions` is required on each.

| type | `criteria` | what it asks |
|---|---|---|
| `choice` | required: an object mapping each option to a description, or to `null` when the option explains itself. 2 to 26 options. | pick one option |
| `noul` | optional: an object describing `"true"` and/or `"false"` | yes or no |
| `score` | required: an array of level descriptions, lowest first. 2 to 26 levels. | a position on the scale |

## The answer

```json
{
  "model": "decide", "provider": "ollama-local", "served_model": "nimble",
  "answers": {
    "detail": {"type": "score", "score": 1.874,
               "legend": {"0": "no detail", "1": "some detail", "2": "everything needed"},
               "probabilities": {"0": 0.039, "1": 0.048, "2": 0.913}, "confidence": 0.677},
    "route":  {"type": "choice", "choice": "billing",
               "probabilities": {"billing": 0.983, "support": 0.007, "sales": 0.011}, "confidence": 0.911},
    "urgent": {"type": "noul", "noul": 0.316}
  },
  "usage": {"input_tokens": 1059, "output_tokens": 4}
}
```

- Each answer is returned exactly as the model gave it. Nothing is renamed or dropped.
- `choice`: the option picked, a probability per option, and `confidence`.
- `noul`: the probability that the answer is yes, from 0 to 1. `0` is a definite no, not a missing answer.
- `score`: the expected position counted from 0 (`1.874` is just below the top level), a `legend` of position to description, and a probability per position.
- `confidence` appears to measure how concentrated the probabilities are: near 1 when one option holds almost all the weight. This is inferred from captured answers, not documented by the model's vendor.
- The same question can come back differently when asked alongside others. Keep the wording and the grouping stable for answers you compare.

## Limits and errors

- 1 to 64 questions per call; 2 to 26 options or levels.
- The whole request must fit the model's context (`nimble` 8,194 tokens, `clef` 16,384) and Ollama's 64 KiB request cap. **Nothing is shortened for you.**

| code | what happened | HTTP | gRPC |
|---|---|---|---|
| `invalid_input`, `bad_question`, `bad_options`, `too_many_questions` | the request is not the shape above | 400 | `InvalidArgument` |
| `model_not_allowed` | the model named is not on the allowed list | 400 | `InvalidArgument` |
| `prompt_too_large` | the request does not fit the model | 413 | `InvalidArgument` |
| `operator_key_restricted` | the caller may not use the operator's provider key and has none of its own | 403 | `PermissionDenied` |
| `token_limit_exceeded` | the caller is at a hard token budget | 429 | `ResourceExhausted` |
| `decision_not_configured` | the deployment declares no decision models | 503 | `FailedPrecondition` |
| `model_not_found` | the provider does not serve the model the alias names | 502 | `FailedPrecondition` |
| `timeout` | the model did not answer in time | 504 | `DeadlineExceeded` |
| `call_failed` | the call did not complete | 502 | `Unavailable` |

- Inside a run, a failed call's text starts `Decision: <code>: ` followed by what to do.
- Over HTTP the body is `{code, error, errorCategory, isRetryable, …}`. Over gRPC the status carries an `ErrorInfo` whose reason is the code.

## The four surfaces

| surface | how | who may call |
|---|---|---|
| in a run | the `Decision` tool | an agent that lists `Decision` |
| HTTP | `POST /v1/_decide`; `GET /v1/_decide/models` lists the allowed models and their limits | a token with `runs:create` |
| gRPC | `Decide`, `ListDecisionModels` | a token with `runs:create` |
| MCP | the `decision` tool | a session whose token has `runs:create`; not an isolated user's session |

- The request and the answer are the same JSON everywhere. Over gRPC, `state`, each question's `criteria` and each answer travel as JSON bytes so numbers arrive unchanged.
- A code agent calls the tool as `JSON.parse(Decision({state, questions}))`.
- Agents can read the full format with worked examples through `Context op=help topic=Decision`.
- A decision call is not held back by a runtime pause.

## Who pays

- **Inside a run:** the call's input and output tokens are charged to the run, and count against its token budgets.
- **Outside a run:** the call is charged to the caller's own tenant and subject, on a usage row with no run. It counts against the operator, tenant and user budgets, and shows in `GET /v1/_usage` under the caller's tenant.
- **Budgets:** a caller at a hard budget is refused before the model is asked. The call that crosses a budget completes.
- **Price:** from the `pricing:` table by provider and served model, with input and output rates. A local model with no price entry costs zero.
- **Provider key:** with `LOOMCYCLE_OPERATOR_KEY_RESTRICTION` on, a caller without the `providers:operator-key` scope is served only on its own stored key for the provider, and refused if it has none.
- **No authentication (open mode):** run-less calls are filed under the shared tenant with no user.

## What it is measured to do

Reranking search results is the one use measured so far (see the Documents guide, "The opt-in rerank").

- **Document rerank, ConditionalQA (2,515 questions):** `nimble` reached recall@5 0.933, level with a 27B chat model's rerank (0.933), with a median rerank time of 1.45 s.
- **Memory rerank, LoCoMo (300 questions):** `nimble` 0.750 recall@5, `clef` 0.739, `clef-flash` 0.711. `nimble` is the recommended default.

No other use (routing, gating, grading) has been measured. Measure on your own data before relying on one.
