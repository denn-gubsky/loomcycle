# exp11 benchmark — does memory pay off for Skybits document work?

Controlled A/B on the live service (2026-08-19), model `claude-sonnet-4-6` via
`anthropic-oauth-dev`, loomcycle v1.59.0+`feature-skybits-integration`.

## Method

- **Fixture:** one real Skybits doc, *Atlas Program Master Register* — 42 KB,
  30 sections, each with a unique codeword / budget / owner (ground truth known).
- **Setup (shared):** the `doc-agent` read the doc once and banked the register
  into user-scoped memory as ONE JSON value (`skybits:atlas:register`, ~2.3 KB).
- **Probe:** 10 fresh runs per arm, each asking codeword+budget+owner for 3
  pseudo-random sections (fixed seed, same triples for every arm — 90 answer
  slots per arm).
- **Arm B — no memory:** `nomem-agent` (no `Memory` tool), must re-read the full
  42 KB doc from Skybits on every run.
- **Arm C — memory:** `doc-agent` answers via its STEP-0 recall.
- **Arm A — memory, polluted:** same as C but with ~109 KB of unrelated blob
  values left under the same `skybits:` prefix from an earlier experiment.

## Results (median per run)

| Metric | B: no memory | C: memory (clean) | A: memory (polluted) |
|---|---|---|---|
| Accuracy (exact match) | 90/90 | 90/90 | 90/90 |
| Skybits API calls | 10 | 0 | 0 |
| Wall time | 10.4 s | 5.4 s | 6.8 s |
| Fresh input tokens | 9,198 | 1,948 | 25,308 |
| Cache-read tokens | 29,317 | 32,951 | 32,951 |
| Output tokens | 100 | 129 | 138 |

## Findings

1. **Recall is lossless for banked facts.** 90/90 exact matches from memory —
   identical to re-reading the source document every time.
2. **Memory cuts the per-session cost of repeated document work ~4.7×** on
   fresh input tokens (1,948 vs 9,198) and eliminates the Skybits API round
   trips entirely; ~2× faster wall time. The one-time banking cost is a single
   setup run (~6 tool calls), amortized after the first reuse.
3. **Memory hygiene is not optional (the negative result).** A broad
   `Memory op=list prefix="skybits:"` recall pulls *values*, so 109 KB of stale
   blob parts inflated every run 13× (25,308 vs 1,948 fresh tokens) — worse
   than re-reading the doc. Keep recalls prefix-narrow, store bulky payloads
   behind meta keys, and delete dead values.
4. **Staleness is the real price** (verified separately): after an external
   edit, memory answers confidently wrong until the agent re-reads. The
   outline-before-edit discipline and explicit drift checks are the mitigation;
   memory is a cache, not the source of truth — Skybits is.

## Caveats

- Best case for memory: needle QA over a stable register. Authoring-heavy
  workloads re-read by necessity.
- One model, one doc shape (flat — `create_doc` does not build heading nodes).
- Token economics assume provider prompt caching; cache-read tokens are
  near-free, fresh input tokens are the real cost.
- Stale-memory risk quantified qualitatively (drift probe), not in this table.

## Reproduce

Boot the example (`./run.sh`), create the fixture from the Skybits Skill
helper or the API, run the setup prompt once, then probe with fresh runs
(same `user_id`); the `nomem-agent` twin used for arm B is a two-line agent
without the `Memory` tool.
