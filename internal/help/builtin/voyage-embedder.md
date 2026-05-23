---
name: voyage-embedder
description: "Voyage AI embedder behind the `provider: anthropic` slot — Anthropic has no native embedding API and explicitly recommends Voyage."
---
Loomcycle v0.10.2 ships a real Voyage AI embedder behind the
`memory.embedder.provider: anthropic` yaml slot. Anthropic has no
native embedding API and explicitly recommends Voyage AI; the
ergonomic operator framing — "use my Anthropic-blessed embedder" —
maps to "talk to Voyage's API" under the hood.

This replaces the v0.9.0–v0.10.1 stub which returned
`embedder_not_implemented` for every Embed() call.

## When to use

Pick `provider: anthropic` (Voyage AI under the hood) when:

- You're already on Anthropic for chat completions and want a single
  vendor relationship for both chat + embeddings.
- You want a privacy-forward embedder — Voyage doesn't train on your
  data, and Anthropic recommends them for that reason.
- You need a Code, Finance, or Law domain-specialised model
  (voyage-code-3, voyage-finance-2, voyage-law-2) — OpenAI + Gemini
  don't offer domain-fine-tuned options today.

Pick `provider: openai` or `provider: gemini` when:

- You're already paying for OpenAI or Gemini API credit (vendor
  consolidation cost win).
- You want the largest dimension counts (`text-embedding-3-large`
  at 3072 vs Voyage's 1024 default).

## Enable

Two env vars, one yaml block:

```sh
export ANTHROPIC_API_KEY=...   # for chat completions (existing)
export VOYAGE_API_KEY=...      # NEW for embeddings
```

```yaml
memory:
  embedder:
    provider: anthropic        # ergonomic alias — routes to Voyage
    model: voyage-3            # see model menu below
    timeout_ms: 30000          # optional; default 30s
    batch_size: 128            # optional; Voyage's voyage-3 cap
```

Restart loomcycle. On boot, no special log line — embedder
configuration is silent on success. A failed first Embed() call (bad
key, unreachable endpoint, etc.) surfaces the error to the agent and
appears in the run transcript.

If `VOYAGE_API_KEY` is empty when the embedder is configured,
loomcycle logs a warning at startup:

```
memory.embedder: provider=anthropic uses Voyage AI; set VOYAGE_API_KEY or Embed() calls will fail at 401
```

## Model menu

The voyage-4 family is current as of 2026-05; voyage-3 family is kept
accessible for back-compat by Voyage. All default to 1024-dimensional
output:

| Model | Use case | Default dim |
|---|---|---|
| `voyage-4` | General-purpose embedding | 1024 |
| `voyage-4-large` | Highest quality; supports 256/512/1024/2048 via `output_dimension` | 1024 |
| `voyage-4-lite` | Cost-optimized | 1024 |
| `voyage-4-nano` | Smallest + fastest | 1024 |
| `voyage-code-3` | Code-aware (Python/Go/Java/etc.) | 1024 |
| `voyage-finance-2` | Financial domain (10-K/earnings/etc.) | 1024 |
| `voyage-law-2` | Legal domain (case law, contracts, etc.) | 1024 |
| `voyage-3` | Legacy general-purpose (back-compat) | 1024 |
| `voyage-3-large` | Legacy large (back-compat) | 1024 |
| `voyage-multilingual-2` | Non-English (kept for back-compat) | 1024 |

Custom models (unknown to loomcycle's per-model dimension map)
construct successfully — `Dimension()` returns 0 and the in-response
sanity check is skipped (the per-batch dimension consistency check at
the store layer still fires).

## Wire shape

Loomcycle's Voyage driver mirrors OpenAI's `/v1/embeddings` shape
(deliberately compatible on Voyage's side):

```
POST https://api.voyageai.com/v1/embeddings
Authorization: Bearer <VOYAGE_API_KEY>
Content-Type: application/json

{"input": ["text 1", "text 2"], "model": "voyage-3"}

→ {"data": [{"index": 0, "embedding": [0.1, ...]},
            {"index": 1, "embedding": [0.2, ...]}],
   "model": "voyage-3",
   "usage": {...}}
```

The driver:

- Bears `Authorization: Bearer $VOYAGE_API_KEY` on every request.
- Batches up to `EmbedderOptions.BatchSize` (default 128 for the
  voyage-3 family; check Voyage's docs for current per-model caps).
- Reorders the response by the `index` field — Voyage delivers
  in-order today but the wire contract permits reordering.
- Applies a per-attempt timeout from `EmbedderOptions.Timeout`. Each
  retry attempt gets a fresh deadline (so a `Retry-After: 30s` from a
  429 doesn't silently neuter retries even when timeout is shorter).
- Wraps the call in `ratelimit.Do` with the standard RFC-7231
  `Retry-After` header parser. Voyage uses the standard semantics.

## Batch sizing

Voyage caps inputs per request at **128** for the voyage-3 family
(and 1000 for voyage-large-2 and older). Set `batch_size: 128` in
yaml to stay safely under the cap; the driver splits larger input
arrays into multiple sequential POSTs.

`batch_size: 0` (the default) sends the full input slice in one call
— works when total inputs ≤ 128, fails with 400 when you exceed it.
Most operators want an explicit cap.

## Pricing

Voyage's pricing (as of 2026-05) — operators check
`https://docs.voyageai.com/docs/pricing` for current numbers:

- voyage-4-nano: ~$0.02 / 1M tokens
- voyage-4-lite: ~$0.06 / 1M tokens
- voyage-4: ~$0.18 / 1M tokens
- voyage-4-large: ~$0.18 / 1M tokens
- voyage-3 family: ~$0.12 / 1M tokens

A typical loomcycle workload (Memory.set with a 200-token average
document) embedding 1000 docs costs ~$0.04 on voyage-4 — pricing
isn't the rate-limiter.

## Migration from v0.10.1 stub

If you were on v0.10.1 with `provider: anthropic` set (and getting
the `embedder_not_implemented` refusal):

1. Set `VOYAGE_API_KEY` in `.env.local`.
2. Restart loomcycle.
3. Your existing `memory.embedder.model` value — if it was a Voyage
   model name like `voyage-3` — will work. If you had a placeholder
   like `claude-embed-future`, change it to `voyage-3` or one of the
   voyage-4 family entries above.

No re-embed is needed if you never successfully embedded anything
under v0.10.1 (the stub refused every call). If somehow you did via
external tooling, see `help vector-memory` for the reembed flow.

## Related topics

- `vector-memory` — overall Vector Memory architecture; Voyage is
  one of three embedder backend choices.
- `sqlite-vec` — SQLite-side vector storage via build-tag opt-in.
  Voyage embeddings stored via sqlite-vec if you build with
  `-tags=sqlite_vec`.
