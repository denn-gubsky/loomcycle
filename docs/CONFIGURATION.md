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

The resolver is `Resolve` in `internal/resolve/matrix.go` (`Resolve(req AgentRequest) (Decision, error)`). The precedence is:

```
Given:  AgentRequest{ Name, Tier, PinProvider, PinModel, Providers, Models, UserTier }

   1. PinProvider AND PinModel both set?
      → resolvePin()
        - Looks up the matrix to confirm (provider, model) is reachable.
        - Returns Decision{provider, model} or ErrPinUnavailable.

   2. Only ONE of (PinProvider, PinModel) set?
      → ErrInvalidArgument (half-pin is config error, caught at load too)

   3. Tier required from here on. If Tier == "":
      → ErrInvalidArgument

   4. Build the candidate list (candidatesFor):
      a. agent.Models[tier] set?  use it. (per-agent override; full replacement)
      b. user_tier.Tiers[tier] set?  use it. (overlay)
      c. library tiers[tier] set?  use it.
      d. otherwise →                ErrTierUnavailable

   5. Build the provider walk order (priorityFor):
      a. agent.Providers AND user_tier.ProviderPriority both set?
         → intersection in agent-order
         → empty intersection → ErrTierAgentNotAvailable  (policy refusal)
      b. only agent.Providers set?               → agent.Providers
      c. only user_tier.ProviderPriority set?    → user_tier order
      d. neither set?                            → library provider_priority

   6. Walk the provider order; for each provider, take its candidates in
      list order, skipping any pair whose:
      - provider is excluded (no API key) OR
      - provider is unreachable (probe failed) OR
      - model is stalled (recent driver error) OR
      - model is rate-limited (until the limit window passes) OR
      - model is not listed by the provider's /v1/models (or equivalent)

   7. First survivor →  Decision{provider, model, effort, ...}
      No survivor    →  ErrTierUnavailable
```

Two error classes worth remembering, because they have different operator semantics:

| Error | When | Caller should |
|---|---|---|
| `ErrTierUnavailable` | Matrix-side problem — every candidate stalled / unreachable | Retry with backoff; surface as 503 |
| `ErrTierAgentNotAvailable` | **Policy-side** — agent's `providers:` and user_tier's `provider_priority` have no overlap, or none of the tier's candidates names a provider the user_tier grants | NOT retry — return 403/"upgrade your plan"; the user genuinely doesn't have access |

This distinction matters because a transient outage and "your plan doesn't allow this agent" are operationally different. Loomcycle separates them at the resolver layer so the app server can map them to different HTTP responses.

### Mutual exclusion at config-load

`ValidateRoutingMode` in `internal/config/config.go` enforces **pin XOR tier**. Config-load calls it for every agent:

```go
func ValidateRoutingMode(provider, model, tier string) error {
	if (provider != "" || model != "") && tier != "" {
		return errors.New("cannot set both explicit provider/model pin and tier (pick one)")
	}
	return nil
}
```

If you set both `tier: middle` AND `model: claude-sonnet-4-6` in an agent's frontmatter, loomcycle refuses to start (`agent "<name>": cannot set both explicit provider/model pin and tier (pick one)`). Pick one path.

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
  max_queue_depth: 32
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
    - { provider: deepseek,  model: deepseek-flash }      # cost floor
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

A `tier: low` agent resolves to `deepseek-flash` first. What happens when DeepSeek then returns a retryable error (429, 5xx, a network timeout, a stream-idle deadline) depends on `fallback_on_error`, which is a `user_tiers` setting (§5) and is **off unless you set it**. The yaml above has no `user_tiers:` block, so as written there is no mid-run fallback: the error surfaces to the caller. The candidate order still matters when a run starts — a provider the availability probe has marked unreachable, or one with no API key, is skipped and the run begins on the next candidate.

To get the mid-run cascade, add an overlay that turns it on:

```yaml
user_tiers:
  default:
    fallback_on_error: true
```

With that set, the resolver marks the failed `(provider, model)` pair stalled and re-picks from the same candidate list, this time selecting the next provider (`gemini`). This continues until a candidate succeeds, the candidates run out, or the run has switched providers `max_fallback_attempts` times (default 3); the run then fails with the original provider error.

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

### Per-agent `routing:` — skip a busy provider

The cascade takes the first *available* candidate, however busy its provider is. With a capped local model (`providers.<id>.max_concurrent`), a run can then wait minutes for a slot while a cloud candidate further down the tier sits idle. `routing.queue_aware` lets an agent go past it:

```yaml
agents:
  triage:
    tier: middle
    routing:
      queue_aware: true
      switch_after_ms: 20000   # default 20000
```

When a run of this agent starts (a new run, a session continuation, a sub-agent), each available candidate's estimated wait is checked in cascade order, and one over `switch_after_ms` is passed over for the next. If every available candidate is over, the one with the shortest wait wins. The estimate is the provider's chat lane: nothing while a slot is free; otherwise the calls queued ahead that could take a freed slot (not those held back by their own model's `model_caps`), shared over the lane's slots, times the model's median call time (measured by `timeout_scaling`, which is on by default) — or, for a model at its own `model_caps` cap, the calls queued for that model shared over its cap, slots free or not — plus one call when `max_models_in_flight` is reached without this model. A model with no measured call yet counts as no wait, so it is never passed over on a guess. A pass-over is logged (`queue-aware routing: agent "triage" runs on …, passing over …`).

- After the start, a run moves at most once more, and only when it has lost its place: at a call boundary after its first turn, if it holds no slot of its provider's chat lane (its `affinity_hold_ms` reservation was preempted by `quantum_calls` or expired, or the lane keeps none) and its next call there would wait past `switch_after_ms`, the cascade is resolved again as at a run start, and the run moves if that picks another candidate. A run that still holds its slot never moves — the slot is its warm prompt cache. The move is a provider switch like a fallback: an `EventProviderFallback` with reason `queue_wait`, the reasoning trace stripped from history, and the prompt cache lost on the new provider. It does not happen under `fallback.pin_after_success` once a turn has succeeded, nor to a provider without vision for a run that carries an image (an `EventFallbackSuppressed` instead).
- A resume or a compaction summary resolves the run where it is, and a retune of the model or tier is adopted at the next operator turn as usual; a retuned routing block (below) governs the move above from the next call boundary.
- Tier-routed agents only. Setting it on an agent with a provider/model pin is refused (config load and AgentDef create/fork), and a fork that pins a model drops the block.
- It is part of the agent's content hash, like `sampling`. It is set in yaml or through AgentDef create/fork; agent `.md` frontmatter does not carry it.
- Estimates are per replica, like the lanes they read.

**Per run.** A run may set its own block, on every run-start surface: `routing` on `POST /v1/runs`, `POST /v1/sessions/{id}/messages`, each `POST /v1/runs:batch` child and a configured run's draft; the `routing` argument of the MCP `spawn_run` / `spawn_runs` tools; `RunRequest.routing` / `ContinueRequest.routing` (a `RunRouting` message) over gRPC; `routing` in the TypeScript and Python clients. It works on an agent with no block of its own:

```json
{"agent": "triage", "prompt": "…", "routing": {"queue_aware": true, "switch_after_ms": 15000}}
```

- It replaces the agent's block whole, like `tool_choice`: fields it leaves out take their defaults, not the agent's values. `"routing": {}` turns queue-aware routing off for that run.
- It chooses only among the candidates the definition declares, so it reaches nothing the agent could not. `queue_aware: true` on an agent pinned to a provider or model — or on a run that pins one itself with `model` (or `provider` on an agent without a tier) — is refused with 400 (gRPC `INVALID_ARGUMENT`, an MCP tool error) naming `routing.queue_aware`. A negative `switch_after_ms` is refused the same way.
- It is a per-run override like the others: an agent with `allow_overrides: false`, or a token whose `overrides` policy does not list `routing`, refuses it (403 `override_not_allowed`). The run's spec records it as `queue_routing`, and `GET /v1/runs/{id}/effective-config` reports it under `routing` with source `run`.
- Untrusted trigger surfaces (webhooks, A2A, schedules) cannot set it, as they cannot set the model.
- **A sub-agent of the same definition inherits it**, as it inherits a per-run model, and records it; a child of another definition runs on its own. A child's provider and model are resolved after inheritance, so whatever it inherits — a model, a tier, a routing block — is what it runs on.
- **It can be retuned while the run is going**: `routing` on `POST /v1/runs/{id}/retune` (and in `overrides` on `POST /v1/runs/{id}/input`), `RetuneRunRequest.routing` / `RunInputRequest.routing` over gRPC, the MCP `retune_run` tool, and `retuneRun` / `retune_run` in the TypeScript and Python clients. It replaces the run's block whole (`{}` turns it off), is validated as at start, and is read at the run's next call boundary — it decides whether the run moves after losing its lane slot. The start is not decided again.

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
    # Inherits library tiers. Used when a caller omits user_tier.
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

If you set `user_tiers:` at all, you must include a `default:` entry — config-load fails without it. It's the overlay used when the caller omits `user_tier` from the run request (e.g., legacy callers).

An unrecognized name (`"premium"` when you only defined `"high"`) does **not** fall back to `default`: `POST /v1/runs` refuses it with `400 unknown user_tier "premium"`.

### `fallback_on_error`

Per overlay. When `true`, a retryable provider error (429, 5xx, a network timeout, a stream-idle deadline) triggers fallback to the next candidate. When `false`, the error surfaces directly to the caller.

**The default is `false`.** The loader applies no default, on `default` or on any other overlay, so an overlay that omits the key has no mid-run fallback — and neither does a config with no `user_tiers:` block at all (the §3 and §4 examples as written). Write `fallback_on_error: true` on every overlay that should cascade.

Leave it off on the free tier if you want a strict no-cascade behaviour (the operator doesn't want a free-tier user accidentally falling into a more expensive provider during an outage). Set to `true` everywhere else for resilience.

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
    - { provider: deepseek,     model: deepseek-flash }
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
        - { provider: deepseek,     model: deepseek-flash }
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
        - { provider: deepseek,     model: deepseek-flash }
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

`LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX` controls the window for **all** `ollama-local` models (`LOOMCYCLE_OLLAMA_NUM_CTX` does the same for the hosted `ollama` provider). It is sent as `options.num_ctx`, so it both **caps the window the model loads** and **is what the UI context gauge reports**.

- **Set** → that value is forced for every local model and reported verbatim. Ollama's `/api/ps` is not consulted at all, so this is also the way to override a window you don't want.
- **Unset** → the driver asks Ollama what the model is currently loaded with (`/api/ps`) and **sends that back** as `options.num_ctx`, so the window it reports is the window it requested. Echoing the loaded window is a no-op for a model already running at that size.
- **Unset and the window is unknowable** (model not in VRAM yet, a different model resident, an Ollama too old to report it) → `num_ctx` is omitted, exactly as before, so Ollama still applies the model's Modelfile `num_ctx`; the gauge reports Ollama's documented **4096** default as a floor. One turn later `/api/ps` knows the real window and the case above takes over.

**Per agent — `max_context_tokens`.** The env/provider `num_ctx` above is one window for *every* agent on the registration. Setting `max_context_tokens` on an agent def overrides it for **that agent only** — it is sent as `options.num_ctx` for that agent's runs, winning over both `LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX` and any `providers.<id>.options.num_ctx`. So a fetch-heavy `researcher` can hold 128K while `chat` stays at 16K on the same host without a second provider registration:

```yaml
agents:
  researcher: { model: qwen3.6-local, max_context_tokens: 131072 }
  chat/local: { model: local-medium }   # unset → env/provider window
```

On a **cloud** model the window is fixed by the model, so there `max_context_tokens` cannot enlarge it — it instead caps the agent's *effective* window (a compaction budget the gauge + `autocompact_at_pct` use), clamped to the model's maximum (it can only lower). It is distinct from `max_tokens` (the OUTPUT cap). Caveat: a bigger window costs KV-cache memory, and Ollama keeps a separate resident copy per `(model, num_ctx)`, so mixing windows for one model on one host means duplicate residency.

**Per run.** The same knob is also a per-run override: `max_context_tokens` on `POST /v1/runs` (and on a continuation's `POST /v1/sessions/{id}/messages`), on the gRPC `RunRequest`/`ContinueRequest`, on the MCP `spawn_run`, and on the `@loomcycle/client` (`maxContextTokens`) + Python (`max_context_tokens`) adapters. A per-run value `> 0` wins over the agent's own `max_context_tokens`; `0`/omitted inherits the agent's (which itself falls through to the provider/driver default). It mirrors the per-run `sampling`/`compaction` overrides exactly. Sub-agent spawns and resumed runs deliberately use the agent-def value only (a per-run override is not inherited by children, nor snapshotted). An agent can read the resolved cap for its own run via `Context op=self` (`max_context_tokens`); the *effective* window after the first turn also shows up there as `context.max_tokens`.

> **What changed (and why you may see a different number).** Before this, an unset `NUM_CTX` meant loomcycle *advertised* the `/api/ps` window while sending **no `num_ctx` at all** — it reported a window the request never asked for. That matters beyond the gauge: `autocompact_at_pct` is a percentage **of the advertised window**, so an over-claim both let Ollama truncate the prompt (it drops the overflow silently — no error, just a model answering from a partial view) and kept compaction from firing. The two numbers are now one number.

**When a prompt doesn't fit**, the server log now says so, once per call:

```
ollama: prompt ~7104 tokens exceeds the 4096-token context window (assumed) for model "glm-4.7-flash" on ollama-local — Ollama silently truncates the overflow; raise the window with LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX
```

The size is an estimate (bytes/4) and gates nothing. `(assumed)` means the window is the 4096 floor above, `(loaded)` that it came from `/api/ps`, `(pinned)` that you set it via env/provider, `(request)` that an agent's `max_context_tokens` set it. Watch for this after a model pull or a restart: a truncated prompt otherwise looks like a model that has simply gone stupid.

Pick a value **every** local model you use can handle (it's global), e.g. `131072`. Too low starves long sessions; too high blows up prefill time and VRAM on a slow GPU. ~128K is a sane middle for a 24GB+ card. A model's *training* context (e.g. 256K) is an upper bound, not what you must load — load only what your GPU can prefill in reasonable time.

> **Symptom:** the gauge shows a small window (e.g. 32K) you didn't expect. Almost always `LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX` is pinned low in the launch env — it's global and overrides the Modelfile. A model also stays resident at whatever window it was first loaded with until it unloads/reloads, so a stale low-context load can linger until the next fresh load. A gauge reading exactly **4096** with nothing pinned is the third case above — Ollama couldn't tell us the window, so that is the floor being reported, not a measurement; it resolves itself once the model is loaded.

### Slow models — timeouts + heartbeat

A big prefill can take a long time before the first token. Two budgets (defaults 300s; raise for very large contexts on slow hardware):

- `LOOMCYCLE_OLLAMA_LOCAL_HEADER_TIMEOUT_MS` — **time to first byte** (the prefill). Exceeding it surfaces as `net/http: timeout awaiting response headers`.
- `LOOMCYCLE_OLLAMA_LOCAL_IDLE_TIMEOUT_MS` — max gap **between** streamed tokens.

A slow-but-alive call no longer trips the stale-run sweeper: the loop pulses the run heartbeat throughout a model call, so a long prefill won't be reaped as `heartbeat_timeout`. The HTTP timeouts above remain the authority on a genuinely stuck call.

### Measured model speed (`timeout_scaling`)

How slow is a local model, really? Every wall-clock budget that wraps model work is sized for a fast hosted model, and guessing a local model's speed is how those budgets end up cutting healthy work. So loomcycle measures it: every model call is timed (wall time, time to first token, and — on Ollama — the server's own load / prompt-eval / generation durations), and per **(provider, model)** it keeps a **slowdown** against a reference machine:

```
T_ref    = ttft_ms/1000 + uncached_input / prefill_tps + output / decode_tps
slowdown = (wall − model load − queue wait) / T_ref        # 1 = reference speed
```

On a decode-heavy call the slowdown is just `decode_tps / your model's tok/s`; on a prefill-heavy one (an extractor reading a long transcript) it carries the prefill cost too. Samples are smoothed like a TCP round-trip time, in log space, and clipped to the model's cap; a cold model load and time spent queued behind other calls are recorded but not counted as slowness. On Ollama the queue time is the wall time outside the server's own load / prompt-eval / generation phases, so a call that waited inside Ollama for the runner (two concurrent calls to one model are served one after the other) has the wait counted as queue, not as work. The **multiplier** it implies is `clamp(exp(mean + deviation), 1, cap)` — exactly 1 for a model at or above the reference. The cap is `max_multiplier`, or the model's own `max_multiplier` under `models:` when you set one. Until a model has `min_samples` calls, the multiplier comes from your `decode_tps` / `multiplier` override for it, else the provider's other measured models, else `local_prior` for a local provider (1 for a hosted one).

Each model is tracked under its **concrete** name: a call reported under a `models:` alias (say `local-medium: { provider: ollama-local, model: qwen3.6:latest }`) counts toward the model the alias names (`qwen3.6:latest`), which is what the routing view lists. A `model_pattern` alias is left as reported, since the model it names depends on the live catalog.

**This version only measures.** The slowdown and the would-be multiplier show on `GET /v1/_routing` (a `throughput` block per candidate; the queue-time median is admin-only), on `/metrics` (`loomcycle_model_slowdown`, `loomcycle_model_timeout_multiplier`, `loomcycle_model_decode_tps`, `loomcycle_model_throughput_samples`), and in `Context op=self` (`timeouts`). **No timeout changes.** The estimate lives in memory per replica and is re-seeded at boot from the per-call timing now recorded on the usage ledger (`token_usage.duration_ms` and friends; the latest 200 calls per model from the last 7 days).

```yaml
timeout_scaling:
  mode: measure            # measure (default) | off. `on` is refused: scaling is not available yet.
  reference:
    decode_tps: 100        # output tokens/s that count as slowdown 1
    prefill_tps: 2000      # prompt tokens/s
    ttft_ms: 1000          # fixed first-token cost
  max_multiplier: 8        # ceiling; also clips each sample
  min_samples: 5           # a model's own estimate is used from this many calls
  local_prior: 4           # assumed multiplier for a local provider before that
  models:                  # per model, keyed provider/model or by a `models:` alias
    ollama-local/qwen3.6:latest: { decode_tps: 20 }   # → ×5 until measured
    local-medium: { multiplier: 6 }                   # a `models:` alias works too
    ollama-local/qwen3.8:latest: { max_multiplier: 24 } # its own cap, always
```

A model's `decode_tps` / `multiplier` (at most one) is a starting estimate, used only until the model has `min_samples` calls. Its `max_multiplier` is a **bound**, not an estimate: it replaces the global cap for that model at every sample count — both the clip on each sample and the clamp on the multiplier — so a model you know runs ~20× slower than the reference (a few tokens/s on a small box) can be measured as 20× instead of saturating at the global 8. It must be between 1 and 100, the same ceiling as the global `max_multiplier`, so every scaled budget stays bounded; a `multiplier` beside it must not exceed it. The routing view's `throughput` block and `Context op=self` `timeouts` report the cap in effect as `max_multiplier`. (When timeouts are scaled in a later version, the per-purpose clamps — e.g. a tighter one for scheduler fires — still apply on top of this cap.)

Env overrides (they win over the yaml): `LOOMCYCLE_TIMEOUT_SCALING` (mode), `LOOMCYCLE_TIMEOUT_SCALING_REFERENCE_TPS` (`reference.decode_tps`), `LOOMCYCLE_TIMEOUT_SCALING_MAX_MULTIPLIER`. A change to the block needs a restart (a reload reports it `restart_required`).

### Compaction — tune it for the prefill cost

On a slow model, the prefill cost of a near-full window is what times you out — so compact **early** and keep a **small** verbatim tail. In a per-agent `compaction:` block:

- `autocompact_at_pct: 55` — compact well before the window fills (vs. the 80% default).
- `keep_last_n: 3` — a tool-heavy agent's tail (big file reads) dominates the post-compaction prefill, so fewer kept turns is the real lever.

When the provider reports a window, the loop also caps the kept tail to ~half the window automatically — folding an over-window tail into the summary — so a compaction always actually relieves pressure (it won't "succeed" yet leave you still over the window).

#### `memory_flush` — keep what a compaction throws away

```yaml
agents:
  assistant:
    memory_scopes: [user]        # REQUIRED — see below
    compaction:
      enabled: true
      memory_flush: true
```

A compaction replaces the conversation with a summary and drops the turns behind it. `memory_flush: true` queues those dropped turns for the memory consolidator first, so durable facts are extracted from them on its next pass.

It exists for a **coverage** gap rather than a capability one. Consolidation selects sessions whose runs are *all terminal* — so an interactive agent that compacts repeatedly and parks at `end_turn` instead of finishing is never consolidated from transcript at all. This is what puts those conversations in front of the consolidator. The bundled `chat/*` agents set it for exactly that reason.

It **banks; it does not flush.** Nothing is extracted at compaction time — the span goes on the same queue `Memory op=add infer=true` uses, and facts appear when the consolidator next runs. Recall will not see them immediately.

**`user` must be in `memory_scopes`, or the flag does nothing useful.** Consolidation drains user scopes only, so a span banked elsewhere would be queued and never read. Rather than fail silently, the runtime names it: every compaction marker then carries a `memory_banked.error` saying the agent has no user scope. Same when the run carries no `user_id`.

What gets banked is **dialogue only** — user and assistant text. Tool calls and tool results are filtered out by block type (a user turn that happens to contain JSON is content and survives verbatim), because feeding the extractor the runtime's own scaffolding is measurably what makes it extract scaffolding.

Reading the outcome: the `context_compaction` transcript event gains a `memory_banked` block with the queue-row id and turn count, or the reason nothing was banked. A failure there **never** fails the compaction — keeping the run alive outranks banking — and an oversized span is refused and named rather than silently truncated.

Once consolidated, a fact records where it came from. `Memory op=get` with `include_provenance: true` returns `origin: compaction` plus the originating run/session and an `origin_available` flag — `false` once retention has deleted that chat. The fact itself is never pruned for having a dead origin; see [§ retention](#9b-multi-tenant-authentication-rfc-l) for the chat clocks.

### `context:` — reasoning-recap retention (a lighter-weight alternative to compaction)

Compaction summarizes the *whole* span it drops — including tool results — into one prose blob at a threshold. On a **long-horizon** run against a **weak or local** model that is exactly where quality suffers: a bloated append-only history poisons the model, and a lossy prose summary throws away the structured facts (which value, which file, which result) the next step needs. The `context:` block offers a different retention strategy tuned for that regime — it keeps the recent structured turns *verbatim* and distils only the older, verbose reasoning into a running note:

```yaml
agents:
  long-runner:
    context:
      mode: recap          # "append" (default, unchanged) | "recap"
      keep_last_n: 6        # recent tool_use/tool_result pairs kept VERBATIM
      reasoning: recap      # recap (default) | drop | keep
      recap_max_chars: 512  # bound on the running recap note
      autorecap_at_pct: 80  # recap when used/window crosses this % (50..95)
```

In `mode: recap` the loop, at an iteration boundary once the footprint crosses `autorecap_at_pct`, rebuilds what it FEEDS the model as: the pinned task + a bounded **running progress recap** + the last `keep_last_n` `tool_use`/`tool_result` pairs verbatim. The recap folds forward incrementally each time (the prior recap is rolled into the next), so the fed prompt stays flat — cost grows with the horizon linearly, not quadratically. Choosing `recap` mode turns auto-recap on; it takes precedence over the `compaction:` auto-trigger (the two are mutually exclusive per run). `Context op=compact` (and the manual compact control) trigger a recap instead of a compaction while in this mode.

- `reasoning: drop` — the cheapest policy: drop the evicted span with **no** recap call (keeps only the preamble + the last-N tail). No extra model round-trip.
- `reasoning: keep` — a no-op control (distils nothing); useful only to A/B the recap's contribution.

As with compaction, the **full transcript is always retained** — audit, replay, resume, and memory facts-extraction see the complete trajectory; only the fed window is distilled. The block is content-identifying (a fork that sets it mints a distinct hash), it is a per-run override on `POST /v1/runs` (`context`), and it flows down the spawn tree (a sub-agent inherits the parent's mode; its own def fills any gaps). An agent reads the resolved policy for its run via `Context op=self` (`context_policy`).

> **Which one?** `append` (+ `compaction`) stays the default and is right for most runs. Reach for `mode: recap` on a **local / weak model doing long procedural work**, where a growing prompt measurably degrades answers — that is the regime where keeping recent observations verbatim while recapping old reasoning wins on both cost and quality. On a frontier model at short horizons the two are quality-equivalent, so the motivation there is cost alone.

#### `mode: stateful` — structured execution state

The strongest retention mode. Instead of a history (full, recapped, or otherwise), the model is fed only three things each step: the preamble, a **structured state object** it maintains, and the **latest observation**. It responds with a single `emit_state` call carrying a *patch* to the state and the *next action*:

```yaml
agents:
  procedural-runner:
    context:
      mode: stateful
      state_schema:              # optional — the shape the state (and every patch) must hold
        type: object
        additionalProperties: false
        properties:
          discovered: {type: array, items: {type: string}}
          current_dir: {type: string}
          done: {type: boolean}
      on_invalid_patch: retry    # retry (default) | fail
      max_patch_retries: 2
```

Each step the runtime: validates the patch against `state_schema` (a subset — object with typed properties + `additionalProperties`; `required` is not enforced per-step since the state accretes), merges it into the state with **JSON merge-patch** semantics (a `null` value deletes a key), records the new state on the transcript (a `context_state` event — a full audit of how the state evolved), then runs the named action to produce the next observation. The agent finishes by omitting the action (or `done: true`) and putting its answer in `final`. Because only the state + one observation is ever fed, the prompt stays **flat** and cost is linear in the horizon — the same O(T) property as recap, but the model carries an explicit, typed working memory instead of prose.

- **Actions, not tool calls.** A stateful agent does not call its tools directly — it *names* one in `action: {tool, input}` and the runtime dispatches it. The agent's tool catalog + the state schema are described to it in the preamble. It can read its own state back mid-run by naming `Context` with `op: state`.
- **Invalid patches** are rolled back and retried (the model is shown its rejected `emit_state` and the reason) up to `max_patch_retries`; `on_invalid_patch: fail` ends the run instead. `state_schema` is optional — omit it and patches are accepted unvalidated (useful before a schema is settled).
- **Model-proposed schemas (curation flow).** You don't have to hand-author `state_schema` up front. Run a new domain with no schema (or a rough one) and the model can **propose** one via `emit_state`'s optional `propose_schema` field — the shape it thinks the state should hold. A proposal is **inert**: it is recorded on the run's transcript (a `context_state` event with `proposed_schema`, and on the run result) for you to review, and it does **not** change validation for that or any run. To **adopt** one, fork the agent def with the proposed schema — `AgentDef fork name=<agent> overlay={"context":{"state_schema": …}}`, then promote it — and subsequent runs validate against it. This is the same propose-then-adopt curation as the ontology, reusing loomcycle's versioned agent-def substrate rather than a bespoke store; nothing an agent proposes takes effect until an operator promotes it.
- **When to use it:** long-horizon procedural work whose progress is a well-defined *state* (files discovered, hypotheses tested, a working directory) rather than a conversation. On loomcycle's benchmark it was the top arm at long horizon on a weak local model. It is **not** for open-ended conversation — the state must be a sufficient summary of the task, which a chat is not. It is opt-in per agent, like every other retention mode.
- **Multi-agent hand-off (the biggest win).** When a fan-out parent spawns a stateful sub-agent (via the `Agent` tool's `spawn` / `parallel_spawn`), the parent receives that child's **final state Σ** as the structured result — not its whole transcript. A single spawn folds the Σ (as JSON) into the child's `tool_result`; a `parallel_spawn` carries each child's Σ as a `state` field on its envelope entry (`{"results":[{"agent":…,"ok":true,"state":{…}}]}`). So a research team of N stateful children hands the parent N compact structured results instead of N growing transcripts — turning an O(T²)-per-agent × N blow-up into N independent O(T) streams. Non-stateful (append/recap) children return their text as before. (Team-orchestrator members still hand results up as text today — the `Team` path is a separate follow-on.)
- **Interactive runs, pause and resume.** An interactive stateful run parks at the end of each turn and waits for the operator, exactly like an append run: the turn boundary on the wire is `awaiting_input`, and the run emits a single `done` only when it actually ends. The operator's message reaches the model as the next **observation**, prefixed `operator:` — it is never written into Σ; the model decides what to record. A runtime pause parks a stateful run between steps, and a resumed or restored run starts from its last recorded Σ and the observation it was waiting on (an action's result, or the operator's message) — never from a replayed transcript. A session continuation of a stateful agent starts from the session's last Σ the same way. An action that was mid-flight when the process stopped is reported to the model as interrupted and is **not** re-run. Manual compaction (`POST /v1/runs/{id}/compact`) is refused with 409 `stateful_run`: Σ is bounded by eviction, not by summarising. Not yet wired: turn-cancel (`POST /v1/runs/{id}/cancel` 409s on a stateful run) and a per-turn step budget. The design rationale lives in RFC DH in the doc store.

#### `mode: auto` — let the resolved provider pick

Rather than hand-picking recap vs stateful per deployment, set `mode: auto` and loomcycle resolves it at run start from the **provider the run actually landed on**:

```yaml
agents:
  worker:
    context:
      mode: auto            # local backend → recap · frontier API → stateful
```

A **local** backend (`ollama-local`, `vllm`, `llamacpp`) resolves to **recap** — schema-free, safe for a weaker model; a **frontier** API resolves to **stateful** — which needs reliable structured output. An **interactive** run takes recap regardless of tier — kept deliberately while interactive stateful chats are being proven in the field (stateful is the less forgiving mode for a model that fumbles its structured reply); an explicit `mode: stateful` is honoured for interactive runs. This is opt-in: an agent with no `context` block, or `mode: append`, is unchanged; an explicit `mode: recap` / `mode: stateful` always wins over `auto`. The mode is fixed at run start from the initially-resolved provider (a mid-run provider fallback keeps the chosen mode). So the same agent runs recap when it lands on your local Ollama and structured state when it lands on a frontier model, with one setting.

The per-run `context` override is available on `POST /v1/runs` and on the MCP `spawn_run` (and fan-out `spawn_runs`); the per-agent block reaches every transport via the resolved def.

#### `recall` — fetch back a detail a distillation dropped

Every retention mode above is **lossy**: compaction summarizes the span it drops, recap keeps only a running note, stateful feeds only the state + the latest observation. That is the point — it keeps a long run affordable — but it means a specific value mentioned early (a token, a file path, a number, a decision) can be gone from what the model sees by the time it needs it. Growing the fed context back is the thing these modes exist to avoid.

`recall: true` closes that gap without regrowing the prompt. As a run distils, each evicted span is embedded into a small **run-scoped index**; the agent gains a `Recall` tool that takes a **free-text** question and returns the most similar *original* spans verbatim:

```yaml
agents:
  long-runner:
    context:
      mode: recap
      recall: true          # off by default; harvest evicted spans + grant the Recall tool
```

So distillation now *shrinks* the fed context while *growing* a searchable index of what it dropped — a needle buried past a distillation boundary stays reachable by asking for it. The query is free text (not an id or exact wording) on purpose: models query fluently in plain language but barely reproduce identifiers, which also makes the tool far likelier to actually be used. `Recall` also transparently searches the agent's **durable memory**, so a fact learned in an earlier session may surface through the same call — the model needn't know where a fact lives.

Notes:
- **The `Recall` tool is auto-granted.** Setting `recall: true` adds `Recall` to the run's toolset automatically — you do **not** also have to list it in `tools:`. (An agent's `tools:` allowlist is otherwise default-deny: an empty list grants *no* tools, so without this a recall agent would have the index but no way to query it.) Recall is read-only over the run's own evicted spans and the agent's own memory, so the auto-grant widens the allowlist but not the trust boundary. Needs an embedder (below); with `recall` off, nothing is granted.
- **Needs an embedder.** The index keys and queries on embeddings, so `memory.embedder` must be configured; without one recall is inert — the index is not built and the tool is not granted.
- **Works in every mode** (`append` + `compaction`, `recap`, `stateful`) — it hooks the same distillation boundary each uses.
- **Off is byte-identical.** `recall` unset/false changes nothing; a resumed run does not re-index (it replays its past distillations).
- Available per-agent and as a per-run `context.recall` override on the same surfaces as the rest of the block.

#### `harvest_to_memory` — keep an evicted span's facts across runs

`recall` (above) keeps an evicted span reachable *within the run*. `harvest_to_memory` is its cross-run sibling: as any distillation mode evicts a span, that span is **banked for the memory consolidator**, which extracts its durable facts into the agent's long-term memory — so a fact the agent learned mid-run survives into future runs:

```yaml
agents:
  long-runner:
    memory_scopes: [user]     # required — consolidation drains user scopes
    context:
      mode: recap
      harvest_to_memory: true # off by default
```

It **generalizes `compaction.memory_flush`** (which banks only on L0 compaction) to the other distillation modes — `recap` evicts spans that `memory_flush` never saw, and a long-lived session that never terminates is one the background consolidator's session scan would otherwise skip entirely; banking its evicted spans gets them consolidated regardless.

Why banking (deferred) rather than extracting inline at each boundary: a measurement (the fact-harvest-quality probe) showed that extracting facts from each distillation span *in isolation* loses the coreference-dependent ones — a fact whose subject was named two spans earlier ("my project *Streaky*" … later "*it* hit 500 users") can't be named from the later span alone, so a per-span extractor drops it while the consolidator's whole-transcript extraction keeps it. Banking hands the consolidator that broader context, so no facts are lost to fragmentation, and it adds no model call to the run's own hot path.

Notes:
- **Needs `user` in `memory_scopes` and a `user_id` on the run** — same prerequisite as `memory_flush` (the consolidator only drains user scopes). A misconfigured agent surfaces a non-fatal error rather than silently never harvesting.
- **Only the conversation is banked.** A span's trusted caller text, the model's replies and operator steers reach the consolidator. Untrusted blocks, tool output (WebFetch, HTTP, MCP results) and the recap or summary text the runtime composed do not. In `stateful` mode an evicted span is tool observations and evicted state, so nothing is banked.
- **Deferred, not immediate.** Facts appear after the consolidator's next pass, not mid-run — which is what cross-run harvest wants (within-run immediacy is `recall`'s job).
- **Off is byte-identical**; a resumed run does not re-bank (it replays its past distillations).
- Available per-agent and as a per-run `context.harvest_to_memory` override.

### Interactive local agents

For a terminal you steer turn-by-turn:

- `unbounded_iterations: true` — an interactive run is operator-driven and Cancel-bounded; don't let the 16-iteration runaway guard end a live session (each steer + each end_turn park burns an iteration).
- add `Interruption` to the agent's `tools` — lets the operator answer the agent's questions inline. Listing the tool enables it; the older separate `interruption: { enabled: true }` flag is no longer required (`kinds` / `max_pending` remain optional policy under `interruption:`).
- `max_tokens: 8192` — the local default (4096) truncates large output.

All of this is wired together in [`loomcycle.local-interactive.example.yaml`](../loomcycle.local-interactive.example.yaml).

---

## 6c. Config-driven providers — add a 3rd-party or self-hosted provider

The built-in providers (anthropic / openai / gemini / deepseek / ollama / ollama-local) need no declaration — they ship as an embedded default layer prepended to every config. To add a **3rd-party or self-hosted** provider (vLLM, groq, together, a second Ollama), declare it in a top-level `providers:` map; the id then becomes a valid `provider:` everywhere the patterns above use one — a `models:` alias, a tier candidate, a `provider_priority` entry, an agent pin.

```yaml
# loomcycle.yaml
providers:
  my-vllm:                    # the id you reference in provider:
    driver: openai            # the wire family it speaks (see below)
    base_url: http://vllm.local:8000/v1
    api_key_env: MY_VLLM_KEY  # env-var NAME; OMIT for a keyless endpoint
    max_concurrent: 4         # optional per-provider concurrency cap

models:
  qwen-big: { provider: my-vllm, model: qwen2.5-72b }
tiers:
  high: [qwen-big]            # my-vllm now serves the high tier
```

- **`driver`** picks the compiled-in wire family: `openai` (OpenAI-compatible — groq / together / most self-hosted), `anthropic`, `gemini`, `deepseek`, `ollama`, and the dedicated local drivers **`vllm`** and **`llamacpp`** (both OpenAI-compatible under the hood, with a distinct id so cost rollups + logs read cleanly — default endpoints `http://localhost:8000/v1` and `http://localhost:8080/v1`; keys via `VLLM_API_KEY` / `LLAMACPP_API_KEY` if secured). A typo lists the compiled-in set at `loomcycle validate`.
- **Keyless** endpoints omit `api_key_env` — declaring the provider *is* the opt-in.
- Operator entries **deep-merge over** the built-ins, so you can also override a built-in (re-point `ollama-local`'s `base_url`, add a `max_concurrent`) without restating it. `LOOMCYCLE_NO_DEFAULT_PROVIDERS=1` drops the built-ins and declares only your own.
- **Per-provider stream timeouts in YAML** — `options: { header_timeout_ms: 600000, idle_timeout_ms: 600000 }` on any `providers:` entry tunes that provider's first-token and idle windows (a slow local backend's cold model load can exceed the global 60s/90s default). This is the YAML home for what were the `LOOMCYCLE_OLLAMA_LOCAL_*_TIMEOUT_MS` env knobs; a YAML value wins over the env.
- **Advertise a local model's window.** vLLM (`--max-model-len`) and llama.cpp (`-c`) fix the context window at server launch — there is no per-request `num_ctx` for them. Set `capabilities: { max_context_tokens: <window> }` on the provider entry so loomcycle's compaction budget, the context gauge, and `Context op=self` match the served window (otherwise it advertises the openai default and can over-claim → truncation). The **`local` preset** (`LOOMCYCLE_PRESETS=base,local`) ships `vllm-local` / `llamacpp-local` templates carrying exactly these settings, commented out, to copy into your own config (`providers:` line included) — `loomcycle presets show local`.

**Per-provider concurrency (`max_concurrent`)** caps the model **calls** in flight to one provider, not its runs: a slot is taken just before a call and given back when the response ends, so nothing is held while a run executes a tool, awaits a sub-agent or waits for its operator. Calls over the cap queue inside loomcycle and never time out, and every call is counted — sub-agents, interactive runs, compaction, the LLM gateway. Its point is a **local model on one GPU**: with `ollama-local: { driver: ollama, max_concurrent: 2 }` the host computes two requests at a time and the agents waiting take turns call by call. Leave cloud providers uncapped.

`max_concurrent` is shorthand for the **chat lane**. A provider can cap each kind of model separately and shape the queue:

```yaml
providers:
  ollama-local:
    driver: ollama
    max_models_in_flight: 1          # different models computing at once (0 = unlimited)
    model_switch_max_wait_ms: 30000  # how long a waiting model can be held off (default)
    lanes:
      chat:     { max_concurrent: 4, quantum_calls: 4, affinity_hold_ms: 5000 }
      decision: { max_concurrent: 2 }
      embedder: { max_concurrent: 2, background_share: 0.25 }
```

- **`affinity_hold_ms`** is the lease: a slot stays reserved that long for the **run** that just used it, so its next call lands on a warm prompt cache. The lease is the run's — it ends when the run does, or when the run stays away longer than the hold. `0` (default) = no reservation.
- **`quantum_calls`** is how many consecutive calls a run may make on its reserved slot while others wait (`0` = unlimited).
- **`background_share`** is the least share of a lane's turns given to background work while live runs also wait (`0` = live runs first).
- **`model_caps`** caps the calls in flight of one model inside a lane — `model_caps: { qwen3.6:latest: 1, ornith-1.5:35b: 1 }` beside `max_concurrent: 4` — for a host that runs some models one request at a time whatever its own parallelism says (on Ollama 0.40.3: the qwen35moe / qwen35 models) next to models that run several. A call to a model at its cap waits in loomcycle's queue rather than inside the host, and calls to other models take the free slots. Keys are model names or `models:` aliases of the provider and cover every context size of the model; each cap is `>= 1` and at most the lane's `max_concurrent` ([details](PROVIDERS.md#lanes-a-queue-per-kind-of-model)).
- **Every model call takes a lane**: a run's calls, the listwise reranker and the unit generator the `chat` lane; the `decision:` block and a `kind: decision` reranker the `decision` lane; the embedder the `embedder` lane. Consolidation passes and the operator-run sweeps (re-index, backfill, re-embed, derived units, image descriptions) queue as background work ([details](PROVIDERS.md#lanes-a-queue-per-kind-of-model)).
- **`decision.max_concurrent` and `memory.reranker.max_concurrent` are removed.** A config that still sets either fails to load and names the lane that replaces it. Their built-in bound stays as a floor: while a provider has no lane for them, decisions to it and reranks are each held to 4 in flight. A lane you configure (`max_concurrent` / `lanes` on that provider) replaces the floor, higher or lower.
- **`max_models_in_flight`** stops several models computing at once on a host that cannot take it. For Ollama a model at two different `num_ctx` sizes counts as two. **`models_in_flight_exempt_kinds: [embedder]`** leaves embeddings out of that count, so they do not wait for the chat model; their lane still caps them.
- **`concurrency.provider_queue_depth`** (`LOOMCYCLE_PROVIDER_QUEUE_DEPTH`, default: the global `max_queue_depth`) bounds only requests sent directly, outside any run — the LLM gateway (including the OpenAI-compatible routes), `POST /v1/embeddings`, `POST /v1/_decide` and the image-describe route — which are refused (`provider_concurrency_exhausted`; HTTP 429 on the gateway and `/v1/embeddings`) once that many calls wait on the provider. A run's calls always wait.

Full field reference (dialect, `options`, capability overrides) + the concurrency internals: [`docs/PROVIDERS.md`](PROVIDERS.md#config-driven-providers-providers-map).

---

## 7. Agent `.md` frontmatter reference

Agent files live under `LOOMCYCLE_AGENTS_ROOT` (set in the env file). Each `<name>.md` has YAML frontmatter between `---` delimiters; the body is the system prompt.

### Frontmatter fields

Parsed into the `frontmatter` struct by `parseAgent` in `internal/agents/loader.go`:

| Field | Type | Purpose | Notes |
|---|---|---|---|
| `name` | string | Agent identifier | Defaults to filename minus `.md`. If set, must match the filename. |
| `description` | string | Human summary | Surfaces in operator tooling; not sent to the LLM as part of the prompt. |
| **Model resolution** | | | |
| `provider` | string | Explicit provider pin | XOR with `tier:`. With `model:` forms the pin path. |
| `model` | string | Model alias OR full model ID | Aliases expand via the `models:` map (`ExpandModelAlias` in `internal/config/config.go`). |
| `tier` | string | `low` / `middle` / `high` | XOR with `provider`/`model`. Triggers tier-driven resolution. |
| `providers` | `[]string` | Per-agent provider priority | Full replacement of library `provider_priority` for this agent. |
| `models` | `map[tier][]TierCandidate` | Per-agent tier candidate lists | Full replacement of library `tiers[]` for this agent. |
| `effort` | string | `low` / `medium` / `high` | Reasoning-effort hint; each driver maps it to its provider's thinking control. On Ollama `medium` / `high` send `think: true` and `low` sends `think: false`, so the model must be thinking-capable. |
| `max_tokens` | int | Per-iteration assistant output cap | 0 = provider default. |
| `max_context_tokens` | int | Per-agent context WINDOW | RFC CJ. 0 = unset. Local (Ollama) → `options.num_ctx`, winning over the env/provider window. Cloud → caps the effective window (a compaction budget clamped to the model max — can only lower). Distinct from `max_tokens` (output). Content-identifying. |
| **Tool fields** | | | |
| `tools` | string OR `[]string` | Tool allowlist | A comma-string (the Claude Code shape) or a YAML list. Absent or empty = zero tools. This is the only key read: the older `allowed_tools:` spelling is not recognised and is silently ignored, so a file that still uses it loads with no tools. |
| `skills` | `[]string` | Skill access allowlist (RFC BA) | Pattern allowlist governing which skills the agent may list / use / author (on-demand via the `Skill` tool — NOT bundled into the prompt). Entries are `/`-globs with an optional `+`/`-` sign: `doc/*` allow, `-doc/secret` deny, `-*` deny all. Empty/absent = allow all. The `Skill` tool is auto-added unless `skills: [-*]`. |
| **System prompt** | | | |
| (body) | string | Inline system prompt | Everything after the closing `---` line. |
| `system_prompt_file` | string | External prompt path | Mutually exclusive with body. Useful for sharing prompts across agents. |
| **Capability fields** | | | |
| `memory_scopes` | `[]string` | Memory tool scope gate | `agent` / `user` / `tenant`. Unset = the caller's own data: `user`, plus `tenant` when the run belongs to a tenant and is not isolated. A declared list is used as written, never widened. See "Unset scope gates" below. |
| `memory_quota_bytes` | int | Per-agent memory byte cap | 0 = global default. |
| `channels` | object | Channel tool ACL | `{publish: [...], subscribe: [...]}`. |
| `agent_def_scopes` | `[]string` | AgentDef tool scope | `self` / `descendants` / `named:<name>` / `any`. Empty = default-deny. |
| `evaluation_scopes` | `[]string` | Evaluation tool scope | `submit_self` / `submit_siblings` / `submit_descendants` / `submit_any` / `read_any`. Unset = `[submit_self]`: the agent may evaluate its own run and nothing else. See "Unset scope gates" below. |
| `volume_def_scopes` | `[]string` | VolumeDef tool scope | `any` / `named:<volume>`. Empty = default-deny. Gates create/delete/purge of dynamic volumes; get/list are tenant-scoped reads. See §9d.1. |

**Unset scope gates.** `memory_scopes` and `evaluation_scopes` used to deny everything when empty. They now resolve, at run time, to what the caller already owns: `memory_scopes` to `user` (plus `tenant` for a non-isolated tenant member) and `evaluation_scopes` to `[submit_self]`. Two yaml-only gates follow the same rule: unset `sql_scopes` resolves to `[user]` and unset `history_scope` to `[user]`. The three `user` defaults need a `user_id` on the run; a run without one gets nothing. `agent_def_scopes` and `volume_def_scopes` are unchanged: empty still denies.

To grant none, set the gate to `["-*"]` in a definition overlay (AgentDef create / fork). The config loader does not accept that value: in a `.md` file or the operator yaml it fails config-load as an unknown scope. For a static agent, leave the tool itself (`Memory`, `Evaluation`, `History`) out of `tools:`.

**Not frontmatter keys.** The loader reads only the fields of the `frontmatter` struct and silently ignores any other key. `sampling`, `compaction`, `context`, `tool_choice`, `output_format`, `volumes`, `sql_scopes` and `history_scope` are not in that struct, so in a `.md` file they do nothing. Set them on the agent's entry in the operator yaml `agents:` map (below) or in a definition overlay.

- `sampling` — `temperature` / `top_p` / `top_k` / `frequency_penalty` / `presence_penalty` / `seed` / `stop`. Each driver applies what its provider supports, drops the rest. `temperature: 0.0` is deterministic (≠ unset). Overridable per-run on `/v1/runs` (`sampling`), merged per field (per-run wins). See `Context op=help topic=sampling`. Anthropic drops temperature/top_p when `effort` engages thinking.
- `volumes` — names of top-level `volumes:` entries the agent's file/exec tools may use. Empty = implicitly bound to `[default]`. Confines the agent to exactly the named volumes (does NOT also grant `default`). See §9d.

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
tier: middle
tools:
  - mcp__jobs__getAgentContext
  - mcp__jobs__getApplication
  - mcp__jobs__postApplicationQaAnswers
---
```

Note: write `tools:` once. A comma-string (`tools: Read, Grep`, the Claude Code shape) and a YAML list are read the same way, so one key serves both consumers. A file that carries the key twice does not load: the frontmatter parse fails with `mapping key "tools" already defined`.

**Alias pin with skills** — privacy-sensitive agent locked to sonnet:

```yaml
---
name: cv-rewriter
description: Rewrites CV or Cover Letter text...
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
tier: middle
tools:
  - WebSearch
  - WebFetch
  - mcp__brave-search__brave_web_search
---
```

Mix of built-in tools (WebSearch, WebFetch) and an MCP tool. Tier-driven resolution applies.

### Claude-Code compatibility

The same `.md` file works in both Claude Code and loomcycle. **Claude-Code-honoured fields**: `name`, `description`, `tools`, `model`. **Loomcycle extensions**: `tier`, `models`, `providers`, `effort`, `max_tokens`, `max_context_tokens`, `skills`, `system_prompt_file`, `memory_scopes`, `memory_quota_bytes`, `channels`, `agent_def_scopes`, `evaluation_scopes`. Claude Code ignores unknown keys; loomcycle treats the format as a superset. One `tools:` key serves both: write it as a comma-string (the Claude Code shape) or as a YAML list, and loomcycle reads either. The older `allowed_tools:` key is no longer read — rename it to `tools:`.

### Operator-yaml `agents:` overlay

The operator yaml's `agents:` map can override any frontmatter field at the deployment level. Useful when you want different model resolution per deployment without forking the .md files. Merge logic is `mergeAgentDef` in `internal/config/config.go` (called from `discoverAgents`):

- Scalar fields (string, int): YAML non-zero value wins
- Boolean fields: YAML `true` switches the field on; YAML `false` is the zero value, so an overlay cannot switch off what the .md enabled
- Slice/map fields: YAML `nil` keeps the discovered value; YAML non-nil (even `[]`) is an explicit override
- `system_prompt` and `system_prompt_file` are mutually exclusive — setting one in YAML clears the other from the merged struct

Example overlay:

```yaml
# loomcycle.yaml
agents:
  cv-rewriter:
    # Override the agent's pin for THIS deployment only.
    # The .md says `model: sonnet`; this deployment runs it on another alias.
    model: opus
```

An overlay replaces a value; it cannot clear one. `model: ""` is the zero value, so it reads as "not set" and the discovered `sonnet` stays. That is why a `.md` that pins `model:` cannot be moved onto a tier from yaml: adding `tier: high` leaves both the pin and the tier on the merged definition, and config-load fails with `cannot set both explicit provider/model pin and tier (pick one)`. Change the routing mode in the `.md` itself.

### System-prompt placeholders

A `system_prompt` may contain placeholders the server expands at run start, before the first model call. Two closed-set families; anything outside the set **fails config load** rather than silently rendering nothing. Each family has a no-argument form any definition may use, and an argument form (a second colon) only an operator-written definition may use.

| Placeholder | Expands to |
|---|---|
| `{{tool:Context.tools}}` | the framed result of calling `Context op=tools` — the agent's own resolved tool inventory, one compact line per tool |
| `{{tool:Context.guide}}` | the framed result of `Context op=guide` — a per-tool call digest (the `op` enum, required arguments, and a one-line usage hint) for the agent's resolved tools: *how* to call each one |
| `{{tool:Context.capabilities}}` | the framed result of `Context op=capabilities` — what the deployment supports (memory, documents, search, sandbox, …) plus its numeric limits; carries no secrets or infrastructure detail |
| `{{memory:core_blocks}}` | every attached core memory block's value (auto-appended if blocks are attached and no placeholder is placed) |
| `{{memory:user_info}}` | the operator-authored user-root document + the learned `human` block |
| `{{memory:search_request}}` | an LLM-free retrieval against the run's initial user input |
| `{{memory:consolidation_bands}}` | the deployment's duplicate-detection similarity bands |
| `{{tool:WebFetch:<url>}}` | the framed body of that page, fetched once at run start — **operator-written definitions only**, host must be on `http_host_allowlist` |
| `{{tool:WebSearch:<query>}}` | the framed results of that search — **operator-written definitions only** |
| `{{memory:key:<key>}}` | one stored memory entry, under the run's own scope — **operator-written definitions only** |
| `{{memory:search:<query>}}` | the top matches for that query, under the run's own scope — **operator-written definitions only** |

```yaml
agents:
  assistant:
    tools: [Read, Write, Grep, WebSearch, Memory]
    system_prompt: |
      You are a helpful assistant.

      ## Tools
      {{tool:Context.tools}}

      Choosing among them:
      - Files: Grep to locate first, then Read.
```

**`{{tool:<Tool>.<op>}}` is limited to an allowlist of read-only calls — `Context.tools`, `Context.guide`, `Context.capabilities`.** Prompt assembly runs at every run entry, sub-agent spawn and resume, so a placeholder naming a mutating tool would write on each one, and one naming `Agent` would spawn during its own parent's assembly. A runtime-authored agent's system prompt is model-writable, so what may be called from a prompt is deliberately not model-chosen. `{{tool:Bash.run}}` is a boot error that lists what is allowed.

**The ARGUMENT forms — a second colon — are gated on who WROTE the definition.** `{{tool:WebFetch:<url>}}`, `{{tool:WebSearch:<query>}}`, `{{memory:key:<key>}}` and `{{memory:search:<query>}}` are available to a definition an **operator** wrote (static YAML, or one authored through an operator's own session — `operator_authored` on the row). A definition an agent wrote may use every no-argument placeholder above; its argument forms render nothing and the refusal is logged with the reason. Existing definitions are unaffected: the gate covers only the argument forms, so nothing that worked before stops working on upgrade. `{{tool:Bash:...}}` is a boot error, like the no-argument form.

**In a TeamDef node's `system` / `input` templates the author is the TEAM**, so it is `operator_authored` on the *teamdefs* row that decides — not the agent the node dispatches to. The two are independent: an operator-written team may use the argument forms while dispatching to an agent-written agent, and a team an agent wrote may not, whichever agent it names. A team node's prompt is also the only place `${...}` variables resolve, which makes it the only path where a *parameterised* fetch can happen — and the one the host allowlist below is really guarding.

**A fetch is bounded twice more.** The argument may contain `${...}` so a fetch can be parameterised from workflow variables — but a resolved URL's **host must be on the operator's static `http_host_allowlist`**, so a variable can never aim the runtime at a host the operator did not list; an unlisted host is refused and the refusal names it. And the network work is bounded by one 5-second budget for the whole assembly and **fails soft**: a refused, unreachable, slow or empty page renders nothing and the run proceeds. Write the prompt so it still reads correctly when the section is absent. (`WebSearch` takes a query, not a target, so the host allowlist does not apply to it.)

Use them in place of a hand-written tool list, which cannot be kept in sync with the `tools:` list beside it — the bundled `chat/*` agents had drifted to naming 12 of their 17 tools. Keep the *guidance* hand-written (which tool to prefer, when to reach for one); let the *inventory*, the *call digest*, and the *capabilities* be generated. Note these add no capability: the full schemas are already sent on every request. They exist because smaller local models under-attend to that array and act as though they have no tools — starting "blind" on which op to call and which fields are required — until asked to check.

**`inject_tool_guide: true`** on an agent delivers the runtime knowledge automatically: prompt assembly appends whichever of `{{tool:Context.capabilities}}` and `{{tool:Context.guide}}` the prompt does not already place (mirroring how `core_blocks` auto-appends). It defaults **off**, so an agent that never sets it is byte-identical to before. The bundled `chat/*` and `doc/manager` agents enable it.

Shared rules: a leading backslash escapes (`\{{tool:Context.tools}}` renders literally); names are case-insensitive; expanded content is framed as reference data and cannot forge its own delimiter; a placeholder appearing *inside* expanded content stays literal; each family has an independent token budget (memory's is `memory_inject_max_tokens`, default 1024, and it also covers the argument forms' content, so a fetched page cannot truncate the tool inventory) so the two never compete on prompt order; expansion is deterministic, so provider prompt-caching still hits. Agents can read the same reference at runtime via `Context op=help topic=system-prompt-placeholders`.

---

## 8. Conflict resolution — what wins when

Single reference table:

| Conflict | Winner | Where enforced |
|---|---|---|
| `tier:` AND (`provider:` / `model:`) both set | **Config-load fails** (an AgentDef create / fork is refused the same way) | `ValidateRoutingMode` in `internal/config/config.go` |
| `tools:` AND the old `allowed_tools:` both set | `tools:` is read; `allowed_tools:` is an unknown key and is ignored, with or without `tools:` | the `frontmatter` struct in `internal/agents/loader.go` (it has no such field) |
| Body AND `system_prompt_file:` both set | **Config-load fails** when one definition ends up with both. A YAML overlay that sets either one clears the other first, so an overlay never trips this | `resolveSystemPromptFiles` (the refusal) and `mergeAgentDef` (the clear) in `internal/config/config.go` |
| Agent `providers:` AND user_tier `provider_priority` both set | **Intersection** (agent-order); empty → `ErrTierAgentNotAvailable` | `priorityFor` in `internal/resolve/matrix.go` (`Resolve` returns the error) |
| Agent `models[tier]:` set | Replaces library `tiers[tier]` AND user_tier `tiers[tier]` for this agent | `candidatesFor` in `internal/resolve/matrix.go` |
| user_tier `tiers[tier]:` set (no agent override) | Replaces library `tiers[tier]` | `candidatesFor` in `internal/resolve/matrix.go` |
| Discovered .md field AND operator-yaml `agents:` overlay both set | YAML non-zero wins; nil slice keeps .md value | `mergeAgentDef` in `internal/config/config.go` |
| `model: sonnet` (alias) AND `models:` map has `sonnet` | Alias expands to `{provider, model}` from the map | `ExpandModelAlias` in `internal/config/config.go` |
| `model: claude-sonnet-4-6` (literal, no alias) | Used as-is as the model ID | `ExpandModelAlias` in `internal/config/config.go` (a non-alias passes through) |
| Probe says provider unreachable | Resolver skips all that provider's candidates | `isAvailableLocked` in `internal/resolve/matrix.go` |
| Provider has no API key set | Marked excluded, treated like unreachable | `runResolveProbeOnce` in `cmd/loomcycle/main.go` calls `SetExcluded` in `internal/resolve/matrix.go` |

---

## 9. Validation + verification

### At config-load

Loomcycle validates the yaml at startup. Common errors:

| Error message | What's wrong |
|---|---|
| `agent "X": cannot set both explicit provider/model pin and tier (pick one)` | `tier:` AND `provider:`/`model:` both present on the same agent |
| `agent "X": no model, no tier, and no defaults.model` | Agent has neither and operator has no `defaults.model` fallback. An agent that declares only capability grants gets a longer form of this message, pointing at a bundle missing from `LOOMCYCLE_PRESETS` / `LOOMCYCLE_CONFIG_DIR`. |
| `user_tiers: a "default" entry is required when the user_tiers block is populated (…)` | You set `user_tiers:` but didn't include `default:` |
| `provider_priority[N]: unknown provider "X" (known: […])` | A `provider_priority` entry doesn't match a registered provider. Per overlay: `user_tiers.<name>.provider_priority[N]: unknown provider "X"`; per agent: `agent "X": providers[N]: unknown provider "X"`. |
| `tiers.<tier>[N]: unknown provider "X"` | A tier candidate names an unregistered provider. Per overlay the path is `user_tiers.<name>.tiers.<tier>[N]`; per agent, `agent "X": models.<tier>[N]`. |
| `models.<alias>: unknown provider "X"` | A `models:` alias names an unregistered provider |

Every message is prefixed `config: `. There is no alias-cycle error: an alias expands once, to the literal `model` it names, so aliases cannot chain.

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

### At runtime — reload changed config (`POST /v1/_config/reload`)

Edited a config file and want it live without a restart (which drops in-flight runs)? `POST /v1/_config/reload` **re-assembles** the layered stack the server booted with (presets → `LOOMCYCLE_CONFIG_DIR` → `LOOMCYCLE_CONFIG_FILES` → `--config`), re-globbing the `loomcycle.*.yaml` section siblings and re-reading `LOOMCYCLE_CONFIG_DIR` fresh — so a config or section file **added or removed** since boot is picked up too — validates it, and applies the sections it can apply live — reporting the rest.

```sh
# Preview the diff without applying:
curl -s -X POST -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  'http://localhost:8787/v1/_config/reload?dry_run=1' | jq .
# Apply:
curl -s -X POST -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  http://localhost:8787/v1/_config/reload | jq .
# → { "dry_run": false, "applied": ["providers"], "restart_required": [] }
```

- **Validate-before-apply.** A candidate that fails to load or validate (a YAML typo, a bad driver) is rejected whole — `422 config_invalid` with the parse/validation error, and the **running config is left untouched** (the server keeps serving). A reload never crashes a live server.
- **Applied live (this build):** `providers`, `models`, `tiers`, `provider_priority`, `user_tiers`, `agents`, `defaults`, `channels`, `concurrency`, `scheduled_runs`. Two mechanisms back them:
  - *Rebuilt in place* — a changed `providers` / `models` / `tiers` / `provider_priority` rebuilds the provider set + resolver policy; a changed `concurrency` retunes the global semaphore caps + per-user cap + the providers' call caps (`max_concurrent` / `lanes` / `max_models_in_flight`); a changed `scheduled_runs` re-materializes the static schedules into the store (the sweeper picks them up next tick). In-flight runs keep the global slot and the provider they already hold; the new values apply to subsequent admissions, and a changed provider cap to the next model call.
  - *Live off the config holder* — `user_tiers` / `agents` / `defaults` (read at run admission) and `channels` defs/scope/ACL are read through an atomic config pointer, so the reload's one swap makes the next run see them.
- **`restart_required` (with reasons):** `memory` — the embedder is captured by-value in many consumers and changing its model/dimension invalidates every stored vector (use the `/v1/_memory/reembed` migration, then restart); `skills` — three aliased holders plus runtime skill authoring make a live swap unsafe; the **queue timeout** and **scheduler tick/allowlist** knobs, **heartbeat-channel timing**, the **listen address**, and the **store DSN**. Host allowlists are **ENV-only** (`LOOMCYCLE_HTTP_HOST_ALLOWLIST`), so a YAML reload can't change them at all. Also: a static `scheduled_runs` entry **removed** from YAML keeps its store row (add/change reload; remove needs a restart).
- Admin-token-gated. The response's `applied` / `restart_required` are the top-level config sections that changed since the last successful reload (or boot).

Every run's SSE stream includes an early event carrying the resolved pair. Smoke run:

```sh
curl -N -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"agent":"<your-agent>","user_tier":"free","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hello"}]}]}' \
  http://localhost:8787/v1/runs
```

The first `event: started` (or `event: resolved`) frame carries `provider=...` and `model=...`. Confirms your config picked what you expected.

The model is also persisted at run start, so `GET /v1/users/{user_id}/agents` shows it during the run, not just at completion.

### `GET /v1/config`

The out-of-band twin of the in-band `Context op=capabilities`: what this instance is and what it can do, without starting a run. Both share one probe (`internal/capabilities`), so they cannot report different answers to the same question.

```sh
curl -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" http://localhost:8787/v1/config
```

It returns the build identity, the feature matrix, the **live** provider/model cascade flattened and deduplicated across plans (`active` = at least one tier would route there now; `selected` = it is what actually runs), and the search-provider cascade.

Three disclosure levels, named in the response as `view` so a consumer can tell "this deployment has none" from "you weren't shown them":

| | `public` | `authenticated` | `admin` |
|---|---|---|---|
| version, features, providers, models, search | ✅ | ✅ | ✅ |
| commit, build_time, `url`, `limits`, `user_tiers` | — | ✅ | ✅ |
| `features.storage` (backend kind) | — | — | ✅ |

**Publishing it.** Set `LOOMCYCLE_PUBLIC_CONFIG=1` to allow a **bearer-less** read, which returns the `public` view — intended for a hosted instance whose landing page shows which providers and models are live. Default OFF, so upgrading never makes a deployment's inventory world-readable without asking. A presented-but-**invalid** bearer still `401`s: a bad credential is an error, not a request for less detail.

The public view never carries a provider error string, a plan name, a request ceiling, the storage backend, or any URL. Its `features` are reduced to plain booleans by reading only each capability's `available` field — a whitelist by **shape**, so a field added to the capability probe later cannot reach a public reader without someone choosing to expose it.

Notes: `active`/`selected` come from the resolver's in-memory snapshot, so a read costs no upstream probe. Under `LOOMCYCLE_OPERATOR_KEY_RESTRICTION` a non-admin (and a public) caller sees only providers it could key itself, exactly as on `/v1/_routing`. `/v1/_routing` remains the per-caller operator view, with `last_error` and the full per-tier cascade.

---

## 9b. Multi-tenant authentication (RFC L)

By default every authenticated caller presents the single shared `LOOMCYCLE_AUTH_TOKEN` — correct for one operator or a single trusted team. For a team or small-VPS service fronting users who don't trust each other's claims, **OperatorTokenDef** issues per-principal bearer tokens, each bound to an **authoritative `(tenant, subject, scopes)`** that the middleware resolves *from the token* and stamps over the wire `tenant_id`/`user_id`. The token's `subject` becomes the run's `user_id` (the fairness key); its `tenant_id` is the memory-isolation boundary.

```sh
# Mint a per-developer token (shown once). Needs an existing admin bearer.
loomcycle operator-token create --name alice --tenant acme --subject alice \
  --scopes runs:create,runs:read
loomcycle operator-token rotate --name alice   # zero-downtime roll (grace window)
loomcycle operator-token retire --name alice   # immediate revoke
```

Migrate the existing shared secret in place — it keeps working as an admin token after the legacy fallback disables:

```sh
loomcycle operator-token create --name ops --tenant default --subject ops --copy-from-env
```

`create` requires `--name` and `--tenant`. It also requires `--scopes`, except with `--copy-from-env`: binding the existing shared secret already says the token is admin. A `create` with no scope list is refused rather than minting an admin token.

Config knobs (full reference: `loomcycle context help operator-tokens` or the `Context.help operator-tokens` tool topic):

| Env var | Purpose |
|---|---|
| `LOOMCYCLE_OPERATOR_TOKEN_PEPPER` | Mixed into the token hash, so someone who can write the database cannot insert a row for a token of their choosing. Optional: unset, a pepper is derived from `LOOMCYCLE_SECRET_KEY` (HKDF-SHA256). With neither, tokens are hashed with no pepper and the server warns at boot when auth is on. Changing an explicit pepper invalidates every token. See [Token pepper](#token-pepper). |
| `LOOMCYCLE_AUTH_CACHE_TTL_SECONDS` | Per-replica resolution-cache TTL (default 30; `0` = direct lookup, immediate revocation). Worst-case revocation lag if a cross-replica invalidation is dropped. |
| `LOOMCYCLE_OPERATOR_TOKEN_ROTATION_GRACE_SECONDS` | Default rotation grace window (default 24h). |
| `LOOMCYCLE_USER_TOKEN_MAX_TTL_SECONDS` | The longest lifetime a per-user token may be minted with. Unset = no cap. When set, a mint asking for more is refused, and a mint asking for no lifetime gets this one. See [Token lifetime](#token-lifetime). |
| `LOOMCYCLE_RUN_RATE_PER_MINUTE` / `LOOMCYCLE_RUN_RATE_BURST` | A default rate for run starts, for a caller whose token carries none. Unset = no default. Operators are not bound by it. See [Limiting how often a token starts runs](#limiting-how-often-a-token-starts-runs). |
| `LOOMCYCLE_EXPIRED_TOKEN_RETENTION_SECONDS` | How long an expired token's row is kept before it is deleted (default 7 days; `0` keeps expired rows). Only rows that carry an expiry are ever deleted. |
| `LOOMCYCLE_LIMITS_ENFORCE` | `strict` makes a hard token budget also stop runs in flight and refuse sub-agents. Unset = checked only when a run starts. See [Strict token budgets](#strict-token-budgets). |
| `LOOMCYCLE_AUDIT_LOG_PATH` | JSONL audit of every create/rotate/retire (never a token or hash). |
| `LOOMCYCLE_AUTH_VERBOSE` | `1` logs a server-side reason on a rejected bearer (the wire 401 stays opaque). |
| `LOOMCYCLE_OPERATOR_KEY_RESTRICTION` | **RFC AX** deployment gate (default OFF). When `1`, a run whose principal lacks `providers:operator-key` may not use the operator's host provider key — resolution routes it only to providers the tenant can key itself (an RFC AR CredentialDef) and refuses `403 operator_key_restricted` if none; the LLM-gateway + embeddings shims refuse a restricted principal outright. OFF ⇒ byte-identical for every existing token. |
| `LOOMCYCLE_PUBLIC_CONFIG` | Deployment gate (default OFF). When `1`, `GET /v1/config` may be read with **no bearer**, returning a narrowed public view — for a hosted instance whose landing page shows which providers and models are live. OFF ⇒ the endpoint requires auth like every other `/v1` read, so upgrading changes nothing. A presented-but-**invalid** bearer still `401`s rather than downgrading to the public view. See [§ GET /v1/config](#get-v1config). |
| `LOOMCYCLE_ALLOW_OPEN_MODE` | `1` lets the server start in **open mode** on a listener beyond loopback. Open mode is no `LOOMCYCLE_AUTH_TOKEN` and no admin operator token, so every route, token minting included, is unauthenticated. Without this, such a boot **exits with an error** when `LOOMCYCLE_LISTEN_ADDR` or `LOOMCYCLE_GRPC_ADDR` binds anything but `127.0.0.0/8`, `::1` or `localhost` (`:8787`, `0.0.0.0` and `::` bind every interface; any other hostname counts as public too). Open mode on loopback needs no opt-in. While open, the server logs a warning every 10 minutes and the Web UI shows an "unauthenticated" banner. Use it only for a throwaway test harness on a private network; anything reachable should set `LOOMCYCLE_AUTH_TOKEN`. |

Routes enforce a scope from a closed catalog; an under-scoped token gets `403` with the JSON body `{"code":"insufficient_scope","error":"insufficient scope","required_scope":"…"}` and the same scope in `WWW-Authenticate: Bearer scope="…"`. The legacy `LOOMCYCLE_AUTH_TOKEN` is disabled only once an admin-scoped token exists (the no-lockout gate). The catalog:

| Scope | Grants |
|---|---|
| `substrate:admin` | **Superuser** — satisfies every scope, incl. token minting, runtime admin (pause/resume/snapshot), and **cross-tenant** focus. ⚠️ **Never implicit.** A mint with no `scopes` used to default here; it is now refused, because that made a mistyped key hand out a full-power token — and minting one also disables the legacy `LOOMCYCLE_AUTH_TOKEN` login, so the accident could lock a deployment out of its own API. Ask for it by name. |
| `substrate:tenant` | **Tenant operator (RFC AF/AG)** — FULL power WITHIN the token's own tenant: runs, channels, authoring all 8 substrate Def families (incl. `_mcpserverdef`, the dynamic-MCP-ingestion surface), registering tool-use hooks, and opening a **tenant-confined** loomcycle-as-MCP-server session (`/v1/_mcp`, RFC AG) — but NOT the operator plane (no minting, no runtime admin, no cross-tenant access). Lets a self-provisioning tenant author its own surface without admin. |
| `runs:create` / `runs:read` | Create/continue runs · read runs, agents, sessions. |
| `channel:publish` / `channel:read` | Publish/ack · subscribe/peek on the per-user + system channel surface. |
| `runs:delete` | Delete a run tree: a run, the runs it spawned and what is stored about them (see [Deleting a run tree](#deleting-a-run-tree)). **Tenant-implied** (`substrate:tenant` and `substrate:admin` already have it). **Not** implied by `substrate:user`, and not among the scopes a member's delegated token gets: grant it by name when the token is minted. |
| `providers:operator-key` | **RFC AX** — permits a run to fall back to the operator's HOST provider key. **Tenant-implied** (`substrate:tenant` and `substrate:admin` already have it), and **inert unless** `LOOMCYCLE_OPERATOR_KEY_RESTRICTION=1`. To make a tenant pay its own way: set the gate, mint that tenant's principals with granular scopes that OMIT this one (e.g. `runs:create,runs:read`), and give the tenant its own key (an RFC AR CredentialDef). |
| `llm:gateway` | Permits the raw model endpoints: `/v1/_llm/chat`, `/v1/chat/completions` and `/v1/embeddings`. They reach a model with no agent in the way and trust the provider and model the caller names. **Tenant-implied**, and a non-isolated member is admitted without it, so every token that could use them before still can — **except an isolated user's** (`substrate:user`), which gets `403` unless the scope is granted by name when the token is minted. Since v1.112.0; before it, any authenticated token could call them. From the same version a chat call through them is refused at the caller's hard token limit and written to the usage ledger, as a run's calls are (an embeddings call is budget-checked; it reports no token count to record). |

`substrate:tenant` satisfies the within-tenant scopes (`runs:*`, `channel:*`, and the def/hook gate) but never `substrate:admin` — so a tenant operator passes the def + hook routes yet is refused minting/runtime-admin. Mint one with `--scopes substrate:tenant`. Confinement is automatic: a non-admin principal's def writes are stamped with its authoritative tenant, cross-tenant reads return an opaque `404`, and a tenant-registered hook fires only on that tenant's runs.

**Operator authority is never exercised from inside a run.** What the definition tools reserve for the operator — a `${NAME}` in an MCP server definition (and, with it, the MCP egress-guard exemption), a free `api_key_env` on a remote memory backend or document source, a `$cred:` webhook secret, `user_bearer` or hook signing secret the author's tenant does not hold yet, a trigger whose runs execute in another tenant, a tenant-less consolidation fan-out, a schedule that resolves `$cred:` in another user's scope — belongs to a `substrate:admin` principal, or the operator of an unauthenticated (open-mode / stdio) surface, calling from **outside a run**: the HTTP and gRPC substrate routes, the Web UI, an MCP session's own tool calls. A run started with an admin token carries the admin's principal into its model's tool calls, but those calls are the model's, so a definition tool reached from inside a run checks them like any tenant author's — a run `spawn_run` starts from an admin MCP session included. (The tools that write these definitions are given only to the operator surfaces today, not to agents; the rule keeps that safe if one is ever wired into a run.)

On the **loomcycle-as-MCP-server transport** (`/v1/_mcp`, RFC AG) a `substrate:tenant` bearer may now OPEN a session — but the route scope decides only that. Inside the session a per-tool gate still withholds the admin-only meta-tools (token minting, runtime admin, snapshot capture/restore, cross-scope channel listing): they are hidden from the tenant's `tools/list` and refused on `tools/call`. The tools a tenant *can* call (def authoring, run lifecycle, memory/channel/path/document, hooks) are tenant-confined exactly like their HTTP twins. `substrate:admin` sees + may call the full meta-tool set. The thin client (`loomcycle mcp --upstream`) forwards the caller's bearer verbatim, so a tenant bearer there drives a tenant-confined session — see `docs/MCP_SERVER.md`.

### Token pepper

A token row stores `SHA-256(pepper ‖ token)`, never the token. The pepper is what stops someone with write access to the database — but not to the server's environment — from inserting a row for a token they chose. The effective pepper is, in order:

1. `LOOMCYCLE_OPERATOR_TOKEN_PEPPER`, if set — hashes exactly as before;
2. else one derived from `LOOMCYCLE_SECRET_KEY` (`HKDF-SHA256(key, info="loomcycle/operator-token-pepper/v1")`). Its hashes are stored as `p1:<pepper fingerprint>:<HMAC-SHA256(pepper, token)>`, so a row on any other form can be told apart;
3. else none, with a boot warning when auth is configured. `loomcycle doctor` reports which one applies.

**Upgrading needs no token reissue.** The first time the server starts with `LOOMCYCLE_SECRET_KEY` set (and no explicit pepper), before it serves, it rewraps every token row still on the unpeppered hash: the stored `SHA-256(token)` becomes `w1:<pepper fingerprint>:<HMAC-SHA256(pepper, "w1" 0x00 ‖ that hash's hex text)>`, which the lookup recomputes from the presented token. Every existing token keeps logging in, and its first login moves the row onto the `p1:` form. The boot log lists the rewrapped rows by `def_id` — a `def_id` you never minted is a row someone wrote into the database. An unpeppered hash never authenticates on its own: only the rows present at that first boot are rewrapped. The first boot records that it ran (a marker row, plus the derived-pepper rows themselves), and from then on an unpeppered row is refused at every boot and logged with its `def_id` as one the server did not write — also on a deployment that had no tokens yet. Two replicas starting together rewrap each row once.

A key rotation works the same way at login: with the old key in `LOOMCYCLE_SECRET_KEY_PREVIOUS`, a token on either form under the pepper it derived still logs in and is moved onto the new one. Those rows cannot be rewrapped at boot (that needs the token); the boot log counts how many are left, so you know when `LOOMCYCLE_SECRET_KEY_PREVIOUS` can go.

**What this does not stop:** someone who can write the database, deletes that marker row and every derived-pepper token row (logging out every token holder), then writes an unpeppered row for a token of their choosing, has the next boot rewrap it. Someone who can only insert rows cannot. Without the pepper they cannot write a row that authenticates any other way.

**Upgrade every replica together, and plan a rollback as a re-mint.** A release without the derived pepper cannot read the `p1:` form, and a release before the rewrap cannot read `w1:`. In a mixed-version cluster, a rewrapped token gets `401` from every old replica, and a token minted on an old replica without `LOOMCYCLE_SECRET_KEY` after a new replica has started is refused by the new ones; upgrade all replicas at once. Rolling back to such a release, or removing `LOOMCYCLE_SECRET_KEY`, logs out every token minted, rewrapped or rehashed since; plan to re-mint them.

**What still invalidates tokens:** changing or removing an explicit pepper; setting an explicit pepper on a deployment that used a derived one; removing `LOOMCYCLE_SECRET_KEY` when the pepper is derived from it; and dropping `LOOMCYCLE_SECRET_KEY_PREVIOUS` before every token has logged in once under the new key.

### Deleting a run tree

**Since v1.112.0.** A run can be deleted together with every run it spawned. Before this, stored run content could be removed only for a whole user (`POST /v1/_erasure`) or by age (the retention sweeper).

| Route | Deletes |
|---|---|
| `DELETE /v1/runs/{run_id}/tree` | The run and every descendant run. |
| `DELETE /v1/sessions/{session_id}?cascade=children` | Every run of the session, each with its descendants. `cascade=children` is required. |

`DELETE /v1/runs/{run_id}` without `/tree` is a different route: it discards a configured run that never started.

| Stored | What happens |
|---|---|
| The runs and their events (the transcript) | Deleted. |
| A session | Deleted when none of its runs is left. A session that was continued holds several runs; it stays while one remains. |
| Interrupts and evaluations of the deleted runs, queued memory rows from them | Deleted. |
| Indexed conversation turns (the trace index) | Deleted. |
| The usage ledger (tokens, model, cost) | **Kept.** It holds no content. |

- **Who may call it.** The caller needs the `runs:delete` scope and must be able to read the run. An admin and a tenant operator have the scope. A member token and an isolated user do not, unless it was granted when the token was minted. A token limited to certain agents cannot delete at all.
- **A live tree is refused.** If any run in the tree is still running, awaiting input or paused, the answer is `409` with code `run_tree_live` and the id of that run, and nothing is deleted. Cancel it first.
- **Unknown is not an error.** A run that does not exist, was already deleted, or is not the caller's to read answers `200` with every count at zero. So a repeated call is safe, and the answer does not reveal whether a run exists.
- **The answer says what it could not reach.** `not_reached` names where a copy may remain: stored snapshots taken while a run was paused, export files a sweep already wrote, facts that memory consolidation derived from the sessions, and — when the tree was started by another run — that run's transcript, which holds the prompt it sent and the answer it got back (`ancestor_transcripts`; delete from that run to remove them). Each has a count where one is cheap to compute.
- **One call deletes at most 10,000 runs.** A larger tree is refused with `run_tree_too_large`; delete its sub-trees first.
- **Each call is recorded** in the audit log when `LOOMCYCLE_AUDIT_LOG_PATH` is set: who called, which root, and the row counts. Never content. Without an audit path the delete still runs and is written to the process log.

The same operation is the `DeleteRunTree` RPC, the `delete_run_tree` MCP tool, `deleteRunTree` / `deleteSession` in the TypeScript client and `delete_run_tree` in the Python client.

```bash
curl -X DELETE -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8787/v1/runs/r_4f2a/tree
```

```json
{
  "run_id": "r_4f2a",
  "deleted_runs": 3,
  "counts": {"runs": 3, "events": 41, "sessions": 3, "session_embeddings": 0,
             "interrupts": 0, "evaluations": 1, "memory_pending": 0, "trace_index": 2},
  "kept": {"usage_ledger": "kept by design: ..."},
  "not_reached": [
    {"name": "snapshots", "count": 0, "reason": "..."},
    {"name": "retention_exports", "reason": "..."},
    {"name": "derived_facts", "count": 4, "reason": "..."}
  ]
}
```

### Token lifetime

**Since v1.112.0.** A token can be given a fixed lifetime when it is minted. It stops authenticating when the lifetime runs out, with the same `401` a retired token gets. This is for a token that lives somewhere you do not control, such as a developer's machine: a copy that leaks stops working on its own.

| Where | How |
|---|---|
| A per-user token | `POST /v1/_users/{subject}/tokens` with a body: `{"ttl_seconds": 86400}` |
| An operator token | `loomcycle operator-token create … --ttl-seconds N`, or `ttl_seconds` on the `create` op |

The per-user mint takes three optional fields. With no body it behaves as before.

| Field | Meaning |
|---|---|
| `ttl_seconds` | The token's lifetime. The answer carries `expires_at`. |
| `name` | A label for the token (`[a-zA-Z0-9_-]{1,62}`). It is stored as `u-<name>`: every per-user token's name carries that prefix, so one can never take an operator token's name. Names behind `u-` are not unique across tenants: `operator-token create` refuses them, and `rotate` / `retire` reach such a token by `--def-id`, not by `--name`. Default: `u-<subject>`. |
| `max_active` | Keep at most this many of the user's tokens alive. After the mint, the oldest beyond the count are retired at once, and the answer lists them in `retired`. |

```sh
curl -X POST "$LOOMCYCLE_BASE_URL/v1/_users/alice/tokens" \
  -H "Authorization: Bearer $TENANT_OPERATOR_TOKEN" \
  -d '{"ttl_seconds": 86400, "max_active": 2}'
```

Rules worth knowing:

- **A rotation does not extend a lifetime.** The replacement token expires when the original would have. A token that has already expired cannot be rotated; mint a new one.
- **A token cannot mint one that outlives it.** When the token doing the mint has a lifetime of its own, a longer `ttl_seconds` is refused, and a mint that asks for none ends when the minting token does.
- **Nor one less restricted than itself.** A minted token — a per-user token, or an operator token from `create` — carries the stricter of each restriction of the minting token and of the request: `retention: none` if either has it, the lower `rate`, only the `overrides` both allow, only the `agents` both list (none in common is refused). A `rotate` of a token less restricted than the rotating token in any field is refused (the caller would get its secret), and never narrows the token, which would take reach from its owner. A minter with no restrictions (an unrestricted admin, the legacy token, open mode) changes nothing.
- **A token holding `substrate:admin` cannot have a lifetime.** One admin token turns the legacy `LOOMCYCLE_AUTH_TOKEN` login off, so an admin token that expires would lock the deployment out on a date.
- **An unknown field in the mint body is refused** (`400`). A misspelled `ttl_seconds` that was silently dropped would hand back a token that never expires. Before v1.112.0 the body was ignored entirely.
- **Expiry holds through the resolution cache.** A token stops at its own end, not up to `LOOMCYCLE_AUTH_CACHE_TTL_SECONDS` later. The same now applies to a rotated-out token at the end of its grace window.
- **Expired rows are cleaned up.** A token's row is deleted `LOOMCYCLE_EXPIRED_TOKEN_RETENTION_SECONDS` after it expired (default 7 days). A token minted without a lifetime is never deleted this way.
- **Mints and revocations of per-user tokens are written to the audit log** when `LOOMCYCLE_AUDIT_LOG_PATH` is set, as operator-token changes already are.

Token listings (`GET /v1/_users/{subject}/tokens`, `operator-token show` / `list`) show `expires_at`, and `active` is false once a token has expired.

### Limiting a token to certain agents

**Since v1.112.0.** A token can be limited to starting particular agents. This is for a token held by a client you do not control: it states the token's one purpose on the token itself, so it holds on every transport and not only behind a proxy route.

| Where | How |
|---|---|
| A per-user token | `POST /v1/_users/{subject}/tokens` with `{"agents": ["score"]}` |
| An operator token | `loomcycle operator-token create … --agents score,review`, or `agents` on the `create` op |
| A declared principal | `agents: [score]` under the principal in `principals:` |

A token with an `agents` list:

- **Starts only the listed agents** as top-level runs. Any other agent is refused with `403 agent_not_allowed`, whether or not it exists. This covers `POST /v1/runs`, `runs:batch`, continuing a session, a configured run, a session replay, and the gRPC and MCP equivalents.
- **Is limited to the run endpoints.** It can start, continue, read and control runs, and is refused everywhere else with `403 restricted_token`, whatever its scopes.
- **Does not limit what a listed agent spawns.** A sub-agent follows the parent agent's own definition. A token for an orchestrating agent reaches the agent that does the work this way.

| A restricted token may use | It is refused |
|---|---|
| `/v1/runs`, `/v1/runs:batch`, `/v1/runs/{id}/…` | every definition route (`/v1/_agentdef`, `_skilldef`, `_teamdef`, `_scheduledef`, …) |
| `/v1/sessions/{id}/…` | starting a team walk; firing or pausing a schedule |
| `/v1/agents/{agent_id}` and its `cancel` | the model gateways (`/v1/_llm/chat`, `/v1/chat/completions`, `/v1/embeddings`) and `/v1/_decide` |
| `GET /v1/users/{id}/agents`, `…/agents/stream`, `…/interrupts` | channels, memory, documents, paths, usage, the credential store |
| `GET /v1/_runnable-agents` (lists only its agents), `GET /v1/_me`, `GET /v1/config` | A2A: such a token is not accepted as an A2A peer |
| `/v1/_mcp` (run tools only) | anything added later, until it is listed as a run endpoint |

Rules worth knowing:

- **The list is fixed at mint.** A rotation carries it over unchanged.
- **An empty list is refused.** Omit `agents` for a token with no limit.
- **A token holding `substrate:admin` cannot have a list.** It could no longer administer, and would still switch the legacy login off.
- **A run started by a schedule, a webhook or a channel hook is not gated.** It runs under the definition's own identity; there is no token there to limit.
- **A token whose stored restrictions this version cannot read is refused at login.** This matters after a downgrade: a newer version may have written a restriction this one does not know how to enforce.
- `GET /v1/_me`, token listings and `Context op=self` show a token's `restrictions`.

### Locking a token's per-run settings

**Since v1.112.0.** A token can be locked out of the per-run overrides, so every run it starts uses its agent's own settings. Use it where results must be comparable, or cost predictable, whoever holds the token: a caller cannot pick another model, a lower effort, a different temperature or a higher limit.

| Where | How |
|---|---|
| A per-user token | `POST /v1/_users/{subject}/tokens` with `{"overrides": "none"}` |
| An operator token | `loomcycle operator-token create … --overrides none`, or `overrides` on the `create` op |
| A declared principal | `overrides: none` under the principal in `principals:` |

`"none"` locks every override. A list names the ones the token may still set, for example `{"overrides": ["effort"]}`.

| Group | Locked settings |
|---|---|
| Which model answers | `model`, `provider`, `tier`, `effort`, `user_tier` |
| How much it may spend | `max_tokens`, `max_iterations`, `unbounded_iterations`, `max_concurrent_children`, `run_timeout_seconds` |
| How it is asked | `sampling`, `compaction`, `context`, `max_context_tokens`, `tool_choice`, `output_format`, `web_search_filter`, `retry_attempts`, `memory_inject_max_tokens`, `memory_index_max_bytes`, `inject_tool_guide` |
| How the run is driven | `interactive`, `review`, `review_ttl_seconds`, `interruption`, `hooks`, `tool_hooks` |

Rules worth knowing:

- **A locked setting is refused, not ignored.** The request gets `403 override_not_allowed` naming the field. A dropped override would let a client believe it got the model it asked for.
- **It applies wherever a setting can be set:** a run start, `runs:batch`, a session continuation, a configured run (create, edit and start), and a retune of a run that is going (`POST /v1/runs/{id}/retune`, `/input` with `overrides`), on HTTP, gRPC and MCP.
- **Not locked:** fields that can only narrow a run (`tools`, `allowed_hosts`), bound it (`max_wall_seconds`), or name and deliver it (`agent_id`, `idempotency_key`, `metadata`, `user_bearer`, `user_credentials`).
- **A sub-agent runs on its own definition.** A locked request carries no overrides for a child to inherit.
- **The lock is fixed at mint** and carried over by a rotation.
- It combines with an agent allowlist: `{"agents": ["score"], "overrides": "none"}` is a token that can start one agent, exactly as defined.

### Locking an agent's per-run settings

**Since v1.112.0.** A token's lock protects one credential. An agent whose settings are meant to be fixed — a judge whose model and effort are part of what it measures — can still be re-tuned by every other caller. `allow_overrides: false` on the agent's definition closes that: every run of the agent uses the definition as written.

```yaml
agents:
  judge:
    tier: middle
    effort: high
    allow_overrides: false
```

It is also a key in a markdown agent's frontmatter and in the `AgentDef` create and fork overlay (`"allow_overrides": false`).

Rules worth knowing:

- **It refuses the same settings a token lock does** (the table above), and for every caller: an admin, the legacy token, a schedule, a webhook and a parent agent included. The request gets `403 override_not_allowed` naming the field.
- **It applies at the same places:** a run start, `runs:batch`, a continuation, a configured run and a retune. In the `Agent` tool it refuses a parent's per-spawn `compaction` override and a resident (interactive) spawn of the agent.
- **A spawn that pins a version (`def_id`) is locked if either the pinned version or the agent's active definition carries the lock.**
- **Not locked:** the fields that only narrow, bound, name or deliver a run, as for a token.
- **Compaction and context settings a parent passes down by inheritance still reach the agent.** Only a parent's explicit per-spawn override is refused. Give the agent its own `compaction:` block if it must not depend on its parent's.
- **A run of the agent that spawns itself passes no overrides down.** A child of the same definition normally inherits its parent's overrides; a locked definition inherits none, including for a parent that started before the lock was added.
- **A schedule or webhook definition that sets an override for this agent fails when it fires**, not when it is saved.
- **To change the agent, change its definition.** A new version (a config change, or an `AgentDef` fork) is the way to re-tune it.
- **A fork can add the lock and cannot remove it.** A fork that does not send `allow_overrides` keeps its parent's; one that sends `true` on a locked parent is refused. The lock comes off where it was put on.
- **A lock written in the server config holds over a tenant's own version of the agent**, including one forked before the lock was added. It is removed in the config and nowhere else.
- **For an agent defined only at runtime the lock belongs to its authors:** a fresh `create`, or promoting an earlier open version, removes it. Limit who may author the agent (`agent_def_scopes`, the `substrate:tenant` scope) if that matters.
- **It is not part of the definition's content hash**, so adding or removing it does not change `content_sha256`. A create that changes only the lock still writes a new version.
- Unset or `true` leaves the overrides to the caller, as before.

#### Asking which settings a run may set

**Since v1.113.0.** A client that builds a run form needs to know which per-run settings it may offer before it sends anything. `GET /v1/_agentdef/{name}/overridable` answers it for the calling token (TypeScript: `getAgentOverridable(name)`):

```json
{
  "agent": "judge",
  "allow_overrides": false,
  "restricted_by_token": false,
  "fields": {
    "model": {"overridable": false, "reason": "agent_locked"},
    "effort": {"overridable": false, "reason": "agent_locked"}
  }
}
```

- **One entry per setting a token lock can name** (the table above), keyed by its run-request field name.
- **The answer is the caller's.** It folds the agent's `allow_overrides` with the token's own `overrides` policy, through the same checks a run is refused with, so it cannot report a setting as open that a run is then refused. `reason` names the lock a run would cite: `token_locked` or `agent_locked` (the token is named when both apply). `restricted_by_token` says the token's policy took part; do not reuse the answer for another token.
- **An open setting may say what the run accepts:** `narrow_only` (`max_concurrent_children`, `interruption` can only be lowered), `max` (the fan-out ceiling), and `values` where the check is against a closed set — the efforts, the configured tiers, and the `providers` and `models` the definition declares. No `values` means any value the deployment accepts (`GET /v1/_models`, `GET /v1/_routing`), not anything at all.
- **It resolves the agent as a run does:** the server config, then the tenant's active `AgentDef` version (`def_id`, absent for an unversioned agent). An unknown agent and another tenant's are the same `404 agent_not_found`. An admin may name another tenant with `?tenant=`.
- **Who may ask:** `substrate:tenant` and a tenant's non-isolated members. A token limited to certain agents may ask about those agents only; any other is `403 agent_not_allowed`, as for a run. An isolated user token may not.

### Limiting how often a token starts runs

**Since v1.112.0.** A token can carry a rate for run starts. A concurrency cap bounds how many runs a caller has going at once; a rate bounds how often it starts one. One run at a time still allows thousands of short runs an hour, and each start costs the server an authentication, a session row and a run row even when the run then does nothing.

| Where | How |
|---|---|
| A per-user token | `POST /v1/_users/{subject}/tokens` with `{"rate": {"per_minute": 6, "burst": 20}}` |
| An operator token | `loomcycle operator-token create … --rate-per-minute 6 --rate-burst 20`, or `rate` on the `create` op |
| A declared principal | `rate: {per_minute: 6, burst: 20}` under the principal in `principals:` |
| Every other caller | `LOOMCYCLE_RUN_RATE_PER_MINUTE` (and `LOOMCYCLE_RUN_RATE_BURST`): a default for a token that carries no rate |

`per_minute` is the steady rate. `burst` is how many starts are allowed at once from idle; it defaults to `per_minute`. The example allows a client that was offline to catch up with 20 runs, then one every ten seconds.

Rules worth knowing:

- **A start past the rate is refused with `429 rate_limited` and a `Retry-After` header** (gRPC `ResourceExhausted`). Nothing is read from the request and no row is written.
- **What counts as a start:** `POST /v1/runs` (a draft included), each child of `runs:batch`, a session continuation, a session replay, starting a configured run, a direct decision, a team walk started directly, and their gRPC and MCP equivalents. Reads, steering and retunes do not count.
- **What does not count:** a sub-agent, a call an agent makes from inside its run, and a run started by a schedule, a webhook or a channel hook.
- **The rate is per user, not per token.** Two tokens of one user share one bucket, so minting a new token does not refill it.
- **The server default does not bind operators.** An admin, a tenant operator and the legacy token are not limited by `LOOMCYCLE_RUN_RATE_PER_MINUTE`. A rate set on such a token by name still applies.
- **It is per replica.** With N replicas a caller's effective rate is up to N times the setting.

### Strict token budgets

**Since v1.112.0.** A hard token budget (`PUT /v1/_limits`) is checked when a run starts: a caller over its budget is refused a new run with `429 token_limit_exceeded`. A run that is already going is not stopped, and neither are the sub-agents it spawns, so one long run can spend well past the budget. Setting `LOOMCYCLE_LIMITS_ENFORCE=strict` closes that:

| Where the budget is checked | Default | `strict` |
|---|---|---|
| A new run, a continuation, a team walk, a direct decision, a gateway or embeddings call | yes | yes |
| Before each model call of a run in flight | no | yes |
| Before a run starts a sub-agent | no | yes |
| A paused run being resumed (after a restore or a restart) | no | yes |

Rules worth knowing:

- **A run stopped in flight ends `failed` with `stop_reason` `token_limit`.** Its error is `token_limit_exceeded: …`, on its stream and on its run row. What it had written is kept, and the session can be continued once the budget allows.
- **A refused sub-agent is the parent's tool error**, in the `business` category and not retryable. No row is written for the child. The parent is then stopped at its own next model call.
- **The overshoot is bounded by one model call per run in flight.** The check runs between calls; a call that has started is not cut off.
- **Runs that take no model call are stopped too.** A code agent over its budget stops at its next step.
- **It is the same counter as the default mode**, so the same limits hold: exact on one replica, best effort across several, and a counter that could not be loaded at boot refuses nothing.
- **Not covered:** a compaction summary and the model calls a tool makes on a run's behalf (an evaluation, a decision, memory consolidation) are not checked one by one. The run's own next call is.
- **A gateway call that ends without a usage report is counted by an estimate**, in either mode: the caller hung up, the provider failed first, or it reported zero. The estimate comes from the size of what was sent and returned and errs high. It counts against the budget and survives a restart, but it is not written to the usage ledger, so `/v1/_usage` and cost reports show only what a provider measured.
- **Any value other than `strict` stops the server at boot**, so a misspelled setting cannot leave a deployment unenforced.

### No-trace runs

**Since v1.112.0.** By default loomcycle keeps everything a run says: transcripts are what make resume, continuation, the console and debugging work. A **no-trace run** is for content that has one purpose and one reader. What it says is delivered to whoever is waiting for it and stored nowhere. "Deleted within the hour" depends on a sweeper and a setting; "never written" does not.

The mode is called `retention`, and it has two values: `none`, and the default.

| Set on | How |
|---|---|
| A run | `"retention": "none"` on `POST /v1/runs`, a `runs:batch` child, gRPC `Run` / `SpawnRunBatch`, MCP `spawn_run` / `spawn_runs` |
| An agent | `retention: none` in the yaml, a markdown agent's frontmatter, or the `AgentDef` create/fork overlay |
| A token | `--retention none` on `operator-token create`, `retention` on the `create` op, `{"retention": "none"}` on `POST /v1/_users/{subject}/tokens`, or `retention: none` on a declared principal |
| A parent run | every sub-agent of a no-trace run is a no-trace run |

**The strictest wins, and nothing loosens it.** A request for `default` under a `none` token or agent runs as `none`. A child of a no-trace run is no-trace whatever its own definition says. An unknown value is refused, so a misspelled `none` cannot become a run that stores everything.

**What is kept, and what is not:**

| Kept | Not stored |
|---|---|
| The run and session rows, their links, agent, user, tenant | Prompts, the system prompt, model text, reasoning |
| Status, a stop reason from the runtime's own list, the error's class | Tool arguments and results, a sub-agent's answer |
| Model, provider, token counts, cost, timings; the usage ledger | The final answer, the error's text, a cancel reason somebody typed |
| A marker for every event: its type, time, size, tool name, ids | The run's settings record, a title or summary, trace-index rows, banked turns |

**What is still delivered:** a caller holding the connection gets the full stream and the full error text, and a parent gets its child's answer. An ordinary parent of a no-trace child stores a marker where its transcript would hold that answer (the tool result, and the copy kept to resume it); its model still reads the answer, and what the parent itself says afterwards is the parent's own content. A detached run returns its id and nothing readable afterwards, so its agent must deliver its own result (a tool call, a channel message).

**What a no-trace run cannot do.** Each needs its content stored, so each is refused with `retention_none_unsupported` and not quietly degraded:

- be interactive or held for review (400); a resident sub-agent of one is refused too;
- join an existing session, be continued, replayed, compacted or recapped (409);
- be configured as a draft (409), sent a message or retuned (409);
- ask a person through the `Interruption` tool, or be evaluated;
- start a team walk (403) — also refused for any caller whose token is `none`.
- be a member of a team walk (403): a walk records its members' answers.

**A token whose runs are no-trace** is held to that on every route it can reach, not only where it starts a run: it cannot create or edit a draft, send a message into a run, answer an interrupt, or attach feedback to a review (each `retention_none_unsupported`), and a reason it types with a cancel is dropped while the cancel goes ahead.

Rules worth knowing:

- **Tools that write on purpose are unchanged.** `Memory`, `Channel`, `Document`, `Write` and MCP tools store what the agent tells them to. Give a no-trace agent only the tools it should be able to write with.
- **Hooks are unchanged.** A hook you configured is sent what it was always sent, including the final answer on `run_end`. That is your endpoint and your choice, which is why only an operator may add or change the `hooks` or `tool_hooks` of a runtime-defined no-trace agent (create, fork or promote; the same rule and audit as taking the mode off, recorded as `retention_none_hooks_changed`). The same holds for a HookDef such an agent names without a version (`name`, not `name@3`): creating, forking, promoting, retiring or deleting it changes the hook the agent runs, so it is an operator's, recorded as `retention_none_hookdef_changed`. Only agents of the HookDef's own tenant count. A HookDef in the shared layer — written with no tenant, and what every tenant's definitions fall back to — may only be changed by an operator at all, whatever names it.
- **A fork can add the mode to an agent and cannot remove it.** A mode written in the server config holds over a tenant's own version of the agent. Only an operator (an admin, or a tenant operator, calling from outside a run) may take the mode off a runtime-defined agent — by creating it anew, forking a version from before it was set, or promoting one; a tenant member or an agent inside a run is refused. Each such removal is written to the audit log (`retention_none_removed`, with who, the agent and the tenant) when one is configured, and is refused if that record cannot be written.
- **A pause does not park a no-trace run.** It keeps running, a snapshot leaves it out, and the pause result counts it in `unpausable_runs_count`.
- **A restart loses it.** At boot the no-trace runs that were in flight are failed with stop reason `not_resumable`, and their idempotency keys are released so a retry with the same key starts a new run. A run whose replica never comes back is failed as `heartbeat_timeout` and keeps its key; vary the key per attempt if that matters.
- **Its chat is deleted at its age and never exported** by the retention sweeper.
- **The console shows the run** with its metadata and the line "content not retained".
- **To remove what an ordinary run did store, delete its run tree** (see [Deleting a run tree](#deleting-a-run-tree)).

#### Debugging a no-trace agent: a timed override

An agent whose runs store nothing cannot be debugged from its transcript. An operator can switch storage back on for **one agent, in one tenant, for a limited time**:

```sh
# Store the runs of `judge` for the next two hours.
curl -X PUT "$LOOMCYCLE/v1/_retention/overrides" -H "Authorization: Bearer $TOKEN" \
  -d '{"agent": "judge", "until": "2026-10-12T18:00:00Z"}'

curl "$LOOMCYCLE/v1/_retention/overrides" -H "Authorization: Bearer $TOKEN"      # what is in force
curl -X DELETE "$LOOMCYCLE/v1/_retention/overrides?agent=judge" -H "Authorization: Bearer $TOKEN"   # end it early
```

Runs of that agent started inside the window are ordinary stored runs. When the debugging is done, remove them with a [run-tree delete](#deleting-a-run-tree).

Rules worth knowing:

- **`until` is at most 24 hours ahead.** Setting one for an agent replaces the last. One that has run out is the same as none.
- **It lifts a mode that came from the agent's definition or from the run request.** It never lifts one that a caller's **token** requires: a developer's own runs stay unstored whatever an operator switches on.
- **It does not lift a child's inherited mode.** A sub-agent of a no-trace run stays no-trace; to store a whole tree, set the override on the parent agent (and on each child whose own definition says `none`).
- **Who may set one:** an admin, for any tenant (`"tenant": "…"` in the body, `?tenant=` on the delete and the list) and for its own when it names none — which, with the single `LOOMCYCLE_AUTH_TOKEN`, is the tenant every run is in; a tenant operator, for its own tenant only. A tenant member, an isolated user and a token limited to certain agents may not set, clear or list them.
- **It is audited.** Setting and clearing each write a record (`retention_override_set`, `retention_override_clear`) to `LOOMCYCLE_AUDIT_LOG_PATH` with who, the agent, the tenant and until when. With an audit log configured, an override whose record cannot be written is not set. Both are also written to the process log.
- **Every transport has it.** HTTP as above; gRPC `RetentionOverride` (`op`: `list`, `set`, `clear`); the MCP tool `retention_override`; `listRetentionOverrides` / `setRetentionOverride` / `clearRetentionOverride` in the TypeScript client and `list_retention_overrides` / `set_retention_override` / `clear_retention_override` in the Python one. All of them call one implementation, so who may use it, what it lifts and what is audited are the same everywhere.
- **The console shows a banner** for as long as one is in force, and `GET /v1/_retention` lists them as `no_trace_overrides` to those who may list them (an empty list for anyone else).
- **It is not carried by a snapshot**, and it is checked when a run starts: a run already going keeps the mode it started with.

### Declared principals — static `(tenant, user)` logins (RFC AO)

Beyond the legacy `LOOMCYCLE_AUTH_TOKEN` (one identity, subject `default`) and runtime-minted `OperatorTokenDef` tokens, you can **declare** stable service identities in the YAML:

```yaml
principals:
  marketing:                                   # informational handle (the map key)
    tenant: acme                               # authoritative tenant — required unless the principal is substrate:admin
    subject: marketing                         # authoritative user id (= scope_id for user-scoped tools)
    scopes: [runs:create, runs:read, substrate:tenant]
    token_env: LOOMCYCLE_TOKEN_MARKETING       # env var holding the SECRET (in .env.local)
  ops-admin:
    subject: ops
    scopes: [substrate:admin]                  # admin is EXPLICIT — declared principals aren't admin by default
    token_env: LOOMCYCLE_TOKEN_OPS
```

- **The yaml carries only `token_env` (a name), never the secret.** The bearer value lives in `.env.local` (e.g. `LOOMCYCLE_TOKEN_MARKETING=lct_…`). `token_env` must be `LOOMCYCLE_*`-prefixed (or an allowlisted third-party name) and may **not** name one of loomcycle's own infra secrets (the DSN / pepper / `LOOMCYCLE_AUTH_TOKEN` / upstream MCP token).
- **`tenant` / `subject` / `scopes`** become the resolved `Principal` — authoritative, server-side, never from the wire. `tenant` may be omitted only for a `substrate:admin` principal (whose empty tenant is the all-tenants view); any other principal without one is a config-load error, because list queries read an empty tenant as *every* tenant. `scopes` is validated against the closed catalog above; an empty/missing `scopes` authenticates but is gated out of everything.
- **Resolution order:** minted `OperatorTokenDef` → **declared principal** → legacy token. A token value shared by two declared principals is a config-load error; an empty `token_env` at boot makes that principal **inert** (a startup warning, not an open door).
- **The payoff — alignment by construction.** Use one declared token for **both** the Web UI login (`/ui/login`) and an MCP thin client (`LOOMCYCLE_MCP_UPSTREAM_TOKEN`); both resolve to the same `(tenant, subject)`. Combined with the per-principal `/v1/_mcp` transport (RFC AG), an MCP agent's user-scoped Documents/Memory land under the same user the UI reads — no synthetic-operator mismatch.

**Trigger-spawned runs choose their tenant in the def (RFC N).** An interactive run inherits its tenant from the caller's token, but a scheduler- or webhook-spawned run has no inbound bearer — so the run-execution tenant is declared in the def via `tenant_id:` on a `scheduled_runs:` entry or a `webhooks:` entry. The spawned run then resolves that tenant's agents/skills/MCP and isolates its memory/runs. It is operator-authored def-content (`""` = shared/default). **Security: for webhooks the tenant comes from the static def ONLY — never from the inbound `payload_mapping`** (the attacker-influenceable body must not be able to select another tenant). See `Context.help scheduled-runs` / `Context.help input-webhooks`.

**The webhook def itself is tenant-isolated too (RFC N completion).** Beyond the run-execution `tenant_id` above, every substrate Def's *active pointer* is keyed on `(tenant_id, name)` — a webhook authored by a tenant principal is owned by that tenant and addressed on its own inbound route: `POST /v1/_webhooks/{tenant}/{name}`. The bare-root `POST /v1/_webhooks/{name}` resolves under the shared `""` tenant, so an existing single-tenant deployment (everything `tenant_id=""`) keeps using the unprefixed URL unchanged. A multi-tenant downstream that authors webhooks under a non-empty tenant **must register its delivery URL with the `/{tenant}/` prefix** for inbound deliveries to resolve. (The admin-only triage endpoints — the dry-run `POST /v1/_webhooks/{name}/test`, `GET /v1/_webhooks/{name}/recent-deliveries` and `GET /v1/_webhooks/{name}/delivery-counts` — resolve under `?tenant=` when given, else the bearer's own principal tenant; pass `?tenant=<tenant>` to reach a tenant-owned webhook.)

---

## 9c. Environment files — secrets vs. config (`.env.local` / `.env.insecure`)

loomcycle is configured entirely through environment variables (the `loomcycle.yaml` carries agents + routing; everything operational is an env var). The **recommended** layout splits those vars across **two files** by sensitivity, so the secret-bearing half can be locked down independently of the operational half:

| File | Holds | Git posture | Safe to read/diff/share? |
|---|---|---|---|
| **`.env.local`** | **Secrets** — provider API keys (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, …), the sidecar bearer `LOOMCYCLE_AUTH_TOKEN`, `BRAVE_API_KEY`, the operator-token pepper, and the secret **values** behind any trigger-credential env names. | **git-ignored**, never committed | **No** — surfacing it leaks credentials |
| **`.env.insecure`** | **Non-secret config** — `LOOMCYCLE_LISTEN_ADDR`, `LOOMCYCLE_DATA_DIR`, host allowlists, feature flags (metrics, webhooks, fallback), timeouts, and the trigger-credential allowlist **names** (`LOOMCYCLE_WEBHOOKS_ENV_ALLOWLIST` / `LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST`). (Filesystem sandboxing moved to the YAML `volumes:` block — RFC AH Phase 3 retired the `READ_ROOT`/`WRITE_ROOT`/`BASH_CWD` env vars.) | git-ignored in *this* repo; commit it in your own deployment repo if you wish | **Yes** — nothing here is a secret |

**Why split.** The two halves have different blast radii. `.env.insecure` is the part you want in code review, in a config-management repo, in a teammate's hands when they ask "what's your sandbox set to?" — none of it is dangerous to expose. `.env.local` is the part one accidental `cat` in a screen-share burns. Keeping them in one file forces the whole thing to the secret tier and makes the operational config needlessly hard to share. The split also matches the security rule in `CLAUDE.md` — agents (and Claude Code) must never open `.env.local`, but `.env.insecure` is freely readable.

**The allowlist-name vs. secret-value seam.** A webhook's `signing_secret_env: LOOMCYCLE_GITEA_WEBHOOK_SECRET` names an env var. The **name** is non-secret and is authorized in `.env.insecure` (`LOOMCYCLE_WEBHOOKS_ENV_ALLOWLIST=…`, or auto-allowed when `LOOMCYCLE_*`-prefixed — see §9b / `Context.help input-webhooks`). The **value** — the actual HMAC secret — lives in `.env.local`. This is exactly why the two files are sourced together: the allowlist in one, the secret it authorizes in the other. loomcycle's own secrets (`LOOMCYCLE_AUTH_TOKEN`, `LOOMCYCLE_PG_DSN`, `LOOMCYCLE_SECRET_KEY`, …) can never be a webhook's secret, whatever the allowlist says. A webhook can instead name a stored credential, `auth.secret: "$cred:<name>"`, which needs neither file and rotates without a restart (`docs/CREDENTIALS.md`). For a forge — GitHub, Gitea or GitLab — set up end to end (events, filters, `dedup_key`, a stored secret, the run's bearer), see `docs/FORGE_WEBHOOKS.md`.

**Bootstrap.** Two committed templates carry the full annotated reference:

```sh
cp .env.local.example     .env.local      # then fill in the secrets
cp .env.insecure.example  .env.insecure   # then adjust paths/flags
```

**How they're loaded.** `loomcycle.sh` and `loomcycle-mcp.sh` source **`.env.insecure` first, then `.env.local`** (config first, secrets last — so a stray config line can never shadow a secret) before exec'ing the binary; either file may be absent. Set `LOOMCYCLE_ENV_FILE=<path>` to collapse the pair back to a single explicit file (the pre-split single-file flow). loomcycle itself reads only process env — it does not parse these files — so any supervisor (systemd `EnvironmentFile=`, Docker `--env-file`, a CI secret store) can substitute for the launcher scripts.

**Secrets at rest (F32).** Two mechanisms keep resolved credentials out of the on-disk state:

- **Definition plane — store the reference, resolve at use-time.** A substrate def persists the `${ENV_NAME}` reference, never the expanded value. Webhook defs store `signing_secret_env` / `bearer_token_env` (env-var *names*) or `auth.secret` / `auth.previous_secret` (`$cred:<name>` references, resolved when a delivery arrives and kept 5 seconds per replica), and a dynamic MCP server's `url` / `headers` keep their `${LOOMCYCLE_*}` placeholders in `mcp_server_defs.content` — the secret is resolved only when the pool dials the server (mirroring how a yaml `mcp_servers.*` entry is expanded at config load). `content_sha256` is computed over the reference, so it stays stable when the token rotates. Only an admin (or the open-mode / stdio operator) may store a `${NAME}` in a dynamic MCP server def: a `substrate:tenant` author or an agent uses `${run.credentials.<name>}` or `$cred:<name>`, and a stored version holding a `${NAME}` that an admin did not save (an *unattributed* one: written before this rule, restored from an older snapshot, or hand-edited) is **refused** when the pool would dial it: the refusal and a boot `WARNING` name the def, version, tenant and fields (never the value). To restore one, an admin acting in that tenant re-saves it (`create` with the same overlay mints an attributed version); otherwise retire it. `LOOMCYCLE_MCP_REFUSE_UNATTRIBUTED_ENV=0` is a temporary upgrade escape hatch that dials such versions anyway (with a `WARNING` once per version); unset, `1`, or any unrecognised value refuses.
- **Transcript plane — redact before persisting (`LOOMCYCLE_REDACT_SECRETS`, default ON).** Tool I/O (tool_call inputs + tool_result outputs) is scanned for secret-shaped substrings and masked before it reaches the events store (and thus snapshots + the `/v1/_events` audit API): the exact values of secret-named env vars (`*_KEY` / `*_TOKEN` / `*_SECRET` / `*_AUTH` / `*_PASSWORD` / `*_CREDENTIAL`) become `[redacted:NAME]`, plus conservative heuristics for `Authorization:` headers, `sk-`/`AKIA`/`xox` keys, GitHub tokens (`ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_`, `github_pat_`), GitLab tokens (`glpat-`), and `*_API_KEY=` assignments. The live SSE stream is **not** redacted (the caller already holds the secret). Set `LOOMCYCLE_REDACT_SECRETS=0` to disable. This is defense-in-depth — agents should still pass secrets out-of-band (env / stdin / credential helper), never inline on a cmdline.

**Sub-agent limits.** Bounds on the children an agent starts with the `Agent` tool. Each is per run, not per tree (a child's own children count against the child); unset or `0` means the default.

| Env var | Default | Bounds |
|---|---|---|
| `LOOMCYCLE_MAX_LIVE_CHILDREN_PER_RUN` | 32 | Children one run has alive at once, across every `Agent` op that starts one; a spawn past it is refused. |
| `LOOMCYCLE_MAX_INTERACTIVE_CHILDREN` | 8 | Resident (`op=open`) children one run holds open; `op=open` past it is refused. |
| `LOOMCYCLE_INTERACTIVE_CHILD_IDLE_TTL_MS` | 1800000 (30 min) | Idle time (no send) after which a resident child is reaped. |
| `LOOMCYCLE_RESIDENT_MAX_TURN_SECONDS` | 7200 (2 h) | A resident child whose current turn runs longer is reaped. No unlimited setting. |
| `LOOMCYCLE_AGENT_CHILD_MAX_TIMEOUT_MS` | 0 (no ceiling) | Ceiling on a spawn's `timeout_ms`; a larger value is refused, not clamped. |
| `LOOMCYCLE_AGENT_POLL_WAIT_CAP_MS` | 60000 (60 s) | Longest one `Agent` poll with `wait` `any`/`all` blocks; a larger `wait_ms` is cut to it. |

**Logging.** Log lines go to stderr. Exact values of secret-named env vars are masked in them, as in the transcript plane above.

| Env var | Default | Values |
|---|---|---|
| `LOOMCYCLE_LOG_FORMAT` | `text` | `text`: the familiar `2006/01/02 15:04:05 message` line, with `WARN`/`ERROR`/`DEBUG` before the message and structured fields appended as `key=value`. `json`: one JSON object per line (`time`, `level`, `msg`, fields), plus `run_id`/`tenant`/`agent`/`user` and `trace_id`/`span_id` where the line belongs to a run. An unknown value warns and uses `text`. |
| `LOOMCYCLE_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`; lines below it are dropped. Lines from code not yet converted to levelled logging are never hidden by the level (they are recorded at the configured level, `info` at minimum), so fatal and startup errors always show. An unknown value warns and uses `info`. |

**Shutdown.** On SIGTERM or SIGINT, loomcycle pauses this replica's runs in flight before it exits, so the next boot resumes them instead of the stale-run sweeper failing them. It is the [pause](ARCHITECTURE.md#pause--resume--snapshot-v0817) the operator can request, made local and bounded:

1. New runs are refused (503) and each run in flight parks at its next iteration boundary, its row `pause_state='paused'`. The HTTP and gRPC listeners drain alongside, each for at most 5 s.
2. At three quarters of the bound, the runs not yet parked have their tool calls interrupted: a `Bash` (or Bashbox fallback) command's process group is killed, the call fails with a note that the runtime shut down while it ran, and the run parks at the boundary after it. An interactive or resident run not yet parked has its current turn stopped as well, as the operator's turn cancel stops it, so it parks waiting for its next message even from inside a model call. A run already parked is left alone.
3. Then the background work stops, the counters and traces flush, and the process exits.

The next boot re-dispatches every paused row. In a cluster, every replica also runs that resume pass periodically (at `LOOMCYCLE_REPLICAS_SWEEP_INTERVAL_MS`, one replica at a time), so a replica that was already running picks up the runs a stopped one parked once that one has gone — the case of a rolling update that starts the new replica first. A resumed run reads the interrupted call's failure in its transcript and decides whether to run it again — the call may have done part of its work. The pause is this replica's alone: nothing is written to the cluster's runtime state and nothing is sent to the other replicas, which keep serving, and the restarted process comes up running.

| Env var | Default | Values |
|---|---|---|
| `LOOMCYCLE_SHUTDOWN_PAUSE_TIMEOUT_MS` | 7000 | Longest the shutdown waits for runs to park, interrupt included; capped at 5 min. `0` skips the pause: runs are abandoned to the stale-run sweeper and their commands keep running, as before. |

The drains run alongside the pause, so the default leaves about 3 s of Docker's default 10 s stop grace for the flushes and closes that follow it. That covers the common case; it is not a guarantee, since each of those steps has a longer worst-case timeout of its own. A non-interactive run still waiting on its model call when the bound runs out is not paused — its partial answer is already in the transcript — and is ended by the stale-run sweeper as before; neither is a no-trace run, which has nothing stored to resume from.

What an interrupt does to a run that is waiting rather than working:

- **A fan-out parent** (an `Agent` `parallel_spawn` still collecting its children) keeps its children only with `LOOMCYCLE_RESUME_FANOUT=1`: the parent is then parked where it waits, the interrupt leaves it alone, and the resume reconciles the children's results. Without it the parent never parks mid-spawn, so the interrupt cancels its spawn and, with it, the children, which end cancelled; the resumed parent reads the failed spawn and decides whether to start them again.
- **A blocking tool call** — an `Interruption` waiting for a person's answer, a `Channel` await, an `Agent` wait or poll — is interrupted like any other call and fails. The resumed run may ask again, so a person can see the same interruption twice: the first one, from before the shutdown, is no longer waited on. On slow models, raise the bound **together with** the platform's grace period, leaving a few seconds over it: `stop_grace_period: 40s` in Docker Compose, `terminationGracePeriodSeconds: 40` in Kubernetes, `TimeoutStopSec=40` in systemd, with `LOOMCYCLE_SHUTDOWN_PAUSE_TIMEOUT_MS=30000`.

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

**A static volume does not reach the dynamic root.** No agent needs the dynamic root bound: `VolumeDef` finds it from the config, and an agent reaches a dynamic or ephemeral volume by its own name. When a static volume's root is the dynamic root or a directory above it (say `default: /work` with the dynamic root at `/work/dynamic`), `Read`, `Write`, `Edit`, `NotebookEdit`, `Glob` and `Grep` refuse paths under the dynamic root through that volume, `Glob` and `Grep` skip it when walking, and `Bashbox` sees it as an empty directory (and refuses to run in a volume that *is* the dynamic root). Otherwise such a binding would reach every tenant's dynamic volumes and write into a read-only ephemeral one. The comparison is on real paths, so a symlinked layout is covered. `Bash`, and host commands `Bashbox` runs outside its sandbox, are not confined this way: the Bash tool is not a sandbox, and a host command is a real process.

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
- **Lifetime.** Resolvable by the whole creating run tree (parent + sub-agents). Auto-**purged when the top-level run completes** — an inline run-completion hook fenced-`RemoveAll`s `<dynamic_root>/_ephemeral/<root_run_id>/` and drops the rows. A sub-agent completing never purges (the tree belongs to the top-level run). There is no `delete`/`purge` op for ephemeral volumes — the lifetime *is* the run.
- **Mode, and who gets it.** Every agent of the run tree reaches an ephemeral volume by name and gets the mode it was created with. A sub-agent's own `volumes:` list neither grants nor narrows it. `Context op=self` lists the run's ephemeral volumes with their mode under `ephemeral_volumes`.
- **Read-only, one way — `set_mode`.** `{"op": "set_mode", "name": "work", "mode": "ro"}` makes the volume read-only for the whole run tree at once: the caller, and sub-agents already running, are refused on their next `Write`, `Edit`, `NotebookEdit` or `Bash`; `Read`, `Grep` and `Glob` go on working. Nothing turns it back: `mode: "rw"` is refused. So it only takes access away, and any agent holding the `volume_def_scopes` grant for the name may call it. The mode is stored on the volume's row, so a resumed run keeps it. It also holds across replicas: before a write to an ephemeral volume that the local process believes writable, the row is read, and a volume another replica made read-only is refused here. That is one indexed read per write-type tool call on such a volume; reads cost nothing extra. If the row cannot be read the write is refused. Under `Bashbox` a write to a read-only volume appears to succeed inside one command and is not kept. Not covered: `Bash`, and host commands `Bashbox` runs outside its sandbox (`LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS`), can reach the volume's directory by its absolute path from any read-write volume. Grant neither to an agent that must not change a read-only volume.
- **Read-only for one agent — `ephemeral_volumes: ro`.** An agent whose definition says `ephemeral_volumes: ro` gets every ephemeral volume of its run read-only, whatever mode the volume has, and so does every agent it spawns. Other agents of the run are not affected, and the agent's own `volumes:` bindings keep their modes. It is a key of an agent in the config, of a markdown agent's frontmatter and of the `AgentDef` create and fork overlay. The only value is `ro`: a fork that does not send it keeps its parent's, none can clear it, and a setting in the operator's config holds over a tenant's own version of the agent. A child resumed after a pause keeps what it inherited. Use it for agents that read material others prepared, such as reviewers.
- **From a git repository — `source`.** `{"op":"create","name":"repo","ephemeral":true,"mode":"ro","source":{"git":{"url":"https://github.com/acme/app","commit":"<40-digit sha>","credential":"$cred:repo-acme-app"}}}` fills the new volume with one commit, fetched inside the runtime. No agent holds the token: an agent that ran `git clone` through a shell tool would, and any text it read could ask it to print it. See §9d.2.1.
- **Requires an active run.** `ephemeral: true` is refused outside a run (no root run id) and on a static-name or in-run-duplicate collision. Same `volume_def_scopes` gate as the persistent op.
- **Crash backstop — `LOOMCYCLE_EPHEMERAL_VOLUME_SWEEP_MS`.** A singleton sweeper (default **60s**; `0` disables — the inline purge still runs) periodically purges ephemeral volumes whose owning run is terminal **and not paused/pausing** (a paused run is parked, not crashed — its volumes survive to be reused on resume; a resumed run rehydrates its in-memory set from the persisted rows). Cluster-gated, so one replica per tick does the work. Skipped when no `dynamic_root` is configured.

### 9d.2.1 A volume from a git source

Off unless the operator lists the hosts a repository may be fetched from.

| Setting | Default | Meaning |
|---|---|---|
| `LOOMCYCLE_GIT_SOURCE_HOSTS` | unset: the feature is off | hosts, comma-separated, as `host` or `host:port` |
| `LOOMCYCLE_GIT_SOURCE_MAX_BYTES` | 256 MiB | bytes fetched, and bytes written, per clone. A request's `max_bytes` may lower it |
| `LOOMCYCLE_GIT_SOURCE_MAX_OBJECT_BYTES` | 64 MiB | the largest single object once inflated. **This is what bounds memory**: a pack is compressed, so the bytes fetched do not. A larger object fails the clone |
| `LOOMCYCLE_GIT_SOURCE_MAX_FILE_BYTES` | 5 MiB | the largest file written. A larger one is left out and listed |
| `LOOMCYCLE_GIT_SOURCE_MAX_FILES` | 100000 | files, links and submodule directories per clone |
| `LOOMCYCLE_GIT_SOURCE_TIMEOUT_SECONDS` | 120 | time for one clone, waiting for a slot included |
| `LOOMCYCLE_GIT_SOURCE_MAX_CONCURRENT` | 1 | clones running at once, per replica |
| `LOOMCYCLE_GIT_SOURCE_MAX_HISTORY` | 500 | commits deep a clone looks for its commit, a diff base, or a merge base |

**Who may ask.** An agent needs `VolumeDef` in its tools, the usual grant for the volume's name, and `source:git` in `volume_def_scopes`. `any` does not include `source:git`.

**The credential.** `$cred:<name>` or `$ghapp:<name>`, resolved for the run. It must carry `allowed_urls` covering the repository's URL (`docs/CREDENTIALS.md`): the URL of a clone can come from untrusted text, and the restriction is what stops the token following it. A credential with no `allowed_urls` is refused. The token is sent as HTTP basic auth. The user name is the credential's own `username` when it has one (`docs/CREDENTIALS.md`); otherwise `oauth2` for a host whose name has a `gitlab` label and `x-access-token` for any other. A `$ghapp:` token always goes with `x-access-token`.

**A GitHub App token is narrowed to the clone.** For `$ghapp:<name>` the runtime asks GitHub for an installation token for the one repository in the URL, with read access to its contents and nothing else. GitHub enforces it: the token cannot reach another repository or write. The narrowing never widens: a config that lists its own `repositories` must include the one being cloned, and a config that lists its own `permissions` must allow `contents` (`read` or `write`); one that leaves contents out is refused. The token request goes through the network guard: a `base_url` on a private network (GitHub Enterprise) must be listed in `LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST`. That applies to `$ghapp:` in headers too.

**What is fetched and how.** One commit, by its full sha, over HTTPS only, through the same guard as the HTTP tools (no private or metadata address unless `LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST` vouches for the host). No redirect is followed. Nothing from the repository runs: no hook, no filter, no LFS, no submodule fetch.

**What is in the volume.**

- Regular files, mode `0644`. No file is executable.
- A symbolic link whose target is relative, has `..` only at its start and no more of them than the link is deep. Every other link is left out.
- A submodule is an empty directory.
- `_meta/omitted.json` lists what was left out and why: `symlink`, `submodule`, `too_large`, `unsafe_path`, `collision`.
- No `.git`.

**A diff.** With `"diff_base": "merge-base:default"` (the merge base of the commit and the repository's default branch, as its HEAD names it) or a 40-digit sha, the volume also holds `_meta/diff.patch` and `_meta/numstat.json`, or the names `outputs` gives under `_meta/`. The merge base is found by deepening both histories from 64 commits, doubling, up to `LOOMCYCLE_GIT_SOURCE_MAX_HISTORY`; not found within it, the clone fails with `merge_base_not_found`. The patch describes the tree the volume holds: a change to a path left out of the tree is listed in `numstat.json` with the reason and has no body. Renames are a deletion and an addition. The patch applies with `git apply`; its text is not byte for byte what `git diff` prints. The patch counts against `LOOMCYCLE_GIT_SOURCE_MAX_BYTES` together with the tree.

**The answer** names the commit and gives counts. It carries no file content, and a failure is one word from a closed list; the remote's own text goes to the operator's log, with the repository's path removed.

**All or nothing.** A clone that fails, or passes a limit, leaves no directory, no object store and no row, and the name can be used again.

**Memory.** A clone holds its pack on disk and at most a few objects in memory, each no larger than the object limit. Budget a small multiple of `LOOMCYCLE_GIT_SOURCE_MAX_OBJECT_BYTES` per clone running at once.


### 9d.3 Volumes console tab (RFC AH Phase 4)

The embedded Web UI (`/ui`) has a dedicated **Volumes** tab (admin-gated, alongside Library / Integrations / Channels / Schedules) to view and manage volumes for the operator's tenant. It's a thin client over the HTTP surface — no new runtime behaviour.

- **Persistent.** A flat table of static volumes (read-only — the operator yaml is ground truth, including the `dynamic_root`) plus the tenant's dynamic `VolumeDef`s. Dynamic rows support **Delete** (non-destructive — unmaps the volume, keeps files) and **Purge** (destructive — `RemoveAll`s the directory tree). A **Create** button provisions a dynamic volume by name + mode (the runtime derives the path). A **bound by** column shows which agents bind each volume.
- **Ephemeral.** A read-only table of the tenant's live ephemeral volumes (auto-purged at run completion), polled every ~10s.
- **Purge is type-to-confirm** — the operator must type the volume's name to enable the confirm button, distinct from the one-click Delete. The server-side `RemoveAll` fence (§6 of the RFC: re-derive, EvalSymlinks, strictly-inside, expected-prefix) remains the real guard.
- **Backed by two read-only endpoints** — `GET /v1/_volumes` (merged static + tenant dynamic) and `GET /v1/_volumes/ephemeral` (the tenant's live ephemeral volumes). Both are tenant-scoped from the authoritative principal (gated at `substrate:tenant`; admin satisfies). CRUD reuses `POST /v1/_volumedef`.

### 9d.4 Bashbox memory: `LOOMCYCLE_BASHBOX_OVERLAY_MAX_BYTES`

`Bashbox` honors a read-only volume by sending the command's writes to an in-memory overlay that is thrown away when the call ends; the empty directory that hides a `dynamic_root` inside a read-write volume is in memory too. That memory is loomcycle's own heap, shared with every run on the replica, so one call may write at most **`LOOMCYCLE_BASHBOX_OVERLAY_MAX_BYTES`** bytes **in total** to in-memory files — deleting does not give it back (default **64 MiB**; a value under 1 MiB is raised to 1 MiB, and a non-positive or malformed one keeps the default). A write past the limit fails with `no space left on device`, the result says the limit was reached, and the call fails. Every byte written counts, every new file, directory and link counts about 512 bytes plus its path (what gbash spends on each entry beyond its name), and so does a file of the read-only volume that `chmod`, `chown`, `touch`, `mv`, `ln` or an append copies into memory: its bytes are charged as they are read in, so a copy bigger than what is left of the limit fails instead of being kept, and two names for one file are two copies. Reading the volume without changing it streams from disk and is not counted. Bytes written to a named pipe count too, because gbash buffers a pipe in memory without limit, so a pipeline that streams more than the limit through a FIFO fails. Writes to a read-write volume go to disk and are not counted.

What it does not bound: several calls at once each get their own limit (concurrent calls × the cap, plus gbash's in-memory files costing about 2–2.6× the bytes written), and the read side has its own gbash limits (8 MiB per file read, 32 MiB per shell variable, 256 MiB across variables, 1 MiB per output stream). Renaming a directory of the read-only volume copies the metadata of every entry under it into memory uncounted; that is bounded by the size of the operator's tree. For mutually hostile tenants on one process, also set a memory limit on loomcycle's container.

---

## 9e. Config layering — stacking multiple config files (RFC AN)

`--config` is **repeatable**, and the files are **deep-merged left→right, last
wins** — so you can compose a bundled config with your own without copy-paste:

```sh
loomcycle --config bundles/document-agent/loomcycle.yaml \
          --config ~/.config/loomcycle/loomcycle.yaml
```

Put your authoritative file **LAST**: the bundle contributes its `agents:` (e.g.
`doc/manager`), and your file wins on `provider_priority` / `tiers` / `volumes` /
`defaults` — so the bundle's agent runs on *your* routing and reads *your*
`default` Volume. Containers/systemd can use the env form instead:
`LOOMCYCLE_CONFIG_FILES=base.yaml:override.yaml` (`:`-separated). When both are
set, `LOOMCYCLE_CONFIG_FILES` files layer as the **base** and explicit `--config`
flags layer **after** them (an explicit flag always wins).

**Section-per-file (`loomcycle.*.yaml`).** For a large config, split it by section
into sibling files next to `loomcycle.yaml` and they auto-layer with **no flags or
env** — run `loomcycle` (no `--config`) in the directory and it loads the base
`loomcycle.yaml` **plus every `loomcycle.*.yaml`** beside it, deep-merged in
lexical order after the base:

```
loomcycle.yaml            # defaults, agents — the small "main" file
loomcycle.providers.yaml  # providers: / models: / tiers: / provider_priority:
loomcycle.memory.yaml     # memory:
loomcycle.channels.yaml   # channels:
```

Because it's the same deep-merge, a section file simply *owns* its section (put
`providers:` in `loomcycle.providers.yaml` and leave it out of `loomcycle.yaml`).

**A `loomcycle.yaml` base brings its siblings however it's reached** — by
auto-discovery, by `--config loomcycle.yaml`, or in `LOOMCYCLE_CONFIG_FILES` — so
`loomcycle validate loomcycle.yaml` / `doctor` see the *same* split config the
server runs (no lockstep drift). `LOOMCYCLE_CONFIG_DIR` is unaffected: it already
layers every `*.yaml` in the directory.

This pairs with runtime reload: edit a section file present at boot and
`POST /v1/_config/reload` picks it up. **Adding or removing** a section file after
boot is also picked up on reload — the reload re-globs the `loomcycle.*.yaml`
siblings (and re-reads `LOOMCYCLE_CONFIG_DIR`) fresh each time, so a newly-dropped
section file takes effect and a removed one is dropped, with no restart. (If a
removal leaves the candidate invalid, the reload fails `422` and the running
config is kept, never corrupted.)

Notes: a base `loomcycle.yaml` must exist (it may be minimal); `*.example.yaml`
siblings are ignored (so the shipped `loomcycle.example.yaml` left beside a live
config can't silently override it), but any *other* `loomcycle.*.yaml` in the
directory is loaded — mind dated backups like `loomcycle.2026-08.yaml`.

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

**An empty key replaces.** A key with nothing under it — typically a section whose
entries are all commented out — is a YAML null, not "no change", and by the rule
above it **replaces** what an earlier layer set there: a bare `providers:` in any
layer drops every built-in provider (it shows as `config layer override: providers`).
Comment the key out along with its entries.

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
  matrix as fallback. `bedrock` adds two Amazon Bedrock providers that
  authenticate with AWS credentials for `LOOMCYCLE_BEDROCK_REGION`, and routes
  nothing to them on its own (see "Amazon Bedrock with AWS credentials" in
  [PROVIDERS.md](PROVIDERS.md)).
- **Bundles** — a preset that *also* defines an agent and its skills **inline**
  (the top-level `skills:` map, §7). `document-agent` ships the `doc/manager`
  Document Assistant agent + its four skills — no skills directory, no
  `LOOMCYCLE_SKILLS_ROOT`. A bundle may also ship pure *declarations* rather
  than agents — `system-channels` is one.

### A Starter that fans out over a document

A `starter` state reads its work from its `source`. By default that is one
channel; with `kind: document` it is one document, and its **top-level
sections** — the root's direct children, in document order — are the wave's
items:

```json
{"state": "review", "handler": {"kind": "starter",
  "source": {"kind": "document", "path": "/specs/acme", "scope": "user"},
  "fanout": {"agent": "reviewer", "per": "chunk", "max": 20},
  "prompt": {"input": "Review this section:\n{{starter.message}}"},
  "sink": {"channel": "verdicts"}}}
```

- **`path`** is a fixed absolute Path-tree path; variables are not supported.
- **`scope`** is `user` (default — the tree of the person the walk runs as, so a
  path in another user's tree reads as not found) or `tenant` (needs the same
  `memory_scopes` and `sql_scopes` tenant grants a Document read does).
- **`fanout.per`** is `chunk` (one run per section; `max` required) or `once`
  (one run holding every section).
- The document is **read once, when the wave dispatches**: an edit during the
  wave does not change the items, and a later visit to the state reads it again.
- Each run gets `{document_id, chunk_id, index, title, markdown}` in
  `{{starter.message}}` (`{{starter.messages}}`, an array, for `per: once`). The
  markdown is the section's whole subtree, rendered like `export_md` without
  metadata. It is data: a `{{…}}` written inside a section is never expanded.
  `binds` project from it (`title: $.title`).
- **More sections than `max` fails the walk**, naming both numbers — a document
  has no cursor, so dispatching the first `max` would drop the rest silently.
  Zero sections fails it too.
- `ack`, `source.wait` / `n` / `wait_ms` / `batch` and `per: message` are refused
  at create/fork: a document has no cursor and nothing to wait for.
- Sink, review, hooks, `timeout_ms` and walk stamping behave as for a channel
  Starter.

### Watching a team walk live

A team walk is a run, and its **`run_id` is also its walk id** — the correlation
stamped on the `parent_context` of every agent run the walk spawns. So one
handle answers "which walk is this" everywhere:

```sh
# start it and keep the handle
curl -XPOST …/v1/_teamdef -d '{"op":"run","name":"triage","mode":"detach"}'
# → {"run_id":"r_4325…","status":"running"}

# watch that walk, and its agents appear and finish, live
curl -N "…/v1/users/{user_id}/agents/stream?walk_id=r_4325…"
```

The filtered stream carries the walk's own run, its members and the runs its
members start. The walk's run is
`running` when the walk starts and ends `completed`, `failed` or `cancelled`.
While the walk is paused at a breakpoint — or on an iteration cap it escalated
to a person — its run is `running` with `awaited_state: "interrupted"`
(`awaited_on: "question"`), and a frame without `awaited_state` follows when
the pause ends, however it ends. `GET /v1/runs/{run_id}` reports the same
`awaited_state` while the walk is paused, and the walk's transcript records the
pause as an `Interruption` `ask` call and its result.

The same filter is on the `stream_user_run_states` MCP tool (`walk_id`). The
`stream_open` event echoes `filter_walk_id`, so a mistyped id shows up as a
wrong echo rather than as a workflow that quietly produced nothing.

The stream shows what happens from the moment you subscribe. To read the walk
as it stands — every run it has started so far, finished or not — list it:

```sh
curl "…/v1/runs?walk_id=r_4325…&limit=100"
# → {"agents":[{"run_id":"r_4325…",…}, …], "next_cursor":"run_…"}
curl "…/v1/runs?walk_id=r_4325…&limit=100&cursor=run_…"   # the next page
```

It returns the walk's own run, every member, and every run a member started
(its sub-agents, at any depth), oldest first, in pages of up to
1000 (default 100); `next_cursor` is empty on the last page. Unlike the user
listing (`/v1/users/{user_id}/agents`, 100 rows) it is not capped, and it needs
no client-side filtering. You may list a walk if you may read the walk's own run
(`GET /v1/runs/{run_id}`); otherwise it is the same 404 an unknown walk gets. The
same listing is `list_runs` with `walk_id` over MCP, the `ListWalkRuns` RPC over
gRPC, `listWalkRuns` in the TypeScript client and `list_walk_runs` in Python.

The walk's own row carries its `result`, as `GET /v1/runs/{run_id}` does:
`final_text` is the last state's output, and `terminal` names the end state the
walk reached — absent when it failed or was cancelled — so a team with two ends
(say `published` and `abandoned`) tells you which one it took. Member rows leave
`result` out, like every listing.

#### One run's children

Any run, in a walk or not, can be asked for the runs it started itself:

```sh
curl "…/v1/runs?parent_run_id=r_0d07…&limit=100"
# → {"agents":[{"run_id":"r_2452…","parent_run_id":"r_0d07…",…}, …], "next_cursor":""}

curl -N "…/v1/users/{user_id}/agents/stream?parent_run_id=r_0d07…"
```

The listing returns the run's direct children, the runs whose `parent_run_id`
is that run, of every status, oldest first, paged like the walk listing. The
run itself is not in it, and neither are its grandchildren: ask again with a
child's id for those. You may list a run's children if you may read the run;
otherwise it is the 404 an unknown run gets. Pass `walk_id` or `parent_run_id`,
not both. It is `list_runs` with `parent_run_id` over MCP, the `ListChildRuns`
RPC over gRPC, `listChildRuns` in the TypeScript client and `list_child_runs`
in Python.

The stream filter is the same rule, live: only events whose `parent_run_id` is
that run. The `stream_open` event echoes it as `filter_parent_run_id`. It is on
`stream_user_run_states` over MCP and gRPC (`parent_run_id`), `parentRunId` in
the TypeScript client and `parent_run_id` in Python. With `walk_id` as well, an
event must pass both.

Every run a walk state starts — an agent and its consolidator, each parallel
member, a standalone consolidator, a starter's wave — carries `walk_id` plus
`state` (the state id it ran in) and `state_visit` on its `parent_context`.
`state_visit` is the walk's own ordinal of state visits (1, 2, …): the runs of
one visit share it, and a revisit of the same state — a pushback loop, or a
`continue` on an iteration cap — gets a new one. A starter's wave members also
carry `wave_id` and `wave_index`, so a client can group a fan-out by wave and
order it by position. The walk's own run is the one whose `run_id` equals the
`walk_id`.

A run a member starts with the `Agent` tool, and any run that one starts, is
not a member and still belongs to the walk. It carries the member's `walk_id`,
`state` and `state_visit`, so it is in the walk's listing and on its stream.
Tell the two apart by `parent_run_id`: a member's is the walk's `run_id`, a
sub-agent's is the run that started it. The wave fields and the board fields
stay the member's alone, so a wave's width is the runs its starter dispatched.
A walk a member starts is such a run too: its own run carries the outer walk's
fields, and the runs it starts carry its own `walk_id`.

### Armed subscriptions — a promoted team that runs itself

`LOOMCYCLE_TEAM_SUBSCRIPTIONS=1` (default **off**) drives a **promoted** team
whose entry state is a `starter`: when its source channel has work, the walk
starts, with nobody asking for it.

⚠️ **This is the only part of the runtime that starts agent runs unprompted.**
Turning it on turns the definition plane into a spend commitment — every message
on a subscribed source becomes a wave of agent runs. Arrive at it deliberately.

- **Promote arms, retire disarms.** There is no arming bookkeeping: each tick
  asks the store what is promoted, so a retired, deleted or unpromoted team is
  simply not in the answer — nothing to leak, nothing to reconcile after a
  crash. `TeamDef op=retire` reports `sources_released` so you can see which
  wires the workflow was reading.
- **A Starter that reads a document is never armed.** It has no channel to wake
  on and no cursor to say what is new; promoting it only selects the version
  `op=run` resolves by name.
- **The source must be `scope: tenant` or `scope: global`.** A sweep has no user
  and no agent, so an `agent`- or `user`-scoped source is refused rather than
  guessed at — picking one of its per-user queues would silently drive one and
  let every other accumulate.
- **One replica at a time.** Cluster deployments gate each team behind a
  Postgres advisory lock held for the whole walk, so two replicas never dispatch
  the same wave concurrently. Single-replica needs no coordinator.
- **A failing walk backs off** (exponential, capped at 10 minutes, cleared by a
  success). A failed walk does not ack its source, so without this a poison
  message would drive the same failure every tick.
- `LOOMCYCLE_TEAM_SUBSCRIPTIONS_TICK_SECONDS` (default 15) is the worst-case
  latency between a message landing and its wave starting.

**`system-channels` — the runtime's own `_system/*` channels.** Declares the
five a workflow can actually read, so a Starter can source a clock or an
interruption feed without hand-written yaml:

| channel | kind | publisher |
|---|---|---|
| `_system/heartbeat-1m` / `-5m` / `-1h` | cadence, `period:` | the heartbeat goroutine |
| `_system/interrupts/pending` | event | `Interruption` op=ask |
| `_system/interrupts/resolved` | event | the resolve endpoint + sweeper |

⚠️ **A ticker writes a row every period, on every deployment, forever** — the
1-minute one is 1,440 rows a day whether or not anything reads it. That is why
this is a bundle rather than an always-on default: `LOOMCYCLE_PRESETS=base,system-channels`
opts in, and an install that never selects it starts no heartbeat goroutine and
declares no `_system/` channel. Each ticker's `default_ttl` is **twice its
period** and `max_messages` is 5, so a clock keeps no history — a heartbeat
means *now*, and a long TTL would let a Starter that was down fire immediately
on a stale tick.

Agents can never publish to a `_system/` channel regardless of ACL; the prefix
is reserved for the internal publisher and the admin endpoint. Declare a
consumer, not a producer.

Two names in the runtime's event-driven registry are deliberately **absent**:
`_system/runtime-state` and `_system/provider-events` have no publisher in the
runtime today, so declaring them would ship channels that can only ever be
empty — which reads as a broken feature rather than an unused one. They stay
reserved; wire a publisher before wiring a consumer.

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

So `base` supplies the provider matrix, `document-agent` registers `doc/manager`
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

## 9g. Model kinds and the `decision:` block

**Model kinds.** A `models:` alias can declare what the model is for:

```yaml
models:
  local-medium:    { provider: ollama-local, model: qwen3.8 }                 # kind: chat (the default)
  local-embedding: { provider: ollama-local, model: bge-m3, kind: embedder }
  decide:          { provider: ollama-local, model: nimble, kind: decision }
```

| where an alias is used | kind it must have |
|---|---|
| an agent's `model:`, a tier candidate, the listwise memory reranker, the unit generator | `chat` |
| the `decision:` block, a memory reranker with `kind: decision` | `decision` |
| `memory.embedder.model` | `embedder` |

- A tagged alias in the wrong place **fails config load**; the error names the alias, its kind and the place.
- An **untagged** alias used as a decision model or an embedder still loads, with a warning at boot and in `loomcycle validate` naming the alias and the tag to add. A later release will make this a failure, so tag them now.
- An untagged alias used as a chat model says nothing. A plain model name (not an alias) is never checked.
- `GET /v1/_models` reports a tagged alias's kind. The kind is never a routing input.

**The `decision:` block** lists the decision models a deployment may ask:

```yaml
decision:
  default: decide                 # required when the block is set
  models: [decide, decide-deep]   # optional; omitted = every alias tagged kind: decision, plus the default
  provider: ollama-local          # only for plain model names in the list
  timeout_ms: 30000               # default 30000; one call, not its wait for a slot
```

- The block has no cap of its own. How many decisions are in flight to a provider is that provider's decision lane: `providers.<id>.lanes.decision.max_concurrent`; with no lane set, a built-in floor holds them to 4 in flight. (`decision.max_concurrent` was removed; a config that sets it fails to load.)

- Config load fails on: a `model_pattern` alias, an undeclared provider, a provider with no decision support (only Ollama has it today), a plain model name with no `decision.provider`, an alias tagged another kind, or an unknown key in the block.
- With `models` omitted, every alias tagged `kind: decision` must be buildable, or load fails.
- No block means the capability is off. Changing the block needs a restart.
- The embedded `local` preset (§9f) ships a `local-decide` alias (`ollama-local` / `nimble`) and no block: add `decision: { default: local-decide }` to your own config to turn it on.
- An agent narrows its own list with `agents.<name>.decision: {default, models}`; it can only narrow.

See [`docs/DECISION-MODELS.md`](DECISION-MODELS.md) for the request and answer formats, the four surfaces and billing.

---

## 10. Cross-references

- [`loomcycle.example.yaml`](../loomcycle.example.yaml) — the repo-root reference yaml. All six user_tiers wired, inline comments on every section. Copy-paste and edit.
- [`loomcycle.example.yaml`](../loomcycle.example.yaml) — the comprehensive example config (aliases-first, every tool/feature exercised). Start here.
- [`loomcycle.local-interactive.example.yaml`](../loomcycle.local-interactive.example.yaml) — a focused config for driving interactive agents on local (Ollama) models, with the slow-model knobs from §6b wired together.
- [`.env.local.example`](../.env.local.example) + [`.env.insecure.example`](../.env.insecure.example) — the two env-file templates (secrets vs. non-secret config; see §9c). Every operational env var is documented inline in one or the other.
- [`docs/MCP_INTEGRATION.md`](MCP_INTEGRATION.md) — MCP server configuration (deliberately out of scope for this doc).
- [`docs/ARCHITECTURE.md`](ARCHITECTURE.md) — broader runtime context, provider driver table, probe semantics.
- [`docs/TOOLS.md`](TOOLS.md) — tool policy and built-in tool reference (the `tools` / `tools` axis).
- [`docs/DECISION-MODELS.md`](DECISION-MODELS.md) — decision models: the `kind` tag, the `decision:` block, the request and answer formats.
- [`docs/POSTGRES.md`](POSTGRES.md) — storage backend configuration.
- [`docs/PLAN.md`](PLAN.md) — historical design rationale, including the v0.8.2 `user_tiers` RFC and the precedence design decisions.
- [`examples/observability/`](../examples/observability/) — three drop-in observability profiles (Grafana+Tempo self-hosted / Honeycomb / Datadog) for sending loomcycle's OTEL traces + Prometheus metrics to your existing stack. Five-minute quickstart per profile.
- MCP spec: https://modelcontextprotocol.io (only relevant if you're also wiring MCP servers — see `MCP_INTEGRATION.md` first).

## 11. Code path index

Single jump-list of the resolver, config-merge and frontmatter code that sections 2, 7 and 8 cite by symbol. The symbol name is the stable handle: grep for it. The line numbers were checked against v1.107.0 and move with every commit, so treat them as a starting point.

| What | Symbol | Where (line at v1.107.0) |
|---|---|---|
| Operator yaml top-level keys | `Config` struct | `internal/config/config.go:34` |
| Alias map value type | `ModelRef` struct | `internal/config/config.go:837` |
| User-tier overlay | `UserTier` struct | `internal/config/config.go:1120` |
| Alias expansion | `ExpandModelAlias` | `internal/config/config.go:5948` |
| Pin-path model resolution (calls the alias expansion) | `ResolveAgentDefModel` | `internal/config/config.go:5985` |
| Agent .md discovery + yaml overlay | `discoverAgents` | `internal/config/config.go:6283` |
| Agent .md / yaml merge logic, including the `system_prompt` / `system_prompt_file` clear | `mergeAgentDef` | `internal/config/config.go:6398` |
| `system_prompt` / `system_prompt_file` mutual-exclusion refusal | `resolveSystemPromptFiles` | `internal/config/config.go:6600` |
| Pin XOR Tier validation | `ValidateRoutingMode` | `internal/config/config.go:6932` |
| Frontmatter struct (every accepted field) | `frontmatter` struct | `internal/agents/loader.go:381` |
| Frontmatter parse | `parseAgent` | `internal/agents/loader.go:433` |
| `tools` comma-string / list coercion | `coerceToolsField` | `internal/agents/loader.go:523` |
| Resolver entry | `Resolve` | `internal/resolve/matrix.go:383` |
| Pin path | `resolvePin` | `internal/resolve/matrix.go:534` |
| Tier candidate-list precedence | `candidatesFor` | `internal/resolve/matrix.go:624` |
| Provider-priority intersection | `priorityFor` | `internal/resolve/matrix.go:653` |
| Per-candidate availability check | `isAvailableLocked` | `internal/resolve/matrix.go:710` |
| Excluded-provider marker | `SetExcluded` | `internal/resolve/matrix.go:921` |
| Provider probe sweep (marks keyless providers excluded) | `runResolveProbeOnce` | `cmd/loomcycle/main.go:3981` |
