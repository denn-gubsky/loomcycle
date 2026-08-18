# exp11 — Skybits shared documents via remote MCP + cross-run Memory

A loomcycle agent collaborates on [Skybits](https://skybits.ai) documents
through Skybits' **remote MCP server**, and **remembers document facts across
runs** via loomcycle memory:

```
operator POST /v1/runs  agent=doc-agent  user_id=exp11
   └▶ Memory op=list scope=user prefix="skybits:"        (recall prior context)
   └▶ mcp__skybits__create_doc / outline_document / read_document / edit_document …
        over Streamable HTTP to https://skybits.ai/mcp (Bearer connector key)
   └▶ Memory op=set scope=user skybits:doc:<slug> …      (record durable facts)

operator POST /v1/runs  agent=doc-agent  user_id=exp11   (run 2 — same user_id)
   └▶ Memory op=list scope=user prefix="skybits:"        (recalls run-1 facts)
   └▶ opens the SAME doc via the recalled doc_url, edits it, updates memory
```

## What it demonstrates
- A **remote 3rd-party MCP server consumed with zero loomcycle code changes** —
  one `mcp_servers:` yaml block (`transport: http`, `url`, `headers`) mounts all
  59 Skybits tools as `mcp__skybits__*`.
- **Out-of-band token provisioning**: loomcycle's MCP client has no OAuth client
  flow, so the Skybits connector API key arrives as a plain Bearer via
  `${LOOMCYCLE_SKYBITS_TOKEN}` (env, expanded at config load) or, per-run, via
  `${run.credentials.skybits}` (`user_credentials.skybits` on `POST /v1/runs`).
- **Cross-run memory**: MCP tool calls are NOT auto-recorded, so the agent
  explicitly writes durable facts (doc URLs, structure, decisions, collaborator
  feedback) with `Memory op=set scope=user`. A later run with the same
  `user_id` recalls them — no doc_url has to be re-told.

## The Skybits tool surface (59 tools)
`outline_document`, `read_document`, `find_in_document`, `edit_document`
(atomic batch ops: `replace` / `insert_before` / `insert_after` / `remove` /
`move`), `create_doc`, `find_documents`, comment threads,
`document_history` / `diff_document` / `restore_document`, tags, sharing,
sync graph, export jobs. Documents are addressed by `doc_url`. There is **no
suggestions tool** — edits apply directly, but are attributed, versioned, and
revertible. Browse the full public catalog (no auth needed):

```bash
curl -s https://skybits.ai/v1/tools | python3 -m json.tool | head -40
```

> **This example is NOT fully self-contained** — it needs a **Skybits account**
> with a connector API key, plus a model provider. exp1–exp3 need only
> loomcycle + a provider.

## Prerequisites

1. **loomcycle** on PATH (or `LOOMCYCLE_BIN`), and a model provider (OAuth login, or
   `DEEPSEEK_API_KEY`).
2. **A Skybits account + connector API key.** Skybits auth is OAuth 2.1
   (auth server `https://auth.skybits.ai`, DCR + PKCE S256 + device-code grant;
   discovery doc at
   `https://skybits.ai/.well-known/oauth-protected-resource/mcp`) for
   interactive clients, but for **autonomous agents** you mint a **connector
   API key** (plain Bearer) via the OAuth device flow using Skybits' public
   helper:
   ```bash
   # fetch the helper bundle described at https://skybits.ai/skill, then:
   python scripts/skybits.py login <name>
   ```
   Follow the device-flow prompt in a browser; the helper prints a connector
   key. (Equivalent API: `POST https://skybits.ai/v1/mcp/connectors` with
   scopes `["mcp.connect","agent.tools.read","agent.tools.write"]`.)
   Put the key in `.env.local` as `LOOMCYCLE_SKYBITS_TOKEN`.

## Setup

Fill `.env.local` (run.sh creates it from the template on first launch):
`LOOMCYCLE_SKYBITS_TOKEN` + `DEEPSEEK_API_KEY` (or `loomcycle anthropic login`
for the OAuth primary). `LOOMCYCLE_AUTH_TOKEN` may stay empty (dev open mode).

## Run + drive
> **`loomcycle validate` passes** on this tier-based config
> (`provider=(by tier) model=tier:middle`). Verify provider reachability by
> **running** and reading the boot `resolve probe:` + `mcp[…]` lines.

```bash
cd examples/exp11-skybits-documents
./run.sh                      # starts the server; boot log: mcp[skybits]: ready, 59/59 tools registered (transport=http)
```

In a second terminal — **run 1**: create a document and save facts to memory:

```bash
cd examples/exp11-skybits-documents
cat > /tmp/exp11-run1.json <<'JSON'
{"agent":"doc-agent","user_id":"exp11","segments":[{"role":"user","content":[{"type":"trusted-text","text":"Create a Skybits document titled 'exp11 loomcycle notes' with two sections, Goals and Log. Add one Log entry: 'run 1 created this doc'. Then record the doc_url, title, and structure to memory (user scope, skybits: keys) and finish with a one-line summary naming the doc_url."}]}]}
JSON
./loomcurl.sh -N -X POST http://127.0.0.1:8787/v1/runs \
  -H 'Content-Type: application/json' -d @/tmp/exp11-run1.json | tee /tmp/exp11-run1.sse
```

**run 2** — a fresh run that recalls the facts without being told the doc_url:

```bash
cat > /tmp/exp11-run2.json <<'JSON'
{"agent":"doc-agent","user_id":"exp11","segments":[{"role":"user","content":[{"type":"trusted-text","text":"Recall from memory which Skybits documents we have worked on (do NOT ask me for a doc_url). Outline the document you find, append one Log entry saying 'run 2 recalled this doc from memory', then update the memory entry. Finish with a one-line summary naming the doc_url."}]}]}
JSON
./loomcurl.sh -N -X POST http://127.0.0.1:8787/v1/runs \
  -H 'Content-Type: application/json' -d @/tmp/exp11-run2.json | tee /tmp/exp11-run2.sse
```

### Optional: per-user Skybits token instead of the env fallback

Pass a different user's connector key on a single run — it wins over
`LOOMCYCLE_SKYBITS_TOKEN` (`${run.credentials.skybits:-…}` resolution order):

```bash
cat > /tmp/exp11-run3.json <<'JSON'
{"agent":"doc-agent","user_id":"alice","user_credentials":{"skybits":"<alice-connector-key>"},"segments":[{"role":"user","content":[{"type":"trusted-text","text":"List my Skybits documents with find_documents and remember the count in memory."}]}]}
JSON
./loomcurl.sh -N -X POST http://127.0.0.1:8787/v1/runs \
  -H 'Content-Type: application/json' -d @/tmp/exp11-run3.json | tee /tmp/exp11-run3.sse
rm /tmp/exp11-run3.json    # the body file holds a token — don't leave it around
```

## Verify

- **Skybits tools mounted:** the boot log shows
  `mcp[skybits]: ready, 59/59 tools registered (transport=http)`. The public catalog cross-check:
  `curl -s https://skybits.ai/v1/tools | python3 -m json.tool | head -40`.
- **Skybits tools work (run 1):** the SSE stream shows
  `mcp__skybits__create_doc` → `outline_document` → `edit_document` calls
  succeeding, and the final summary names a real `doc_url`. Open the doc in the
  Skybits UI — the edits are attributed to your connector and appear in
  `document_history` / are revertible via `restore_document`.
- **Memory written (after run 1):**
  ```bash
  ./loomcurl.sh 'http://127.0.0.1:8787/v1/_memory/scopes/user/exp11/keys?prefix=skybits:'
  ```
  lists the `skybits:doc:<slug>` entry carrying the `doc_url`.
- **Memory recalled (run 2):** run 2's first tool calls are
  `Memory op=list scope=user` (no doc_url in its prompt), yet it opens the SAME
  `doc_url` run 1 created and appends its Log entry. The Skybits doc's history
  shows both runs' edits; `./data` (SQLite) persists the memory across server
  restarts — rerun run 2 after a Ctrl-C + `./run.sh` and it still recalls.

## Caveats / gotchas
- **Empty token = silent 401s.** If neither `user_credentials.skybits` nor
  `LOOMCYCLE_SKYBITS_TOKEN` is set, the Authorization header goes out with an
  empty bearer and every Skybits call 401s. The agent is prompted to say so and
  stop — check the SSE stream for "Skybits auth failed".
- **`user` scope needs `user_id`.** All verification steps use
  `user_id: "exp11"` — the scope_id is resolved server-side from the run's
  `user_id`, so two runs only share memory when their `user_id` matches.
- **`Memory op=search` / `recall` need the vector stack** (embedder + a
  vector-capable backend) which this example does not configure — the agent
  uses the key/value ops (`list`/`get`/`set`), which work on the default
  SQLite backend. `op=add` enqueues for background consolidation; the keyed
  `op=set` entries are the canonical store.
- **Token hygiene:** loomcycle redacts secret values from persisted
  transcripts; `loomcurl.sh` keeps the API bearer off argv; delete any
  `/tmp/exp11-*.json` body that embeds a connector key.
- **Edits are direct, not suggested.** Skybits has no suggestions tool — every
  `edit_document` lands immediately (attributed + revertible). Review in the
  Skybits UI or via `mcp__skybits__diff_document` if a run surprises you.
- **Batched tool calls vs `max_tokens` (observed live).** A single assistant
  turn emitting dozens of tool calls (e.g. 30 `Memory op=set`) can hit the
  output-token cap mid-batch: the run ends with `stop_reason: max_tokens` and
  the pending tool calls of that turn are NEVER executed — no error, the keys
  just never land. That is why `max_tokens` is 16384 here and the prompt tells
  the agent to store many-fact payloads as ONE JSON value under a single key.
  Watch for `stop_reason: max_tokens` in the SSE stream when a run "succeeds"
  but its writes are missing.

## Teardown
Ctrl-C the server; delete `./data` for a clean slate (memory included);
`rm /tmp/exp11-run*.json /tmp/exp11-run*.sse`. Delete the Skybits documents
from the Skybits UI (or `mcp__skybits__*`) if you don't want to keep them.

## Files
| File | Purpose |
|---|---|
| `loomcycle.yaml` | routing + `doc-agent` (Memory + `mcp__skybits__*`) + the `skybits` remote MCP server |
| `run.sh` | launcher (sources `.env.local`, sets data dir / listen addr / OAuth dev flag) |
| `.env.local.example` | secret template (empty values): API bearer, provider key, `LOOMCYCLE_SKYBITS_TOKEN` |
| `loomcurl.sh` | token-safe loomcycle REST helper |
| `data/` | SQLite state incl. memory (gitignored; delete for a clean slate) |
