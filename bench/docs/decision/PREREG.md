# Decision-model reranking on ConditionalQA (pre-registration)

Written and committed **before** any pool is fetched or any rerank is run. Nothing below
may change after the first result is seen; any change is an amendment, dated and
committed before the run it affects.

## Question

The shipped rerank is one listwise call to a general local LLM (phase M: `qwen3.8`, R@5
0.860 → 0.933 on ConditionalQA). It asks for a JSON array of numbers, which a server has to
repair and which can fail as `unparseable`.

**Decision models** (TypeSafe's Jev design, served by Ollama ≥ 0.35 at `/v1/systemone`)
return typed answers with probabilities and never generate text. This probe asks three
things:

1. Can a decision model rerank as well as the shipped one?
2. How fast is it on our hardware?
3. Are its probabilities good enough to say "nothing in this pool answers the question"?
   Retrieval scores cannot say that (the QPP probe).

## Corpus, pools and baseline

- **Questions:** ConditionalQA, the **800-question subset** phase M pre-drew
  (`bench/docs/measure/reader_subset.json`, seed 20260929).
- **Store:** phase M's pooled `cqa-header` store, rebuilt with M's `ingest.py` and M's
  embedder (`bge-m3` on the Spark).
- **Pools:** each question's pool is `Document op=search` with limit 20 and no rerank: the
  20 candidates the shipped rerank reorders.
- **Baseline `qwen_list`:** phase M's `header_rr` rankings, the shipped rerank over the
  same pools. Instrument checks 1 and 2 establish that the pools are M's. Rankings are
  compared by section identity (page, section), since chunk ids differ between the two
  stores.
- **Candidate text:** every candidate is shown as the shipped rerank shows it. That is its
  index text, `<page title> — <heading path>` plus the text, truncated to 1,200 characters.

## Arms (all on the TrueNAS Ollama 0.35.0, over Tailscale)

| arm | model | method |
|---|---|---|
| `none` | — | the pool order (phase M's `header` arm) |
| `qwen_list` | qwen3.8 | the shipped listwise rerank (phase M) |
| `nimble_choice` | `nimble` 9B | one `choice` call per question: the 20 candidates as options A–T, ordered by probability |
| `nimble_point` | `nimble` 9B | one `noul` call per (question, candidate), "Does the passage contain the answer to the question?", ordered by probability |
| `tev1_point` | `tev1:4b` | as `nimble_point` |

**Rules:**
- Ties keep pool order.
- `nimble` scores the whole prompt within 8,192 tokens. A `choice` request refused for
  size (HTTP 400) is retried with every candidate cut to 600, then 300 characters. How
  often that happens is reported.
- `tev1:4b` has a ~2,000-token budget per question, so it gets no `choice` arm.
- Point calls run 4 in flight per question.
- One decision model is loaded at a time.

## Hypotheses

**Co-primary** (R@5 over the 800 questions; paired bootstrap, 10,000 resamples, seed 1;
Bonferroni over two, so 97.5% CIs):

- **H1** — `nimble_choice` is not worse than `qwen_list` by more than 0.03: the CI lower
  bound of (arm − qwen_list) is above −0.03.
- **H2** — `nimble_point` is not worse than `qwen_list` by more than 0.03. Same test.

The margin is 0.03, about 40% of the rerank's own gain in phase M (+0.073).

**Secondary** (reported whatever they show):
- `tev1_point` against `qwen_list`, by the same test.
- Each decision arm against `none` (exact McNemar).
- R@1/3/10 and MRR@10.
- Latency per question (p50 / p95 wall time) and mean input tokens.
  - Latency is measured on TrueNAS, a slower GPU than the Spark where `qwen_list` ran. The
    two are **not** compared as a speed claim; any speed comparison needs one host.

**Calibration and gate** (pointwise arms, descriptive; thresholds fixed now):
- **Calibration:** over every (question, candidate) pair, with label = the candidate is a
  gold section. Reported: ECE (10 equal-width bins), Brier, pair AUC and the reliability
  bins.
- **Gate, as minimal pairs:** for each question whose pool holds a gold section, compare the
  pool's top probability with the top probability of the SAME pool with its gold sections
  removed. The gate AUC is how often the first outranks the second.
- **"Usable as a gate":** gate AUC ≥ 0.85 **and** ECE ≤ 0.10.
- **Label caveat:** ConditionalQA's gold marks the evidence sections, and another section
  can also answer. Removed-gold pools may still hold an answer, so both numbers are
  pessimistic.

## Instrument checks (each can fail; a failed check voids what depends on it)

1. The rebuilt pool's top 10 equals phase M's `header` top 10, by section identity, for
   ≥ 98% of the questions. Voids H1, H2 and every comparison with `qwen_list`.
2. Phase M's `header_rr` top 10 lies within the rebuilt pool of 20 for ≥ 98% of the
   questions. Same voids.
3. Every pool has 20 candidates for ≥ 98% of the questions. Voids nothing, but is reported.
4. No decision call is left failed. A question that still fails after retries aborts the
   arm. An aborted arm is void.

## Decision

- **H1 or H2 holds:** a decision-model rerank mode is worth an RFC. The latency and gate
  results say whether its case is speed, robustness (no reply to parse) or the gate.
- **Both fail:** decision models are recorded as not matching the shipped rerank on this
  corpus. The gate result is still reported, since it is independent of ranking.
- **The gate is usable:** a "nothing here answers" signal gets its own probe on a corpus
  with real unanswerable questions, before any feature is proposed.
