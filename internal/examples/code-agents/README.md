# Bundled code-agent examples (RFC J)

Reference `code-js` agents. Copy a directory under your
`$LOOMCYCLE_CODE_AGENTS_ROOT` (default `./agent_code`) as
`<agent-name>/index.js`, declare the agent with `provider: code-js`, and
enable code agents with `LOOMCYCLE_CODE_AGENTS_ENABLED=1`.

See the `code-agents` help topic (`Context.help code-agents`) for the JS-side
API, the default-deny sandbox, and the honest-determinism scope.

## `ats-scraper/` — nightly ATS scrape, zero LLM

The RFC J worked example: fetch listings across four job boards with the
built-in `WebFetch` tool, dedupe against per-user `memory`, and hand fresh
jobs to the consumer's own `mcp__jobs__ingestJobs` MCP tool. No tokens, no
hallucination, ~network-bound wall time.

```yaml
# loomcycle.yaml
agents:
  ats-scraper:
    provider: code-js
    # Canonical tool names. WebFetch is a loomcycle built-in;
    # mcp__jobs__ingestJobs is exposed by the jobs MCP server below.
    allowed_tools: [WebFetch, Memory, mcp__jobs__ingestJobs]
    memory_scopes: [user]            # required for Memory.*
    allowed_hosts: ["*.example"]     # WebFetch host policy (operator floor)
    description: "Nightly ATS scrape across four job boards (deterministic, no LLM)."

mcp_servers:
  jobs:
    url: https://jobs-search-agent.internal/api/mcp
    headers:
      Authorization: "Bearer ${run.credentials.jobs}"

scheduled_runs:
  nightly-ats:
    agent: ats-scraper
    user_id: alice@example.com
    schedule: "0 3 * * *"
    timezone: "Europe/Berlin"
    enabled: true
    user_credentials_from_env:
      jobs: "LOOMCYCLE_JOBS_MCP_TOKEN"
```

`WebFetch` and `mcp__jobs__ingestJobs` are both dispatched by the loop, so
WebFetch's host allowlist and the MCP server's `${run.credentials.jobs}`
bearer apply exactly as they would for an LLM agent — the JS never sees the
token. The scheduler fires the agent like any LLM agent; an A2A peer reaching
it via the well-known card sees one more skill; an LLM agent can
`Agent.spawn` it for deterministic results without paying for tokens.

## Deferred examples

The RFC also sketches `sql-query/`, `format-converter/`, and `router/`
examples. They are follow-up work — the `ats-scraper` here covers the
built-in-tool + memory + MCP-tool + metadata surface end-to-end.
