# exp3 — multi-agent refine/evaluate loop

Three agents collaborate over channels + shared memory to refine an answer across
**5 hops**:

```
kickoff {hop:0,question} ─▶ exp3-ch1 ─▶ answerer ─(answer:hopN, {hop:N})▶ exp3-ch2 ─▶ evaluator
                                ▲                                              │ Evaluation.submit + advice
                                └────────────── {hop:H} ───────────────────────┘
                                                          (hop 5) {done:true} ─▶ exp3-ch3 ─▶ aggregator
                                                                                  picks winner, retires memory
```
The evaluator scores each answer (`Evaluation.submit`) and writes improvement
advice; the answerer incorporates it next hop — so **scores climb** as the answer is
refined. After hop 5 the aggregator reads all answers + scores, picks the highest,
and deletes the shared memory keys.

## What it demonstrates
- **Channel** pub/sub across 3 channels (cursor-based at-least-once delivery).
- **Memory** `scope: user` shared across agents (the `memory_scopes:[user]` gate).
- **Evaluation** submit + `list_for_run` retrieval (the `evaluation_scopes` gate).
- **Context** tool discovery (`op=tools`/`op=doc`/`op=self`).
- Routing fallback: Anthropic OAuth (sonnet) → deepseek-v4-pro.

## Prerequisites
Same as exp1/exp2: `loomcycle` on PATH; a provider (OAuth via `loomcycle anthropic
login`, **or** `DEEPSEEK_API_KEY`). No file/shell tools. `run.sh` raises
`LOOMCYCLE_CHANNELS_LONGPOLL_CAP_MS` to 180000 so the agents' long `wait_ms`
subscribes aren't truncated to the 30 s default.

## Run it
> **`loomcycle validate` note:** tier-based config (for the OAuth→deepseek fallback) —
> `validate` reports `no provider resolved` because it doesn't probe providers. Not a
> config bug; verify by **running** and reading the boot `resolve probe:` lines.

```bash
cd examples/exp3-multiagent-loop
./run.sh            # first launch bootstraps .env.local; edit secrets, re-run
```
Server on `127.0.0.1:8787`. Drive from a second terminal.

## Drive the experiment
The three agents are long-lived subscribers — spawn all three (background SSE,
`user_id=exp3`), then publish the kickoff to `exp3-ch1`.
```bash
cd examples/exp3-multiagent-loop
for a in exp3-answerer exp3-evaluator exp3-aggregator; do
  printf '{"agent":"%s","user_id":"exp3","segments":[{"role":"user","content":[{"type":"trusted-text","text":"Begin your loop now."}]}]}' "$a" > /tmp/$a.json
  ( timeout 600 ./loomcurl.sh -N -X POST http://127.0.0.1:8787/v1/runs \
      -H 'Content-Type: application/json' -d @/tmp/$a.json > /tmp/$a.sse 2>&1 ) &
  echo "spawned $a"
done
sleep 6   # let them reach their subscribe

# kickoff (admin publish to the global channel) — a question to refine:
./loomcurl.sh -X POST http://127.0.0.1:8787/v1/_channels/exp3-ch1/publish \
  -H 'Content-Type: application/json' \
  -d '{"payload":{"hop":0,"question":"What is courage?"}}'
```
The loop runs ~2–5 min. Watch progress (the example ships its SQLite store at
`./data/loomcycle.db`):
```bash
watch -n5 'sqlite3 data/loomcycle.db "SELECT count(*) AS evaluations FROM evaluations;"; \
           grep -c "AGGREGATOR DONE" /tmp/exp3-aggregator.sse 2>/dev/null'
```

## Verify independently
```bash
# 5 evaluations stored, overall score climbing across hops:
sqlite3 data/loomcycle.db "SELECT substr(judgement,1,12) AS hop, substr(score,1,6) AS overall FROM evaluations;"
#   {"hop":1}|0.83   {"hop":2}|0.94   {"hop":3}|0.97   {"hop":4}|0.98   {"hop":5}|0.99   (illustrative)

# memory retired by the aggregator (0 keys after a clean finish):
sqlite3 data/loomcycle.db "SELECT count(*) AS memory_keys FROM memory;"

# the aggregator's winner verdict (reconstruct the split SSE text events):
python3 - <<'PY'
import json
t=[json.loads(l.strip()[5:]).get("text","") for l in open('/tmp/exp3-aggregator.sse')
   if l.strip().startswith('data:') and l.strip()[5:].strip() and '"type":"text"' in l]
f="".join(t); i=f.rfind("Winner"); print(f[max(0,i-30):i+200] if i>=0 else f[-300:])
PY
```
Expect: 5 evaluations (hops 1–5), monotonically improving overall scores, the
aggregator naming **hop 5** as the winner, and memory cleaned to 0 keys. The
consolidator/aggregator run's `done.usage.provider` confirms the route
(`anthropic-oauth-dev` or `deepseek`).

## Notes
- Channels are **global-scope** so the curl kickoff (admin publish, which targets
  global) is delivered; the queue semantic buffers the kickoff so agent start order
  doesn't matter.
- If the aggregator names hop 4 instead of hop 5, it read `list_for_run` before the
  hop-5 score was visible — a prompt read-ordering nuance, not a substrate fault;
  the store has all 5 scores.

## Teardown
Ctrl-C the server; `kill %1 %2 %3` any lingering SSE curls; delete `./data` for a
clean slate.

## Files
| File | Purpose |
|---|---|
| `loomcycle.yaml` | routing + 3 channels + the answerer/evaluator/aggregator agents |
| `run.sh` | self-contained launcher (raises the long-poll cap) |
| `.env.local.example` | secret template (empty) |
| `loomcurl.sh` | token-safe REST helper |
