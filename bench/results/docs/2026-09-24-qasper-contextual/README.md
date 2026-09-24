# Contextual retrieval on structural chunks — QASPER probe, 2026-09-24

First probe of the document-processing epic. Structural (heading) chunking is kept as it is;
the question is whether prepending a situating context to each chunk, as in Anthropic's
Contextual Retrieval, improves retrieval — and whether an LLM is needed to write it.

**Result.** The best pipeline is `mapall`, with Answer-F1 0.412 against 0.287 for plain
chunks and 0.347 for the free header (95% CI of the paired difference against the header
+0.018…+0.112). It works in two steps:

1. The model writes a short description of each section.
2. The descriptions are assembled into a document map. The map (a) replaces Anthropic's
   whole-paper prompt as the source for each chunk's context, and (b) becomes the document
   root, which the reader sees whenever it is retrieved.

The gold-evidence ceiling is 0.510, so this closes 56% of the gap from plain chunks.

What each piece does:

- **A deterministic header** (`Paper: <title>. Section: <path>.`) is free and does the
  "which document" job. The LLM contexts almost never name the document (14 of 647).
- **A context written from the map is as good as one written from the whole paper**
  (0.384 against 0.377; retrieval R@5 0.719 against 0.705) for half the prompt tokens.
- **The map helps as reading, not as retrieval.** It takes retrieval slots (R@5 drops), but
  answers improve, and all of the gain is on the 124 questions where the reader saw it.
- **Two uses of the descriptions add nothing:** as separate searchable units (`desc`), or
  as links followed to the best-matching sections (`map+hop`).

⚠️ The reader is not deterministic from one pass to the next. Answer comparisons use three
passes averaged per question; see "Reader noise".

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

## Answering, single pass (superseded — see the three-pass section below)

Reader: ornith-1.5:35b, direct Ollama, temperature 0, told to answer with a short phrase,
"Yes"/"No", or "Unanswerable". The metric is QASPER's official Answer-F1 (token F1, max over
annotators). No judge. `oracle` gives the reader the gold evidence instead of retrieved
chunks. `/hidden-ctx` retrieves with that arm's index but strips the LLM context from what
the reader sees, which is how Anthropic uses it.

| arm | F1 | Δ vs plain, 95% CI | wins/losses | extractive (86) | abstractive (44) | yes/no (16) |
|---|---|---|---|---|---|---|
| oracle | 0.510 | +0.148…+0.289 | +77/−23 | 0.586 | 0.363 | 0.509 |
| plain | 0.293 | | | 0.336 | 0.219 | 0.265 |
| **header** | **0.362** | **+0.006…+0.131** | +43/−31 | 0.424 | 0.299 | 0.195 |
| context | 0.307 | −0.038…+0.064 | +35/−30 | 0.362 | 0.243 | 0.192 |
| context/hidden-ctx | 0.298 | −0.045…+0.056 | +32/−30 | 0.342 | 0.248 | 0.202 |
| both | 0.347 | −0.008…+0.115 | +40/−34 | 0.414 | 0.252 | 0.250 |
| **both/hidden-ctx** | **0.363** | **+0.008…+0.131** | +46/−27 | 0.440 | 0.275 | 0.195 |

This table is one reader pass, and one pass is inside the reader's own noise (next
sections). Under three passes `both/hidden-ctx` scores 0.377 against the header's 0.347, not
0.363 against 0.362. The conclusion drawn from this table, that the LLM context adds nothing,
does not survive.

## Descriptions, the document map, and map-based context

A second round built on `header`, all in the pooled store with the hybrid retriever:

- **Descriptions:** `contextualize.py --mode describe`. The model reads ONE section with
  only its title and heading path, and writes one or two sentences naming the methods,
  datasets, numbers and findings in it — the light analogue of fact extraction. 647 calls,
  22 min, median 117 output tokens. 39 reached the 200-token cap and end mid-sentence.
- **Document map:** the title, then every section's heading path and description, in order.

| arm | index (what search matches) | what the reader sees |
|---|---|---|
| `header` | header + section | header + section |
| `both` | header + **whole-paper** context + section | header + section (context hidden) |
| `mapctx` | header + **map-based** context + section | header + section (context hidden) |
| `map` | header + section; root = the map | sections, and the map when it is hit |
| `mapall` | `mapctx`'s chunks; root = the map | as `map` |
| `map+hop` | as `map` | a map hit is replaced by the 2 linked sections whose descriptions best match the question (bge-m3 cosine) |
| `desc` | header + section; each section has a child chunk holding its description | a description hit resolves to its section |

The map root has a `describes` edge to every section, and each `desc` child has one to its
section, so the store carries the links whether or not an arm follows them.

## Answering, three passes (the comparison of record)

Answer-F1 is averaged over three reader passes per question: one in order, two with the
question order shuffled (seeds 11, 23). The paired bootstrap runs on those averages. Recall
is measured on what the READER saw, after each arm's resolution step.

| arm | F1 | pass means | vs header, 95% CI | vs plain, 95% CI | R@1 | R@3 | R@5 | R@10 |
|---|---|---|---|---|---|---|---|---|
| plain | 0.287 | .287/.286/.287 | −0.118…−0.002 | | 0.103 | 0.363 | 0.610 | 0.829 |
| header | 0.347 | .346/.347/.347 | | +0.005…+0.119 | 0.205 | 0.507 | 0.671 | 0.904 |
| both | 0.377 | .373/.380/.380 | −0.005…+0.067 | | 0.233 | 0.562 | 0.705 | 0.918 |
| mapctx | 0.384 | .377/.388/.388 | +0.002…+0.076 | +0.040…+0.157 | 0.192 | 0.568 | 0.719 | 0.897 |
| map | 0.375 | .375/.376/.375 | −0.017…+0.076 | +0.021…+0.155 | 0.137 | 0.425 | 0.575 | 0.863 |
| **mapall** | **0.412** | .411/.411/.412 | **+0.018…+0.112** | | 0.151 | 0.459 | 0.664 | 0.863 |
| map+hop | 0.337 | .343/.335/.332 | −0.055…+0.032 | +0.001…+0.103 | 0.171 | 0.459 | 0.644 | 0.904 |
| desc | 0.349 | .358/.344/.345 | −0.038…+0.041 | +0.005…+0.117 | 0.185 | 0.500 | 0.678 | 0.890 |

Also:

- `mapctx` against `both`: −0.037…+0.051. They are equivalent, but the map-based context
  costs half the prompt tokens.
- `mapall` against `both`: −0.012…+0.080. Against `mapctx`: −0.016…+0.072.

**Where `mapall`'s gain comes from.** The map was in the reader's top 5 for 124 of 146
questions. On those, `mapall` scores 0.433 against `mapctx`'s 0.401; on the other 22,
0.292 against 0.293, the same chunks read the same way. The map is read, not just matched.

### Cost of the context sources

| step | calls | prompt tokens | output tokens | local GPU time |
|---|---|---|---|---|
| whole-paper context (`both`) | 647 | 4.25M (6.6K per call) | 35K | 21 min |
| descriptions | 647 | 0.28M | 76K | 22 min |
| map-based context | 647 | 1.81M (2.8K per call) | 41K | 25 min |

The map path (descriptions plus map context) uses half the prompt tokens of the
whole-paper path, which is what a metered API bills. Locally it takes about twice the GPU
time, because output tokens set the pace and the descriptions are long. The whole-paper
prompt also grows with the square of document length and stops fitting in context long
before a map does.

## Reader noise

With retrieval held fixed, two `header` passes back-to-back on an idle GPU are identical
(146 of 146). Across passes that differ in what ran before, about 27 of 146 questions change
score and the mean moves by up to 0.02 F1. Candidate causes: Ollama's prefix cache (the
preceding prompt shares a prefix with the next) and batching when another job shares the
GPU. One pass therefore cannot separate arms closer than about 0.04. Three passes averaged
per question bring the arms' pass means within 0.01 of each other.

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
- **One reader, one prompt, top 5.** A reader that is better at ignoring a context prefix,
  or one given more chunks, may weigh the arms differently. In particular, the map costs a
  slot, and a larger k would change that trade.
- **Nothing here is held out.** The arms, the map format and the reader prompt were chosen
  and evaluated on these same 146 questions. `mapall` should be confirmed on fresh papers
  before it drives a design.
- **The GPU faulted with ornith alone.** One reader call hit `CUDA error: an illegal memory
  access` with only ornith and bge-m3 loaded (the same prompt twice, about 1 in 750 calls).
  It passed on retry. gpt-oss co-residency is not the only trigger.

## Reproduce

Scripts are in `bench/docs/contextual/`: `prepare.py` → `contextualize.py` → `ingest.py` →
`retrieve.py` → `analyze.py --rank ev_rank_content`, then `answer.py` (plus
`answer.py --arms both,context --hide-context`) → `analyze_answers.py`. Second round:
`contextualize.py --mode describe`, then `--mode mapcontext` → `build_arms.py` →
`pipeline.py` (three passes: `--out answers3{a,c,d}.jsonl`, `--shuffle-seed` 0/11/23) →
`analyze_passes.py`. `describe.jsonl` and `mapcontext.jsonl` are the generated texts. `contexts.jsonl` here is the generated
context set, so the arms can be rebuilt without re-running the model.
