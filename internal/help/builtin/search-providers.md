---
name: search-providers
description: "Web search is a first-class, config-declared provider catalog with a fallback circuit (RFC BB) — Brave/Serper/Exa/Tavily/SearXNG behind the WebSearch tool, with per-agent priority + operator/tenant keys + a routing view."
aliases: [search, websearch, web-search]
---
The `WebSearch` tool is a **multi-provider fallback circuit** (RFC BB), not a
single backend. You call it the same way — `WebSearch("your query")` → a numbered
list of title / URL / snippet — and the runtime routes the query to the first
usable provider in a configured cascade, falling over to the next on an error,
rate-limit, or empty result. The model sees the same shape regardless of which
provider answered.

You don't pick a provider per call. The operator configures the catalog + a
default order; an agent may narrow/re-order it with a per-agent list.

## The catalog

Five built-in providers ship today:

```
brave     Brave Search API (own index).          key: BRAVE_API_KEY
serper    Serper.dev — cheap Google SERP.        key: SERPER_API_KEY
exa       Exa — neural/semantic + free tier.     key: EXA_API_KEY
tavily    Tavily — RAG-tuned search.             key: TAVILY_API_KEY
searxng   Self-hosted meta-search (70+ engines). KEYLESS (needs base_url)
```

## Operator config

```yaml
search_providers:            # which providers are enabled
  serper: {}
  exa: {}
  brave: {}
  searxng: { base_url: "http://searxng:8080" }   # SearXNG needs its URL
search_priority: [serper, exa, brave, searxng]   # the global fallback order
```

With no `search_providers:` block, WebSearch defaults to Brave when
`BRAVE_API_KEY` is set (the pre-RFC-BB behaviour, unchanged).

## Per-agent fallback list

An agent narrows or re-orders the cascade with its own `search_providers:` — a
full override of the global order (empty = use the global default):

```yaml
agents:
  researcher:
    tools: [WebSearch]
    search_providers: [serper, exa]   # try Serper, then Exa; nothing else
```

Every entry must be an enabled top-level `search_providers` key (validated at
config load). A runtime-authored agent (AgentDef create/fork) carries the same
field; it is content-identifying (part of `content_sha256`, like `providers:`).

## Keys — operator and tenant

Each keyed provider resolves its key the same way LLM providers do
(`help(topic="per-run-credentials")`):

1. A **tenant/user CredentialDef** of the provider's env-var name (e.g.
   `SERPER_API_KEY`) — a tenant searches on its own quota. Author it via the
   `CredentialDef` tool or the Web UI Settings → Credentials page.
2. Else the **operator host key** from that env var (unless the run is
   operator-key-restricted, RFC AX).

A provider you have no key for is skipped silently (not a failure); SearXNG is
keyless, so it always runs. A failed provider is put in a short cooldown before
the circuit tries it again.

## The routing view

**Settings → Routing** shows the search cascade with, per provider: whether you
can key it, whether it's available right now (last-outcome — paid APIs aren't
health-probed), and which one is **selected** (the first available = what runs
now). A restricted tenant sees only the providers it can key itself. Same data
over `GET /v1/_routing` (the `search` block).

## Provider-specific power features

The fallback circuit is deliberately the lowest-common-denominator "web
discovery" shape (title/URL/snippet). A provider's richer capabilities (Exa's
`findSimilar`, Firecrawl's scrape) are **out** of the circuit — reach them by
mounting that provider's own MCP server and calling its specific tool.

## Cross-references

- `help(topic="per-run-credentials")` — how `$cred:` / tenant keys resolve.
- `Context op=self` — the resolved provider/model + your scope.
- `Context op=tools` — confirm you hold the `WebSearch` tool.
