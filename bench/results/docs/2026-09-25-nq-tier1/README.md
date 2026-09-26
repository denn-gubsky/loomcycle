# NQ tier 1 — header and rerank at scale (pre-registered)

**Both co-primary hypotheses hold.** On 911 Natural Questions over 911 Wikipedia pages,
asked with bare open-domain questions, a deterministic `Article: <title>. Section: <path>.`
header lifts R@5 from 0.912 to 0.935 (p=0.0046). A local listwise rerank lifts it further
to 0.959 (p=0.0054) and R@1 from 0.515 to 0.754. By the rule committed before the run
(`PREREG.md`), **RFC DM proposes both**: index-only header text beside the chunk, and an
opt-in rerank step.

## Why this corpus

The QASPER probe left two things open. Its held-out set confirmed the rerank's retrieval
gain, but the header had only been measured on the development papers. And QASPER's
answer metric saturated near a 0.46 reader ceiling. NQ is out-of-sample for every choice
fixed on QASPER. Its pages have a real heading tree, and its short answers give an answer
ceiling well above that (oracle EM 0.644, F1 0.796).

## Setup

- **Corpus:** NQ validation shards 0 and 1, seed 7; every question with a paragraph long
  answer, a short answer, and a gold paragraph found in the converted page. That is 911
  questions and 911 pages, 13,247 chunks per store (11,312 with a body), all in one pooled
  store per arm.
- **Query:** the bare question. No page title is given; finding the page is part of the
  task.
- **Arms:** `plain`, `header` (prepended to the indexed body), `header+rr` (top 20
  reranked by the local model, top 5 to the reader).
- **Models:** reader and reranker `qwen3.6:latest`, thinking off (PREREG amendment 1, made
  before any result); embedder bge-m3; hybrid retrieval (`/v1/_memory/search`).
- **Protocol:** three reader passes (seeds 0, 11, 23) averaged per question for answers.
  Retrieval is scored on pass A with an exact McNemar test. Zero cloud calls: no
  loomcycle runs and no token_usage rows since the run started.

## Results

| arm | R@1 | R@3 | R@5 | R@10 | EM (3-pass) | F1 (3-pass) |
|---|---|---|---|---|---|---|
| plain | 0.519 | 0.825 | 0.912 | 0.952 | 0.528 | 0.680 |
| header | 0.515 | 0.849 | 0.935 | 0.975 | 0.519 | 0.684 |
| **header+rr** | **0.754** | **0.933** | **0.959** | **0.981** | **0.537** | **0.699** |
| oracle (1 pass) | | | | | 0.644 | 0.796 |

| hypothesis | result | verdict |
|---|---|---|
| **H1** `header` > `plain`, R@5, McNemar p < 0.025 | 0.912 → 0.935, +36/−15, p=0.0046 | **holds** |
| **H2** `header+rr` > `header`, R@5, McNemar p < 0.025 | 0.935 → 0.959, +40/−18, p=0.0054 | **holds** |
| H3 `header` > `plain`, EM, CI lower bound > 0 | −0.009 (−0.030…+0.013) | fails |
| H4 `header+rr` > `header`, EM, CI lower bound > 0 | +0.018 (+0.002…+0.035) | holds |

The rerank moved the evidence up on 338 questions and down on 82. Two rankings in 911
could not be parsed and kept the original order.

## Reading it

- **The header is a retrieval gain, not an answer gain.** It helps from R@3 down
  (R@10 +24/−3, p=5e-5), and not at rank 1. Answers do not improve (EM −0.009, n.s.).
  One likely reason: in this probe the header is part of the body, so the reader sees it
  too. The RFC keeps it index-only, which is what the feature would ship.
- **The rerank is the lever that reaches the reader.** R@1 rises from 0.515 to 0.754, and
  it is the only arm whose answer gain clears zero (EM +0.018, F1 +0.015, both CIs above
  0). This matches QASPER, where the rerank's retrieval gain replicated on held-out papers.
- **The reader is still the limit.** `header+rr` puts the evidence in the top 5 for 95.9%
  of questions, yet EM is 0.537 against the oracle's 0.644. The remaining gap is reading,
  not finding.

## Caveats

- **One corpus, one reader.** The numbers are qwen3.6 numbers and are not comparable to
  the QASPER ones (ornith reader).
- **Short Wikipedia pages.** The median page is 2.1K words, so this does not test long
  documents; the long-page tier (map and context) was deferred.
- **One question hit a CUDA fault on every attempt in its first run** (the same prompt had
  faulted under ornith). The fill-in re-run answered it, and every arm is complete in every
  pass.
- **The stores were rebuilt once before this run.** The server's dead-link sweeper had
  deleted bodies of live chunks in these >10K-chunk scopes. That is fixed in PR #1389;
  this run used the sweeper disabled and a chunk-rows = body-rows check (13,247 = 13,247
  in both stores).

Outputs: `nq-result.txt` (all four analyses: EM and F1, each against plain and against
header) and `summary-nq.json`.
