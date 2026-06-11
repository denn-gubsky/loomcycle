# exp1 — tool access (write → save → run → verify)

A single coding agent (`code-guru`) exercises loomcycle's **built-in tools**
(`Read`/`Write`/`Edit`/`Bash`/`Grep`/`Glob`/`WebSearch`/`WebFetch`), all scoped to
`./work`. You ask it to write a program, it saves the file, runs it, reads the
output back, and reports a PASS/FAIL verdict — the end-to-end "operator enables
tools → agent uses them → result is independently verifiable" loop.

This is the simplest example: one agent, no channels/MCP/webhooks. Start here.

## What it demonstrates
- Operator tool-enablement (env roots + `LOOMCYCLE_BASH_ENABLED`) ∩ the agent's
  `allowed_tools` anchored allowlist.
- The sandbox path model: `Read`/`Write` resolve relative paths against the process
  CWD; `run.sh` launches from `./work`, so the Write base, Bash CWD, and the
  read/write roots all line up (no surprises).
- Provider routing with fallback: **Anthropic OAuth (sonnet) → deepseek-v4-pro**.

## Prerequisites
- **loomcycle** on your PATH (`brew install denn-gubsky/loomcycle/loomcycle`), or set
  `LOOMCYCLE_BIN=/path/to/loomcycle`.
- **A model provider** — either of:
  - **Anthropic OAuth (primary):** run `loomcycle anthropic login` once, then keep
    `LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED=1` (set by `run.sh`). Research/dev only.
  - **DeepSeek (fallback):** put `DEEPSEEK_API_KEY` in `.env.local`. If OAuth isn't
    logged in, the provider is excluded and runs use `deepseek-v4-pro` automatically.
- `python3` (the agent writes+runs a small Python program by default; it may choose
  another language).

## Run it
> **`loomcycle validate` note:** this example is **tier-based** (for the OAuth→deepseek
> fallback). `validate` resolves explicit pins but doesn't probe providers, so it reports
> `no provider resolved` for a tier agent — a static-check limitation, not a config bug.
> Verify by **running** (below) and reading the boot `resolve probe:` lines.

```bash
cd examples/exp1-tools-usage
./run.sh            # first launch copies .env.local.example → .env.local; edit secrets, re-run
```
`run.sh` starts the HTTP/SSE server on `127.0.0.1:8787` (override `LOOMCYCLE_LISTEN_ADDR`).
Leave it running; drive it from a second terminal.

Fill `.env.local` (created on first run): set `DEEPSEEK_API_KEY` (and/or do the OAuth
login). `LOOMCYCLE_AUTH_TOKEN` may stay empty for local dev (open mode); set it
(`openssl rand -hex 32`) to require a bearer.

## Drive the experiment (second terminal)
Spawn `code-guru` with a verifiable task. The run streams Server-Sent Events; the
terminating `done` event carries `usage.{provider,model}` + `stop_reason`.
```bash
cd examples/exp1-tools-usage
cat > /tmp/exp1.json <<'JSON'
{"agent":"code-guru","user_id":"exp1","segments":[{"role":"user","content":[{"type":"trusted-text",
"text":"Compute the first 100 prime numbers. Create a subdirectory exp1-out/ and write a small program exp1-out/primes.py that prints them one per line. Run it and show the output. State the absolute path of the file you created."}]}]}
JSON
./loomcurl.sh -N -X POST http://127.0.0.1:8787/v1/runs \
  -H 'Content-Type: application/json' -d @/tmp/exp1.json
```
Expect SSE events: `session → agent → started → text/tool_call/tool_result… → usage → done`.
The tool calls should include `Bash` and `Write`; `done` reports the resolved
`provider` (`anthropic-oauth-dev` or `deepseek`) and `model`.

## Verify independently (don't trust the agent's report)
```bash
# the file landed in the jail, not the example root:
ls -la work/exp1-out/                 # primes.py present here
ls exp1-out 2>/dev/null && echo "LEAK (should NOT exist at the example root)" || echo "no root leak ✓"

# re-run the agent's program and check the primes against a fresh computation:
( cd work/exp1-out && python3 primes.py ) > /tmp/agent_primes.txt
python3 - <<'PY'
def first_n_primes(n):
    ps=[]; c=2
    while len(ps)<n:
        if all(c%p for p in ps if p*p<=c): ps.append(c)
        c+=1
    return ps
exp=first_n_primes(100)
got=[int(x) for x in open('/tmp/agent_primes.txt').read().split()]
print("count", len(got), "(want 100); 100th =", got[-1] if got else None, "(want 541)")
print("MATCH ✓" if got==exp else "MISMATCH ✗")
PY
```

## Teardown
Stop the server with Ctrl-C. State lives in `./data` (SQLite); delete it for a clean
slate. The agent's output files are under `./work`.

## Files
| File | Purpose |
|---|---|
| `loomcycle.yaml` | providers/tiers (OAuth→deepseek fallback) + the `code-guru` agent |
| `run.sh` | self-contained launcher (bootstraps `.env.local`, sets the tool sandbox, starts the server) |
| `.env.local.example` | secret template (empty values) — copied to `.env.local` on first run |
| `loomcurl.sh` | token-safe REST helper (omits the bearer in open mode) |
| `work/` | the tool sandbox (agent reads/writes/runs here) |
| `data/` | SQLite state |
