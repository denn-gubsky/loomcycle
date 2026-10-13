# RFC EI phase P0 — what the model host does with concurrent agents (pre-registration)

Written and committed **before** any experiment below has run. The thresholds and the
decision rules may not change after the first result is seen. Any change is an amendment:
dated, and committed before the run it affects. A mechanics smoke test (2 agents,
3 turns, never scored) runs first and is discarded.

## Question

RFC EI replaces the run-level provider gate with per-call lanes. Three of its defaults
cannot be chosen from first principles. They depend on what the model host does with
its KV cache when several agents share it:

- **`affinity_hold_ms`:** whether a run should keep its slot between calls. This is
  worth it only if the host reuses a cached prompt prefix, and reuses it per slot.
- **`quantum_calls`:** how many consecutive calls a lease holder gets while others wait.
- **The lane caps** (`max_concurrent` per kind) and **`max_loaded_models`**.

P0 measures the host, not loomcycle. The probe (`bench/cmd/schedprobe`) drives the
**shipped** Ollama chat driver, embedder and decision driver directly, under a
scheduler of its own that implements the policies below.

## Instrument

Each call is recorded with the probe's queue wait, wall time, TTFT, and the server's
own `prompt_eval_count`, `load`, `prefill` and `decode` durations.

The prompt size is in characters (`prompt_chars`). The probe never tokenizes.

**Cold calls.** A cold call (`-cold`) prefixes the system prompt with a fresh nonce, so
no cached prefix can match.

**Model calibration** (`analyze.py`). It comes from the **E1 cold arm only**: one agent,
alone on the host, every call cold. For each model it fits:
- tokens per character: the median of `prompt_eval_count` / `prompt_chars`. Valid because on a cold call the whole prompt is evaluated.
- cold prefill time: a least-squares line, `prefill_ms = a + b · prompt_chars`, over the arm's calls.

**Two reuse readings per warm call:**
- `count_ratio` = `prompt_eval_count` / (`prompt_chars` × tokens per character). Near 1 means the server counts the whole prompt either way. Well below 1 means it counts only what it evaluated.
- `reuse` = 1 − `prefill_ms` / (a + b · `prompt_chars`). This works whichever way the server counts.

**Bias, known in advance.** In multi-agent runs the host's concurrent prefill can lengthen
a call's `prefill_ms`, which reads as less reuse. The estimate is conservative there; that
is accepted, because what scheduling changes is the prefill time actually paid.

**Instrument checks, on E1, which must be able to fail.** If either fails, the measure
is broken: nothing after E1 is scored, and P0 reports that.

1. **The nonce busts the cache.** In the cold arm, prefill grows with the prompt:
   - Let the size ratio be `prompt_chars` on turn 10 / `prompt_chars` on turn 1.
   - Then `prefill_ms` on turn 10 / `prefill_ms` on turn 1 must be **≥ 0.8 ×** that size ratio.
   - If the nonce left a prefix reusable, later cold turns would evaluate only the new text, and this ratio collapses.
2. **The warm arm starts cold.** Turn 1 is the first sight of the prompt in both arms, so the warm arm's turn-1 `prefill_ms` must be within **±30 %** of the cold arm's turn 1.

## Workload

`schedprobe -gen` builds the workloads with a fixed seed:

- **Agents:**
  - Each has its own system prompt of 16,000 characters: synthetic instructions plus tool-schema-like JSON.
  - Each is seeded separately, so two agents share no prefix.
- **Turns:**
  - 10 per agent.
  - Each turn adds 1,500 characters of tool-result-like text and asks for a short summary, at `max_tokens` 256.
- **Reproducibility:**
  - Temperature 0, seed 1, `effort: low` (thinking off), so a cold run and a warm run produce the same conversation.
  - The assistant turns are the model's own replies, as in a real loop, so a warm cache covers them.
- **Context window:** the chat agents' `num_ctx` is the host's working value, recorded in the run label.
- **Tool time:** a sleep between an agent's calls, `-tool-ms` (default 500).

## Experiments and decision rules

| # | Setup | Pass / decision |
|---|---|---|
| **E1** | 1 agent; one cold run, one warm run | **Instrument checks above.** Then: warm median reuse on turns 2–10 **≥ 0.5** means reuse is observable. Below 0.5 means **no affinity is built**: `affinity_hold_ms` is fixed at 0 and the chat lane does call-level round-robin only. |
| **E2** | Policy `call` (quantum 1, no hold); K = 2, 4, 6, 8 agents; host `OLLAMA_NUM_PARALLEL` N = 2 and 4 | **Slot-bound cache:** reuse ≥ 0.5 at K ≤ N and **< 0.3** at K ≥ 2N. **Unbounded cache:** reuse ≥ 0.5 at K = 2N; then affinity is unnecessary and the hold defaults to 0. **No prefix matching:** reuse < 0.3 at K ≤ N; affinity cannot help on this host; hold 0. |
| **E3** | K = 8 agents, host N = 4, probe chat slots 4. Policies: `none`, `run` (today), `call`, and `lease` with quantum ∈ {1, 2, 4, 8} × hold ∈ {0, 2000, 5000} ms | Metrics: aggregate output tokens/s (Σ `eval_count` / makespan), p50 and p95 TTFT, and the spread of agent finish times (sd / mean). **Choose:** the `lease` setting with the highest tokens/s among those whose p95 TTFT is within **1.25×** of the best p95 of all `lease` settings. On a tie (within 5%), take the smaller quantum, then the shorter hold. If E1/E2 ruled affinity out, only hold 0 is eligible. |
| **E4** | Concurrent: 4 chat agents, 2 embed agents, 1 decision agent; 3 distinct models; probe lanes chat 4 / embed 2 / decision 1 | **No thrash:** after each model's first call, **0** calls with `load_ms` > 1000. **Isolation:** embed and decision p95 latency under the chat load **≤ 2×** their solo baseline (the same agents with the chat lane empty). Failing isolation means the lanes need a reserved host share, which becomes a P2 design input. |
| **E5** | 1 model, 2 agents alternating calls at two `num_ctx` values | **≥ 1 reload** (`load_ms` > 1000) per switch means the model identity is `(model, num_ctx)`, as RFC EI assumes. No reloads means identity is the model alone. |

**Side check, not scored:** whether `prompt_eval_count` on a warm call counts only the
evaluated tokens. If it does, the usage ledger under-reports Ollama input tokens today;
P0 reports it as a finding.

## Host

Nothing else may run on the host during a run. A run that sees a `load_ms` it did not
cause is discarded and repeated.

### Amendment 1 — 2026-10-13, before E1

**Host.** The owner's local AMD inference box, `ollama-host`, running Ollama 0.40.3.

| setting | value |
|---|---|
| `OLLAMA_CONTEXT_LENGTH` | 65536 |
| `OLLAMA_KEEP_ALIVE` | 10m |
| `OLLAMA_NUM_PARALLEL` | 4 |
| `OLLAMA_MAX_LOADED_MODELS` | 4 |

**Models.**
- chat: `qwen3.6:latest` (36B MoE, 23.6 GB loaded at `num_ctx` 65536);
- embed: `bge-m3:latest`;
- decision: `nimble:latest`.

**Chat `num_ctx`** is 65536, so the E5 pair is 8192 and 65536.

**Mechanics smoke tests — seen, not scored, and the reason for the changes below:**
- Two of the first runs lost calls to runner crashes: `llama-server process has terminated: signal: aborted (core dumped)` and `unexpected EOF`.
  - The first crash happened while the three models were loading together.
  - The second happened with all three loaded and in use together, and took all three down at once.
- Four diagnostic runs did **not** crash:
  - D1: four agents on the chat model;
  - D2: chat + embed;
  - D3: chat + decision;
  - D4: chat + embed + decision, all at once.
- So the fault is intermittent rather than tied to a model combination.
- In those runs `prompt_eval_count` reported the **full** prompt on warm calls, while `prefill_ms` fell from about 12 s to about 2 s. Reuse is therefore read from time, as the instrument section already provides.

**Changes:**
1. **A run in which any call fails is discarded and repeated once.** A second failure is reported as a host fault for that setting, not scored. A crash costs every agent in flight a call, which would otherwise read as a scheduling effect.
2. **E2 and E3 use 6 turns per agent, not 10. E1 keeps 10.**
   - On this host a cold 16,000-character turn costs about 12 s of prefill, a warm one about 2 s, and a reply about 6 s.
   - At 10 turns the E3 grid alone takes about four hours.
   - Reuse is read from turn 2 on, so six turns give five readings per agent.
3. **E2 runs at host N = 4 only**, with K = 2, 4, 6, 8. Changing N needs a restart of the owner's host. The slot-bound rule still decides: it compares K ≤ N (2 and 4) against K ≥ 2N (8). The N = 2 arm is dropped, not deferred.
4. **E4's three models** are the chat, embed and decision models above.

## Output

- **Per run:** `bench/docs/schedprobe/runs/<label>.jsonl` (one record per call).
- **Analysis:** `analyze.py` writes the tables.
- **Defaults:** they go into a research document under `/loomcycle/docs/`, linked to RFC EI.
