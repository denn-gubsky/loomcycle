# Bundled code-agent examples (RFC J)

Reference `code-js` agents. Copy a directory under your
`$LOOMCYCLE_CODE_AGENTS_ROOT` (default `./agent_code`) as
`<agent-name>/index.js`, declare the agent with `provider: code-js`, and
enable code agents with `LOOMCYCLE_CODE_AGENTS_ENABLED=1`.

See the `code-agents` help topic (`Context.help code-agents`) for the JS-side
API, the default-deny sandbox, and the honest-determinism scope.

## `ats-scraper/` — nightly ATS scrape, zero LLM

The RFC J worked example: fetch listings across four job boards, dedupe
against per-user `memory`, publish fresh jobs to a channel. No tokens, no
hallucination, ~network-bound wall time.

```yaml
# loomcycle.yaml
agents:
  ats-scraper:
    provider: code-js
    allowed_tools: [memory, channel, mcp__http_fetch__get]
    description: "Nightly ATS scrape across four job boards (deterministic, no LLM)."

mcp_servers:
  http_fetch:
    url: https://internal.example/mcp-http-fetch
    headers:
      Authorization: "Bearer ${run.credentials.http_fetch}"

scheduled_runs:
  nightly-ats:
    agent: ats-scraper
    user_id: alice@example.com
    schedule: "0 3 * * *"
    timezone: "Europe/Berlin"
    enabled: true
    user_credentials_from_env:
      http_fetch: "LOOMCYCLE_HTTP_FETCH_TOKEN"
```

The scheduler fires it like any LLM agent; an A2A peer reaching it via the
well-known card sees one more skill; an LLM agent can `agent.spawn` it for
deterministic results without paying for tokens to "think about scraping."

## Deferred examples

The RFC also sketches `sql-query/`, `format-converter/`, and `router/`
examples. They are follow-up work — the `ats-scraper` here covers the
memory + channel + MCP-tool + metadata surface end-to-end.
