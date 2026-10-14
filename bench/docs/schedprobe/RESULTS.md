# RFC EI phase P0 — results (2026-10-13)

Host: `ollama-host`, Ollama 0.40.3, `NUM_PARALLEL` 4, `MAX_LOADED_MODELS` 4,
`CONTEXT_LENGTH` 65536. Chat `qwen3.6:latest`, embed `bge-m3:latest`, decision
`nimble:latest`. Rules and amendments: [PREREG.md](PREREG.md). Records: `runs/`.

## Verdicts

| # | Verdict | Decides |
|---|---|---|
| **E1** | Both instrument checks pass. Warm reuse **0.89** (bar 0.5). | Reuse is observable; the measure works. |
| **E2** | Reuse **0.87–0.88** at 2, 4, 6 and 8 agents on 4 host slots: the "unbounded cache" case. | `affinity_hold_ms` defaults to **0**. |
| **E3** | Rule output: **quantum 1, hold 0** — per-call round-robin. | `quantum_calls` defaults to **1**. |
| **E4** | **Host fault, not scored.** The mixed run failed on both tries. | Nothing; it is a finding (below). |
| **E5** | Every `num_ctx` switch reloaded the model (about 10.7 s) and lost all reuse. | Model identity is **(model, `num_ctx`)**. |
| **E4 re-run** (2026-10-14, amendment 3) | Chat + embed **passes** (no reload, embed isolated). Chat + decision and all three are **host faults** on both tries: the chat runner dies whenever a second LLM is loaded beside it. Decision lane 2 vs 1: **1.78×** (threshold 1.8). | The fault follows **two LLMs at once**, not the one-slot chat model. The decision lane default stays 1 (below). |

Side check: on warm calls `prompt_eval_count` still reports the full prompt (count
ratio 1.00). The usage ledger does not under-report Ollama input tokens.

## E1 — reuse is observable

- Calibration from the cold arm: `prefill_ms = −796 + 0.6705 · chars`, 0.209 tokens per character.
- Check 1 (the nonce busts the cache): cold prefill grew ×2.35 while the prompt grew ×2.25. Pass (bar ×1.80).
- Check 2 (the warm arm starts cold): warm turn-1 prefill was 1.00 × the cold arm's. Pass.
- A warm turn pays about 11 % of the cold prefill: roughly 2 s against 12–26 s.

## E2 — the cache is not bound to a request slot

| agents (host slots = 4) | reuse, turn 2+ | output tok/s | TTFT p50 |
|---|---|---|---|
| 2 | 0.88 | 12.4 | 10 s |
| 4 | 0.88 | 13.2 | 31 s |
| 6 | 0.87 | 13.7 | 56 s |
| 8 | 0.87 | 13.4 | 76 s |

- Reuse does not fall when agents outnumber host slots.
- Throughput is flat: the host does one prompt's work at a time. More concurrent agents only lengthen the wait.
- **Not measured:** the cache's capacity. Eight agents at 6–8K tokens fit easily in a cache sized for 4 × 65,536 tokens. Eviction, if it exists, starts beyond that.

## E3 — scheduling changes who waits, not how much gets done

8 agents, 6 turns, host slots 4, probe chat slots 4. Re-run in full on the cooled host
(amendment 2); no failed calls, host speed steady at 596–608 ms per 1,000 characters.
"First token" is from the request, including the wait for a slot.

| policy | quantum / hold | out tok/s | first token p50 | first token p95 | first / last agent done | finish spread |
|---|---|---|---|---|---|---|
| none (no loomcycle gate) | – | 12.9 | 64 s | 121 s | 433 / 493 s | 0.04 |
| **run (today's gate)** | – | 13.6 | 34 s | **257 s** | **204 / 527 s** | **0.40** |
| call | 1 / 0 | 13.1 | 62 s | 119 s | 423 / 483 s | 0.04 |
| lease | 1 / 0 | 13.0 | 62 s | 115 s | 414 / 470 s | 0.04 |
| lease | 2 / 0 | 13.6 | 72 s | 129 s | 476 / 546 s | 0.05 |
| lease | 4 / 0 | 12.9 | 59 s | 121 s | 410 / 469 s | 0.04 |
| lease | 8 / 0 | 13.0 | 76 s | 140 s | 507 / 581 s | 0.04 |
| lease | 1 / 5000 | 13.4 | 69 s | 131 s | 461 / 529 s | 0.05 |
| lease | 2 / 5000 | 14.0 | 52 s | 158 s | 455 / 578 s | 0.09 |
| lease | 8 / 5000 | 13.2 | 31 s | **304 s** | 243 / 503 s | **0.31** |

The other lease settings (hold 2000; quantum 4 at hold 5000) are in `runs/` and fall
between these.

**Applying the rule.**
- Best lease p95 TTFT is 57.0 s, so the bar is 71.2 s. Every lease setting clears it except quantum 8 / hold 0 (73.6 s).
- E2 ruled affinity out, so only hold 0 is eligible: quantum 1 (13.0 tok/s), 2 (13.6), 4 (12.9).
- Quantum 2 is highest. Quantum 1 is within 5 % of it, so it ties, and the smaller quantum wins: **quantum 1, hold 0**.

**What the table shows.**
- **Throughput is flat** (12.9–14.0 tok/s) and reuse is flat (0.87–0.88) across every policy. No policy makes this host do more work.
- **Today's run-level gate starves half the agents.** Four agents finish in 204 s while the other four wait for whole conversations: p95 first token 257 s against about 120 s, finish spread 0.40.
- **A long hold with a large quantum brings the starvation back** (quantum 8 / hold 5000: p95 304 s, spread 0.31). A lease is the run-level gate in small.
- **Gating at the host's slot count costs nothing.** `call` and `none` give the same first-token times; the wait just moves from Ollama's queue into the probe's, where loomcycle can see it and no HTTP timer runs.

**Limits of this reading.**
- One run per setting. The tok/s differences (about ±4 %) are within what one run can resolve; the rule's tie-break, not a measured effect, picks quantum 1.
- Each run has its own workload seed, so reply lengths and makespans differ between runs. Output tok/s is the comparable figure.

## E4 — three models in use at once took the host down

- **Solo baseline** (embed and decision agents alone): embed p50 0.11 s, p95 2.3 s; decision p50 1.9 s, p95 1.9 s.
- **Mixed, try 1:** 12 of 70 calls failed within 84 s — `llama-server process no longer running`, `unexpected EOF` — across chat, decision and embed.
- **Mixed, try 2:** the host stopped answering. Chat calls hung for 26 minutes each and ended in connection errors; 38 of 70 calls failed.
- Per amendment 1 this is a host fault for the setting, **not scored**. The isolation question (do embed and decision calls stay fast under chat load) is unanswered.
- Short diagnostic runs of each pair (chat + embed, chat + decision) and of all three had passed before E1. The fault needs sustained load to show.

## E4 re-run (2026-10-14) — two LLMs at once take the chat runner down

Amendment 3: chat on `gemma4:latest` (four real slots), embed `bge-m3`, decision `nimble` (one slot), chat `num_ctx` 65536, lanes chat 4 / embed 2 / decision 1. `qwen3.6` arms not run: they were conditional on the three mixed arms passing.

| arm | calls | failed | chat p50 / p95 | embed p50 / p95 | decision p50 / p95 | reloads after the first load |
|---|---|---|---|---|---|---|
| `E4-solo-r` | 30 | 0 | — | 0.15 s / 2.2 s | 1.8 s / 9.5 s | 0 |
| `E4-ce` | 60 | 0 | 18.5 s / 52 s | 0.14 s / 0.19 s | — | 0 |
| `E4-cd` try 1 | 50 | 5 (chat) | 24.9 s / 87 s | — | 3.2 s / 83 s | 4 (chat) |
| `E4-cd` try 2 | 50 | 3 (chat) | 23.0 s / 99 s | — | 3.3 s / 18 s | 4 (chat) |
| `E4-ced` try 1 | 70 | 3 chat, 1 decision | 18.5 s / 210 s | 0.16 s / 62 s | 3.4 s / 18 s | 5 (chat) |
| `E4-ced` try 2 | 70 | 4 (chat) | 18.7 s / 102 s | 0.15 s / 2.3 s | 3.1 s / 85 s | 5 (chat) |
| `E4-d1` | 20 | 0 | — | — | 1.9 s (wait 1.4 s) | 0 |
| `E4-d2` | 20 | 0 | — | — | 3.3 s | 0 |

(The solo decision p95 is its first call, which paid the 8.8 s load; there are 10 decision calls.)

- **Chat + embed passes both rules.** No reload after the first load; embed under chat load p50 0.14 s, p95 0.19 s against the solo 0.15 s / 2.2 s.
- **Every arm with `gemma4` and `nimble` loaded together is a host fault**, on both tries. The failures are chat calls: `unexpected EOF` while running the model, and 500s, each followed by a reload of `gemma4` (4–5 per run). The decision model alone (`E4-d1`, `E4-d2`) and the chat model with the embedder (`E4-ce`) never fail.
- **So the fault is not the one-slot chat model**: `gemma4` has four real slots and still dies, and only when a second LLM is resident. An embedder beside it is fine. The likeliest cause is memory: two LLMs, one with four 64K-context slots, on the host's shared memory. Not confirmed — the host's own logs were not read.
- **Isolation** is met where the run is clean (`E4-ce`). In the faulted arms the decision p95 reached 83–85 s, but those runs are not scored.
- **Decision lane:** two calls at once took 3.34 s each against 1.88 s for one: **1.78×**, just under the pre-registered 1.8 threshold, so by the rule the host does not serialise the decision model. The gain is small: 0.60 calls/s at lane 2 against 0.53 calls/s at lane 1 (+13 %).

**Decides, per amendment 3:**
- The multi-model fault follows **two LLMs computing at once**. On this host `max_models_in_flight: 1` on the Ollama provider is the setting that avoids it. It also makes the embedder wait for the chat model, which `E4-ce` showed is not needed: the cap counts every model, and no setting today leaves the embedder out of it (a follow-up, if that wait matters).
- Isolation did not fail in a clean arm: no reserved host share is needed.
- `E4-d2` does not serialise by the rule, but the 13 % gain does not pay for a second decision slot beside a chat model: the decision lane default on Ollama stays **1**.

## E5 — a different `num_ctx` is a different model

Two agents on one model, alternating calls at `num_ctx` 8192 and 65536:
- **Every call reloaded the model:** `load_ms` 10.6–12.9 s on all 12 calls.
- **Every call was fully cold:** prefill grew from 10.5 s to 17.1 s with the prompt, exactly the cold curve. No reuse survived a switch.

## Host faults seen

| when | what | load at the time |
|---|---|---|
| before E1 (smoke) | 2 runs lost calls | three models loading, then in use |
| E3 first grid | 3 runs lost calls | chat model only; host overheating (fixed by the owner) |
| E3 re-run, 15:50 | 1 run lost 4 calls | chat model only, after the cooling fix |
| E4 mixed | both tries failed; host unresponsive on the second | three models in use |
| E5 try 1 | first 2 calls failed | host recovering from E4 |
| E4 re-run, `E4-cd` and `E4-ced` | every try lost 3–5 chat calls (`unexpected EOF`, 500) and reloaded `gemma4` | `gemma4` (4 slots, 64K) and `nimble` both loaded |

## What RFC EI takes from this

1. **No lease.** `affinity_hold_ms` 0 and `quantum_calls` 1: the chat lane is a per-call fair queue. The lease and quantum machinery is not built for Ollama. Explicit pinning (llama.cpp `id_slot`) stays a P4 question and needs its own measurement.
2. **Chat lane cap = the host's parallel slots** (4 here). Throughput is the same from 2 to 8 concurrent agents, so the cap costs nothing and keeps the waiting inside loomcycle.
3. **Call-level gating is the fix for the starvation**, independent of any tuning: it removes the 257 s p95 and the 0.40 spread of the run-level gate.
4. **Model identity is (model, `num_ctx`).** Two agents on one model with different context sizes cost a reload and a full cold prefill on every switch. P2 must treat them as different models and should warn when an agent's context size differs from the provider's.
5. **Lanes cannot assume a host tolerates several models computing at once.** P2 needs an opt-in cap on models in flight per provider, and a call must survive a runner crash (retry once the runner is back).
6. **Answered by the E4 re-run (2026-10-14):** the fault follows two LLMs computing at once on this host, not the one-slot model and not the embedder. `max_models_in_flight: 1` on the Ollama provider avoids it, at the cost of making embeddings wait for the chat model (the cap counts every model; leaving the embedder out of it would be a follow-up). Decision lane 1 per decision model. Whether two LLMs fit on a host with more memory is a property of the host, not of loomcycle.
