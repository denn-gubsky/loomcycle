# Configuration — provider routing, tiers, and agent files

This guide is for operators wiring loomcycle for the first time, or developers tuning individual agents. It answers: **given my setup, what yaml do I write?**

Scope: the **model-routing axis** of `loomcycle.yaml` — providers, tier candidate lists, the `models:` alias map, the `user_tiers:` overlay — plus the agent `.md` frontmatter fields that control per-agent routing. MCP-server configuration lives in [`docs/MCP_INTEGRATION.md`](MCP_INTEGRATION.md); storage in [`docs/POSTGRES.md`](POSTGRES.md); concurrency/cache/hooks are not covered here.

---

## 0. Why this doc exists

Loomcycle's resolver runs **four nested decision layers** to pick the `(provider, model)` for any given agent run:

```
            ┌───────────────────────────────────────────┐
            │  4. Explicit pin on the agent             │  (highest)
            │     agent.provider + agent.model          │
            └─────────────────┬─────────────────────────┘
                              │ falls through if no pin
            ┌─────────────────▼─────────────────────────┐
            │  3. Per-agent override                    │
            │     agent.providers / agent.models[tier]  │
            └─────────────────┬─────────────────────────┘
                              │ falls through if no override
            ┌─────────────────▼─────────────────────────┐
            │  2. user_tier overlay                     │
            │     user_tiers.<tier>.provider_priority   │
            │     user_tiers.<tier>.tiers[agent.tier]   │
            └─────────────────┬─────────────────────────┘
                              │ falls through if user_tier
                              │ not set or has no tiers map
            ┌─────────────────▼─────────────────────────┐
            │  1. Library defaults                      │  (lowest)
            │     provider_priority / tiers             │
            └───────────────────────────────────────────┘
```

The doc's job is to make you confident about **which layer to touch when**. Most operators only need layer 1 (library defaults) and stop there. As your deployment grows — multi-tenant, multi-plan, mixed-provider — you climb the layers.

**One sentence**: configure routing top-down for the common case (library defaults), then push specific exceptions UP the precedence stack (user_tier overlay for plan-tier policy, per-agent override for "this agent is special," explicit pin for unambiguous "exactly this model").

The doc presents this as **four cookbook patterns**. Find the pattern matching your setup, copy the yaml, adjust to your providers.

---

## 1. The four resolution axes (overview)

| Axis | Yaml field | Purpose | When you touch it |
|---|---|---|---|
| **Library `provider_priority`** | top-level `provider_priority:` | The walk order across providers when nothing else overrides | Always — every config sets this |
| **Library `tiers`** | top-level `tiers:` | Per-task-tier (`low`/`middle`/`high`) ordered candidate lists | Always — every agent that uses `tier:` reads this |
| **`models:` alias map** | top-level `models:` | Aliases like `sonnet → {anthropic, claude-sonnet-4-6}`. Reference by name in tier candidate lists (`- sonnet`), per-agent pins (`model: sonnet`), and per-agent `models[tier]` — define the model once, single-source the id | The recommended way to name models — always; raw `{provider, model}` pairs still work but repeat the id |
| **`user_tiers:` overlay** | top-level `user_tiers:` | Per-user-class policy (free/low/medium/high). Restricts which providers + models a user's runs may touch | When your app has multiple plan tiers OR multi-tenant cost/privacy boundaries |
| **Per-agent `providers:`** | agent .md `providers:` | Replaces the priority order for THIS agent only | When one agent must skip certain providers (privacy, capability) |
| **Per-agent `models[tier]:`** | agent .md `models:` | Replaces the tier candidate list for THIS agent | When one agent needs specific models in its tier slot |
| **Explicit pin** | agent .md `provider:` + `model:` | Hard-pin to exactly one `(provider, model)` | When the agent has a sensitive-paths reason to never fall through |

The resolver walks **top-down** through this table on every request. The first axis that has something to say wins. See § 2 for the exact decision tree.

---

## 2. Resolution precedence — decision tree

The resolver lives in `internal/resolve/matrix.go:281` (`Resolve(req AgentRequest) (Decision, error)`). The precedence is:

```
Given:  AgentRequest{ Name, Tier, PinProvider, PinModel, Providers, Models, UserTier }

   1. PinProvider AND PinModel both set?
      → resolvePin()       (matrix.go:293)
        - Looks up the matrix to confirm (provider, model) is reachable.
        - Returns Decision{provider, model} or ErrPinUnavailable.

   2. Only ONE of (PinProvider, PinModel) set?
      → ErrInvalidArgument (half-pin is config error, caught at load too)

   3. Tier required from here on. If Tier == "":
      → ErrInvalidArgument

   4. Build the candidate list:
      a. agent.Models[tier] set?  use it. (per-agent override; full replacement)
      b. user_tier.Tiers[tier] set?  use it. (overlay)
      c. library tiers[tier] set?  use it.
      d. otherwise →                ErrTierUnavailable

   5. Build the provider walk order:
      a. agent.Providers AND user_tier.ProviderPriority both set?
         → intersection in agent-order (matrix.go:440)
         → empty intersection → ErrTierAgentNotAvailable  (policy refusal)
      b. only agent.Providers set?               → agent.Providers
      c. only user_tier.ProviderPriority set?    → user_tier order
      d. neither set?                            → library provider_priority

   6. Walk the candidate list, skipping any pair whose:
      - provider is excluded (no API key) OR
      - provider is unreachable (probe failed) OR
      - model is stalled (recent driver error)
      - model is not listed by the provider's /v1/models (or equivalent)

   7. First survivor →  Decision{provider, model, effort, ...}
      No survivor    →  ErrTierUnavailable
```

Two error classes worth remembering, because they have different operator semantics:

| Error | When | Caller should |
|---|---|---|
| `ErrTierUnavailable` | Matrix-side problem — every candidate stalled / unreachable | Retry with backoff; surface as 503 |
| `ErrTierAgentNotAvailable` | **Policy-side** — agent's `providers:` and user_tier's `provider_priority` have no overlap | NOT retry — return 403/"upgrade your plan"; the user genuinely doesn't have access |

This distinction matters because a transient outage and "your plan doesn't allow this agent" are operationally different. Loomcycle separates them at the resolver layer so the app server can map them to different HTTP responses.

### Mutual exclusion at config-load

`internal/config/config.go:1985` enforces **pin XOR tier** at config-load time:

```go
hasPin := agent.Provider != "" || agent.Model != ""
hasTier := agent.Tier != ""
if hasPin && hasTier {
    return fmt.Errorf("agent %q: cannot set both explicit provider/model pin and tier (pick one)", name)
}
```

If you set both `tier: middle` AND `model: claude-sonnet-4-6` in an agent's frontmatter, loomcycle refuses to start. Pick one path.

---

## 3. Pattern 1 — Single provider, model tiers + model aliases

**Setup:** You have one provider (Anthropic). You want agents to declare `tier: low/middle/high` and pick the matching Anthropic model automatically. You also want short aliases (`sonnet`) so agents can pin a specific model without typing the full ID.

```yaml
# loomcycle.yaml — single-provider, tier-driven

# Aliases FIRST — name every model once; reference it by name everywhere
# (tier candidates, agent pins). Editing one right-hand side re-points
# every agent/tier that uses the alias.
models:
  haiku:  { provider: anthropic, model: claude-haiku-4-5 }
  sonnet: { provider: anthropic, model: claude-sonnet-4-6 }
  opus:   { provider: anthropic, model: claude-opus-4-7 }

provider_priority:
  - anthropic

# Tier candidates are bare ALIASES (v0.35.0+) — `- haiku` is the same as
# `- { provider: anthropic, model: claude-haiku-4-5 }`, single-sourced from
# the models: map above. A raw {provider, model} pair still works too.
tiers:
  low:    [haiku]
  middle: [sonnet]
  high:   [opus]

# Default for agents without tier or explicit pin (rare; back-compat path).
# NOT alias-expanded — pin a concrete provider+model (an alias name here is
# sent to the provider verbatim, not resolved).
defaults:
  provider: anthropic
  model:    claude-sonnet-4-6

concurrency:
  max_concurrent_runs: 8
  max_queue_depth: 16
  queue_timeout_ms: 30000
```

### Agent .md examples in this pattern

**Tier-driven** — let the resolver pick the model:

```markdown
---
name: ats-filter
description: Score CV bullets against a job posting; return JSON.
tier: low
tools: [Read]
---
You are an ATS filter...
```

→ resolves to `(anthropic, claude-haiku-4-5)` via `tiers.low`.

**Alias pin** — agent specifically wants sonnet:

```markdown
---
name: cv-rewriter
description: Rewrites CV text in the user's voice.
model: sonnet
tools: [Read]
---
You rewrite text while preserving voice...
```

→ resolves to `(anthropic, claude-sonnet-4-6)` via the `models:` alias.

**Full pin** — bypass aliases entirely:

```markdown
---
name: cv-rewriter
provider: anthropic
model: claude-sonnet-4-6
tools: [Read]
---
```

Same effective result. Use whichever reads more naturally to your team.

### Why this pattern

Operators who only use Anthropic still benefit from the tier abstraction: when Anthropic ships a new model family (haiku-5, sonnet-5), you change ONE place (the `tiers:` block) and every tier-driven agent picks up the new model. Aliases give you the same benefit for explicitly-pinned agents.

---

## 4. Pattern 2 — Multiple providers, model tiers + model names

**Setup:** Anthropic + DeepSeek + Gemini. You want a cost-floor cascade: try DeepSeek first (cheapest), fall through to Gemini, fall through to Anthropic. Some agents (privacy-sensitive ones) override that order.

```yaml
# loomcycle.yaml — multi-provider, cost-floor-first

provider_priority:
  - deepseek          # cost floor
  - gemini            # cheap quality alternative
  - anthropic         # last-resort fallback

tiers:
  low:
    - { provider: deepseek,  model: deepseek-v4-flash }   # 16/16 CAPABLE at $0.0010/pass
    - { provider: gemini,    model: gemini-2.5-flash-lite }
    - { provider: anthropic, model: claude-haiku-4-5 }    # baseline
  middle:
    - { provider: deepseek,  model: deepseek-v4-pro }
    - { provider: gemini,    model: gemini-2.5-pro }
    - { provider: anthropic, model: claude-sonnet-4-6 }
  high:
    # Premium-privacy: anthropic only at high tier.
    - { provider: anthropic, model: claude-sonnet-4-6 }
    - { provider: anthropic, model: claude-opus-4-7 }

models:
  haiku:  { provider: anthropic, model: claude-haiku-4-5 }
  sonnet: { provider: anthropic, model: claude-sonnet-4-6 }
  opus:   { provider: anthropic, model: claude-opus-4-7 }

defaults:
  provider: anthropic
  model:    claude-sonnet-4-6
```

### Cascade behaviour

A `tier: low` agent runs against `deepseek-v4-flash` first. If DeepSeek returns 429 or 5xx — and `fallback_on_error` is true (default) — the resolver re-picks from the same candidate list, this time selecting the next provider (`gemini`). This continues until a candidate succeeds or the cascade is exhausted (`ErrTierUnavailable`).

### Agent .md: per-agent `providers:` override

For one agent that must skip DeepSeek (e.g., a CV-handling agent where you don't want CV text leaving the Anthropic boundary):

```markdown
---
name: cv-adapter
description: Adapts a CV to a target job posting.
tier: middle
providers: [anthropic]         # ← full replacement of provider_priority
tools: [Read]
---
```

The resolver now walks `[anthropic]` only, so this agent always resolves to `claude-sonnet-4-6` (the only middle-tier anthropic candidate). It never touches DeepSeek or Gemini.

### Agent .md: per-agent `models[tier]:` override

If you want full control over the candidate list (e.g., an analytical agent that benefits from opus, not sonnet):

```markdown
---
name: research-analyst
description: Deep analytical reasoning for the high-tier slot.
tier: high
models:
  high:
    - { provider: anthropic, model: claude-opus-4-7 }
    - { provider: anthropic, model: claude-sonnet-4-6 }   # fallback
tools: [Read, WebFetch]
---
```

The library `tiers.high` is ignored for this agent; the resolver walks the agent-declared list instead.

### Why this pattern

This is the most common production setup for operators with multiple provider keys. The library `provider_priority` does the cost-cascade work; per-agent overrides handle the exceptions (privacy, capability, cost-cap-on-sensitive-paths).

---

## 5. Pattern 3 — Single provider, multiple user tiers

**Setup:** Only Anthropic, but per-plan model gating: free users get haiku, medium gets sonnet, high gets opus. The agent's `tier:` field stays the same; what changes is **which user's run is asking**, conveyed via the `user_tier` field on the run request.

```yaml
# loomcycle.yaml — single-provider, multi-user-tier

provider_priority:
  - anthropic

tiers:
  low:
    - { provider: anthropic, model: claude-haiku-4-5 }
  middle:
    - { provider: anthropic, model: claude-sonnet-4-6 }
  high:
    - { provider: anthropic, model: claude-opus-4-7 }

models:
  haiku:  { provider: anthropic, model: claude-haiku-4-5 }
  sonnet: { provider: anthropic, model: claude-sonnet-4-6 }
  opus:   { provider: anthropic, model: claude-opus-4-7 }

# v0.8.2+: per-user-class policy overlays.
user_tiers:
  default:
    # Inherits library priority + tiers. Used when a caller omits
    # user_tier or passes an unrecognized name.
    provider_priority: [anthropic]
    fallback_on_error: true

  free:
    # Free users get haiku only — regardless of agent's tier:.
    provider_priority: [anthropic]
    fallback_on_error: true
    tiers:
      low:    [{ provider: anthropic, model: claude-haiku-4-5 }]
      middle: [{ provider: anthropic, model: claude-haiku-4-5 }]   # locked down
      high:   [{ provider: anthropic, model: claude-haiku-4-5 }]   # locked down

  medium:
    # Medium users get haiku + sonnet (no opus).
    provider_priority: [anthropic]
    fallback_on_error: true
    tiers:
      low:    [{ provider: anthropic, model: claude-haiku-4-5 }]
      middle: [{ provider: anthropic, model: claude-sonnet-4-6 }]
      high:   [{ provider: anthropic, model: claude-sonnet-4-6 }]   # cap at sonnet

  high:
    # High users get the full menu.
    provider_priority: [anthropic]
    fallback_on_error: true
    tiers:
      low:    [{ provider: anthropic, model: claude-haiku-4-5 }]
      middle: [{ provider: anthropic, model: claude-sonnet-4-6 }]
      high:   [{ provider: anthropic, model: claude-opus-4-7 }]

defaults:
  provider: anthropic
  model:    claude-sonnet-4-6
```

### How the caller picks a user_tier

The app server includes a `user_tier` field on the run request:

```http
POST /v1/runs
{
  "agent": "my-tier-middle-agent",
  "user_id": "u_42",
  "user_tier": "free",          ← caller-supplied; loomcycle reads it
  "segments": [...]
}
```

Loomcycle looks up `user_tiers.free` in the operator yaml, applies the overlay, and resolves. With the yaml above, a `tier: middle` agent run on `user_tier: free` resolves to `claude-haiku-4-5`. The same agent on `user_tier: high` resolves to `claude-opus-4-7`. The agent .md file doesn't change; the user's plan controls the model.

### Required: the `default` user_tier

If you set `user_tiers:` at all, you must include a `default:` entry. It's the fallback when:
- The caller omits `user_tier` from the run request (e.g., legacy callers)
- The caller passes an unrecognized name (`"premium"` when you only defined `"high"`)

Without it, the resolver has nothing to walk for unknown user_tiers, and runs error out.

### `fallback_on_error`

Per overlay. When `true` (default), a retryable provider error triggers fallback to the next candidate. When `false`, the error surfaces directly to the caller.

Set to `false` on the free tier if you want a strict no-cascade behaviour (the operator doesn't want a free-tier user accidentally falling into a more expensive provider during an outage). Set to `true` everywhere else for resilience.

### Why this pattern

The simplest "SaaS with plan tiers" shape: one provider, but the cost/capability boundary lives in the user_tier overlay rather than in agent .md frontmatter. Adding a new agent: just give it a `tier:` and you don't have to touch each plan's gating — the overlay already handles it.

---

## 6. Pattern 4 — Multiple providers + multiple user tiers (production-grade)

**Setup:** All the providers, all the plan tiers, with a privacy boundary on the top tier. This is what real production loomcycle deployments look like.

```yaml
# loomcycle.yaml — multi-provider, multi-user-tier

provider_priority:
  - ollama-local    # local first when available (denn-desktop.local)
  - ollama          # Ollama Cloud (subscription billing — counts as cost floor)
  - gemini
  - deepseek
  - anthropic
  - openai

# Library tiers — the "default user_tier" backstop.
tiers:
  low:
    - { provider: ollama-local, model: glm-4.7-flash:q4_K_M }
    - { provider: ollama,       model: glm-4.7 }
    - { provider: deepseek,     model: deepseek-v4-flash }
    - { provider: gemini,       model: gemini-2.5-flash-lite }
    - { provider: anthropic,    model: claude-haiku-4-5 }
  middle:
    - { provider: ollama,       model: deepseek-v4-pro }   # cloud-ollama subscription path
    - { provider: deepseek,     model: deepseek-v4-pro }   # per-token fallback
    - { provider: gemini,       model: gemini-2.5-pro }
    - { provider: anthropic,    model: claude-sonnet-4-6 }
  high:
    # Premium-privacy: anthropic only at library high.
    - { provider: anthropic,    model: claude-sonnet-4-6 }

models:
  haiku:  { provider: anthropic, model: claude-haiku-4-5 }
  sonnet: { provider: anthropic, model: claude-sonnet-4-6 }
  opus:   { provider: anthropic, model: claude-opus-4-7 }

user_tiers:
  default:
    # Inherits library priority + tiers. Local-first cascade.
    provider_priority: [ollama-local, ollama, gemini, deepseek, anthropic, openai]
    fallback_on_error: true

  free:
    # Cost cap: subscription-billing only. NO per-token providers.
    # Ollama-local + Ollama Cloud (subscription) + Gemini cloud.
    provider_priority: [ollama-local, ollama, gemini]
    fallback_on_error: true
    tiers:
      low:
        - { provider: ollama-local, model: glm-4.7-flash:q4_K_M }
        - { provider: ollama,       model: glm-4.7 }
        - { provider: gemini,       model: gemini-2.5-flash-lite }
      middle:
        - { provider: ollama,       model: deepseek-v4-pro }
        - { provider: gemini,       model: gemini-2.5-pro }
      high:
        # Defined for safety; free-tier high-tier agents should be
        # blocked at the app's route layer, not just here.
        - { provider: ollama,       model: deepseek-v4-pro }
        - { provider: gemini,       model: gemini-2.5-pro }

  low:
    # Cheapest paid tier. Local floor → cloud subscription → per-token → anthropic.
    provider_priority: [ollama-local, ollama, deepseek, gemini, anthropic]
    fallback_on_error: true
    tiers:
      low:
        - { provider: ollama-local, model: glm-4.7-flash:q4_K_M }
        - { provider: ollama,       model: glm-4.7 }
        - { provider: deepseek,     model: deepseek-v4-flash }
        - { provider: gemini,       model: gemini-2.5-flash-lite }
        - { provider: anthropic,    model: claude-haiku-4-5 }
      middle:
        - { provider: ollama,       model: deepseek-v4-pro }
        - { provider: deepseek,     model: deepseek-v4-pro }
        - { provider: gemini,       model: gemini-2.5-pro }
        - { provider: anthropic,    model: claude-sonnet-4-6 }
      high:
        - { provider: ollama,       model: deepseek-v4-pro }
        - { provider: deepseek,     model: deepseek-v4-pro }
        - { provider: anthropic,    model: claude-sonnet-4-6 }

  medium:
    # Same cost-floor pattern as low, plus anthropic-premium for high-tier agents.
    provider_priority: [ollama-local, ollama, deepseek, gemini, anthropic, openai]
    fallback_on_error: true
    tiers:
      low:
        - { provider: ollama-local, model: glm-4.7-flash:q4_K_M }
        - { provider: ollama,       model: glm-4.7 }
        - { provider: deepseek,     model: deepseek-v4-flash }
        - { provider: gemini,       model: gemini-2.5-flash-lite }
        - { provider: anthropic,    model: claude-haiku-4-5 }
      middle:
        - { provider: ollama,       model: deepseek-v4-pro }
        - { provider: deepseek,     model: deepseek-v4-pro }
        - { provider: gemini,       model: gemini-2.5-pro }
        - { provider: anthropic,    model: claude-sonnet-4-6 }
      high:
        # Medium tier IS allowed anthropic-premium for the high slot.
        - { provider: anthropic,    model: claude-sonnet-4-6 }
        - { provider: anthropic,    model: claude-opus-4-7 }

  high:
    # PREMIUM-PRIVACY. NO third-party LLMs touch user data, EVER.
    # provider_priority alone enforces this — no ollama / gemini /
    # deepseek anywhere. Falls back from anthropic → openai (if key)
    # → hard fail. Never escapes the anthropic/openai boundary.
    provider_priority: [anthropic, openai]
    fallback_on_error: true
    # No tiers: override — library tiers are filtered to anthropic+openai
    # by the provider_priority intersection. With library high tier =
    # [sonnet], high user_tier traffic gets sonnet.

defaults:
  provider: gemini
  model:    gemini-2.5-flash
```

### Routing table for this yaml

| `agent.tier` | `user_tier=free` | `user_tier=low` | `user_tier=medium` | `user_tier=high` |
|---|---|---|---|---|
| `low` | `ollama-local/glm-4.7-flash` | same | same | `anthropic/sonnet` (library high) |
| `middle` | `ollama/deepseek-v4-pro` | `ollama/deepseek-v4-pro` | `ollama/deepseek-v4-pro` | `anthropic/sonnet` |
| `high` | `ollama/deepseek-v4-pro` | `ollama/deepseek-v4-pro` | `anthropic/sonnet` | `anthropic/sonnet` |

(First-pick on a healthy resolver matrix. Stalled candidates fall through to the next entry in the list.)

### Privacy boundary on user_tier=high

The `high` user_tier deliberately omits `tiers:` so the library tiers are inherited — BUT `provider_priority: [anthropic, openai]` filters them. Any candidate whose provider isn't in `[anthropic, openai]` is skipped at resolve time. So a library `tiers.low` that lists `ollama-local/glm` first will, for `user_tier=high`, walk past it and land on `anthropic/haiku`.

This is the **strict privacy boundary**: no matter what library tiers say, a high-tier user's run never touches a third-party cloud. If both anthropic and openai are down, the user gets a hard 503 — never a silent fallback to deepseek.

### Per-agent overrides intersect with user_tier

An agent with `providers: [anthropic]` on a request with `user_tier=free` (whose `provider_priority` is `[ollama-local, ollama, gemini]`) produces an **empty intersection** → `ErrTierAgentNotAvailable`. The app server sees this as "this user's plan doesn't cover this agent — upgrade required." The same agent on `user_tier=high` resolves cleanly.

This is the load-bearing security property: the resolver enforces "the agent can use anthropic" AND "the user is allowed to use anthropic" at the same gate.

### Why this pattern

It's what real production deployments look like. The library is the backstop; user_tiers carve out specific cost/privacy/capability boundaries; per-agent overrides handle the exceptions. Most agents stay tier-driven and inherit from this matrix.

---

## 6b. Local models (Ollama) — configuring + slow-model advice

Local models change the economics. A frontier cloud model prefills a 100k-token context in about a second; a single consumer GPU can take minutes. The knobs below exist because of that. For a ready-to-run config, see [`loomcycle.local-interactive.example.yaml`](../loomcycle.local-interactive.example.yaml).

### Two Ollama providers

| Provider id | What it is | Auth | Set |
|---|---|---|---|
| `ollama-local` | Ollama on your workstation / LAN / Tailscale host | none | `OLLAMA_BASE_URL=http://<host>:11434` |
| `ollama` | Hosted Ollama cloud (ollama.com) — for models too big to run locally | Bearer | `OLLAMA_API_KEY` |

`OLLAMA_BASE_URL` is the **host**, not a model — the model comes from a `models:` alias (`local-coder: { provider: ollama-local, model: qwen3-coder-next }`). Name your aliases to match what `ollama list` shows on that host.

### The context window (`num_ctx`) — the knob that bites

`LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX` controls the window for **all** `ollama-local` models. It is sent as `options.num_ctx`, so it both **caps the window the model loads** and **is what the UI context gauge reports**.

- **Unset** → `num_ctx` is omitted; Ollama uses each model's Modelfile `num_ctx`, and the gauge reads the **actual loaded window** from Ollama's `/api/ps` (after the model is in VRAM — it may read 0 while loading).
- **Set** → that value is forced for every local model and reported verbatim.

Pick a value **every** local model you use can handle (it's global), e.g. `131072`. Too low starves long sessions; too high blows up prefill time and VRAM on a slow GPU. ~128K is a sane middle for a 24GB+ card. A model's *training* context (e.g. 256K) is an upper bound, not what you must load — load only what your GPU can prefill in reasonable time.

> **Symptom:** the gauge shows a small window (e.g. 32K) you didn't expect. Almost always `LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX` is pinned low in the launch env — it's global and overrides the Modelfile. A model also stays resident at whatever window it was first loaded with until it unloads/reloads, so a stale low-context load can linger until the next fresh load.

### Slow models — timeouts + heartbeat

A big prefill can take a long time before the first token. Two budgets (defaults 300s; raise for very large contexts on slow hardware):

- `LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS` — **time to first byte** (the prefill). Exceeding it surfaces as `net/http: timeout awaiting response headers`.
- `LOOMCYCLE_OLLAMA_LOCAL_IDLE_TIMEOUT_MS` — max gap **between** streamed tokens.

A slow-but-alive call no longer trips the stale-run sweeper: the loop pulses the run heartbeat throughout a model call, so a long prefill won't be reaped as `heartbeat_timeout`. The HTTP timeouts above remain the authority on a genuinely stuck call.

### Compaction — tune it for the prefill cost

On a slow model, the prefill cost of a near-full window is what times you out — so compact **early** and keep a **small** verbatim tail. In a per-agent `compaction:` block:

- `autocompact_at_pct: 55` — compact well before the window fills (vs. the 80% default).
- `keep_last_n: 3` — a tool-heavy agent's tail (big file reads) dominates the post-compaction prefill, so fewer kept turns is the real lever.

When the provider reports a window, the loop also caps the kept tail to ~half the window automatically — folding an over-window tail into the summary — so a compaction always actually relieves pressure (it won't "succeed" yet leave you still over the window).

### Interactive local agents

For a terminal you steer turn-by-turn:

- `unbounded_iterations: true` — an interactive run is operator-driven and Cancel-bounded; don't let the 16-iteration runaway guard end a live session (each steer + each end_turn park burns an iteration).
- `interruption: { enabled: true }` — let the operator answer the agent's questions inline.
- `max_tokens: 8192` — the local default (4096) truncates large output.

All of this is wired together in [`loomcycle.local-interactive.example.yaml`](../loomcycle.local-interactive.example.yaml).

---

## 7. Agent `.md` frontmatter reference

Agent files live under `LOOMCYCLE_AGENTS_ROOT` (set in the env file). Each `<name>.md` has YAML frontmatter between `---` delimiters; the body is the system prompt.

### Frontmatter fields

Parsed at `internal/agents/loader.go:199` (the `frontmatter` struct):

| Field | Type | Purpose | Notes |
|---|---|---|---|
| `name` | string | Agent identifier | Defaults to filename minus `.md`. If set, must match the filename. |
| `description` | string | Human summary | Surfaces in operator tooling; not sent to the LLM as part of the prompt. |
| **Model resolution** | | | |
| `provider` | string | Explicit provider pin | XOR with `tier:`. With `model:` forms the pin path. |
| `model` | string | Model alias OR full model ID | Aliases expand via `models:` map at `config.go:1370`. |
| `tier` | string | `low` / `middle` / `high` | XOR with `provider`/`model`. Triggers tier-driven resolution. |
| `providers` | `[]string` | Per-agent provider priority | Full replacement of library `provider_priority` for this agent. |
| `models` | `map[tier][]TierCandidate` | Per-agent tier candidate lists | Full replacement of library `tiers[]` for this agent. |
| `effort` | string | `low` / `medium` / `high` | Reasoning-effort hint. Anthropic + OpenAI honour it; Ollama ignores. |
| `max_tokens` | int | Per-iteration assistant output cap | 0 = provider default. |
| `sampling` | object | LLM sampling params | `temperature` / `top_p` / `top_k` / `frequency_penalty` / `presence_penalty` / `seed` / `stop`. Each driver applies what its provider supports, drops the rest. `temperature: 0.0` is deterministic (≠ unset). Overridable per-run on `/v1/runs` (`sampling`), merged per field (per-run wins). See `Context op=help topic=sampling`. Anthropic drops temperature/top_p when `effort` engages thinking. |
| **Tool fields** | | | |
| `tools` | `[]string` | Tool allowlist (loomcycle form) | Empty list = zero tools. Always wins over `tools:`. |
| `tools` | string OR `[]string` | Claude-Code-compatible form | Comma-string or list. Tolerated for Claude-Code compatibility; `tools` takes precedence when both are set. |
| `skills` | `[]string` | Skill access allowlist (RFC BA) | Pattern allowlist governing which skills the agent may list / use / author (on-demand via the `Skill` tool — NOT bundled into the prompt). Entries are `/`-globs with an optional `+`/`-` sign: `doc/*` allow, `-doc/secret` deny, `-*` deny all. Empty/absent = allow all. The `Skill` tool is auto-added unless `skills: [-*]`. |
| **System prompt** | | | |
| (body) | string | Inline system prompt | Everything after the closing `---` line. |
| `system_prompt_file` | string | External prompt path | Mutually exclusive with body. Useful for sharing prompts across agents. |
| **Capability fields** | | | |
| `memory_scopes` | `[]string` | Memory tool scope gate | `agent` / `user`. Empty = default-deny (no Memory tool access). |
| `memory_quota_bytes` | int | Per-agent memory byte cap | 0 = global default. |
| `channels` | object | Channel tool ACL | `{publish: [...], subscribe: [...]}`. |
| `agent_def_scopes` | `[]string` | AgentDef tool scope | `self` / `descendants` / `named:<name>` / `any`. Empty = default-deny. |
| `evaluation_scopes` | `[]string` | Evaluation tool scope | `submit_self` / `submit_siblings` / etc. Empty = default-deny. |
| `volumes` | `[]string` | Filesystem-volume binding | Names of top-level `volumes:` entries the agent's file/exec tools may use. Empty = implicitly bound to `[default]`. Confines the agent to exactly the named volumes (does NOT also grant `default`). See §9d. |
| `volume_def_scopes` | `[]string` | VolumeDef tool scope | `any` / `named:<volume>`. Empty = default-deny. Gates create/delete/purge of dynamic volumes; get/list are tenant-scoped reads. See §9d.1. |

### Worked examples (real agents from jobs-search-agent)

**Bare tier-driven** — resolver picks everything:

```yaml
---
name: feedback-triage
description: Triage free-form feedback...
tier: low
tools: []
---
```

**MCP tools + tier**:

```yaml
---
name: qa-agent
description: Q&A answer generator for job applications.
tools: mcp__jobs__getAgentContext
tier: middle
tools:
  - mcp__jobs__getAgentContext
  - mcp__jobs__getApplication
  - mcp__jobs__postApplicationQaAnswers
---
```

Note: `tools:` (the Claude-Code form) is present so the same file works in Claude Code, but `tools:` (the loomcycle form) takes precedence and is the authoritative list at runtime.

**Alias pin with skills** — privacy-sensitive agent locked to sonnet:

```yaml
---
name: cv-rewriter
description: Rewrites CV or Cover Letter text...
tools: mcp__jobs__getAgentContext
tools:
  - mcp__jobs__getAgentContext
  - Read
  - Skill
skills:
  - voice-applier
  - cv-voice-applier
model: sonnet
---
```

No `tier:` — uses pin path. `model: sonnet` expands via `models:` alias to `(anthropic, claude-sonnet-4-6)`. This agent never falls through to a non-Anthropic provider.

**Defining skills inline (the top-level `skills:` map)** — instead of a `LOOMCYCLE_SKILLS_ROOT` directory of `SKILL.md` files, you can define skills directly in YAML, at the same level as `agents:` and `models:`:

```yaml
skills:
  voice-applier:
    description: Apply the house voice to drafted copy.   # informational
    tools: [Read]     # must be a SUBSET of the invoking agent's tools (enforced at invoke)
    body: |
      When rewriting, prefer active voice and short sentences …
agents:
  cv-rewriter:
    tools: [Read]              # `Skill` is auto-added (RFC BA)
    skills: [voice-applier]    # allowlist: this agent may list/use/author `voice-applier`
    model: sonnet
```

Inline skills join the on-demand catalog alongside `LOOMCYCLE_SKILLS_ROOT` (inline wins on a name collision); either source alone is fine — **no skills root is required** when every skill is defined inline. Inline skills **merge by key across config layers** (§9e), exactly like `agents:` — so a bundled config layer can ship an agent *and* its skills together, and a later layer can override a skill by re-declaring its key. The agent's `skills:` field is a **pattern allowlist** (RFC BA), not a bundle list: it governs which skills the agent may list / use / author. Bodies are loaded on demand via the `Skill` tool, never baked into the prompt. The security invariant holds at **invoke time**: a skill's `tools` must be ⊆ the invoking agent's, or the load is refused. (`SKILL.md` frontmatter uses the hyphenated `allowed-tools`; the inline map uses `tools` to match the rest of the loomcycle YAML.)

**Multi-tool research agent**:

```yaml
---
name: company-researcher
description: Researches ONE company for a job application...
tools: WebSearch, WebFetch, mcp__brave-search__brave_web_search
tier: middle
tools:
  - WebSearch
  - WebFetch
  - mcp__brave-search__brave_web_search
---
```

Mix of built-in tools (WebSearch, WebFetch) and an MCP tool. Tier-driven resolution applies.

### Claude-Code compatibility

The same `.md` file works in both Claude Code and loomcycle. **Claude-Code-honoured fields**: `name`, `description`, `tools` (comma-string), `model`. **Loomcycle extensions**: `tier`, `models`, `providers`, `effort`, `max_tokens`, `sampling`, `skills`, `tools` (list form), `system_prompt_file`, `memory_scopes`, `memory_quota_bytes`, `channels`, `agent_def_scopes`, `evaluation_scopes`. Claude Code ignores unknown keys; loomcycle treats the format as a superset. Keep your agents portable by including both `tools:` (Claude Code shape) and `tools:` (loomcycle shape) when you want the same file used in both.

### Operator-yaml `agents:` overlay

The operator yaml's `agents:` map can override any frontmatter field at the deployment level. Useful when you want different model resolution per deployment without forking the .md files. Merge logic at `internal/config/config.go:1531`:

- Scalar fields (string, int): YAML non-zero value wins
- Slice/map fields: YAML `nil` keeps the discovered value; YAML non-nil (even `[]`) is an explicit override
- `system_prompt` and `system_prompt_file` are mutually exclusive — setting one in YAML clears the other from the merged struct

Example overlay:

```yaml
# loomcycle.yaml
agents:
  cv-rewriter:
    # Override the agent's pin for THIS deployment only.
    # Removes the model: sonnet from the merged config and uses tier instead.
    tier: high
    model: ""
    provider: ""
```

(Setting `model: ""` is the explicit "clear" — without it, the discovered `sonnet` would stay.)

---

## 8. Conflict resolution — what wins when

Single reference table:

| Conflict | Winner | Where enforced |
|---|---|---|
| `tier:` AND (`provider:` / `model:`) both set | **Config-load fails** | `config.go:1985` |
| `tools:` AND `tools:` both set | `tools:` wins | `loader.go:295` |
| Body AND `system_prompt_file:` both set | Setting either via YAML overlay clears the other | `config.go:1564` |
| Agent `providers:` AND user_tier `provider_priority` both set | **Intersection** (agent-order); empty → `ErrTierAgentNotAvailable` | `matrix.go:440` |
| Agent `models[tier]:` set | Replaces library `tiers[tier]` AND user_tier `tiers[tier]` for this agent | `matrix.go` candidate-list build |
| user_tier `tiers[tier]:` set (no agent override) | Replaces library `tiers[tier]` | resolver candidate-list build |
| Discovered .md field AND operator-yaml `agents:` overlay both set | YAML non-zero wins; nil slice keeps .md value | `config.go:1531` |
| `model: sonnet` (alias) AND `models:` map has `sonnet` | Alias expands to `{provider, model}` from the map | `config.go:1370` |
| `model: claude-sonnet-4-6` (literal, no alias) | Used as-is as the model ID | same path |
| Probe says provider unreachable | Resolver skips all that provider's candidates | `matrix.go` per-candidate check |
| Provider has no API key set | Marked excluded, treated like unreachable | startup probe |

---

## 9. Validation + verification

### At config-load

Loomcycle validates the yaml at startup. Common errors:

| Error message | What's wrong |
|---|---|
| `agent X: cannot set both explicit provider/model pin and tier (pick one)` | `tier:` AND `provider:`/`model:` both present on the same agent |
| `agent X: no model, no tier, and no defaults.model` | Agent has neither and operator has no `defaults.model` fallback |
| `user_tiers: missing "default" entry` | You set `user_tiers:` but didn't include `default:` |
| `unknown provider: X` | Provider in `provider_priority` or a candidate doesn't match a registered driver |
| `model alias cycle: X → Y → X` | `models:` map has a cycle |

### At runtime — inspect the resolver matrix

`/v1/_resolver` returns the live availability matrix (auth-gated by `LOOMCYCLE_AUTH_TOKEN`):

```sh
curl -s -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  http://localhost:8787/v1/_resolver | jq .
```

Shows: which providers are reachable, which models each lists, which models are stalled, the last probe time. Useful for confirming "is ollama-local actually probing successfully?"

### At runtime — force an immediate re-probe

The resolver re-probes every provider on a fixed interval (`LOOMCYCLE_RESOLVE_PROBE_INTERVAL_MS`, default 15 min). If a transient outage (DNS hiccup, brief upstream blip, the VM losing egress for a few seconds) stalls every provider mid-probe, runs can 503 for up to a full interval until the next tick. `POST /v1/_resolve/probe` triggers an immediate, synchronous re-probe so an operator can unstick the matrix without a restart (a restart drops in-flight runs):

```sh
curl -s -X POST -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  http://localhost:8787/v1/_resolve/probe | jq .
```

Returns the **post-probe** matrix in the same shape as `GET /v1/_resolver`. A provider still unreachable after the probe comes back as `reachable: false` with its `last_error` set — that's data, not an error (200). The endpoint only 503s when it can't probe at all: `resolver_unavailable` (degraded startup, no resolver) or `probe_unavailable` (no probe loop wired, e.g. a degraded startup). Also handy for post-deploy / post-config validation before serving traffic.

The same operation is available through every transport that consumes the Connector: gRPC `ResolveProbe`, the MCP meta-tool `resolve_probe`, and the TypeScript client's `client.resolveProbe()`.

### At runtime — confirm the resolved (provider, model)

Every run's SSE stream includes an early event carrying the resolved pair. Smoke run:

```sh
curl -N -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"agent":"<your-agent>","user_tier":"free","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hello"}]}]}' \
  http://localhost:8787/v1/runs
```

The first `event: started` (or `event: resolved`) frame carries `provider=...` and `model=...`. Confirms your config picked what you expected.

After v0.8.16 (PR #116), the model is also persisted at run start, so `GET /v1/users/{id}/agents` shows it during the run, not just at completion.

---

## 9b. Multi-tenant authentication (RFC L)

By default every authenticated caller presents the single shared `LOOMCYCLE_AUTH_TOKEN` — correct for one operator or a single trusted team. For a team or small-VPS service fronting users who don't trust each other's claims, **OperatorTokenDef** issues per-principal bearer tokens, each bound to an **authoritative `(tenant, subject, scopes)`** that the middleware resolves *from the token* and stamps over the wire `tenant_id`/`user_id`. The token's `subject` becomes the run's `user_id` (the fairness key); its `tenant_id` is the memory-isolation boundary.

```sh
# Mint a per-developer token (shown once). Needs an existing admin bearer.
loomcycle operator-token create --tenant acme --subject alice \
  --scopes runs:create,runs:read
loomcycle operator-token rotate --name alice   # zero-downtime roll (grace window)
loomcycle operator-token retire --name alice   # immediate revoke
```

Migrate the existing shared secret in place — it keeps working as an admin token after the legacy fallback disables:

```sh
loomcycle operator-token create --tenant default --subject ops --copy-from-env
```

Config knobs (full reference: `loomcycle context help operator-tokens` or the `Context.help operator-tokens` tool topic):

| Env var | Purpose |
|---|---|
| `LOOMCYCLE_OPERATOR_TOKEN_PEPPER` | Mixed into the token hash; a stolen DB dump without it yields no usable lookup. Set it for multi-tenant deployments. |
| `LOOMCYCLE_AUTH_CACHE_TTL_SECONDS` | Per-replica resolution-cache TTL (default 30; `0` = direct lookup, immediate revocation). Worst-case revocation lag if a cross-replica invalidation is dropped. |
| `LOOMCYCLE_OPERATOR_TOKEN_ROTATION_GRACE_SECONDS` | Default rotation grace window (default 24h). |
| `LOOMCYCLE_AUDIT_LOG_PATH` | JSONL audit of every create/rotate/retire (never a token or hash). |
| `LOOMCYCLE_AUTH_VERBOSE` | `1` logs a server-side reason on a rejected bearer (the wire 401 stays opaque). |
| `LOOMCYCLE_OPERATOR_KEY_RESTRICTION` | **RFC AX** deployment gate (default OFF). When `1`, a run whose principal lacks `providers:operator-key` may not use the operator's host provider key — resolution routes it only to providers the tenant can key itself (an RFC AR CredentialDef) and refuses `403 operator_key_restricted` if none; the LLM-gateway + embeddings shims refuse a restricted principal outright. OFF ⇒ byte-identical for every existing token. |

Routes enforce a scope from a closed catalog; an under-scoped token gets `403` + `WWW-Authenticate: Bearer scope="…"`. The legacy `LOOMCYCLE_AUTH_TOKEN` is disabled only once an admin-scoped token exists (the no-lockout gate). The catalog:

| Scope | Grants |
|---|---|
| `substrate:admin` | **Superuser** — satisfies every scope, incl. token minting, runtime admin (pause/resume/snapshot), and **cross-tenant** focus. The create-time default. |
| `substrate:tenant` | **Tenant operator (RFC AF/AG)** — FULL power WITHIN the token's own tenant: runs, channels, authoring all 8 substrate Def families (incl. `_mcpserverdef`, the dynamic-MCP-ingestion surface), registering tool-use hooks, and opening a **tenant-confined** loomcycle-as-MCP-server session (`/v1/_mcp`, RFC AG) — but NOT the operator plane (no minting, no runtime admin, no cross-tenant access). Lets a self-provisioning tenant author its own surface without admin. |
| `runs:create` / `runs:read` | Create/continue runs · read runs, agents, sessions. |
| `channel:publish` / `channel:read` | Publish/ack · subscribe/peek on the per-user + system channel surface. |
| `providers:operator-key` | **RFC AX** — permits a run to fall back to the operator's HOST provider key. **Tenant-implied** (`substrate:tenant` and `substrate:admin` already have it), and **inert unless** `LOOMCYCLE_OPERATOR_KEY_RESTRICTION=1`. To make a tenant pay its own way: set the gate, mint that tenant's principals with granular scopes that OMIT this one (e.g. `runs:create,runs:read`), and give the tenant its own key (an RFC AR CredentialDef). |

`substrate:tenant` satisfies the within-tenant scopes (`runs:*`, `channel:*`, and the def/hook gate) but never `substrate:admin` — so a tenant operator passes the def + hook routes yet is refused minting/runtime-admin. Mint one with `--scopes substrate:tenant`. Confinement is automatic: a non-admin principal's def writes are stamped with its authoritative tenant, cross-tenant reads return an opaque `404`, and a tenant-registered hook fires only on that tenant's runs.

On the **loomcycle-as-MCP-server transport** (`/v1/_mcp`, RFC AG) a `substrate:tenant` bearer may now OPEN a session — but the route scope decides only that. Inside the session a per-tool gate still withholds the admin-only meta-tools (token minting, runtime admin, snapshot capture/restore, cross-scope channel listing): they are hidden from the tenant's `tools/list` and refused on `tools/call`. The tools a tenant *can* call (def authoring, run lifecycle, memory/channel/path/document, hooks) are tenant-confined exactly like their HTTP twins. `substrate:admin` sees + may call the full meta-tool set. The thin client (`loomcycle mcp --upstream`) forwards the caller's bearer verbatim, so a tenant bearer there drives a tenant-confined session — see `docs/MCP_SERVER.md`.

### Declared principals — static `(tenant, user)` logins (RFC AO)

Beyond the legacy `LOOMCYCLE_AUTH_TOKEN` (one identity, subject `default`) and runtime-minted `OperatorTokenDef` tokens, you can **declare** stable service identities in the YAML:

```yaml
principals:
  marketing:                                   # informational handle (the map key)
    tenant: acme                               # authoritative tenant ("" = the shared/operator tenant)
    subject: marketing                         # authoritative user id (= scope_id for user-scoped tools)
    scopes: [runs:create, runs:read, substrate:tenant]
    token_env: LOOMCYCLE_TOKEN_MARKETING       # env var holding the SECRET (in .env.local)
  ops-admin:
    subject: ops
    scopes: [substrate:admin]                  # admin is EXPLICIT — declared principals aren't admin by default
    token_env: LOOMCYCLE_TOKEN_OPS
```

- **The yaml carries only `token_env` (a name), never the secret.** The bearer value lives in `.env.local` (e.g. `LOOMCYCLE_TOKEN_MARKETING=lct_…`). `token_env` must be `LOOMCYCLE_*`-prefixed (or an allowlisted third-party name) and may **not** name one of loomcycle's own infra secrets (the DSN / pepper / `LOOMCYCLE_AUTH_TOKEN` / upstream MCP token).
- **`tenant` / `subject` / `scopes`** become the resolved `Principal` — authoritative, server-side, never from the wire. `scopes` is validated against the closed catalog above; an empty/missing `scopes` authenticates but is gated out of everything.
- **Resolution order:** minted `OperatorTokenDef` → **declared principal** → legacy token. A token value shared by two declared principals is a config-load error; an empty `token_env` at boot makes that principal **inert** (a startup warning, not an open door).
- **The payoff — alignment by construction.** Use one declared token for **both** the Web UI login (`/ui/login`) and an MCP thin client (`LOOMCYCLE_MCP_UPSTREAM_TOKEN`); both resolve to the same `(tenant, subject)`. Combined with the per-principal `/v1/_mcp` transport (RFC AG), an MCP agent's user-scoped Documents/Memory land under the same user the UI reads — no synthetic-operator mismatch.

**Trigger-spawned runs choose their tenant in the def (RFC N).** An interactive run inherits its tenant from the caller's token, but a scheduler- or webhook-spawned run has no inbound bearer — so the run-execution tenant is declared in the def via `tenant_id:` on a `scheduled_runs:` entry or a `webhooks:` entry. The spawned run then resolves that tenant's agents/skills/MCP and isolates its memory/runs. It is operator-authored def-content (`""` = shared/default). **Security: for webhooks the tenant comes from the static def ONLY — never from the inbound `payload_mapping`** (the attacker-influenceable body must not be able to select another tenant). See `Context.help scheduled-runs` / `Context.help input-webhooks`.

**The webhook def itself is tenant-isolated too (RFC N completion).** Beyond the run-execution `tenant_id` above, every substrate Def's *active pointer* is keyed on `(tenant_id, name)` — a webhook authored by a tenant principal is owned by that tenant and addressed on its own inbound route: `POST /v1/_webhooks/{tenant}/{name}`. The bare-root `POST /v1/_webhooks/{name}` resolves under the shared `""` tenant, so an existing single-tenant deployment (everything `tenant_id=""`) keeps using the unprefixed URL unchanged. A multi-tenant downstream that authors webhooks under a non-empty tenant **must register its delivery URL with the `/{tenant}/` prefix** for inbound deliveries to resolve. (The admin dry-run `POST /v1/_webhooks/{name}/test` resolves under the bearer's own principal tenant.)

---

## 9c. Environment files — secrets vs. config (`.env.local` / `.env.insecure`)

loomcycle is configured entirely through environment variables (the `loomcycle.yaml` carries agents + routing; everything operational is an env var). The **recommended** layout splits those vars across **two files** by sensitivity, so the secret-bearing half can be locked down independently of the operational half:

| File | Holds | Git posture | Safe to read/diff/share? |
|---|---|---|---|
| **`.env.local`** | **Secrets** — provider API keys (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, …), the sidecar bearer `LOOMCYCLE_AUTH_TOKEN`, `BRAVE_API_KEY`, the operator-token pepper, and the secret **values** behind any trigger-credential env names. | **git-ignored**, never committed | **No** — surfacing it leaks credentials |
| **`.env.insecure`** | **Non-secret config** — `LOOMCYCLE_LISTEN_ADDR`, `LOOMCYCLE_DATA_DIR`, host allowlists, feature flags (metrics, webhooks, fallback), timeouts, and the trigger-credential allowlist **names** (`LOOMCYCLE_WEBHOOKS_ENV_ALLOWLIST` / `LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST`). (Filesystem sandboxing moved to the YAML `volumes:` block — RFC AH Phase 3 retired the `READ_ROOT`/`WRITE_ROOT`/`BASH_CWD` env vars.) | git-ignored in *this* repo; commit it in your own deployment repo if you wish | **Yes** — nothing here is a secret |

**Why split.** The two halves have different blast radii. `.env.insecure` is the part you want in code review, in a config-management repo, in a teammate's hands when they ask "what's your sandbox set to?" — none of it is dangerous to expose. `.env.local` is the part one accidental `cat` in a screen-share burns. Keeping them in one file forces the whole thing to the secret tier and makes the operational config needlessly hard to share. The split also matches the security rule in `CLAUDE.md` — agents (and Claude Code) must never open `.env.local`, but `.env.insecure` is freely readable.

**The allowlist-name vs. secret-value seam.** A webhook's `signing_secret_env: LOOMCYCLE_GITEA_WEBHOOK_SECRET` names an env var. The **name** is non-secret and is authorized in `.env.insecure` (`LOOMCYCLE_WEBHOOKS_ENV_ALLOWLIST=…`, or auto-allowed when `LOOMCYCLE_*`-prefixed — see §9b / `Context.help input-webhooks`). The **value** — the actual HMAC secret — lives in `.env.local`. This is exactly why the two files are sourced together: the allowlist in one, the secret it authorizes in the other.

**Bootstrap.** Two committed templates carry the full annotated reference:

```sh
cp .env.local.example     .env.local      # then fill in the secrets
cp .env.insecure.example  .env.insecure   # then adjust paths/flags
```

**How they're loaded.** `loomcycle.sh` and `loomcycle-mcp.sh` source **`.env.insecure` first, then `.env.local`** (config first, secrets last — so a stray config line can never shadow a secret) before exec'ing the binary; either file may be absent. Set `LOOMCYCLE_ENV_FILE=<path>` to collapse the pair back to a single explicit file (the pre-split single-file flow). loomcycle itself reads only process env — it does not parse these files — so any supervisor (systemd `EnvironmentFile=`, Docker `--env-file`, a CI secret store) can substitute for the launcher scripts.

**Secrets at rest (F32).** Two mechanisms keep resolved credentials out of the on-disk state:

- **Definition plane — store the reference, resolve at use-time.** A substrate def persists the `${ENV_NAME}` reference, never the expanded value. Webhook defs store `signing_secret_env` / `bearer_token_env` (env-var *names*), and a dynamic MCP server's `url` / `headers` keep their `${LOOMCYCLE_*}` placeholders in `mcp_server_defs.content` — the secret is resolved only when the pool dials the server (mirroring how a yaml `mcp_servers.*` entry is expanded at config load). `content_sha256` is computed over the reference, so it stays stable when the token rotates.
- **Transcript plane — redact before persisting (`LOOMCYCLE_REDACT_SECRETS`, default ON).** Tool I/O (tool_call inputs + tool_result outputs) is scanned for secret-shaped substrings and masked before it reaches the events store (and thus snapshots + the `/v1/_events` audit API): the exact values of secret-named env vars (`*_KEY` / `*_TOKEN` / `*_SECRET` / `*_AUTH` / `*_PASSWORD` / `*_CREDENTIAL`) become `[redacted:NAME]`, plus conservative heuristics for `Authorization:` headers, `sk-`/`AKIA`/`xox`/`ghp_` keys, and `*_API_KEY=` assignments. The live SSE stream is **not** redacted (the caller already holds the secret). Set `LOOMCYCLE_REDACT_SECRETS=0` to disable. This is defense-in-depth — agents should still pass secrets out-of-band (env / stdin / credential helper), never inline on a cmdline.

---

## 9d. Filesystem Volumes — per-agent ro/rw scopes (RFC AH)

The file/exec tools (Read / Write / Edit / Glob / Grep / Bash / NotebookEdit) used to share a single per-instance jail (`LOOMCYCLE_READ_ROOT` / `WRITE_ROOT` / `BASH_CWD`). **Phase 3 retired that jail** — a **Volume** (a named, per-agent, read-only-or-read-write root) is now the *sole* filesystem mechanism, the filesystem analog of the caller-authoritative `allowed_hosts` host policy. Two ensembles in one runtime can be confined to separate working trees, and **disk access is sandbox-by-default**: an agent bound to no volume has none.

**Top-level `volumes:` map** — the universe of bindable roots (the filesystem analog of registered tools). Declared once by the operator:

```yaml
volumes:
  default:   { path: /work/sandbox,     mode: rw, default: true }
  shared-ro: { path: /work/reference,   mode: ro }
  repo-a:    { path: /work/ensembles/a, mode: rw }
```

- `mode` is `rw` (read+write) or `ro` (read-only); empty defaults to `rw`.
- Each `path` **must already exist and be a directory** — validated at config-load (static volumes map existing infrastructure; the runtime never `mkdir`s them). Paths are resolved to absolute.
- At most one volume may be `default: true` — it's the one a tool call uses when it omits the `volume` argument.

**Per-agent `volumes:` binding** — which volumes an agent's tools may use, validated against the map above (exactly like `tools` against registered tools):

```yaml
agents:
  ensemble-a-lead:
    tools: [Read, Write, Edit, Glob, Grep, Bash, Agent]
    volumes: [repo-a, shared-ro]   # confined to these; cannot touch default or repo-b
```

- An agent that declares **no** `volumes` is implicitly bound to `[default]` — *if* a `default` volume exists. With no `default` volume declared, an unbound agent has **no filesystem access** (sandbox-by-default).
- An agent that declares volumes is confined to **exactly those** — it does *not* implicitly also get `default`.

**ro / rw enforcement.** Read / Glob / Grep operate on any bound volume. Write / Edit / NotebookEdit require a `rw` volume (a `ro` target is refused). **Bash requires `rw` and is refused on a `ro` volume** — a shell can write via absolute paths and redirection, so loomcycle refuses rather than ship a read-only guarantee it cannot keep (CLAUDE.md rule #7).

**Spawn confinement.** A sub-agent inherits its parent's confinement: an *unbound* child gets the parent's policy verbatim; a child that *declares* volumes is **narrowed** to (child-declared) ∩ (parent's active bindings), with ro/rw resolving to the more restrictive. A child can never gain a volume its parent lacks; a child that shares none of the parent's volumes is confined to nothing — its file tools are denied. Mirrors host-allowlist narrowing.

**Sandbox-by-default & migration (RFC AH Phase 3).** The legacy `LOOMCYCLE_READ_ROOT` / `WRITE_ROOT` / `BASH_CWD` jail is **retired** — volumes are the only filesystem mechanism. With no `volumes:` block and no agent bindings, every file/exec tool **refuses** (no disk access), mirroring the network model (no `allowed_hosts` → no egress). **Setting any of the three retired env vars now fails startup** with a migration hint. To restore the old single shared jail, declare one `default` volume:

```yaml
volumes:
  default: { path: /work/sandbox, mode: rw, default: true }
```

Unbound agents then bind to it. (There is no auto-synthesis from the old env vars — a single root can't reproduce three distinct ones, and a `READ_ROOT`-only "writes disabled" deploy must not silently gain write access — so the operator declares the replacement volume once, explicitly.)

**Introspection.** `Context op=self` reports the bound volumes (`volumes.bindings`: name / path / mode / default), so an agent knows precisely which roots it may touch and which verb each allows. An agent with no volume bound reports `filesystem: "none — no volume bound"`.

### 9d.1 Dynamic volumes — the `VolumeDef` substrate (RFC AH Phase 2a)

Static volumes (above) require the operator to pre-declare every volume in yaml. The **`VolumeDef`** tool adds runtime-mutable, tenant-scoped, **confined** volumes so a tenant can provision a working tree per job without a config change.

**The dynamic root.** Mark exactly one static volume `dynamic_root: true` — the operator-blessed parent under which all dynamic volumes are provisioned and confined:

```yaml
volumes:
  default: { path: /work/sandbox, mode: rw, default: true }
  pool:    { path: /work/dynamic, mode: rw, dynamic_root: true }
```

At most one volume may set `dynamic_root` (config-load error otherwise), and like any static volume its `path` must already exist and be a directory. With no `dynamic_root` declared, `VolumeDef create` refuses.

**The `VolumeDef` tool.** A per-agent in-loop tool (also reachable over the MCP server and the `POST /v1/_volumedef` admin endpoint). Ops:

| op | effect |
|----|--------|
| `create {name, mode}` | Derive `path = <dynamic_root>/<tenant-segment>/<name>` (tenant-segment = the tenant id, or `_shared` for the shared tenant), `mkdir` it (`0700`), persist the mapping. `mode` is `rw` (default) or `ro`, caller-chosen. **Idempotent** — same mode is a no-op, a different mode updates. Refused on a static-name collision (yaml is ground truth) or when no `dynamic_root` is configured. |
| `create {name, mode, ephemeral:true}` | Provision a **run-scoped ephemeral** volume instead (see §9d.2): `path = <dynamic_root>/_ephemeral/<root_run_id>/<name>`, auto-purged when the top-level run finishes. Requires an active run. |
| `get {name}` / `list` | Tenant-scoped reads — another tenant's volume is reported as not-found. |
| `delete {name}` | Remove the mapping, **LEAVE files on disk** (unmap). |
| `purge {name}` | Remove the mapping **AND** delete the directory tree (the destructive op). |

**The path is runtime-derived — never caller-supplied.** `create` takes a name + mode only; the path is derived. Names must match `^[a-z0-9][a-z0-9_-]{0,63}$` (no slashes/dots), so a name can't inject a path component. `purge` re-derives the path (it never trusts the stored value), `EvalSymlinks` it, and refuses unless the resolved real path is strictly inside the dynamic root under the tenant's segment — so a recursive delete can only ever target the tenant's own volume directory.

**Capability gate — `volume_def_scopes`.** The tool is default-deny. Grant it per-agent (closed set: `any` or `named:<volume>`):

```yaml
agents:
  ensemble-launcher:
    tools: [VolumeDef, Agent, Read, Write, Bash]
    volume_def_scopes: [any]        # may create/delete/purge any dynamic volume
    # volume_def_scopes: [named:repo-a]   # or only the named volume(s)
```

Without a grant, create/delete/purge are refused; `get`/`list` are tenant-scoped reads available to any agent the tool is attached to. The `POST /v1/_volumedef` endpoint is bearer-authed under the RFC AF `substrate:tenant` scope (the operator-trust admin caller is granted `any`).

**Binding.** After `create`, an agent binds to a dynamic volume by name exactly like a static one (`volumes: [repo-a]`). Run-start resolves the name static-first, then the agent's own tenant's dynamic volumes, then the shared tenant's — an operator static volume can never be shadowed by a dynamic one. Spawn narrowing is unchanged.

> **Not in Phase 2a:** gRPC / MCP-meta-tool parity for the authoring surface; Web UI; versioning. The in-loop tool and the HTTP endpoint are the Phase 2a authoring surfaces. Ephemeral run-scoped volumes shipped in Phase 2b (§9d.2).

### 9d.2 Ephemeral (run-scoped) volumes (RFC AH Phase 2b)

A dynamic volume (§9d.1) is tenant-shared; an **ephemeral** volume is scoped to the creating run **tree** and torn down when the top-level run finishes — per-ensemble scratch with no cross-run collision, even for two concurrent runs in one tenant.

Create one with `ephemeral: true`:

```jsonc
{"op": "create", "name": "work", "mode": "rw", "ephemeral": true}
```

- **Path.** Derived as `<dynamic_root>/_ephemeral/<root_run_id>/<name>`. `_ephemeral` is a reserved first segment under the dynamic root — a tenant id literally equal to `_ephemeral` is rejected (like `_shared`), and the name charset forbids a leading underscore, so the two purge fences never blur. Run ids are globally unique, so two runs (any tenant) never collide.
- **Lifetime.** Resolvable by the whole creating run tree (parent + sub-agents, inherited under the same narrow-only spawn rule). Auto-**purged when the top-level run completes** — an inline run-completion hook fenced-`RemoveAll`s `<dynamic_root>/_ephemeral/<root_run_id>/` and drops the rows. A sub-agent completing never purges (the tree belongs to the top-level run). There is no `delete`/`purge` op for ephemeral volumes — the lifetime *is* the run.
- **Requires an active run.** `ephemeral: true` is refused outside a run (no root run id) and on a static-name or in-run-duplicate collision. Same `volume_def_scopes` gate as the persistent op.
- **Crash backstop — `LOOMCYCLE_EPHEMERAL_VOLUME_SWEEP_MS`.** A singleton sweeper (default **60s**; `0` disables — the inline purge still runs) periodically purges ephemeral volumes whose owning run is terminal **and not paused/pausing** (a paused run is parked, not crashed — its volumes survive to be reused on resume; a resumed run rehydrates its in-memory set from the persisted rows). Cluster-gated, so one replica per tick does the work. Skipped when no `dynamic_root` is configured.

### 9d.3 Volumes console tab (RFC AH Phase 4)

The embedded Web UI (`/ui`) has a dedicated **Volumes** tab (admin-gated, alongside Library / Integrations / Channels / Schedules) to view and manage volumes for the operator's tenant. It's a thin client over the HTTP surface — no new runtime behaviour.

- **Persistent.** A flat table of static volumes (read-only — the operator yaml is ground truth, including the `dynamic_root`) plus the tenant's dynamic `VolumeDef`s. Dynamic rows support **Delete** (non-destructive — unmaps the volume, keeps files) and **Purge** (destructive — `RemoveAll`s the directory tree). A **Create** button provisions a dynamic volume by name + mode (the runtime derives the path). A **bound by** column shows which agents bind each volume.
- **Ephemeral.** A read-only table of the tenant's live ephemeral volumes (auto-purged at run completion), polled every ~10s.
- **Purge is type-to-confirm** — the operator must type the volume's name to enable the confirm button, distinct from the one-click Delete. The server-side `RemoveAll` fence (§6 of the RFC: re-derive, EvalSymlinks, strictly-inside, expected-prefix) remains the real guard.
- **Backed by two read-only endpoints** — `GET /v1/_volumes` (merged static + tenant dynamic) and `GET /v1/_volumes/ephemeral` (the tenant's live ephemeral volumes). Both are tenant-scoped from the authoritative principal (gated at `substrate:tenant`; admin satisfies). CRUD reuses `POST /v1/_volumedef`.

---

## 9e. Config layering — stacking multiple config files (RFC AN)

`--config` is **repeatable**, and the files are **deep-merged left→right, last
wins** — so you can compose a bundled config with your own without copy-paste:

```sh
loomcycle --config bundles/document-agent/loomcycle.yaml \
          --config ~/.config/loomcycle/loomcycle.yaml
```

Put your authoritative file **LAST**: the bundle contributes its `agents:` (e.g.
`doc-manager`), and your file wins on `provider_priority` / `tiers` / `volumes` /
`defaults` — so the bundle's agent runs on *your* routing and reads *your*
`default` Volume. Containers/systemd can use the env form instead:
`LOOMCYCLE_CONFIG_FILES=base.yaml:override.yaml` (`:`-separated). When both are
set, `LOOMCYCLE_CONFIG_FILES` files layer as the **base** and explicit `--config`
flags layer **after** them (an explicit flag always wins).

**The merge rule (one rule, all sections).** Files are merged at the YAML-tree
level *before* typed unmarshal, so a key's presence is explicit:

> **mapping ⊕ mapping → merge keys recursively. Otherwise (scalar, sequence, or
> type mismatch) → the later layer replaces.**

| Section | Merge result |
|---|---|
| `agents`, `models`, `mcp_servers`, `channels`, `volumes`, `user_tiers`, `webhooks`, `a2a_*`, `memory_backends`, `scheduled_runs`, `principals` | **by key** — a new entry is added; a same-named entry **field-merges** (last wins per field, matching the `LOOMCYCLE_AGENTS_ROOT` / `mergeAgentDef` precedent) |
| `tiers` | per-tier **by key**; each tier's candidate list **replaces** wholesale (or composes when the overlay tags it `!prepend` / `!append` — see below) |
| `provider_priority`, `context_plugins` | **replaced** wholesale by default; an overlay can **compose** instead by tagging the sequence `!prepend` / `!append` (RFC AQ — see below) |
| `defaults`, `concurrency`, `cache`, `storage`, `interruption`, `hooks`, `memory` | **field-by-field** (a layer may override one field) |
| top-level scalars | last layer wins |

**Composing sequences — `!prepend` / `!append` (RFC AQ).** An overlay sequence
tagged `!prepend` merges its items in FRONT of the accumulated sequence;
`!append` merges them AFTER; an **untagged** sequence still replaces wholesale.
Duplicates (deep-equal) are dropped keeping the **first** occurrence — so
`!prepend`-ing a re-listed provider promotes it. A tagged merge is a deliberate
compose, so it is **not** a cross-layer conflict (no override warning, never trips
`LOOMCYCLE_CONFIG_STRICT`). This is what lets the embedded `oauth` / `local`
presets (§9f) be one-provider-per-file:

```yaml
# an overlay that puts OAuth on top WITHOUT restating the base matrix:
provider_priority: !prepend [anthropic-oauth-dev]
tiers:
  middle: !prepend [oauth-sonnet]
```

**Conflicts are explicit.** Every leaf a later layer *replaces* (a key set
differently in an earlier layer) is logged at startup
(`config layer override: <dotted.path> (set by <file>, …)`). Set
**`LOOMCYCLE_CONFIG_STRICT=1`** to make any cross-layer conflict a **fatal load
error** (recommended for production — an accidental clobber of `provider_priority`
or a host allowlist can't slip through silently). Adding a new key or re-setting a
key to the *same* value is not a conflict.

**Notes.** Each file keeps its own `${ENV}` expansion (a later layer can't inject
into an earlier layer's text). The merged whole runs the **same `validate()`** as a
single file — layering only *assembles* a config, it can't produce one a single
file couldn't. A single `--config` is byte-identical to before. This is orthogonal
to the `.env.local` / `.env.insecure` split (§9c) — that's env vars; this is YAML.
Relative `system_prompt_file` paths resolve against the **last** file's directory
(bundles should inline the prompt or use an absolute path). *(Deferred: an in-YAML
`include:` directive and `loomcycle config render` — RFC AN Phases 2–3.)*

---

## 9f. Embedded presets & bundles (RFC AQ)

The binary ships a curated set of config layers so an install resolves a sane
base — and built-in agents — **without a source checkout**. Two kinds:

- **Presets** — pure provider/tier/model config (no agents, no secrets — only
  `token_env` *names*). `base` is the full provider matrix (mirrors the provider
  half of `loomcycle.example.yaml`); `oauth` and `local` are one-provider-per-file
  overlays that `!prepend` their provider onto `base` (§9e) — `oauth` puts
  Anthropic OAuth-dev on top, `local` puts Ollama on top, each keeping base's
  matrix as fallback.
- **Bundles** — a preset that *also* defines an agent and its skills **inline**
  (the top-level `skills:` map, §7). `document-agent` ships the `doc-manager`
  Document Assistant agent + its four skills — no skills directory, no
  `LOOMCYCLE_SKILLS_ROOT`.

List + read them (works on any install, no source tree):

```sh
loomcycle presets                  # name / kind / description table
loomcycle presets show base        # print a unit's YAML (read or fork it)
loomcycle env-template             # print the embedded .env.insecure.example
```

On a no-shell deployment the same three are read-only in the **Web UI Settings
hub** (the top-right gear → **Presets**, admin-only) — backed by
`GET /v1/_presets`, `/v1/_presets/{name}`, `/v1/_env_template`.

**Selecting them.** `LOOMCYCLE_PRESETS=base,document-agent` (comma-separated,
ordered) — or the repeatable `--preset` flag (flags override the env list) —
layers the named units as the **base** of the config stack, under your files:

```sh
# OAuth-first base + the Document Assistant, under your thin overlay:
LOOMCYCLE_PRESETS=base,oauth,document-agent loomcycle --config ~/.config/loomcycle/loomcycle.yaml
```

Because `oauth` / `local` `!prepend` (§9e), `base,oauth` resolves to OAuth on top
with base's providers retained as fallback — no restatement of the matrix.

The **full precedence chain** (base → top, last wins, RFC AN merge):

```
embedded presets (LOOMCYCLE_PRESETS, in order)
  → LOOMCYCLE_CONFIG_DIR/*.yaml   (a dir of layers, lexical order)
  → LOOMCYCLE_CONFIG_FILES        (':'-separated)
  → --config flags                (your authoritative overlay, wins)
```

`LOOMCYCLE_CONFIG_DIR` (RFC AQ) is a directory whose `*.yaml` / `*.yml` files
layer as a group in lexical filename order (so prefix them `10-`, `20-`, …) —
handy for a mounted overlay dir or an image-baked drop-in set. A set-but-missing
dir is a fatal error; an empty dir is fine.

So `base` supplies the provider matrix, `document-agent` registers `doc-manager`
with its skills, and your `--config` wins on anything it sets (e.g. retarget the
agent's tier, narrow its `tools` — you can't *widen* it past the def's
ceiling, or swap a skill body by re-declaring its key). Selecting presets with
**no config file at all** boots from the embedded base alone (the bare-start
case). An unknown unit name is a **fatal** error listing the available names.

**Opt-in.** With `LOOMCYCLE_PRESETS` unset and no `--preset`, boot is exactly as
before (no presets) — embedded presets are a deliberate opt-in, not a silent new
base. `document-agent` needs SQL Memory (`LOOMCYCLE_SQLMEM_ENABLED=1`) + a
`middle` tier to actually run; absent those it's a registered-but-idle def.

---

## 10. Cross-references

- [`loomcycle.example.yaml`](../loomcycle.example.yaml) — the repo-root reference yaml. All six user_tiers wired, inline comments on every section. Copy-paste and edit.
- [`loomcycle.example.yaml`](../loomcycle.example.yaml) — the comprehensive example config (aliases-first, every tool/feature exercised). Start here.
- [`loomcycle.local-interactive.example.yaml`](../loomcycle.local-interactive.example.yaml) — a focused config for driving interactive agents on local (Ollama) models, with the slow-model knobs from §6b wired together.
- [`.env.local.example`](../.env.local.example) + [`.env.insecure.example`](../.env.insecure.example) — the two env-file templates (secrets vs. non-secret config; see §9c). Every operational env var is documented inline in one or the other.
- [`docs/MCP_INTEGRATION.md`](MCP_INTEGRATION.md) — MCP server configuration (deliberately out of scope for this doc).
- [`docs/ARCHITECTURE.md`](ARCHITECTURE.md) — broader runtime context, provider driver table, probe semantics.
- [`docs/TOOLS.md`](TOOLS.md) — tool policy and built-in tool reference (the `tools` / `tools` axis).
- [`docs/POSTGRES.md`](POSTGRES.md) — storage backend configuration.
- [`docs/PLAN.md`](PLAN.md) — historical design rationale, including the v0.8.2 `user_tiers` RFC and the precedence design decisions.
- [`examples/observability/`](../examples/observability/) — three drop-in observability profiles (Grafana+Tempo self-hosted / Honeycomb / Datadog) for sending loomcycle's OTEL traces + Prometheus metrics to your existing stack. Five-minute quickstart per profile.
- MCP spec: https://modelcontextprotocol.io (only relevant if you're also wiring MCP servers — see `MCP_INTEGRATION.md` first).

## 11. Code path index

Single jump-list of every file:line cited above. As of v0.8.16:

| What | Where |
|---|---|
| Operator yaml `Config` struct + top-level keys | `internal/config/config.go:22` |
| `ModelRef` (alias map value type) | `internal/config/config.go:154` |
| `UserTier` overlay struct | `internal/config/config.go:177` |
| Alias expansion (`ResolveAgentDefModel`) | `internal/config/config.go:1366–1390` |
| Agent .md / yaml merge logic | `internal/config/config.go:1531–1612` |
| `system_prompt` / `system_prompt_file` mutual-exclusion clear | `internal/config/config.go:1564` |
| Pin XOR Tier validation | `internal/config/config.go:1985` |
| Frontmatter struct (every accepted field) | `internal/agents/loader.go:199` |
| `tools` vs `tools` precedence | `internal/agents/loader.go:295` |
| Resolver entry — `Resolve(req)` | `internal/resolve/matrix.go:281` |
| `priorityFor` intersection logic | `internal/resolve/matrix.go:440` |
| `resolvePin` (pin path) | `internal/resolve/matrix.go:293` |
