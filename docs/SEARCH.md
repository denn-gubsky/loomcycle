# Search providers (RFC BB)

Web search in loomcycle is a **first-class, config-declared provider catalog with
a fallback circuit** — the search analog of the LLM `provider_priority` / `tiers`
stack. The `WebSearch` tool walks an ordered list of providers, resolves each
one's key, and on an error / rate-limit / empty result **falls over to the next**
— the model sees the same `title / URL / snippet` output regardless of which
provider answered, and follows no protocol.

This guide covers the catalog, the config, per-agent lists, keys, the routing
view, and — the reason most people land here — **how to run a self-hosted SearXNG
provider** (a reproducible deploy recipe).

> **Why this, not a raw Brave key?** Brave killed its free tier in the 2026
> restructure ($5/1K, no volume discount). Serper/Exa/Tavily are 5–50× cheaper or
> add capabilities Brave can't, and **SearXNG is free** (you host it). The
> fallback circuit lets you prefer a cheap/self-hosted primary and keep a paid
> provider as a safety net — without the agent knowing.

## The catalog

Five built-in providers ship today:

| id        | backend                              | key env var       |
|-----------|--------------------------------------|-------------------|
| `brave`   | Brave Search API (own index)         | `BRAVE_API_KEY`   |
| `serper`  | Serper.dev — cheap Google SERP JSON  | `SERPER_API_KEY`  |
| `exa`     | Exa — neural/semantic + free tier    | `EXA_API_KEY`     |
| `tavily`  | Tavily — RAG-tuned search            | `TAVILY_API_KEY`  |
| `searxng` | Self-hosted metasearch (70+ engines) | **keyless** (needs `base_url`) |

## Config

Declare the enabled providers + the global fallback order in `loomcycle.yaml`:

```yaml
search_providers:
  searxng:
    base_url: "http://searxng:8080"   # required for SearXNG (see the recipe below)
  brave: {}                            # key from BRAVE_API_KEY
  # serper: {}                         # key from SERPER_API_KEY
  # exa: {}
  # tavily: {}
search_priority: [searxng, brave]      # try SearXNG first, fall over to Brave
```

- **`search_priority`** is the global cascade. Every entry must be an enabled
  `search_providers` key. Empty = the enabled set in map order.
- **Back-compat:** with **no** `search_providers` block, `WebSearch` defaults to
  Brave when `BRAVE_API_KEY` is set — existing deployments need no change.
- **Availability** is tracked by *last outcome* (a short cooldown after a failed
  call), not active probing — paid APIs aren't health-checked on a timer.

### Per-agent override

An agent can narrow/re-order the cascade with its own list (a full replacement of
`search_priority`; empty = the global default). It's content-identifying (part of
`content_sha256`, like `providers:`):

```yaml
agents:
  researcher:
    tools: [WebSearch]
    search_providers: [serper, exa]   # this agent: Serper then Exa, nothing else
```

## Keys — operator and tenant

Each keyed provider resolves its key the same way LLM providers do (see
[`docs/CREDENTIALS.md`](CREDENTIALS.md)):

1. A **tenant/user `CredentialDef`** of the provider's env-var name (e.g.
   `SERPER_API_KEY`) — a tenant searches on its own quota. Author it via the
   `CredentialDef` tool or the Web UI **Settings → Credentials** page.
2. Else the **operator host key** from that env var (unless the run is
   operator-key-restricted).

SearXNG is keyless — no key needed. A provider with no usable key is skipped in
the cascade (not a failure).

## The routing view

**Settings → Routing** shows the search cascade with, per provider, whether you
can key it, whether it's available right now, and which one is **selected** (the
first available = what runs now). Same data over `GET /v1/_routing` (the `search`
block). Set `LOOMCYCLE_WEBSEARCH_PROVENANCE=1` to also append a `(via searxng)` /
`(via brave — searxng fell over)` footer to each result, so a fallover is visible
per query.

---

## Deploying self-hosted SearXNG (recipe)

This is the exact pattern used on the reference VM. SearXNG runs as a **sidecar
container on the same network as loomcycle**, reachable in-network at
`http://searxng:8080` — no host port required.

### 1. Add the SearXNG service

Docker-compose sibling of your `loomcycle` service (same network):

```yaml
services:
  searxng:
    image: searxng/searxng:latest
    container_name: searxng
    restart: unless-stopped
    volumes:
      - ./searxng:/etc/searxng:rw
    environment:
      - SEARXNG_BASE_URL=http://searxng:8080/
    # No ports: — only loomcycle needs to reach it, in-network.
```

### 2. `searxng/settings.yml` — the THREE required knobs

Create `./searxng/settings.yml` next to your compose file:

```yaml
use_default_settings: true
server:
  # ① REQUIRED — SearXNG refuses to start on the default key.
  secret_key: "PASTE THE OUTPUT OF: openssl rand -hex 32"
  # ② REQUIRED — the bot-detection limiter 429s loomcycle's plain HTTP GET.
  #    Safe here: this instance is private + in-network only.
  limiter: false
search:
  formats:
    - html
    - json   # ③ REQUIRED — loomcycle calls /search?format=json (403 without it).
```

> **The three failure modes if a knob is missing:** no `secret_key` → SearXNG
> won't start; `limiter` on → loomcycle's requests are 429'd; no `json` format →
> `/search?format=json` returns **403** and the WebSearch driver errors (and, if
> you configured a fallback like Brave, quietly falls over to it — so SearXNG
> would look "configured" but never actually serve). All three are required.

### 3. Point loomcycle at it

In `loomcycle.yaml`:

```yaml
search_providers:
  searxng:
    base_url: "http://searxng:8080"   # the compose service name
  brave: {}                            # optional paid fallback
search_priority: [searxng, brave]
```

SearXNG needs no key. Keep whatever paid providers you also want as fallbacks.

### 4. Verify (in order)

```sh
# a) SearXNG returns JSON in-network — the decisive check:
docker run --rm --network <your-net> curlimages/curl -s \
  "http://searxng:8080/search?q=test&format=json" | head -c 200
#    → JSON with "results": [...]   (NOT HTML, NOT 403/429)

# b) loomcycle boot log names the provider:
docker logs loomcycle | grep 'search:'
#    → search: N provider(s) configured; priority [searxng ...]

# c) end-to-end — watch SearXNG receive a real WebSearch:
docker logs -f searxng 2>&1 | grep 'GET /search'
#    then run an agent that uses WebSearch (or check Settings → Routing:
#    searxng should show SELECTED). A `GET /search?...&format=json → 200`
#    line = loomcycle → SearXNG confirmed live.
```

If `Settings → Routing` shows `searxng` **SELECTED** and the SearXNG access log
shows the `/search?format=json → 200`, it's working end-to-end. If a fallback
provider is `selected` instead, SearXNG failed the call — re-check the three
`settings.yml` knobs (usually the `json` format or the limiter).

## See also

- `loomcycle help search-providers` — the in-agent help topic.
- [`loomcycle.example.yaml`](../loomcycle.example.yaml) — the full `search_providers` block.
- [`docker-compose.example.yaml`](../docker-compose.example.yaml) — the commented SearXNG sidecar.
- [`docs/CREDENTIALS.md`](CREDENTIALS.md) — per-tenant provider keys via `CredentialDef`.
