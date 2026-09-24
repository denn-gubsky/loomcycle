# Contextual retrieval on structural chunks — QASPER probe, 2026-09-24

First probe of the document-processing epic. Structural (heading) chunking is kept as it is;
the question is whether prepending a situating context to each chunk, as in Anthropic's
Contextual Retrieval, improves retrieval — and whether an LLM is needed to write it.

**Answer: most of the gain is free.** A deterministic header naming the paper and the
section path does the "which document" work. The LLM context helps only in finding the
right section within one document (R@1 +9pp), and it barely names the document at all.
The best arm is the header plus the context.

## Setup

- **Corpus:** QASPER dev v0.3, 40 papers (seed 7), 146 questions with cited evidence.
  647 section chunks, 724 counting document roots and empty parent headings.
- **Chunking:** `Document op=import_md`, one chunk per heading, unchanged across arms.
- **Arms**, each prepended to the chunk BODY, which feeds both the embedding and the
  full-text index (no code change):
  - `plain` — the section text alone
  - `header` — `Paper: <title>. Section: <heading path>.`, deterministic, no model
  - `context` — ornith-1.5:35b with Anthropic's published prompt: the whole paper plus the
    chunk, called directly on Ollama. 647 calls, 21 min, about 52 output tokens each.
  - `both` — header plus context
- **Layouts:** `pool` puts all 40 papers in one scope and asks `In the paper "<title>": <q>`
  (QASPER questions never name the paper). `paper` gives each paper its own scope and asks
  the bare question.
- **Retrievers:** `hybrid` is `/v1/_memory/search sources=[documents]` (vector plus full-text,
  fused by RRF — the agent recall path). `vector` is `Document op=search` (cosine only).
  Embedder: bge-m3, local.
- **Scoring:** rank of the first chunk containing a gold evidence paragraph, counting only
  chunks that have a body (`ev_rank_content`; see below). No judge and no cloud call anywhere.
  Significance is an exact McNemar test on paired questions.

## Results (R@k, `ev_rank_content`)

| layout / retriever | arm | R@1 | R@3 | R@5 | R@10 | R@20 | MRR |
|---|---|---|---|---|---|---|---|
| pool / hybrid | plain | 0.103 | 0.363 | 0.610 | 0.829 | 0.884 | 0.303 |
| | header | 0.205 | 0.507 | 0.671 | 0.904 | 0.986 | 0.408 |
| | context | 0.130 | 0.445 | 0.651 | 0.856 | 0.925 | 0.338 |
| | **both** | **0.233** | **0.555** | **0.699** | **0.918** | **0.993** | **0.437** |
| paper / hybrid | plain | 0.329 | 0.685 | 0.842 | 0.952 | 0.993 | 0.539 |
| | header | 0.370 | 0.692 | 0.870 | 0.952 | 0.993 | 0.565 |
| | context | 0.418 | 0.726 | 0.884 | 0.966 | 0.993 | 0.595 |
| | **both** | **0.432** | **0.733** | 0.877 | **0.973** | 0.993 | **0.609** |

The vector-only rows are in `summary.json`. They follow the same pattern, but the pooled
header loses R@5 there, so the full-text leg carries a large share of the header's value.

Paired tests against `plain`:

- pool/hybrid, `both`: R@1 +25/−6 (p=0.0009), R@3 +36/−8 (p=2.5e-5), R@10 p=0.015.
- pool/hybrid, `header`: R@1 p=0.008, R@3 p=0.0015.
- paper/hybrid, `context`: R@1 +19/−6 (p=0.015). `header` there is p=0.24 — not significant.
- `both` vs `header` in the pool is not significant at R@5 or R@10.

## Why the LLM context did not do the document's job

Only **14 of 647** contexts name the paper's full title; 165 carry most of its words. The
typical one reads *"This chunk is from the Introduction section of the paper, providing an
overview of …"*. It names its own section in 547 of 647. Anthropic's gain in a pooled corpus
leans on contexts that say which document a chunk belongs to. ornith does not do this
unprompted, and a header does it for free, every time.

## ⚠️ Caveats

- **Small:** 146 questions, 40 papers, one corpus of one genre. Effects of 3–5pp at R@5 are
  below what this sample can resolve.
- **The pooled query names the paper by its exact title.** That rewards the header's lexical
  overlap more than a real user saying "the BioASQ paper" would.
- **Document roots rank first.** A bodyless root chunk is embedded from its title, so a query
  naming the paper returns the root at rank 1 on every question. The raw ranks
  (`ev_rank`, which `retrieve.py` records alongside) show this: pool/hybrid `plain` R@1 is 0.027. That is a
  real property of the store for an agent, and a candidate fix in its own right.
- **Retrieval only.** Whether a reader answers better from these chunks (QASPER Answer-F1)
  is not measured yet.

## Reproduce

Scripts are in `bench/docs/contextual/`: `prepare.py` → `contextualize.py` → `ingest.py` →
`retrieve.py` → `analyze.py --rank ev_rank_content`. `contexts.jsonl` here is the generated
context set, so the arms can be rebuilt without re-running the model.
