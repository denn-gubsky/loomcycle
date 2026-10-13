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

**Model calibration** (`analyze.py`). From the cold calls of a model, it fits:
- tokens per character, from `prompt_eval_count` over `prompt_chars`. Valid because on a cold call the whole prompt is evaluated.
- prefill ms per token, as a function of prompt size: a linear fit on the cold calls.

**Two reuse readings per warm call:**
- `count_ratio` = `prompt_eval_count` / the expected tokens. Near 1 means the server counts the whole prompt either way. Well below 1 means it counts only what it evaluated.
- `time_reuse` = 1 − `prefill_ms` / the expected cold prefill at that size. This works whichever way the server counts.

**Reuse** is `time_reuse`, unless `count_ratio` turns out to separate the cold and warm
arms better (decided by the instrument check below).

**Instrument check, which must be able to fail.** In E1 the cold arm must read
median reuse **≤ 0.2** on turns 2–10. If it does not, the measure is broken: nothing
after E1 is scored, and P0 reports that.

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
| **E1** | 1 agent; one cold run, one warm run | **Instrument check above.** Then: warm median reuse on turns 2–10 **≥ 0.5** means reuse is observable. Below 0.5 means **no affinity is built**: `affinity_hold_ms` is fixed at 0 and the chat lane does call-level round-robin only. |
| **E2** | Policy `call` (quantum 1, no hold); K = 2, 4, 6, 8 agents; host `OLLAMA_NUM_PARALLEL` N = 2 and 4 | **Slot-bound cache:** reuse ≥ 0.5 at K ≤ N and **< 0.3** at K ≥ 2N. **Unbounded cache:** reuse ≥ 0.5 at K = 2N; then affinity is unnecessary and the hold defaults to 0. **No prefix matching:** reuse < 0.3 at K ≤ N; affinity cannot help on this host; hold 0. |
| **E3** | K = 8 agents, host N = 4, probe chat slots 4. Policies: `none`, `run` (today), `call`, and `lease` with quantum ∈ {1, 2, 4, 8} × hold ∈ {0, 2000, 5000} ms | Metrics: aggregate output tokens/s (Σ `eval_count` / makespan), p50 and p95 TTFT, and the spread of agent finish times (sd / mean). **Choose:** the `lease` setting with the highest tokens/s among those whose p95 TTFT is within **1.25×** of the best p95 of all `lease` settings. On a tie (within 5%), take the smaller quantum, then the shorter hold. If E1/E2 ruled affinity out, only hold 0 is eligible. |
| **E4** | Concurrent: 4 chat agents, 2 embed agents, 1 decision agent; 3 distinct models; probe lanes chat 4 / embed 2 / decision 1 | **No thrash:** after each model's first call, **0** calls with `load_ms` > 1000. **Isolation:** embed and decision p95 latency under the chat load **≤ 2×** their solo baseline (the same agents with the chat lane empty). Failing isolation means the lanes need a reserved host share, which becomes a P2 design input. |
| **E5** | 1 model, 2 agents alternating calls at two `num_ctx` values | **≥ 1 reload** (`load_ms` > 1000) per switch means the model identity is `(model, num_ctx)`, as RFC EI assumes. No reloads means identity is the model alone. |

**Side check, not scored:** whether `prompt_eval_count` on a warm call counts only the
evaluated tokens. If it does, the usage ledger under-reports Ollama input tokens today;
P0 reports it as a finding.

## Host

To be fixed by amendment before E1 runs:
- the host;
- its Ollama version;
- `OLLAMA_NUM_PARALLEL` and `OLLAMA_MAX_LOADED_MODELS`;
- the models per kind;
- the chat `num_ctx`.

Nothing else may run on the host during a run. A run that sees a `load_ms` it did not
cause is discarded and repeated.

## Output

- **Per run:** `bench/docs/schedprobe/runs/<label>.jsonl` (one record per call).
- **Analysis:** `analyze.py` writes the tables.
- **Defaults:** they go into a research document under `/loomcycle/docs/`, linked to RFC EI.
