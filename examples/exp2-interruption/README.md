# exp2 — interruption (human-in-the-loop Yes/No gating)

`yesno-agent` calls the **Interruption** tool (`op=ask`) to ask "Approve the
deployment?", **blocks**, and waits for a human answer delivered over REST. You
resolve the interrupt with `Yes` or `No`; the run wakes and branches its final
one-line output: `RESULT: PROCEED` (Yes) or `RESULT: ABORT` (No).

## What it demonstrates
- The Interruption primitive: an agent pausing for human input mid-run.
- The two-part gate: `tools:[Interruption]` **and** `interruption.enabled:true`
  (tools alone makes the tool refuse).
- **Same-runtime resolve:** the resolve hits the runtime that owns the run, so the
  blocked loop wakes immediately (no cross-process bus issue).
- Routing fallback: Anthropic OAuth (sonnet) → deepseek-v4-pro.

## Prerequisites
Same as exp1: `loomcycle` on PATH; a provider (OAuth via `loomcycle anthropic login`,
**or** `DEEPSEEK_API_KEY` in `.env.local`). No file/shell tools are used.

## Run it
> **`loomcycle validate` note:** tier-based config (for the OAuth→deepseek fallback) —
> `validate` reports `no provider resolved` because it doesn't probe providers. That's a
> static-check limitation, not a config bug; verify by **running** and reading the boot
> `resolve probe:` lines.

```bash
cd examples/exp2-interruption
./run.sh            # first launch bootstraps .env.local; edit secrets, re-run
```
Server listens on `127.0.0.1:8787`. Drive it from a second terminal.

## Drive the experiment — the resolve flow
The run **blocks** on `Interruption.ask`, so stream it in the background, read the
`interruption_pending` SSE event to get the `run_id` + `interrupt_id`, then resolve.
A ready-made driver (`exp2.sh`):
```bash
cd examples/exp2-interruption
cat > exp2.sh <<'DRV'
#!/usr/bin/env bash
# usage: ./exp2.sh <Yes|No>
set -uo pipefail
ANSWER="${1:-Yes}"; HERE="$(cd "$(dirname "$0")" && pwd)"; BASE="http://127.0.0.1:8787"
OUT="$(mktemp)"
printf '{"agent":"yesno-agent","user_id":"exp2","segments":[{"role":"user","content":[{"type":"trusted-text","text":"Begin."}]}]}' > /tmp/exp2.body.json
( timeout 120 "$HERE/loomcurl.sh" -N -X POST "$BASE/v1/runs" -H 'Content-Type: application/json' -d @/tmp/exp2.body.json > "$OUT" 2>&1 ) &
SSE=$!
RID=""; IID=""
for i in $(seq 1 60); do
  RID=$(grep -oE '"run_id":"[^"]+"' "$OUT" | head -1 | cut -d'"' -f4)
  IID=$(grep -oE '"interrupt_id":"[^"]+"' "$OUT" | head -1 | cut -d'"' -f4)
  [ -n "$RID" ] && [ -n "$IID" ] && break; sleep 1
done
echo "run_id=$RID interrupt_id=$IID  → answering $ANSWER"
"$HERE/loomcurl.sh" -X POST "$BASE/v1/runs/$RID/interrupts/$IID/resolve" \
  -H 'Content-Type: application/json' -d "{\"answer\":\"$ANSWER\"}"; echo
wait $SSE 2>/dev/null
echo "--- final text + done ---"; grep -E '"text":"RESULT|"stop_reason"|"provider"' "$OUT" | tail -3
DRV
chmod +x exp2.sh

./exp2.sh Yes     # expect: RESULT: PROCEED
./exp2.sh No      # expect: RESULT: ABORT
```

### What you should see
- An `interruption_pending` SSE event carrying `interrupt_id` (shape `intr_…`).
- The resolve `POST` returns `{"status":"resolved"}`.
- The run wakes and the final `text` event is exactly `RESULT: PROCEED` (Yes) or
  `RESULT: ABORT` (No); `done` reports the resolved `provider`/`model`.

### REST endpoints used
- `POST /v1/runs` — spawn (SSE stream).
- `POST /v1/runs/{run_id}/interrupts/{interrupt_id}/resolve` — body `{"answer":"Yes"|"No"}`.
- (also useful) `GET /v1/runs/{run_id}/interrupts` — list pending interrupts for a run.

## Teardown
Ctrl-C the server. Delete `./data` for a clean slate.

## Files
| File | Purpose |
|---|---|
| `loomcycle.yaml` | routing (OAuth→deepseek) + `yesno-agent` (Interruption gate enabled) |
| `run.sh` | self-contained launcher |
| `.env.local.example` | secret template (empty) |
| `loomcurl.sh` | token-safe REST helper |
