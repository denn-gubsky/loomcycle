# Verified writes (RFC CC)

Every fact loomcycle stores carries **the span of source text it was drawn from**. A judge checks the claim against that span, and a fact that fails stops being returned — without being deleted. On top of that, a lookup question can be answered with the stored claim quoted verbatim next to its citation, with no generated text in the answer path at all.

It is **off by default**. A deployment that never enables it behaves exactly as it did before.

## The failure this exists for

A memory pipeline writes what a model tells it to. Three things observed on a live store in one week:

- a curator proposed entity types whose counts and example titles were copied out of its own prompt,
- the extractor typed `location:user` as the subject of "the user resides in Cluj-Napoca",
- a fact recorded an outage duration and an affected-user count that appear in no transcript.

None of these look wrong. They are fluent, on-topic, and confidently stated, which is exactly why a better prompt does not fix them: **the component that fabricates is the component you would be asking not to.** Prompting was tried twice on the curator before this line was built.

So the check moved out of the prompt and into the store: a claim must carry the text it came from, and something other than its author must be able to compare the two.

## How it fits together

| stage | what it does | model? |
|---|---|---|
| **span** | the consolidator derives each fact's supporting sentence from the transcript | no |
| **deterministic gate** | rejects what word overlap alone can reject, at zero cost | no |
| **judge** | asks whether the quote actually carries the claim | yes, batched |
| **backfill** | sweeps facts stored before verification was on | yes, batched |
| **verbatim answers** | quotes a verified fact instead of generating one | no |

The span is **derived, not requested**. Asking the extractor for a `quote` field was measured and made it worse — it cost rule-following on a small local model, against a recorded baseline the unchanged prompt still hits. A derived span also cannot be fabricated: it is selected *from* the source, so it is in the source by construction.

## Turning it on

```yaml
memory:
  consolidation:
    verify_writes: true
```

The consolidation pass reads this at runtime through its capabilities report, the same way it reads the similarity bands — so it takes effect on the next pass with no restart and no change to the bundled agent.

**What it costs.** One model call per eight facts written, plus up to six calls per pass working through the backlog. The judge runs on a cheaper tier than the extractor, and it runs *after* the watermark advances, so nothing it does can cost a fact, block a pass, or make a chat be re-read.

## What a verdict means

The judge answers one question per fact in four words. The server owns what each is worth, so no caller can invent a scale:

| verdict | confidence | effect |
|---|---|---|
| `supported` | 0.9 | the quote carries the claim |
| `unclear` | 0.5 | it carries part of it, or cannot be told |
| `mistyped` | 0.4 | the claim is true but filed as the wrong kind of thing |
| `unsupported` | 0.0 | the quote does not carry the claim — **withheld** |

Facts below **0.25** are withheld. Only `unsupported` falls below it: a judge that is unsure is not evidence against a claim, and a fact withheld on a maybe is a fact nobody will notice is gone.

**Withheld is not deleted.** The claim stays in the store and stays readable with `include_refuted: true`, which returns it with the judge's stated reason. A wrong verdict is always recoverable by re-judging, never by restoring content something overwrote.

**Never assessed is not refuted.** A fact with no verdict has a NULL confidence and stays fully visible. That is what makes a judge outage degrade *verification* rather than emptying memory: no judge configured, unreachable, timed out, or answering in a shape the caller cannot read all leave facts exactly where they were.

## Where verification is visible — and where it is not

The fact surfaces withhold: `list_facts`, both `graph_recall` paths.

**`Memory.recall` does not.** Verification state lives on the entity tier, and nothing in the recall path reads it — making recall respect a verdict would mean either a second copy of one truth behind a store migration or a cross-tier join on every recall. The line is drawn where the state already is: the k/v tier records what was *said*, the entity tier is the curated view of what is *believed*.

The consequence, stated so it is not discovered later: **a plain `Memory.recall` still returns a refuted claim's text.**

## Measuring the judge — `loomcycle memory-eval-judge`

A judge is a classifier in the write path, and its two error directions are not symmetric. A **false refusal** withholds a true fact and the loss is invisible; a **false admission** keeps a fabrication, which is where the store already is. So the gate is on false refusals, and the fabrication cases are measured without blocking.

```bash
loomcycle memory-eval-judge --provider ollama-local --model gemma4:latest
```

| flag | effect |
|---|---|
| `--provider` / `--model` | required; a score is only meaningful against a named triple |
| `--effort` | defaults to the judge agent's configured effort |
| `--baseline <file>` | compare against a committed baseline and gate on regressions |
| `--update-baseline <file>` | record this run (refuses an incomplete or violation-bearing run) |
| `--batch <n>` | candidates per call; defaults to what the consolidator ships |
| `--no-gate` | report only |

The corpus measures **both directions** — known-good facts that must be admitted, the fabrications actually observed, ambiguous cases that must come back `unclear`, and mistyping in both directions. A corpus of nothing but fabrications is scored perfectly by a judge that refuses everything, so the harness refuses a one-directional corpus outright.

### What the measurement showed

Same prompt, same corpus, effort low:

| model | entailment | fabrication | partial | mistyping | false refusals |
|---|---|---|---|---|---|
| gemma4 (low tier — what ships) | 1.00 | 1.00 | 0.50 | 0.50 | **0** |
| qwen3.6 (the extractor's tier) | 1.00 | 1.00 | 0.00 | 0.00 | **1** |

The cheaper tier is not merely adequate here, it is **better on the axis that gates**. Both admit every plainly-supported claim and catch every fabrication. The difference is at the margin: the stronger model read "works on loomcycle and deploys it on TrueNAS" against a quote covering only the first half and answered `unsupported` — defensible as strictness, and a withheld true fact all the same.

Entailment against a supplied span rewards a literal reader, and a stronger model brings inference to a task that does not want any. If your own numbers are bad, change the tier in your **tier policy** — never by pinning a provider or model on the agent.

## Reading your coverage

```json
{"op": "verification_stats", "scope": "user"}
```

```json
{"facts": 412, "with_span": 380, "judged": 210, "supported": 190,
 "withheld": 12, "unverifiable_no_span": 32, "awaiting_judge": 170,
 "verified_share": 0.46}
```

The two unverified populations are counted separately because they mean different things: a fact with **no span** can never be verified by anyone — the transcript it came from may be gone — while one merely **awaiting a judge** can. `verified_share` is the number to quote when deciding whether to trust the verbatim path.

## Answering verbatim

```json
{"op": "verbatim_answer", "scope": "user", "query": "what is my github username"}
```

Returns the stored claim and the span it was verified against, with no generated text:

```json
{"answered": true, "answer": "The user's github username is denn.",
 "source": "my github username is denn", "confidence": 0.9, "score": 0.94}
```

**The error direction is inverted from the judge's.** Here the dangerous failure is the confident *wrong* answer, because verbatim delivery reads as authority — a synthesised sentence invites the doubt a quotation suppresses. So every ambiguity resolves to no answer, and each refusal says which:

- the best-matching fact is unverified, or was checked and not affirmed
- it does not clear the similarity floor
- a second fact matches about equally well

The top-ranked fact must itself be the verified one. Quoting a worse-matching fact because it happens to be verified would answer with something known not to be the closest thing in the store.

**`min_score` is not calibrated for your embedder.** Cosine scale is a property of the embedding model — the same pair measures 0.7675 on one and 0.9005 on another, which is why the consolidation bands are calibrated per deployment. The default is deliberately conservative, it is overridable per call, and the actual score is reported on every response including refusals so you can see what your embedder does before trusting it.

It only works for lookup. "What is my GitHub username" has a verbatim answer; "how should I structure this migration" does not. This is an opportunistic fast path, never the whole answer path.

## Known limits

- **`mistyped` is not reliable** on the local models measured. One misses the filing error; the other gets it backwards in both directions, calling a correctly filed fact mistyped. It only ever reduces confidence and never withholds, so it costs precision rather than data — but an absence of mistyping verdicts is not evidence of an absence of mistyped facts.
- **`Memory.recall` still returns refuted text**, as above.
- **The verbatim similarity floor is uncalibrated.** There is no `memory-calibrate` equivalent for it yet.
- **Facts stored before this shipped mostly have no span** and can only be counted, never verified. The backfill reports how many.
