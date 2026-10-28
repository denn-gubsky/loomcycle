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
| `shell_credentials` | `[]string` | Credentials a shell receives | Keeps only these names of the operator's `LOOMCYCLE_BASH_ALLOWED_CREDS` and `LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_CREDS` for the agent's `Bash` and Bashbox host commands; never adds one, and a `cmd:NAME` binding stays its command's. Unset = every credential the operator lists. `[]` = none. A sub-agent gets only the names both it and its parent allow. A host variable admitted by `LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_ENV` whose name the agent gave up is withheld too. Not per-run overridable. See [CREDENTIALS.md](CREDENTIALS.md), "Into a shell". |
| `git_remote` | `[]string` | Git tool network operations | `fetch` (clone, fetch, pull, ls_remote) and `push`; `push` needs `fetch` alongside it, so `[push]` alone is refused, as is any other value. Unset or `[]` = none: the `Git` tool works on local repositories only. A push also needs its host in `LOOMCYCLE_GIT_PUSH_HOSTS` (a boot warning says so when that is empty). A sub-agent may perform an operation only when both it and its parent may. Not per-run overridable. |
| `volume_def_scopes` | `[]string` | VolumeDef tool scope | `any` / `named:<volume>`. Empty = default-deny. Gates create/delete/purge of dynamic volumes; get/list are tenant-scoped reads. See §9d.1. |

**Unset scope gates.** `memory_scopes` and `evaluation_scopes` used to deny everything when empty. They now resolve, at run time, to what the caller already owns: `memory_scopes` to `user` (plus `tenant` for a non-isolated tenant member) and `evaluation_scopes` to `[submit_self]`. Two yaml-only gates follow the same rule: unset `sql_scopes` resolves to `[user]` and unset `history_scope` to `[user]`. The three `user` defaults need a `user_id` on the run; a run without one gets nothing. `agent_def_scopes` and `volume_def_scopes` are unchanged: empty still denies.

To grant none, set the gate to `["-*"]` in a definition overlay (AgentDef create / fork). The config loader does not accept that value: in a `.md` file or the operator yaml it fails config-load as an unknown scope. For a static agent, leave the tool itself (`Memory`, `Evaluation`, `History`) out of `tools:`.

**Not frontmatter keys.** The loader reads only the fields of the `frontmatter` struct and silently ignores any other key. `sampling`, `compaction`, `context`, `tool_choice`, `output_format`, `volumes`, `sql_scopes` and `history_scope` are not in that struct, so in a `.md` file they do nothing. Set them on the agent's entry in the operator yaml `agents:` map (below) or in a definition overlay.

- `sampling` — `temperature` / `top_p` / `top_k` / `frequency_penalty` / `presence_penalty` / `seed` / `stop`. Each driver applies what its provider supports, drops the rest. `temperature: 0.0` is deterministic (≠ unset). Overridable per-run on `/v1/runs` (`sampling`), merged per field (per-run wins). See `Context op=help topic=sampling`. Anthropic drops temperature/top_p when `effort` engages thinking, and drops temperature/top_p/top_k outright on the models that reject non-default sampling (Opus 4.7 and later, Sonnet 5.x, Haiku 5.x, Fable, Mythos — direct or on Bedrock).
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

The same `.md` file works in both Claude Code and loomcycle. **Claude-Code-honoured fields**: `name`, `description`, `tools`, `model`. **Loomcycle extensions**: `tier`, `models`, `providers`, `effort`, `max_tokens`, `max_context_tokens`, `skills`, `system_prompt_file`, `memory_scopes`, `memory_quota_bytes`, `channels`, `agent_def_scopes`, `evaluation_scopes`, `shell_credentials`, `git_remote`. Claude Code ignores unknown keys; loomcycle treats the format as a superset. One `tools:` key serves both: write it as a comma-string (the Claude Code shape) or as a YAML list, and loomcycle reads either. The older `allowed_tools:` key is no longer read — rename it to `tools:`.

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
| `{{identity}}` | the person the run acts for, from its access-control set: display name and status (from their user record in the run's tenant), group names, and the tools their access rules withheld — a snapshot at run start, framed as data, with a runtime line after it when the account is `pending`. Renders nothing when the run acts for nobody (open mode, a trigger with no owner) and in a team node's templates. Values are folded onto one line and bounded (16 groups / 16 tools, then a count), and the block is exempt from `memory_inject_max_tokens` |
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
- **A reload never loosens how the server authenticates.** Both checks read the config alone — a minted admin operator token in the store never counts, because it can be retired right after the reload (from another replica, say). A candidate that would turn auth off (it declares no principals and sets no `LOOMCYCLE_AUTH_TOKEN` while the running config turns auth on) is refused `422 config_invalid`, and so is one that would drop the last **admin credential in config** (no `substrate:admin` principal that can log in and no `LOOMCYCLE_AUTH_TOKEN`, while the running config has one). To drop the admin principal of a deployment that will rest on minted admin tokens, restart, or set `LOOMCYCLE_AUTH_TOKEN` in the new config. A dry run reports the same refusals.
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
| `LOOMCYCLE_ACL` | **RFC EY access control rollout mode** — `compat` (default), `shadow` or `enforce`. Under `compat` the scope gates above decide exactly as before and an explicit ACL **deny** bound to the caller (by user, group, token or tenant-wide) is honoured on top; under `shadow` the scope gates decide and every divergence from the engine's answer is logged (`authz: acl_shadow_diverged …`) and audited; under `enforce` the engine decides — a question no rule allows is refused (`403 acl_denied`), and a request whose ACL set could not be read is refused with `503 acl_unavailable` rather than admitted. Under `shadow` a run also keeps every grant its agent definition gives, and each narrowing `enforce` would apply at its start (a tool withheld, a volume made read-only or dropped, a memory, SQL-memory or history scope dropped, a memory or SQL-memory scope made read-only, a skill deny appended to the agent's `skills:` patterns, or every skill when no rule allows one) and each `spawn agent:<child>` it would refuse is logged and audited as `acl_shadow_diverged` with the run's `run_id` (`acl_run_id` in the audit record), once per question per run tree: a sub-agent does not repeat what its parent's start recorded, and a resumed run records again. Every token class carries a built-in baseline list (`builtin:admin`, `builtin:tenant-operator`, `builtin:member`, `builtin:isolated`, plus `builtin:run-plane` for an agent-allowlisted token) that reproduces today's reach, so `enforce` with no lists authored behaves like `compat` — a test holds each list to its class's scope gates on every HTTP route, gRPC RPC and MCP meta-tool. A baseline grants the widest reach of its class and the token's scopes stay the ceiling (the isolated list allows the model gateways, which an isolated token still reaches only with `llm:gateway` granted by name); the one deliberate difference is that under `enforce` an isolated user may not list its tenant's users (`GET /v1/_users`). The gates run on HTTP, gRPC and the MCP meta-tools; a run intake additionally asks `run agent:<name>`. **Inside a run the agent definition's grants are intersected with the caller's rules** (RFC EY §3.7): a tool the caller may not `invoke` (built-in, or a mounted MCP / A2A / client tool — `tool:Bash`, `mcp:jobs/patch*`) is withheld from the model's schema and reported once at run start as a `capability_inert` event with gate `acl`; a volume the caller may not `write` is bound read-only and one it may not `read` is dropped; memory, SQL-memory and history scopes the caller may not `read` are dropped, and a memory or SQL-memory scope it may read but not `write` stays readable and becomes read-only (every Memory op that changes it — `set`, `delete`, `merge`, `add`, `pending_ack`, the consolidation cursor, `sql_exec`, `sql_begin` … — is refused, `Context op=permissions` lists it under `read_only_scopes` / `sql_read_only_scopes`, and an agent the run authors cannot be granted it); skill denies are appended to the agent's `skills:` patterns; and the `Agent` tool asks `spawn agent:<child>` before it starts a child. The Document, Path and CredentialDef tools also ask on EVERY call — in a run and on their off-run HTTP, gRPC and MCP surfaces — about the instance the call resolves to (RFC EZ §1.5): a Document op asks `read`, `write`, `delete` or `link` of `document:<scope>:<path>` at each Path-tree name the document is filed under (a document filed nowhere answers to the rules on its whole scope; a new document asks `write` of the name it is about to get); a Path op asks the same verb of the name it touches (a memory entry asks `memory:<scope>`, a volume mount `volume:<name>`, and a move or a recursive `rm` asks about every name it reaches, old and new); an op that reaches a scope's documents without naming them (`list_facts`, `graph_recall`, `query_chunks`, `upsert_chunk`, `backlinks`, …) is refused when any rule denies part of that scope or carries a condition only a document can answer; `query_documents`, `documents_summary`, `search`, Path `ls` and CredentialDef `list` leave out what the caller may not read; CredentialDef asks `use credential:<scope>:<name>` for `get` and `manage` for every write, rotate, retire or export, and a KMS key's `$cred:` references are each asked `use`. The originating caller's rules bind every sub-agent (`Context op=self` reports the `acl` block: mode, groups, removed tools). The run's set is taken at its start and kept for the whole run, unless `LOOMCYCLE_ACL_LIVE=1` (below) makes it follow the caller's rules as they change. A run that is paused and resumed — for a snapshot, after a crash, or across a restart — is narrowed again, with each of its sub-agents, by its caller's CURRENT rules (the run records whose rules bound it, never the rules themselves or a credential), so a binding revoked during the pause holds after it; if those rules cannot be read, the run stays paused (`acl_unavailable`) rather than resume with its agent's full grants. A run still queued when the server restarts starts under its caller's current rules the same way — a `run agent:` deny bound while it waited refuses it, and unreadable rules leave it queued. A run a trigger fires is narrowed by the rules of the trigger's owner as they stand at each fire — a ScheduleDef's or WebhookDef's author (recorded on the definition; a hook edit rebinds it to the editor), or an A2A peer's own credential — and `run agent:` is asked for that owner: a deny refuses the fire (a webhook answers `403`), unreadable rules refuse it with `acl_unavailable` (a schedule fires the slot again on the next tick, a webhook answers `503`), and a yaml trigger, which has no owner, fires as before. A `delivery: team` schedule or webhook binds the walk it starts the same way: `run team:<name>` is asked for the owner, with the same refusals, and every member the walk spawns is narrowed by the owner's rules and asked `spawn agent:` for them. A walk started on the plane with no enclosing run (`POST /v1/_teamdef` `op=run`, the gRPC `TeamDef` RPC, the MCP `teamdef` tool) binds its members by its starter's rules the same way, so an operator's own denies bind the walks it starts; a team subscription's walk is not bound (its promoter is recorded without a subject). A channel hook's run is not narrowed: it is only a record that files the hook's asks and decisions, with no model loop and no tools, and a channel records no author whose rules could bind it. `GET /v1/_me` reports `acl_mode`, the caller's `groups` and `acl_version`, and `effective.surfaces`: for each Web UI page (`run`, `runs`, `channels`, `access`, `users`, …), whether a request to the route that page loads first would pass the gates it would face — the scope gate with its member and self-service admissions, the run-plane limit, and the route's ACL question in the current mode (every page is `true` in open mode). The Web UI's nav and Settings tabs show exactly those pages. The rule lists, groups and bindings are store rows (migration 0109), authored over HTTP by a tenant operator (admin focuses another tenant with `?tenant=`; members are refused, and learn their own reach from `/v1/_me` and `/v1/_authz/explain`): `GET/POST /v1/_groups`, `GET/DELETE /v1/_groups/{name}`, `POST /v1/_groups/{name}/members` `{kind: user|group, id}`, `DELETE /v1/_groups/{name}/members/{kind}/{id}`; `POST /v1/_acldef` with `op` ∈ `create` (`name`, `rules`, `description`), `get`, `list` (+ the built-in baselines), `retire`, `validate`, `catalog` (every type, verb and scope a rule may name) and `GET /v1/_acldef/names`; `GET/POST /v1/_aclbinding` (`acl`, `subject_kind` ∈ user/group/token/tenant, `subject_id`, `expires_at`) and `DELETE /v1/_aclbinding/{id}`. Every write is audited (`acl_written`, `group_written`, `binding_written`, …) and invalidates the cached sets on every replica. An `_acldef` op asks its own question of the list it names: `create`, `update` and `put` need `author def:acl/<name>`, `retire` needs `retire def:acl/<name>` and `get` needs `read`, so a deny on authoring a list holds for a tenant operator too — the way to stop an operator lifting a list that binds it. Every other definition family asks the same of the definition an op names, on every transport and inside a run: a write (`create`, `fork`, `promote`, or any op not listed here) needs `author def:<family>/<name>` (`def:agent/…`, `def:skill/…`, `def:team/…`, `def:volume/…`, …), `retire`, `delete` and `purge` need `retire`, and `get`, `list` and `verify` need `read`, an op by `def_id` being asked of that definition's own name. A team walk's ops ask the team: TeamDef `run` needs `run team:<name>` (of its `def_id`'s team too, and, with `read`, of the team whose walk a reused `idempotency_key` answers with), `poll` needs `read` and `cancel` needs `cancel` of each walk's team, as do Agent `poll` and `cancel` of a walk and every cancel of a walk from outside a run (`POST /v1/runs/{run_id}/cancel`, the `CancelTurn` RPC, MCP `cancel_run` of `team:<name>`). A token limited to certain agents (`builtin:run-plane`) is denied `run team:*`, so its runs cannot start team walks. A delegated writer (not an operator) is refused a binding, a membership, or a write or retire of a list, that would change its OWN authority: a list bound to it, its token, a group it is in or its tenant, or a bound name not yet created (`403 acl_self_elevation`). The AuthZEN-shaped questions are open to any principal for ITSELF, and to a tenant operator for any subject of its tenant: `POST /v1/_authz/evaluation` `{subject?:{id}, action:{name}, resource:{type,id?,owner?}, context?:{transport,interactive,tier,user_tier,agent,depth}}` → `{decision, outcome: allow|deny|no_match, reasons, mode}` (`decision` is true only when a rule allowed; a `no_match` in compat or shadow means the scope gate decides), `POST /v1/_authz/evaluations` (a batch with `options.evaluations_semantic` ∈ `execute_all`/`deny_on_first_deny`/`permit_on_first_permit`), `POST /v1/_authz/explain` (every rule of the type and why it did or did not match, the applied bindings, the groups), and `POST /v1/_authz/simulate` (`acls` and `bindings` as a DRAFT merged over the stored state, `requests` evaluated `now` and `with_draft`; nothing is written). The rule shape: `{"effect":"allow|deny","actions":[verbs],"resources":["type:selector", …],"when":{…}}` — selectors are `/`-globs (`agent:chat/*`, `agent:chat/**`), scoped names (`memory:tenant`, `document:tenant:/kb/**`, `def:agent/chat/*`, `principal:user:support-*`) or enums (`run:own`, `gateway:chat`); `when` is the closed set `transport`, `time{days,between,tz}`, `interactive`, `tier`, `user_tier`, `source_cidr`, `owner_is_self`, `agent`, `depth{max}`. Explicit deny beats any allow; order never matters. The same operations are op-dispatched on gRPC (the `GroupDef`, `AclDef`, `AclBinding` and `Authz` RPCs), on the MCP server (the `groupdef`, `acldef`, `aclbinding` and `authz` meta-tools) and on the command line (`loomcycle acl groups|policies|bindings|check|explain|simulate`): the input is the route's JSON body plus `op` (and an admin's `tenant` focus), and every transport calls the one implementation the routes call, which holds every gate above, so a member is refused management and a deny on authoring lists holds on each. None is reachable from inside a run. The TS/Python adapter and Web-UI twins follow. **Config-declared access control** (the same shapes in yaml, validated at load by the engine's own strict parser so a bad entry fails `loomcycle validate` and the boot): `acls:` (name → `{description, rules}`), `groups:` (name → `{tenant, description, external_id, members: [subjects], groups: [member groups]}`) and `acl_bindings:` (a list of `{acl, user | group | token | tenant_wide: true, tenant, expires_at}`). The resolver merges them UNDER the store's rows: a stored list of the same name shadows the yaml one (dynamic over static, as for agents), a yaml list is reachable from a binding of any tenant, a yaml group or binding belongs to the tenant it names (`""` = the shared tenant); a config reload is honoured on the next evaluation. An example follows the table. |
| `LOOMCYCLE_ACL_CACHE_TTL_SECONDS` | How long a replica trusts a principal's compiled ACL set before re-reading the tenant's `acl_version` (default 5; `0` = re-read on every request). A write on another replica also invalidates it over the cluster bus, so this is the worst-case lag when a bus message is dropped. |
| `LOOMCYCLE_ACL_LIVE` | `1` makes a run in flight follow its caller's access-control rules as they change (RFC EY §3.7). Default off: a run keeps the rules it started with until it ends, so a binding removed mid-run reaches a long interactive run only when that run is over. When on, a run reads its caller's rules again before each model call and before each tool call — a cached lookup that costs nothing while the tenant's `acl_version` has not moved; a change is seen as soon as `LOOMCYCLE_ACL_CACHE_TTL_SECONDS` or a bus invalidation lets the replica see it. Whatever the current rules no longer allow is taken away for the rest of the run: a tool leaves the schema of the next model call and a call to it is refused with `acl_denied` before it runs; a volume becomes read-only or is dropped; a memory, SQL-memory or history scope is dropped, and a memory or SQL-memory scope the caller may read but no longer write becomes read-only; skill denies are added; and the Document, Path, CredentialDef, definition and `spawn agent:` questions are asked of the current rules. Each change is reported once on the run's transcript as a `capability_inert` event with gate `acl` (one per tool, one for the volumes, scopes and skills), and each refused call is audited as `acl_denied`. A run never gains more than it started with: a binding added mid-run grants it nothing new, and a binding restored after a removal gives back only what the run started with. A tool call already running when the rules change finishes; the next call is checked. Sub-agents follow the same rules, each from what it was spawned with, and report on their own transcripts. If the rules cannot be read again, `compat` and `shadow` keep the last reading, and `enforce` refuses every tool call with `acl_unavailable` (retryable) until a read succeeds. `shadow` refuses nothing, as at the start. A stateful (`context.mode: stateful`) run still lists a withdrawn tool in its prompt, and a `code-js` agent keeps it in its script (the script replays against the tools it started with), but a call to it is refused. |
| `LOOMCYCLE_OPERATOR_TOKEN_ROTATION_GRACE_SECONDS` | Default rotation grace window (default 24h). |
| `LOOMCYCLE_USER_TOKEN_MAX_TTL_SECONDS` | The longest lifetime a per-user token may be minted with. Unset = no cap. When set, a mint asking for more is refused, and a mint asking for no lifetime gets this one. See [Token lifetime](#token-lifetime). |
| `LOOMCYCLE_TOKEN_AUTHORITY_INHERIT` | `1` mints per-user tokens (`POST /v1/_users/{subject}/tokens` and every gate-issued token) with authority `inherit`: their scopes follow the user's **current** `access_mode` on every request, so a promotion or demotion needs no re-mint. Unset (default) = minted `frozen`, exactly as before; a mint may ask for either with `"authority"`. Operator tokens and config principals are always frozen; existing tokens keep their mode. See [Token authority](#token-authority-frozen-or-inherit). |
| `LOOMCYCLE_RUN_RATE_PER_MINUTE` / `LOOMCYCLE_RUN_RATE_BURST` | A default rate for run starts, for a caller whose token carries none. Unset = no default. Operators are not bound by it. See [Limiting how often a token starts runs](#limiting-how-often-a-token-starts-runs). |
| `LOOMCYCLE_EXPIRED_TOKEN_RETENTION_SECONDS` | How long an expired token's row is kept before it is deleted (default 7 days; `0` keeps expired rows). Only rows that carry an expiry are ever deleted. |
| `LOOMCYCLE_LIMITS_ENFORCE` | `strict` makes a hard token budget also stop runs in flight and refuse sub-agents. Unset = checked only when a run starts. See [Strict token budgets](#strict-token-budgets). |
| `LOOMCYCLE_AUDIT_LOG_PATH` | JSONL audit of every create/rotate/retire (never a token or hash). |
| `LOOMCYCLE_AUTH_VERBOSE` | `1` logs a server-side reason on a rejected bearer (the wire 401 stays opaque). |
| `LOOMCYCLE_OPERATOR_KEY_RESTRICTION` | **RFC AX** deployment gate (default OFF). When `1`, a run whose principal lacks `providers:operator-key` may not use the operator's host provider key — resolution routes it only to providers the tenant can key itself (an RFC AR CredentialDef) and refuses `403 operator_key_restricted` if none; the LLM-gateway + embeddings shims refuse a restricted principal outright. OFF ⇒ byte-identical for every existing token. |
| `LOOMCYCLE_PUBLIC_CONFIG` | Deployment gate (default OFF). When `1`, `GET /v1/config` may be read with **no bearer**, returning a narrowed public view — for a hosted instance whose landing page shows which providers and models are live. OFF ⇒ the endpoint requires auth like every other `/v1` read, so upgrading changes nothing. A presented-but-**invalid** bearer still `401`s rather than downgrading to the public view. See [§ GET /v1/config](#get-v1config). |
| `LOOMCYCLE_ALLOW_OPEN_MODE` | `1` lets the server start in **open mode** on a listener beyond loopback. Open mode is no `LOOMCYCLE_AUTH_TOKEN`, no config `principals:` and no admin operator token, so every route, token minting included, is unauthenticated. Without this, such a boot **exits with an error** when `LOOMCYCLE_LISTEN_ADDR` or `LOOMCYCLE_GRPC_ADDR` binds anything but `127.0.0.0/8`, `::1` or `localhost` (`:8787`, `0.0.0.0` and `::` bind every interface; any other hostname counts as public too). Open mode on loopback needs no opt-in. While open, the server logs a warning every 10 minutes and the Web UI shows an "unauthenticated" banner. Use it only for a throwaway test harness on a private network; anything reachable should set `LOOMCYCLE_AUTH_TOKEN` or declare `principals:`. |
| `LOOMCYCLE_AUTH_MODE` | The fleet default for `auth.mode`: `token` (default — today's paste-a-token sign-in) or `strict` (a person signs in to the Web UI only through a gate listed in `auth.gates`). A config's `auth.mode` may raise it but not lower it: `auth.mode: token` under `LOOMCYCLE_AUTH_MODE=strict` fails the load. `open` is not a value — open mode is what a server with no credential runs as, and strict mode with no credential refuses to boot. See [Web UI sign-in](#web-ui-sign-in--sessions-authmode-and-gates-rfc-fa). |
| `LOOMCYCLE_UI_INSECURE_COOKIES` | `1` lets **strict** mode start Web UI sessions over plain HTTP, with a session cookie that is not `Secure`, on any host — for a private network such as a tailnet. Warned at boot. Token mode needs no opt-in (it issues the plain `lc_session` cookie over HTTP and warns). |
| `LOOMCYCLE_WEBUI_DIR` | Serve the Web UI from an unpacked UI bundle in this directory instead of the UI built into the binary, so a UI fix can reach users without a new binary. Read once at startup. The directory holds the built SPA and a `manifest.json`: `{"ui_version": "…", "api_level_min": N, "api_level_max": M, "files": {"<relative path>": "<sha256 hex>", …}}`. The bundle is served only if the server's UI API level (`ui_api_level` in `/healthz`) is within `[api_level_min, api_level_max]`, every listed file is present and matches its digest, the directory holds no other file (`manifest.json` aside) and no symlink, `index.html` is listed, and it fits the limits (manifest 1 MiB, bundle 64 MiB, 10,000 files). The verified files are held in memory, so a later change to the directory has no effect until a restart. On any failure the server logs the reason and serves the built-in UI. `/healthz` reports `ui_bundle`: `source` (`embedded` or `dir`), `ui_version`, and `fallback_reason` after a refusal (a fixed code such as `digest_mismatch` or `api_level_unsupported`; the file and path are only in the log). The session, login and logout routes stay in the server, so a bundle cannot change cookie handling, but a bundle runs with the signed-in operator's session: point this only at a directory only the operator can write. |
| `LOOMCYCLE_WEBUI_BUNDLE_URL` | Serve the Web UI from a bundle **archive** fetched by URL instead of an unpacked directory — the `.tar.gz` a Web UI release publishes, whose top level is the bundle (`manifest.json` at the root of the tar, no leading directory). Set together with `LOOMCYCLE_WEBUI_BUNDLE_SHA256`, and not with `LOOMCYCLE_WEBUI_DIR`; the load **fails** otherwise, as it does for a URL that is not `https`, has no host or carries user info. At startup the server uses `<LOOMCYCLE_DATA_DIR>/webui/<sha256>/` if it exists (no network; still fully re-verified), else downloads the archive: `https` only (redirects too, at most 5), through the egress guard (a host on a private network needs an entry in `LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST`, and no proxy from the environment is used), within 60 s and 96 MiB. The archive's SHA-256 must match before anything is unpacked. The unpack accepts only regular files and directories with clean relative names (no links, devices, absolute paths, `..`, backslashes or duplicate members) within the bundle limits, into a temporary directory that is renamed into place only after the bundle passes every `LOOMCYCLE_WEBUI_DIR` check; a refused archive leaves nothing in the cache. On any failure the server logs the reason and serves the built-in UI — a bad download never stops the boot. `/healthz` reports `ui_bundle.source` `url`, or `embedded` with a `fallback_reason` such as `download_failed`, `archive_too_large`, `archive_digest_mismatch`, `archive_invalid`, `cache_unusable` or one of the directory codes. Neither `/healthz` nor the log carries the URL's path or query string (the log names its host), so a signed URL's token stays out of both — but the variable itself is not treated as a secret. Old digests are not pruned from the cache. |
| `LOOMCYCLE_WEBUI_BUNDLE_SHA256` | The SHA-256 of the archive `LOOMCYCLE_WEBUI_BUNDLE_URL` names — of the `.tar.gz` bytes, not of its contents — as 64 lowercase hex characters (the `.sha256` asset published with the release). Required with the URL. It is what makes the download trustworthy: TLS and the egress guard only decide where the bytes come from. To move to a new UI release, change both variables and restart. |

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

**Operator authority is never exercised from inside a run.** What the definition tools reserve for the operator — a `${NAME}` in an MCP server definition (and, with it, the MCP egress-guard exemption), a free `api_key_env` on a remote memory backend or document source, a `$cred:` webhook secret, `user_bearer` or hook signing secret the author's tenant does not hold yet, a trigger whose runs execute in another tenant, a tenant-less consolidation fan-out, a schedule that resolves `$cred:` in another user's scope — belongs to a `substrate:admin` principal, or the operator of an unauthenticated (open-mode / stdio) surface, calling from **outside a run**: the HTTP and gRPC substrate routes, the Web UI, an MCP session's own tool calls. A run started with an admin token carries the admin's principal into its model's tool calls, but those calls are the model's, so a definition tool reached from inside a run checks them like any tenant author's — a run `spawn_run` starts from an admin MCP session included. (The tools that write these definitions are given only to the operator surfaces today, not to agents; the rule keeps that safe if one is ever wired into a run.) The same holds for an admin's cross-tenant reach in the tools agents do hold (AgentDef, SkillDef, TeamDef, Evaluation, Context): inside a run an admin token's principal is a tenant caller. Every tenant caller, in a run or not, gets two tiers of agent, skill and team definitions — **its own tenant's**, which it may read (get, list, Context `op=lineage`, a TeamDef `run` or `render_diagram` by `def_id`), fork, retire and promote; and the **shared `""` tier** (defined with no tenant: in open mode or before the first token, by a yaml schedule or webhook, or through the stdio `loomcycle mcp`), which it may read and fork from but **not retire or promote**. Both tiers number their versions from 1, so `get` and `list` mark a shared-tier row `"shared": true` (on every transport: the in-run tools, `POST /v1/_agentdef` / `_skilldef` / `_teamdef`, the gRPC def RPCs and the MCP `agentdef` / `skilldef` / `teamdef` tools); every other row omits the field, and a caller whose own tenant is `""` (open mode, the stdio MCP server) owns that tier and sees no mark. Another tenant's definitions read as not found. Evaluation reads are confined to the caller's own tenant's runs, so a run started with the legacy `LOOMCYCLE_AUTH_TOKEN` (tenant `default`) does not see the evaluations of runs fired by a yaml schedule or webhook (tenant `""`). Only an admin calling from outside a run reaches every tenant. One way around the rule is yours to avoid: an agent whose MCP server points at loomcycle's own `/v1/_mcp` with an admin bearer arrives there as a fresh, off-run operator session — **don't hand agents admin bearers**.

On the **loomcycle-as-MCP-server transport** (`/v1/_mcp`, RFC AG) a `substrate:tenant` bearer may now OPEN a session — but the route scope decides only that. Inside the session a per-tool gate still withholds the admin-only meta-tools (token minting, runtime admin, snapshot capture/restore, cross-scope channel listing): they are hidden from the tenant's `tools/list` and refused on `tools/call`. The tools a tenant *can* call (def authoring, run lifecycle, memory/channel/path/document, hooks) are tenant-confined exactly like their HTTP twins. `substrate:admin` sees + may call the full meta-tool set. The thin client (`loomcycle mcp --upstream`) forwards the caller's bearer verbatim, so a tenant bearer there drives a tenant-confined session — see `docs/MCP_SERVER.md`.


#### Config-declared access control (RFC EY)

The three blocks, as a tenant `acme` might declare them:

```yaml
acls:
  support-tier-1:
    description: Support staff
    rules:
      - { effect: allow, actions: [run, read], resources: ["agent:chat/*", "agent:support/**"] }
      - { effect: allow, actions: [read],      resources: ["volume:shared-ro"] }
      - { effect: deny,  actions: [invoke],    resources: ["tool:Bash", "tool:Bashbox"] }
      - { effect: deny,  actions: [run],       resources: ["agent:admin/**"] }
groups:
  support:
    tenant: acme
    members: [alice, bob]
    groups: [support-contractors]
  support-contractors:
    tenant: acme
    members: [carol]
acl_bindings:
  - { acl: support-tier-1, group: support, tenant: acme }
```

### Token pepper

A token row stores `SHA-256(pepper ‖ token)`, never the token. The pepper is what stops someone with write access to the database — but not to the server's environment — from inserting a row for a token they chose. The effective pepper is, in order:

1. `LOOMCYCLE_OPERATOR_TOKEN_PEPPER`, if set — hashes exactly as before;
2. else one derived from `LOOMCYCLE_SECRET_KEY` (`HKDF-SHA256(key, info="loomcycle/operator-token-pepper/v1")`). Its hashes are stored as `p1:<pepper fingerprint>:<HMAC-SHA256(pepper, token)>`, so a row on any other form can be told apart;
3. else none, with a boot warning when auth is configured. `loomcycle doctor` reports which one applies.

**Tokens minted before the pepper: review, then rewrap.** A token row minted with no pepper stores `SHA-256(token)`, which anyone who can write the database could compute for a token of their own, so under a derived pepper such a row never authenticates as it is. The server does not change these rows. At every start it logs how many there are, by `def_id`, and they are refused at login until you move the ones you minted onto the pepper with `loomcycle tokens rewrap`, run with the server's own config and environment — the same `--config` and `--preset` flags, or, in a container, `docker exec <container> /usr/local/bin/loomcycle tokens rewrap`, which already has them:

1. `loomcycle tokens rewrap` prints the derived pepper's fingerprint, then lists each **live** unpeppered row — `def_id`, name, tenant, subject, scopes (an admin token is flagged), created / retired / expiry times, and a per-row fingerprint — then the row count and a digest of the exact set. It writes nothing. Retired or expired unpeppered rows are listed apart as not rewrappable: they can never log in, so they are never wrapped (a rotation grace window still counts as live). Every field is printed quoted, with control characters escaped, so a row cannot redraw the terminal to hide itself. A row no mint writes — a `def_id` other than `def_` + 16 lowercase hex, or control or non-printable characters in a field — is flagged **SUSPICIOUS** and cannot be rewrapped; non-ASCII in a name, tenant or subject draws a lookalike warning.
2. Rewrap the rows you recognise, naming each with the fingerprint the review printed: `loomcycle tokens rewrap --def-id <def_id>@<fingerprint> [--def-id ...]`, or every listed row with `--all-listed <digest>`. Either is refused if a named row was added, retired or changed since the review. The fingerprint covers the row's `def_id`, stored hash, tenant, subject, scopes, retired and expiry times. A row you did not mint was written into the database: retire it (`loomcycle operator-token retire --def-id <id>`) rather than name it; a retired row leaves the rewrap set. `--dry-run` with either checks the selection and writes nothing. The result re-prints each rewrapped row, and lists any named row that changed in between as not moved (exit 1).

**Run it with the server's key.** The fingerprint the command prints must match the one in the server's boot log (`auth: operator tokens use a pepper derived from LOOMCYCLE_SECRET_KEY, fingerprint …`). Rows wrapped under a different key cannot log in. The command refuses when it finds rows under another key and none under its own, the usual sign of the wrong key. If a rewrap did run under the wrong key, set that key as `LOOMCYCLE_SECRET_KEY_PREVIOUS` on the server: those rows then log in and move onto the current key at their next login.

A rewrapped row's stored hash becomes `w1:<pepper fingerprint>:<HMAC-SHA256(pepper, "w1" 0x00 ‖ that hash's hex text)>`, which the lookup recomputes from the presented token; its first login moves it onto the `p1:` form. Only the rows you name are rewrapped, in one transaction; a row nobody named stays refused. The command is safe while servers run: each write is a compare-and-set against the hash the row was listed with, two runs at once wrap a row once, and a server that started with those rows present logs their tokens in as soon as they are rewrapped, with no restart.

**Upgrade note.** A deployment that already started v1.119.0 with `LOOMCYCLE_SECRET_KEY` set had its rows rewrapped by that start and has nothing to do. One upgrading from a release before v1.119.0 straight to a later one must run the command once, or its pre-pepper tokens stop working. While unusable, an admin token row does not switch the shared `LOOMCYCLE_AUTH_TOKEN` off, so that secret keeps working meanwhile (to retire a row you did not mint, for one).

A key rotation works the same way at login: with the old key in `LOOMCYCLE_SECRET_KEY_PREVIOUS`, a token on either form under the pepper it derived still logs in and is moved onto the new one. Those rows cannot be rewrapped without the token; the boot log counts how many are left, so you know when `LOOMCYCLE_SECRET_KEY_PREVIOUS` can go.

**What this does not stop:** the listing shows what a row grants, not whose token it holds. Someone who can write the database and replaces the hash of a genuine pre-pepper row before you review it gets that row rewrapped for a token of their choosing. A change after the review is caught: both the per-row fingerprint and the digest pin the hash. Rewrapping soon after upgrading keeps that window short. Nothing they do makes the server rewrap a row on its own, and without the pepper they cannot write a row that authenticates any other way.

**Upgrade every replica together, and plan a rollback as a re-mint.** A release without the derived pepper cannot read the `p1:` form, and a release before the rewrap cannot read `w1:`. In a mixed-version cluster, a rewrapped token gets `401` from every old replica, and a token minted on an old replica without `LOOMCYCLE_SECRET_KEY` after a new replica has started is refused by the new ones; upgrade all replicas at once. Rolling back to such a release, or removing `LOOMCYCLE_SECRET_KEY`, logs out every token minted, rewrapped or rehashed since; plan to re-mint them.

**What still invalidates tokens:** changing or removing an explicit pepper (rewrapping those rows under a derived pepper does not bring them back: a rewrap keeps only tokens minted with no pepper working); setting an explicit pepper on a deployment that used a derived one; removing `LOOMCYCLE_SECRET_KEY` when the pepper is derived from it; and dropping `LOOMCYCLE_SECRET_KEY_PREVIOUS` before every token has logged in once under the new key.

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

### Token authority: frozen or inherit

A per-user token is minted in one of two authority modes (RFC EY §8):

| Mode | The token's scopes | Minted by default for |
|---|---|---|
| `frozen` | The grant of the user's `access_mode` **at mint**, fixed for the token's life. | Every token, unless `LOOMCYCLE_TOKEN_AUTHORITY_INHERIT=1`. Operator tokens and config `principals:` are always frozen. |
| `inherit` | The grant of the user's **current** `access_mode`, re-read on every request the token authenticates. Promote a user from `isolated` to `tenant` and its token reaches the tenant plane on the next request; demote it and the token narrows the same way — no re-mint. | With `LOOMCYCLE_TOKEN_AUTHORITY_INHERIT=1`: per-user tokens a tenant mints (`POST /v1/_users/{subject}/tokens`) and every token an auth gate issues (password, OIDC, OAuth, external gates and the bridges that sign in through them). |

The mint body takes an optional `"authority": "inherit" | "frozen"` to ask for either mode whatever the default; any other value is a `400`. It names no scope: an inherit token's ceiling is still exactly what the user's `access_mode` grants a delegated token, never an operator scope. The mint answer and the token listing carry `"authority": "inherit"` for an inherit token (absent for a frozen one, so a mint with the switch off answers as before), and `scopes` there is the grant at mint. `GET /v1/_me` and `Context op=self` report the caller's `authority` (`inherit` or `frozen`).

Rules worth knowing:

- **An inherit token stands only while its user exists and is `active`.** A disabled, pending or deleted user's inherit token is refused on its next request, on every replica, whatever the resolution cache holds — the user row is read on each request, as for a gate-issued token.
- **Restrictions still only narrow.** `agents`, `overrides`, `rate` and `retention` stay on the token; a promotion widens the scopes, never the restrictions.
- **A rotation keeps the mode.** `rotate` of an inherit token (by `--def-id`) mints an inherit token.
- **Existing tokens keep their mode.** Turning the switch on or off changes only what is minted afterwards; every token minted before this mode existed is frozen.
- **Cost:** an inherit token costs one indexed user-row read per request; a frozen token costs none.

### A token on a browser WebSocket

A browser cannot set an `Authorization` header on a WebSocket, so the client-tools connection (`GET /v1/client-tools`, the `client-tools` help topic) also accepts the bearer as a `bearer.<token>` entry in `Sec-WebSocket-Protocol`. loomcycle reads that entry only on a WebSocket upgrade to that one path; on every other request it is ignored. The server answers with its own subprotocol and never echoes the token. In strict mode (`auth.mode: strict`) the subprotocol entry is not read: a browser authenticates the socket by its Web UI session, from the server's own origin.

The token still travels in a request header that many reverse proxies, load balancers and CDNs log, and it sits in browser JavaScript where any script on the page can read it. Give a browser client a per-user token with a short `ttl_seconds` and only the scopes and agents it needs (see [Token lifetime](#token-lifetime) and [Limiting a token to certain agents](#limiting-a-token-to-certain-agents)), never an operator or admin bearer, and keep `Sec-WebSocket-Protocol` out of proxy access logs where you can. A client that can set headers, such as Node or a CLI, should send `Authorization: Bearer` instead.

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

### Web UI sign-in — sessions, `auth.mode` and gates (RFC FA)

**Why sessions, not the token in a cookie.** The Web UI used to keep the pasted bearer in its cookie: the browser held the API credential itself, nothing proved who pasted it, and logging out ended nothing on the server. Now a sign-in creates a **server-side session**. The browser holds an opaque, HttpOnly session cookie and never the token; the server stores only the SHA-256 of the session id, so a database read yields no cookie that works. Signing out ends the session on every replica.

```yaml
auth:
  mode: token                     # token (default) | strict; env LOOMCYCLE_AUTH_MODE is the fleet default
  session: { idle: 8h, absolute: 168h, reauth_window: 15m }
  gates:                          # the sign-in options the login page offers
    paste: { kind: token, label: "Access token" }
```

- **`auth.mode: token`** (the upgrade default) is today's behaviour: the login page offers the token paste box (and the `LOOMCYCLE_UI_LOGIN_ORIGINS` postMessage handoff) whether or not gates are listed; listing a gate never switches the mode. A browser still holding the old `loomcycle_session` bearer cookie keeps working **for this release only** (logged); signing in again moves it to a session.
- **`auth.mode: strict`** offers only the listed gates. The paste box exists only if a `kind: token` gate is listed, the handoff is off, the old bearer cookie and the WebSocket subprotocol bearer are not read, and Web UI sessions need HTTPS (see below). Strict mode with no interactive gate fails the load (nobody could sign in); strict mode with no credential at all refuses to boot. A config reload cannot lower the mode (audited `auth_downgrade_refused`); that takes a restart. API bearers are unaffected.
- **Gates.** `kind: token` (paste a loomcycle token), `kind: password` (a login and password loomcycle keeps — see [Password gate](#password-gate--kind-password-rfc-fa-p2)), `kind: oidc` (sign in with an identity provider — see [OIDC gate](#oidc-gate--kind-oidc-rfc-fa-p1)), `kind: oauth` (GitHub, GitLab, Gitea or a generic OAuth provider — see [OAuth gate](#oauth-gate--kind-oauth-rfc-fa-p5)), `kind: external` (a separate process, such as a chat bridge, that signs who it is talking to — see [External gate](#external-gate--kind-external-rfc-fa-p4)), `kind: header` (a signed assertion from a proxy in front: Cloudflare Access, an AWS ALB, a forward-auth proxy) and `kind: awsiam` (AWS workloads, API tokens only) — see [Header and AWS IAM gates](#header-and-aws-iam-gates--kind-header-kind-awsiam-rfc-fa-p3); the other kind the RFC names (`device`) is refused as not yet supported. A gate name is lowercase letters, digits, `-` and `_` (at most 32); at most one `kind: token` gate.
- **`auth.session`.** `idle` (default 8h) ends a session after that long without a request; `absolute` (default 7 days) caps it whatever the activity. Shortening either applies to sessions already issued. `reauth_window` (default 15m) is validated now and used once sensitive actions ask for a recent sign-in.
- **A session never outlives or out-reaches its token.** A session made from a pasted token re-checks that token on every request: retiring or expiring a minted token, changing a principal's secret, removing it, or changing `LOOMCYCLE_AUTH_TOKEN` ends the session on its next request, and the session always carries the token's **current** scopes. A changed token pepper (`LOOMCYCLE_OPERATOR_TOKEN_PEPPER` / `LOOMCYCLE_SECRET_KEY`) ends every session. A browser that presents the cookie with a different User-Agent ends it too.

**The cookie, and HTTPS behind a proxy.** Over HTTPS the cookie is `__Host-lc_session` (`Secure; HttpOnly; SameSite=Strict; Path=/`, no `Domain`, no expiry beyond the browser session). Behind a TLS terminator the server sees plain HTTP, so it relies on `LOOMCYCLE_PUBLIC_URL`: when that is an `https://` URL, a request whose `Host` is that URL's host, or that comes from a loopback peer (a terminator on the same machine), counts as HTTPS. No forwarded header is trusted. Over plain HTTP the cookie is `lc_session`, without `Secure`: issued with a warning in token mode, refused in strict mode unless `LOOMCYCLE_UI_INSECURE_COOKIES=1`.

| Deployment | Set |
|---|---|
| Cloudflare Tunnel (TLS at the edge, plain HTTP to loomcycle) | `LOOMCYCLE_PUBLIC_URL=https://<public host>`; the tunnel must forward the public `Host` (its default; do not set `httpHostHeader`). |
| `tailscale serve` (tailscaled terminates HTTPS, proxies to `localhost`) | `LOOMCYCLE_PUBLIC_URL=https://<machine>.<tailnet>.ts.net`; the proxied request comes from loopback. |
| A tailnet name or IP over plain HTTP (WireGuard encrypts the path) | Token mode: nothing. Strict mode: `LOOMCYCLE_UI_INSECURE_COOKIES=1`. |
| A LAN install on `http://host:8787` | Token mode: nothing (warned). |

**CSRF.** A request a session cookie authenticates that changes state must carry `X-Loomcycle-UI: 1` and come from the server's own origin (`Sec-Fetch-Site: same-origin`, or an `Origin` naming this host); a WebSocket upgrade it authenticates needs the same origin. The login route needs a same-origin request too. Requests with an `Authorization` header are not affected. **A UI bundle built for an older server (`LOOMCYCLE_WEBUI_DIR` / `LOOMCYCLE_WEBUI_BUNDLE_URL`) does not send the header**, so the server's UI API level is now 2 and such a bundle is refused in favour of the embedded UI.

**Routes.** `POST /ui/session` (the token gate; `Authorization: Bearer`; 204 + cookie, or an opaque `401 invalid_credentials`), `POST /v1/_auth/logout` (revokes the session, clears the cookies, sends `Clear-Site-Data`), `GET /ui/login-config.json` (the mode, the gates' names, kinds and labels, the handoff origins, and `session_refused` when this request cannot sign in), and `GET /v1/_me`'s `auth` block (`mode`; for a session also `gate`, `amr`, `auth_time`, `session_expires_at`). `GET /ui?token=…` no longer signs anyone in: it redirects to the login page. With `LOOMCYCLE_AUDIT_LOG_PATH` set, `auth_login`, `auth_login_failed`, `auth_logout`, `auth_session_expired` and `auth_session_revoked` are recorded with the gate, tenant, subject and source address, never a token or session id. Ended sessions are deleted hourly.

### OIDC gate — `kind: oidc` (RFC FA P1)

Sign people in to the Web UI through Google Workspace, Microsoft Entra, Okta, Keycloak, Cognito, IAM Identity Center, Auth0 or any OpenID Connect provider. loomcycle is a confidential client: it redeems the code, verifies the ID token and keeps the provider's tokens to itself; the browser only gets loomcycle's session cookie.

```yaml
auth:
  lifecycle:
    new_user: { access_mode: isolated, groups: [everyone, new-users], status: active, token_ttl: 12h }
  gates:
    google:
      kind: oidc
      preset: google
      label: "Sign in with Google"
      client_id: 1234.apps.googleusercontent.com
      client_secret_env: LOOMCYCLE_GATE_GOOGLE_SECRET   # an env var NAME; a literal or $cred: secret is refused
      allowed_domains: [acme.com]                     # the hd claim; email_verified must be true too
      end_session: false                              # true: logout also visits the provider's end-session page
      identity_map:
        tenant: acme                                  # or tenant_claim: <claim> + allowed_tenants: {<value>: <tenant>}
        provision: auto                               # never | known (default) | auto
```

- **Redirect URI.** Register exactly `<LOOMCYCLE_PUBLIC_URL>/v1/_auth/complete/<gate>` at the provider. `LOOMCYCLE_PUBLIC_URL` is required.
- **Presets.** `google` (issuer fixed, `allowed_domains` required: the `hd` claim, never the e-mail's domain), `entra` (`allowed_tids`: one GUID uses that tenant's issuer; several use the `organizations` endpoint and each token's own issuer), `cognito` (`issuer: https://cognito-idp.<region>.amazonaws.com/<pool>`; `token_use` must be `id`), `okta`, `keycloak`, `identity-center`, `auth0` (each needs `issuer`), `generic` (`issuer`, optional `discovery_url`). Other keys: `scopes` (default `openid email profile`), `token_auth: basic|post`, `allowed_return_to` (absolute URLs a login may return to, matched exactly; a `/ui` path is always allowed), `trusted`.
- **Checks.** Code flow with PKCE S256 only; `state` bound to a ten-minute `__Host-lc_login` cookie (`lc_login` over plain HTTP) and a single-use server row; `nonce`; `iss`/`aud`/`azp`; `exp` and `iat` within 5 minutes; asymmetric signature algorithms only; the key by `kid`, refetched on rotation at most once a minute. Starting a login is throttled to 30 per client source per 10 minutes.
- **Identity map.** The subject is always `<gate>:<sub>` — never the e-mail. An identity lands in the configured tenant or, with `tenant_claim`, in the tenant `allowed_tenants` maps its claim to; anything else is refused, never the shared tenant. `provision: known` admits only users an operator created (subject `<gate>:<sub>`); `auto` creates them at `auth.lifecycle.new_user` (the floor, which may not be tenant reach). A gate's `identity_map.new_user` may go beyond the floor (tenant reach, more groups, `active` over a `pending` floor) and its `groups` block may sync groups only with `trusted: true`.
- **Group sync.** `identity_map.groups: {claim: groups, provision: true, map: {hr-admins: admins}}` adds and removes the user's memberships, at every login, in the groups whose `external_id` is `<gate>:<provider name>` only; local groups and other gates' groups are never touched. Audited `auth_groups_synced`.
- **Approval queue.** `new_user.status: pending` (recommended for strict deployments) lets a new person sign in to an "awaiting approval" page and nothing else, until an operator approves them in the Users console (`POST /v1/_users/{subject}/approve`).
- **Linking.** A user links another gate's account from the account page, within `auth.session.reauth_window` of signing in, through a fresh provider sign-in; never inferred from an e-mail. Only a password or identity-gate session may link (not a pasted-token session). `DELETE /v1/_users/{subject}/identities/{gate}` unlinks.
- **Issued tokens.** Tokens a gate issues for non-browser clients are tagged `issued_by_gate`, live at most `new_user.token_ttl` (and `LOOMCYCLE_USER_TOKEN_MAX_TTL_SECONDS`), and cannot be rotated.

### Password gate — `kind: password` (RFC FA P2)

**Why a password gate, and why loomcycle never takes the password over its API.** A website that runs its own sign-up (loomcycle.cloud) and a tenant that adds its own people both need accounts that are not pasted tokens. loomcycle keeps those passwords itself, to the current standard, but the person always types theirs into **loomcycle's own pages** — the login form, an invitation page, a reset page. A website or an operator only ever handles **links**: loomcycle issues them, the caller delivers them (loomcycle sends no mail), and no management route accepts a password.

```yaml
auth:
  trusted_proxies: [127.0.0.1]      # see "The client address behind a proxy" below
  gates:
    local:
      kind: password
      label: "Sign in"
      require_mfa: true             # default: true in strict mode, false in token mode
      trusted: false                # true: an operator's invite may grant access_mode tenant and groups
      invites: { default_ttl: 168h, max_ttl: 720h }   # max at most 30 days
      resets: { ttl: 1h }                              # at most 24h
      allowed_return_to: ["https://www.example.com/welcome"]
      password: { denylist_file: /etc/loomcycle/denylist.txt, max_failures: 10, lockout_minutes: 15 }
      new_user: { access_mode: isolated, groups: [everyone, new-users] }   # the provisioning floor (these are the defaults)
      manager_may_recover_owners: false
```

A password gate needs **`LOOMCYCLE_SECRET_KEY`** (the password pepper and the key that seals authenticator secrets derive from it) and **`LOOMCYCLE_PUBLIC_URL`** (invite and reset links are built from it); without either the config fails to load, so `loomcycle validate` reports it. A password gate is an interactive gate, so strict mode may list it alone.

- **Storage.** Argon2id (`m=19456, t=2, p=1`) in a PHC string whose digest is an HMAC under a pepper derived from `LOOMCYCLE_SECRET_KEY` and never stored, so a copy of the database verifies nothing. `LOOMCYCLE_SECRET_KEY_PREVIOUS` keeps verifying during a key rotation, and a password is re-hashed on its next sign-in. Authenticator secrets are sealed with the same key the credential store uses, bound to the user; recovery codes and every invite and reset code are kept only as hashes.
- **Policy** (NIST SP 800-63B-4): at least 15 characters when the password is the only factor, 8 with an authenticator; no composition rules, no expiry, paste allowed, Unicode-normalised. Refused: the built-in list of the 10,000 most common passwords (SecLists, MIT), the lines of `password.denylist_file` (one per line, `#` comments, compared case-insensitively), and the person's own login or e-mail.
- **Logins** are unique per gate across every tenant, compared after Unicode normalisation and case folding (`Alice@Example.com` and `alice@example.com` are one login), so the login form asks for no tenant. A user the gate creates gets the subject `<gate>:<random>`; the login is an attribute and can be renamed without moving the user's data.
- **Sign-in and throttling.** An unknown login, a wrong password, a locked account and a disabled user get the same `401 invalid_credentials`, after the same Argon2id work. Failures are counted per account (a short backoff from the fourth, then a lockout of `lockout_minutes` at `max_failures`, audited `auth_login_locked`), per client address, and per client address and login together (so a client guessing one account slows itself there first); invite and reset links are throttled per link too. The counters are in the store, so every replica sees them. An account with an authenticator is asked for its 6-digit code (or a recovery code) only after the password was right; a code is accepted once.
- **The client address behind a proxy.** Throttling (and the audit's source address) needs the real client, not the proxy every request arrives from. `auth.trusted_proxies` lists the proxies allowed to say who the client is, and `auth.client_ip_header` the header they say it in; without a header, the right-most `X-Forwarded-For` entry that is not a trusted proxy is used. A request from any other peer is taken at its own address, whatever headers it sends. The server warns at boot when a password gate runs with an `https` `LOOMCYCLE_PUBLIC_URL` and no `trusted_proxies`. Trust only the proxy's own fixed address: any other container that can reach loomcycle could otherwise forge the header (`cloud-deployment/` pins cloudflared on a dedicated `edge` network for this).

  | Deployment | Set |
  |---|---|
  | Cloudflare Tunnel (`cloudflared` in its own container) | `auth.trusted_proxies: [<cloudflared's fixed address>/32]`, `auth.client_ip_header: CF-Connecting-IP` |
  | `tailscale serve` on the same machine | `auth.trusted_proxies: [127.0.0.1, "::1"]` (it sends `X-Forwarded-For`) |
  | Direct (browsers connect to loomcycle itself) | nothing |
- **Authenticator (TOTP).** Enrolled on the invite page when `require_mfa` is set (optional otherwise), or later on `/ui/account`. Ten single-use recovery codes are shown once. `require_mfa` on a gate refuses a sign-in without one, and the authenticator cannot be removed there. Elsewhere removing it leaves the password as the only factor, so a password that was long enough only beside it (8–14 characters) must be replaced in the same request.
- **Lost the authenticator and the recovery codes.** A reset never clears the authenticator (the link alone must not get past the second factor), so the way back is two operator steps: `DELETE /v1/_authgate/{gate}/credentials/{subject}` removes the password, authenticator and recovery codes (the login stays reserved), then `POST /v1/_authgate/{gate}/resets` issues a link on which the person sets a new password and, where the gate requires one, enrols a new authenticator.
- **Sessions.** A password sign-in ends in the same Web UI session as a pasted token, bound to the stored password: a reset, a password change or a removed password ends every session made from the old one. The session carries the user's own reach — the scopes its `access_mode` grants a delegated token, and its access-control set — never more, with one exception: a tenant's **bootstrapped owner** (below) holds a user binding to the built-in `builtin:tenant-operator` list, and its session is its tenant's operator (`substrate:tenant`, confined to that tenant). The binding is listed in the Access console; retiring it takes the operator plane away on the owner's next request. Only the bootstrap writes it: member invites, attach invites and recoveries never do.

**Links.** An invite or reset link is `https://<LOOMCYCLE_PUBLIC_URL>/ui/invite#lci_…` or `/ui/reset#lcr_…`. The code is in the fragment, so it reaches no server log or `Referer`; the page sends it in a POST body and drops it from the address bar. A code is single-use, expires, and is stored only as its SHA-256.

**Management API** (`/v1/_authgate/{gate}/…`; `{gate}` must be a password gate). None of these takes a password; a body field the route does not know is refused.

| Route | Does |
|---|---|
| `POST invites {tenant?, subject?, access_mode?, groups?, login?, email?, ttl?, return_to?}` | A single-use invite → `{invite_id, url, expires_at}`. With `subject`, attaches a password to that existing user (no login or password at the gate yet; its access mode and groups are kept and never raised). `login` fixes the login; `email` suggests one. |
| `GET invites`, `DELETE invites/{id}` | List (never a code); revoke a live one. |
| `POST resets {tenant?, subject}` | A one-time reset link for a user with a login at the gate; a newer one supersedes it. Completing it ends the user's sessions and retires its delegated tokens; an account with an authenticator must also give a code from it. |
| `DELETE credentials/{subject}` | Removes the password, authenticator and recovery codes; the login stays reserved, so a reset restores it. |
| `PATCH logins/{subject} {login}` | Renames the login. |
| `GET identities?subject=` | The user's logins at every gate, and whether it has a password and an authenticator. |
| `GET tenant-status?tenant=` | `has_users`, `bootstrapped` (ever had a user) and `bootstrap_invite_live` — what a website needs for an owner bootstrap. |

**Who may call.** A `substrate:tenant` operator for its own tenant (`tenant` defaults to it; another is `403 tenant_mismatch`); an admin for any tenant; and an **`auth:manage`** token — a website's user-management credential, minted alone (`scopes: ["auth:manage"]`), which reaches only these routes and the `/v1/_users` list, create, update and delete, in any tenant it names with `tenant` / `?tenant=` (it must name one), and nothing else: no runs, no data, no whoami, no token minting, no groups or ACLs, no gRPC.

**The floor.** What a user the gate provisions gets unasked is `new_user` (default `isolated` in `[everyone, new-users]`). On a gate that is not `trusted`, an invite may grant no more. A user is **above the floor** when its access mode is higher, it is in any other group (stored or config-declared, directly or through nesting), an access-control binding applies to it other than tenant-wide ones and those on the floor groups, or it holds a token with a scope the floor does not grant; a store error counts as above. What an `auth:manage` token may do:

| Action | Floor user | Above the floor | Above, with `manager_may_recover_owners: true` |
|---|---|---|---|
| Invite a new user | at the floor (any gate, `trusted` or not) | — | — |
| Owner bootstrap (`access_mode: tenant` invite or `POST /v1/_users`) | only for a tenant that never had a users row and has no live owner invite; operator tokens in the tenant do not count, so a tenant created by the old cloud mint path gets its owner exactly once. The owner is bound to `builtin:tenant-operator` in the same transaction (audited `auth_owner_bound`), which puts it above the floor | — | — |
| Attach a password (invite with `subject`) | yes | refused | yes, if the user has no login or password at the gate yet (audited `auth_recovery`) |
| Reset link | yes | refused | yes (audited `auth_recovery`) |
| Delete, disable, re-enable, change `access_mode`, remove the password, rename the login | yes (`access_mode` never raised) | refused | refused |
| Change `display_name` | yes | yes | yes |

Refusals are `403 target_beyond_floor`, audited `auth_manage_refused`. A link an `auth:manage` token issued is checked against the floor **again when it is used**, and any write that raises the user's authority (a group membership, a binding on it or its token, `access_mode: tenant`, a new token) revokes it at once. **`manager_may_recover_owners` makes the website's credential able to take over any owner's account** — the website then owns the accounts, which is loomcycle.cloud's trust model; keep it `false` everywhere else. Deleting every user of a tenant does not re-open its bootstrap: a tenant that ever had a user stays marked, and the marker travels in snapshots.

**Person-facing routes** (loomcycle's own pages call them; each is same-origin with `X-Loomcycle-UI: 1`): `POST /v1/_auth/complete/{gate}` (sign in: `login`, `password`, `otp`), `POST /v1/_auth/invite/info` and `POST /v1/_auth/invite`, `POST /v1/_auth/reset/info` and `POST /v1/_auth/reset`, and on a password session `GET /v1/_auth/account`, `POST /v1/_auth/password`, `POST /v1/_auth/totp/begin`, `POST /v1/_auth/totp/confirm`, `DELETE /v1/_auth/totp` — each change re-asks the current password (and code). The pages are `/ui/login`, `/ui/invite`, `/ui/reset` and `/ui/account`.

**Audit** (with `LOOMCYCLE_AUDIT_LOG_PATH`): `auth_invite_created`, `auth_invite_revoked`, `auth_invite_redeemed`, `auth_reset_issued`, `auth_password_set`, `auth_credential_removed`, `auth_login_renamed`, `auth_totp_enrolled`, `auth_totp_removed`, `auth_login_locked`, `auth_manage_refused`, `auth_manage_recovery`, `auth_owner_bound`, besides the sign-in events above — with the gate, tenant, subject and caller, never a code, link or password. Dead invites and resets are deleted a week after they stop working; idle throttle counters hourly.

### OAuth gate — `kind: oauth` (RFC FA P5)

**Why a separate kind, not OIDC.** GitHub, GitLab and Gitea sign people in with OAuth 2.0 but issue no signed ID token, so there is nothing to verify the way the OIDC gate does. The OAuth gate runs the same redirect login (code + PKCE, a `state` bound to the browser's login cookie, `GET /v1/_auth/login/{gate}` → the provider → `GET /v1/_auth/complete/{gate}`), then asks the provider's API who the person is. Everything after that — `identity_map`, provisioning, group sync, the Web UI session — is the OIDC gate's, unchanged.

```yaml
auth:
  gates:
    gh:
      kind: oauth
      preset: github                  # github | gitlab | gitea | generic
      label: "Sign in with GitHub"
      client_id: Iv1.0123abcd
      client_secret_env: LOOMCYCLE_GATE_GH_SECRET   # an env var NAME; a literal is refused
      # base_url: https://ghe.example # GitHub Enterprise / self-hosted GitLab; required for gitea
      allowed_orgs: [acme]            # only these orgs (and their teams, as acme/<team>) become groups
      require_org: true               # refuse a person in none of them
      trusted: true                   # needed for identity_map.groups
      identity_map: { tenant: acme, provision: auto, groups: { provision: true } }
    sso:
      kind: oauth
      preset: generic
      client_id: loomcycle
      client_secret_env: LOOMCYCLE_GATE_SSO_SECRET
      auth_url: https://sso.example/authorize
      token_url: https://sso.example/token
      userinfo_url: https://sso.example/userinfo
      claims: { subject: sub, email: email, email_verified: email_verified, name: name, groups: roles }
      allowed_orgs: [staff]           # generic: the group values admitted
      identity_map: { tenant: acme, provision: known }
```

- **The subject** is `<gate>:<the provider's numeric user id>` — never the login name, which its owner can rename and someone else can then take. An `identity_map` may use only `sub`, a verified e-mail's domain and the gate's own groups; one that names `login`, `name` or `username` fails to load.
- **E-mail** is recorded only when the provider says it is verified: on GitHub the primary *and* verified address from `/user/emails` (the public profile address is ignored); on GitLab the primary address only when `/api/v4/user/emails` lists it with its own `confirmed_at` (the account's `confirmed_at` is not enough); for `generic` only a JSON `true` in the `email_verified` claim. And only when `trust_email` is on: its default is on for github.com and gitlab.com and **off** for a self-managed instance (`base_url`) and for `generic`, whose address confirmation loomcycle cannot see. An `identity_map` that picks the tenant from the e-mail domain (`tenant_claim: email_domain`) is only as safe as the provider's confirmation: use it only with a provider that enforces it.
- **Who may sign in.** On public github.com or gitlab.com anyone has an account, so a `github` or `gitlab` gate without `base_url` refuses to load unless it sets `allowed_orgs` with `require_org: true`, or says `allow_any_account: true` to admit any account there on purpose (as the OIDC `google` preset requires `allowed_domains`). A self-managed instance (`base_url`) controls its own accounts and is not held to this.
- **Groups.** An organisation, team or group becomes a loomcycle group only when `allowed_orgs` lists it (GitHub/Gitea: the org and `org/team`; GitLab: a group path under a listed top-level group; generic: the listed values of `claims.groups`). With no `allowed_orgs` the gate asserts no groups and does not even read memberships. Syncing them into loomcycle groups needs `trusted: true` and `identity_map.groups`, as on the OIDC gate. GitLab asks for `read_api` only when `allowed_orgs` is set.
- **Needs** `LOOMCYCLE_PUBLIC_URL` (the redirect URI is `<public URL>/v1/_auth/complete/<gate>` — register exactly that with the provider) and an `identity_map`. `base_url`, `auth_url`, `token_url` and `userinfo_url` must be `https` (http only on loopback). `scopes` replaces the preset's; `token_auth: basic | post` picks how the client authenticates at the token endpoint.
- **Account linking** through an OAuth gate is refused: an OAuth provider cannot prove that the person signed in afresh, which linking requires. Link through an OIDC or password gate.

### External gate — `kind: external` (RFC FA P4)

**Why.** A chat bridge, a website or a broker in front of an identity provider loomcycle has never heard of already knows who it is talking to. As an external gate it says so in a JWT signed with its own key, and loomcycle does the rest: it checks the signature, provisions the user by the gate's `identity_map`, and issues the token. The gate holds no loomcycle token, so a stolen bridge cannot act as an operator. It can only assert people inside its own `identity_map`.

```yaml
auth:
  gates:
    telegram:
      kind: external
      public_key: { kty: OKP, crv: Ed25519, x: "<the gate key's public x>" }   # or P-256 (ES256)
      # hmac_secret_env: LOOMCYCLE_GATE_TG_SECRET   # instead of public_key, for a co-located gate
      issued_token: { ttl: 12h, agents: [chat], overrides: [interactive], rate: { per_minute: 6, burst: 10 }, max_active: 2 }
      identity_map: { tenant: acme, provision: auto }
      # on_removed: { unbind_after: 72h }           # this gate's removal policy, over auth.lifecycle.on_removed
    hr:
      kind: external
      public_key: { kty: EC, crv: P-256, x: "…", y: "…" }
      callback_url: https://hr-sso.example/loomcycle/login   # a browser login starts here
      allowed_return_to: [https://app.example/welcome]
      identity_map: { tenant: acme, provision: known }
```

- **Routes.** None takes a bearer: the gate's signature is the authentication. Each verifies under that gate's key only, checks `aud` (`LOOMCYCLE_PUBLIC_URL`, required), a lifetime of at most five minutes, the route's purpose (`lc_use`) and a single-use `jti` shared by every replica. Failures are throttled per client source and refused with one opaque error, and audited; a request the gate's key signed still goes through from a throttled source.
  - `POST /v1/_auth/assert {gate, assertion}` — the person's issued token, held to `issued_token` (an assertion can only narrow it). This is what a bridge does per person.
  - `POST /v1/_auth/revoke {gate, request}` — the person is gone; they are removed by the removal policy below. A gate can revoke only users linked to it.
  - `POST /v1/_auth/sync {gate, request}` — bulk provisioning and group sync. With `complete: true`, from a gate whose `identity_map` says `authoritative_for_users`, the users it omits are removed. A complete sync of nobody is refused, and so is one that would remove more than `max_sync_removals` (a count, `25`, or a share of the gate's linked users, `20%`; default 10%, never fewer than 5): `409 sync_removal_limit`, and nothing changes. One sync lists at most 10,000 users, so a gate with more cannot send a complete sync yet.
  - With `callback_url`: `GET /v1/_auth/login/{gate}` sends the browser to the gate with a one-time `login_state`, and the gate sends it back to `/v1/_auth/complete/{gate}` with the assertion. The state is bound to the browser that began the login.
  - `GET /v1/_authgate` (admin) lists the gates with each key's fingerprint, never the key; `?probe=1` also reads each gate's `/.well-known/loomcycle-authgate`.
- **What a gate may not say.** The tenant and the reach come from the `identity_map`, not the assertion. Only a gate marked `trusted: true` may assert an `access_mode` or groups, and only its own `<gate>:` groups. The sign-in method it reports is not taken: its sessions and tokens show `amr: [ext]`, so a gate can never stand in for a second factor. Its `auth_time` is never later than when it signed.
- **Keys.** `public_key` is an Ed25519 or P-256 public JWK. `hmac_secret_env` names a `LOOMCYCLE_*` env var holding an HS256 secret (at least 32 bytes); a literal or a `$cred:` reference is refused. A key is its gate's alone: two gates with the same key (one public key, or one secret under one env var or two) fail the load, since the gate's name is only a claim the key holder writes, and so does an `hmac_secret_env` that also holds a principal's `token_env` or a gate's `client_secret_env`. A gate is registered in config only: its key is a trust anchor, and one an API call could add is one a stolen admin token could add too.
- **Issued tokens** stop the moment their user is not `active` (disabled, revoked, pending), or their gate is taken out of the config, checked on every request on every replica. `issued_token.ttl` is at most 30 days; `max_active` (0–100) caps a user's live tokens, retiring the oldest.
- **Removal.** A revoke, an authoritative sync, or (with `auth.lifecycle.dormant_after`) no sign-in through any gate for that long starts a removal in three stages that never skip:

  ```yaml
  auth:
    lifecycle:
      on_removed: { disable: at_once, unbind_after: 7d, erase_after: never, reactivate: operator }
      dormant_after: never     # or e.g. 90d; at least 24h
  ```

  **Disable** at once: sessions end, issued tokens retire, nothing is deleted. **Unbind** after `unbind_after`: gate-managed group memberships and the user's own ACL bindings go, and the login identities stay, so a returning person is recognised. **Erase** after `erase_after` (at least 24h): the subject erasure, by default never (an operator runs it), and only with `LOOMCYCLE_AUDIT_LOG_PATH` set. The due times are fixed when the removal starts. An operator who sets the user `active` again re-enables them at the new-user defaults, not their old groups.
- **A bridge as a gate.** `@loomcycle/bridge-core` 0.3 and later can run a bridge this way: its README lists the `BRIDGE_GATE*` settings. A person is the loomcycle user `<gate>:<platform id>`.
- **Keeping a bridge's existing users.** A bridge that ran with a loomcycle token made its users as `<platform>-<id>` (`telegram-42`, a group chat `telegram-conv-<hash>`). Set `identity_map.legacy_subject_prefix: telegram-` on its gate and a person's first login links that user instead of making `telegram:42`, so their runs, memory and documents stay theirs; the link is audited as `auth_identity_linked` with the reason `legacy subject`. The legacy subject is the prefix plus the platform id with `:` read as `-`. It links only a user in the gate's tenant that has no password and no identity at any gate, so a person's own account is never taken; such a user gets a new `<gate>:<id>` user instead. The prefix ends in `-` and holds no `:`, and only an external gate takes it. Once everyone has signed in through the gate, the prefix can go: the links stay.

### Header and AWS IAM gates — `kind: header`, `kind: awsiam` (RFC FA P3)

**Why a signed assertion, never an identity header.** A proxy in front of loomcycle (Cloudflare Access, an AWS ALB, Traefik/nginx forward-auth, oauth2-proxy) has often already signed the person in. Trusting a plain `X-Forwarded-User` from it is how the 2026 ForwardAuth header-smuggling CVEs happened: the proxy strips `X-Forwarded-User`, the client sends `X_Forwarded_User`, and a backend that merges the two believes the client. So a header gate believes only a **signed** assertion, only from the proxy's own addresses, and strips every alias of its header (any spelling that differs only in case, `-`, `_`, `.` or other punctuation) from every request before any handler runs.

```yaml
auth:
  gates:
    cloudflare:
      kind: header
      preset: cloudflare-access          # Cf-Access-Jwt-Assertion, RS256, the team's JWKS
      team_domain: acme.cloudflareaccess.com
      audience: 9f3c…                    # the Access application's AUD tag
      trusted_proxy_cidrs: [172.18.0.0/16]   # the cloudflared container's network
      identity_map: { tenant: acme, provision: known }
    alb:
      kind: header
      preset: aws-alb                    # x-amzn-oidc-data, ES256, key per kid from AWS
      alb_arn: arn:aws:elasticloadbalancing:eu-west-1:123456789012:loadbalancer/app/web/50dc6c495c0c9188
      issuer: https://login.acme.com     # optional: the IdP the ALB is configured with
      client_id: loomcycle-web           # optional: the ALB's OIDC client id
      trusted_proxy_cidrs: [10.0.0.0/20] # the ALB's subnets
      identity_map: { tenant: acme, provision: auto }
    proxy:
      kind: header
      preset: forward-auth-hmac
      secret_env: LOOMCYCLE_GATE_PROXY_SECRET   # the operator's environment only
      trusted_proxy_cidrs: [127.0.0.1]
      identity_map: { tenant: acme, provision: auto }
    ci:
      kind: awsiam
      server_id: loomcycle.acme.internal        # default: LOOMCYCLE_PUBLIC_URL's host
      allowed_arns: ["arn:aws:iam::123456789012:role/ci-*"]
      sts_regions: [eu-west-1]                  # regional STS hosts accepted besides sts.amazonaws.com
      identity_map: { tenant: acme, subject: arn, provision: auto }
```

- **Every key is required where it guards something.** `trusted_proxy_cidrs` is required for every header preset (a header from any other peer is refused, and `0.0.0.0/0` is refused at load). `aws-alb` needs `alb_arn`: the regional key endpoint serves keys for **every** ALB in the region, so a valid signature from an ALB in someone else's account verifies unless its `signer` is checked (the "ALBeast" bypass). `awsiam` needs `allowed_arns` with a literal 12-digit account in each pattern, because STS answers for every AWS account in the world (`*` may appear only after the account). Both kinds need an `identity_map`: the gate says who someone is, never which tenant they belong to. An unknown preset, a key of another preset, and a literal secret are load errors (`loomcycle validate` reports them), and so are two header gates of one preset with different `trusted_proxy_cidrs`: they read the same headers, and a header one gate does not trust from a peer is removed before the other gate sees it.
- **forward-auth-hmac**, the format your proxy implements: on each request it sets `X-Loomcycle-Identity` to a JSON object (`sub` required; `email`, `email_verified`, `name`, `groups`, `tenant` optional) and `X-Loomcycle-Identity-Signature: t=<unix seconds>,n=<nonce>,v1=<hex>`, where `v1` is HMAC-SHA256 under the shared secret of `"loomcycle-identity-v1\n" + t + "\n" + n + "\n" + <the identity header, byte for byte>`, `n` is 16–128 random `[A-Za-z0-9_-]` characters, new per request, and `t` within five minutes of the server's clock. A nonce is accepted once. The secret is at least 32 bytes; `secret_env` must be a `LOOMCYCLE_*` (or allowlisted) name that is not one of loomcycle's own secrets. It is never a literal in the file and never a stored credential (`$cred:`): a secret that verifies identities must be the operator's, since a tenant able to set it could forge any identity the gate maps into that tenant.
- **How a browser signs in.** The login page shows "Continue as signed in by *label*" (the label defaults to "Cloudflare Access", "your AWS load balancer", or the gate's name). The button's same-origin `POST /v1/_auth/complete/<gate>` carries the proxy's assertion; a verified one is mapped through `identity_map` (provisioned per `provision`, with the `auth.lifecycle` defaults and group sync for a `trusted` gate) and becomes a session. The page never signs in by itself when it opens. **The session stays bound to the proxy:** every request it authenticates must carry a valid assertion of the same gate that maps to the same tenant and subject, or the session ends. Signing out of Cloudflare Access ends the loomcycle session on the next request, and a copied cookie does nothing without the person's edge session. When the gate itself cannot check an assertion (the JWKS or the ALB key endpoint unreachable, the replay record down) the request is refused but the session is kept, and a sign-in answers `503 gate_unavailable`: an outage does not sign everyone out. Cloudflare Access issues a token only after its identity provider (or its one-time PIN) authenticated the person, so the gate treats the token's e-mail as verified; with `tenant_claim: email_domain`, put an identity provider behind Access that verifies addresses.
- **How a machine gets a token (`kind: awsiam`).** The workload SigV4-signs an `sts:GetCallerIdentity` request (with `X-Loomcycle-AWS-Server-ID: <server_id>` among the signed headers) and posts it, unsent, to `POST /v1/_auth/assert` as `{"gate": "ci", "iam_http_request_method": "POST", "iam_request_url": b64, "iam_request_headers": b64(JSON), "iam_request_body": b64}` — Vault's AWS login field names, so its client helpers produce the body. loomcycle sends it only to `sts.amazonaws.com` or an `sts_regions` host (never to the URL the client names), accepts each signature once on every replica, and maps the answered ARN (an assumed role as its role ARN) through `allowed_arns` and `identity_map`. The answer is an issued token: the user's access_mode scopes, tagged with the gate, living at most an hour (GetCallerIdentity does not say when the caller's credentials expire, so it takes an assumed-role session's default), and never beyond `identity_map.new_user.token_ttl` or `LOOMCYCLE_USER_TOKEN_MAX_TTL_SECONDS`. A refusal is the external gates' opaque `401 invalid_assertion`, whatever the reason, so the route does not say which gates are AWS IAM gates; refused attempts are throttled per client address (`429 throttled`) in the same count as the external gates' refusals. A throttled address is still verified, because workloads share egress addresses (a NAT gateway), and one failing must not lock the others out; it may hold only two STS checks at a time on each replica. A signed request whose signed headers include a hop-by-hop or encoding header (`Accept-Encoding`, `Connection`, `Transfer-Encoding`, …) is refused before STS is asked. If the replay record cannot be written, the login answers `503 gate_unavailable` and is not counted. STS puts no authorization in front of GetCallerIdentity, so MFA cannot be required here: this gate is for machines, and it never appears on the login page.
- **Restart, not reload.** Like the password gate, the edge gates are built at boot; a reload that adds one leaves it refusing until a restart.
- **`loomcycle doctor`** asks each endpoint an edge gate reaches at sign-in time (a `cloudflare-access` team's JWKS, which must answer 200; the ALB region's key host; STS, globally and in each `sts_regions` region) and warns about one it cannot reach.

### App sign-in — `auth.clients` and ClientDef (RFC FL)

**Why loomcycle signs its apps in, and not each app.** An app such as loomboard (web, Chrome extension, desktop) should not ask a person to paste a token, nor carry its own copy of every gate and its secrets. loomcycle is an **OAuth 2.0 authorization server for its own registered apps**: the app sends the person to loomcycle's sign-in page, which offers exactly the gates configured above, and gets back a short-lived token for that same user in that same tenant. The app never sees a password, a provider secret or the gate configuration, and every call it makes is authorized as the user is anywhere else (`access_mode`, groups, ACL bindings, `pending`/`disabled`). It is not an OpenID Connect provider (no `id_token`) and has no consent screen: only clients the operator registered exist.

```yaml
auth:
  clients:
    loomboard:
      kind: public                      # no secret; PKCE S256 is mandatory (the only kind)
      label: loomboard                  # what the sign-in page shows
      redirect_uris:
        - https://board.example.com/auth/callback       # web: exact match
        - https://<extension-id>.chromiumapp.org/        # Chrome extension
        - http://127.0.0.1/callback                       # desktop: loopback, any port, exact path
        - loomboard://auth/callback                       # desktop: private-use scheme
      allowed_origins: [https://board.example.com]      # CORS on /v1/_oauth/token and /revoke only
      access_token_ttl: 1h              # 1m–24h
      refresh: { idle: 7d, absolute: 30d }   # Go durations or whole days; absolute ≤ 365d
      restrictions: {}                  # narrows every token of this client (agents, overrides, rate, retention)
```

**Redirect URIs** are `https` with a host; `http` only to the loopback literals `127.0.0.1` or `[::1]` (any port at sign-in, path exact — a desktop app listens where the OS lets it); or a private-use scheme that names the app (`loomboard://…`, `com.example.app:/…`). Never `http` to another host, a fragment, a wildcard, userinfo, or a browser scheme (`javascript:`, `data:`, …). They are matched **exactly**. A redirect URI decides where a person's sign-in code goes: list only addresses the app really owns.

**The flow** (authorization code with PKCE; RFC 6749, 7636, 8252, 9700):

1. The app opens `GET /v1/_oauth/authorize?response_type=code&client_id=…&redirect_uri=…&code_challenge=<S256>&code_challenge_method=S256&state=…`. An unknown client or an unregistered `redirect_uri` ends on loomcycle's own error page — it never redirects there.
2. With no session in this browser, loomcycle sends the person to `/ui/login?oauth=<the request>`; the page names the app and where the sign-in goes, offers the configured gates, and returns to the authorize step once they are signed in. A browser already signed in to the Web UI goes straight through (single sign-on).
3. loomcycle redirects to `redirect_uri?code=…&state=…&iss=…`. The code is single-use, lives 60 s, and is bound to the client, the exact `redirect_uri`, the PKCE challenge, the user and the session. A `pending` user gets `error=access_denied&error_description=pending` and no code.
4. The app exchanges it: `POST /v1/_oauth/token` (form-encoded, nothing in the URL) with `grant_type=authorization_code, code, redirect_uri, client_id, code_verifier` → `{access_token, token_type: Bearer, expires_in, refresh_token}`, `Cache-Control: no-store`.
5. Every `/v1/*` call carries `Authorization: Bearer <access_token>`; `GET /v1/_me` reports `auth.app`, `auth.gate` and `auth.amr`.
6. `grant_type=refresh_token` rotates the refresh token. Presenting a used one again revokes the whole sign-in (every token it issued). A refresh is refused for a revoked or expired sign-in, a user who is no longer `active`, a client that was removed or retired, and a gate taken out of the config.
7. `POST /v1/_oauth/revoke` (RFC 7009) — a refresh token ends its sign-in, an access token is retired. Signing out of the Web UI does **not** end app sign-ins.

**The token never exceeds the user**: it is an ordinary gate-issued user token (scopes from the user's `access_mode`, named `u-app-<client>`, tagged with the gate that signed the person in), capped by the signing-in session's own scopes and restrictions and by the client's `restrictions`. The tenant is the session's, never the request's. It stands only while its client does: removing a client from the config or retiring its ClientDef stops its tokens on their next use.

**Metadata** (RFC 8414) is published at `GET /.well-known/oauth-authorization-server` with `issuer` = `LOOMCYCLE_PUBLIC_URL` (404 until that is set). The endpoints are throttled per client and client address. Audit actions: `oauth_code_issued`, `oauth_token_issued`, `oauth_code_reuse`, `oauth_refresh_reuse`, `oauth_revoked`, `oauth_refused`, `oauth_token_refused`.

**Runtime clients (ClientDef).** A client can also be registered at runtime, with the same fields under `client`, through `POST /v1/_clientdef` (`op`: `create`, `update`, `get`, `list`, `retire`, `verify`; `GET /v1/_clientdef/names`), the MCP `clientdef` tool and the gRPC `ClientDef` RPC:

```json
{"op": "create", "name": "acme-board", "description": "Acme's board",
 "client": {"kind": "public", "redirect_uris": ["https://board.acme.example/cb"], "allowed_origins": ["https://board.acme.example"]}}
```

- **Who**: an admin for any tenant (`"tenant": "acme"`), or for every tenant (no `tenant`); a tenant operator (`substrate:tenant`) for its own tenant only, asked the ACL verb `author def:client/<name>`. A member cannot. Config clients are listed read-only and are never changed or retired through the API.
- **Whom**: a tenant's client signs in only that tenant's people. Anyone else — including a person already signed in to another tenant — is stopped on loomcycle's page and gets no code. Config and every-tenant clients serve every tenant; the token is always confined to the user's own tenant.
- **The name is the `client_id`**, which travels in every authorize link, so it belongs to one tenant across the deployment: another tenant's create of a taken name (even a retired one) is refused.
- **After**: `update` writes a new version and voids the client's unused codes; `retire` also revokes every sign-in made through it and retires their access tokens at once, on every replica. Every create, change and retire is audited (`client_def_created|changed|retired`) with its author and redirect URIs.
- ClientDefs are not carried in snapshots: a restored one would send this deployment's sign-in codes to the source's apps.

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
- **Declaring a principal turns authentication on.** With at least one entry in `principals:`, the server is not in open mode even with no `LOOMCYCLE_AUTH_TOKEN` and no minted admin token: a request without a valid bearer gets `401` (HTTP, gRPC, A2A), `GET /v1/_me` reports the caller's own principal (no `open_mode`), and the server boots on `0.0.0.0`. An **inert** principal counts too: it authenticates nobody, but the declaration says a bearer is required, so a missing env var never opens the server. A config reload that would drop the last principal is refused unless the new config sets `LOOMCYCLE_AUTH_TOKEN`; a minted admin token does not count, because it can be retired right after (from another replica, say) and leave the server open. To move a deployment from principals to minted tokens alone, restart.
- **Who is admin.** Only a principal that lists `substrate:admin` (or the legacy token, or a minted admin token) can mint tokens and use the admin routes. A deployment whose principals are all tenant-scoped has no admin: add an admin principal or set `LOOMCYCLE_AUTH_TOKEN` when you need one. Declaring principals does **not** retire the legacy token (only a minted admin token does). Retiring the last minted admin token stays refused unless the legacy token is set or an admin principal can log in — both judged on the current config, so a reload that adds either one lifts the refusal at once — and a config reload that drops the last admin principal is refused unless the new config sets `LOOMCYCLE_AUTH_TOKEN` — a minted admin token does not count, for the same reason as above; to move admin to minted tokens alone, restart.
- **Resolution order:** minted `OperatorTokenDef` → **declared principal** → legacy token. A token value shared by two declared principals is a config-load error; an empty `token_env` at boot makes that principal **inert** (a startup warning, not an open door).
- **The payoff — alignment by construction.** Use one declared token for **both** the Web UI login (`/ui/login`) and an MCP thin client (`LOOMCYCLE_MCP_UPSTREAM_TOKEN`); both resolve to the same `(tenant, subject)`. Combined with the per-principal `/v1/_mcp` transport (RFC AG), an MCP agent's user-scoped Documents/Memory land under the same user the UI reads — no synthetic-operator mismatch.

**Trigger-spawned runs choose their tenant in the def (RFC N).** An interactive run inherits its tenant from the caller's token, but a scheduler- or webhook-spawned run has no inbound bearer — so the run-execution tenant is declared in the def via `tenant_id:` on a `scheduled_runs:` entry or a `webhooks:` entry. The spawned run then resolves that tenant's agents/skills/MCP and isolates its memory/runs. It is operator-authored def-content (`""` = shared/default). **Security: for webhooks the tenant comes from the static def ONLY — never from the inbound `payload_mapping`** (the attacker-influenceable body must not be able to select another tenant). See `Context.help scheduled-runs` / `Context.help input-webhooks`.

**The webhook def itself is tenant-isolated too (RFC N completion).** Beyond the run-execution `tenant_id` above, every substrate Def's *active pointer* is keyed on `(tenant_id, name)` — a webhook authored by a tenant principal is owned by that tenant and addressed on its own inbound route: `POST /v1/_webhooks/{tenant}/{name}`. The bare-root `POST /v1/_webhooks/{name}` resolves under the shared `""` tenant, so an existing single-tenant deployment (everything `tenant_id=""`) keeps using the unprefixed URL unchanged. A multi-tenant downstream that authors webhooks under a non-empty tenant **must register its delivery URL with the `/{tenant}/` prefix** for inbound deliveries to resolve. (The admin-only triage endpoints — the dry-run `POST /v1/_webhooks/{name}/test`, `GET /v1/_webhooks/{name}/recent-deliveries` and `GET /v1/_webhooks/{name}/delivery-counts` — resolve under `?tenant=` when given, else the bearer's own principal tenant; pass `?tenant=<tenant>` to reach a tenant-owned webhook.)

---

### Managing groups, ACL lists and bindings

The access-control management surface (the `LOOMCYCLE_ACL` row above lists its routes) is served on four transports through one implementation: the HTTP routes, the gRPC `GroupDef` / `AclDef` / `AclBinding` / `Authz` RPCs, the MCP meta-tools `groupdef` / `acldef` / `aclbinding` / `authz`, and the `loomcycle acl` CLI. The twins take the route's JSON body plus `op` (and an admin's `tenant` focus) and answer with the route's JSON body; every gate — tenant confinement, the tenant-operator floor for management, each op's own access-control question, the self-elevation guards, audit and cross-replica invalidation — is held by that implementation, so no transport can reach the logic around it, and none of it is reachable from inside a run.

| Family | Ops (twins) | HTTP |
|---|---|---|
| groups | `list`, `get`, `effective`, `create`, `delete`, `add_member`, `remove_member` | `/v1/_groups`, `/v1/_groups/{name}`, `/v1/_groups/{name}/effective`, `/v1/_groups/{name}/members[/{kind}/{id}]` |
| lists | `create`, `update`, `put`, `get`, `list`, `retire`, `validate`, `catalog`, `names` | `POST /v1/_acldef`, `GET /v1/_acldef/names` |
| bindings | `list`, `create`, `delete` | `/v1/_aclbinding`, `/v1/_aclbinding/{id}` |
| questions | `evaluation`, `evaluations`, `explain`, `simulate` | `POST /v1/_authz/*` |

- **`GET /v1/_groups/{name}/effective`** returns every user the group holds, through the groups nested in it at any depth, config-declared members included: `{"group", "users": [...], "groups": [nested groups walked]}`.
- **Config-declared entries are listed.** The list of ACL lists (and `names`, and `get`), of groups and of bindings include what `acls:` / `groups:` / `acl_bindings:` declare, each marked `"source": "config"` and read-only (a stored row carries no `source`). A config list is shown only where a binding would resolve to it: a live stored list of the same name — in the tenant or the shared tenant — shadows it, as the resolver does. A config group whose name the store also has is shown as the stored row; the resolver unions the two groups' members.
- **A binding that could never be in force is refused** (`400 acl_binding_invalid`): one to a list no layer has (not stored live in the tenant or the shared tenant, not declared in config; a retired list and a built-in name count as missing), and one whose `expires_at` has already passed. `"expires_at": ""` means no expiry.
- **A group synced from an identity provider** (`external_id` set) is read-only here: a member add or remove, and an upsert over the group, are refused with `409 group_external`; manage its members at the provider.
- **Unset times are omitted.** A live list has no `retired_at`; a binding with no expiry has no `expires_at` (they used to read `0001-01-01T00:00:00Z`).
- `GET /v1/_me` reports `acl_mode` in open mode too.
- **Subject erasure removes a user's memberships, bindings and member tokens.** `GET /v1/_erasure` (and the `erasure` MCP tool / gRPC twin) counts them in `tier2_uncovered` as `acl_memberships`, `acl_bindings`, `operator_tokens` and `acl_token_bindings` — retired bindings included, since they still name the user — and names them in the structured `access` field: `{groups, lists, tokens: [{def_id, name}]}` (never a bearer); a note says the same in prose. A live `POST /v1/_erasure` first revokes the user's member tokens — the ones user delete/disable retire — so the bearer stops authenticating at once on every replica, then deletes the memberships, the user's bindings and the bindings to its revoked tokens in one transaction, bumps the tenant's `acl_version`, drops the cached sets on every replica and records `subject_access_erased` in the audit log, as a management write does; `access` names what went. A dry run revokes and writes nothing and names what would go. A token holding `substrate:tenant` or `substrate:admin` that shares the subject is a role, not the member's credential: it is left, and named under `retained.operator_tokens_privileged` — retire it with `operatortokendef`. Revoked token rows are retired, not deleted (`retained.operator_token_rows`). The groups and lists themselves and group-kind bindings stay. Memberships and bindings declared in `groups:` / `acl_bindings:` cannot be erased from the store: they are listed in the report's notes and in the result's `retained.access_control_config` — remove them from the config file. Every live erasure, over HTTP, gRPC or MCP, writes the `erase_intent` / `erase_result` audit pair, and is refused (HTTP 503, gRPC `Unavailable`, an MCP tool error) when the intent cannot be recorded.

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
- **Transcript plane — redact before persisting (`LOOMCYCLE_REDACT_SECRETS`, default ON).** Tool I/O (tool_call inputs + tool_result outputs) is scanned for secret-shaped substrings and masked before it reaches the events store (and thus snapshots + the `/v1/_events` audit API): the exact values of secret-named env vars (`*_KEY` / `*_TOKEN` / `*_SECRET` / `*_AUTH` / `*_PASSWORD` / `*_CREDENTIAL`) become `[redacted:NAME]`, plus conservative heuristics for `Authorization:` headers, `sk-`/`AKIA`/`xox` keys, GitHub tokens (`ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_`, `github_pat_`), GitLab tokens (`glpat-`), and `*_API_KEY=` assignments. The value of a signed-URL credential parameter in a URL query is masked too, with its name kept (`tempauth=[redacted:signed-url]`): `tempauth`, `sig`, `X-Amz-Signature`, `X-Amz-Credential`, `X-Amz-Security-Token`, `X-Goog-Signature`, `X-Goog-Credential`, `access_token` and `token`, plus CloudFront's `Signature` beside `Key-Pair-Id` or `Policy` and an OAuth `code` beside `state`; the same words outside a URL query are left alone. The live SSE stream is **not** redacted (the caller already holds the secret). Set `LOOMCYCLE_REDACT_SECRETS=0` to disable. This is defense-in-depth — agents should still pass secrets out-of-band (env / stdin / credential helper), never inline on a cmdline.

**Secrets on read.** A yaml `mcp_servers.*` entry's `${NAME}` references are expanded on the raw yaml before it is parsed, so the running config holds the values, not the references. The Library read (`GET /v1/_library/mcp-servers`, and the Web UI Library page over it) therefore masks every header value (keeping an auth scheme: `Bearer ****`), every `env` value, and the `url`'s userinfo, query values and fragment as `****`, then applies the redactor above to the rest (a secret-named env var's value in `args`, `command` or the URL path becomes `[redacted:NAME]`). The Library editor refuses to save a fork that still holds a `****`: enter the value, or a `$cred:<name>` / `${run.credentials.<name>}` reference.

**Interactive runs.** A top-level interactive run, and an interactive session continuation, waits parked at `awaiting_input` for its operator's next message. One that waits longer than the idle timeout ends: status `completed`, stop reason `idle_timeout` on the run and on its `done` event, so a client can show "ended after being idle — send a message to continue". Its session lock, run slot and goroutine are released and its session is kept, so the next message continues it (`POST /v1/sessions/{id}/messages`, or `POST /v1/runs` with `session_id`), with its history. A message sent to the ended run (`POST /v1/runs/{id}/input`) is refused 404 and says to continue the session; one that arrives as the deadline passes is taken and the run goes on. Each park gets the full window, and a deadline that passes while the runtime is paused waits for the resume. A run restored after a restart or a snapshot gets the window again from its resume. Review holds (`review_ttl_seconds`) and resident sub-agents (`LOOMCYCLE_INTERACTIVE_CHILD_IDLE_TTL_MS`, below) keep their own bounds.

| Env var | Default | Bounds |
|---|---|---|
| `LOOMCYCLE_INTERACTIVE_IDLE_TIMEOUT` | `1h` | How long a parked interactive run waits for a message before it ends. A Go duration (`30m`, `2h`); `0` waits forever. A value that does not parse is refused at startup. |
| `LOOMCYCLE_BATCH_JOIN_TIMEOUT` | `1h` | How long a batch in mode `join` (`POST /v1/runs:batch`, MCP `spawn_runs`, gRPC `SpawnRunBatch`) waits for its children when the request gives no `timeout_ms`; a request's `timeout_ms` replaces it, larger or smaller. When it passes the join only stops waiting: no child is cancelled. Each child still going comes back in its slot with its current status (`running`, or `queued`), its `run_id` and an `error` saying the join timed out, and goes on after the call returns (read it by `run_id`, stop it with cancel); the finished children are reported as usual. Only a request's own `timeout_ms` cancels the batch's unfinished children; a child joined to an existing run by `idempotency_key` is never cancelled. It also bounds a single blocking `spawn_run` (MCP, connector) whose `idempotency_key` joins a run another request started, when the caller set no deadline of its own (no `timeout_ms`, no `LOOMCYCLE_MCP_SPAWN_RUN_TIMEOUT_MS`): at the bound that run is reported as it is, with the same "join timed out … the run goes on" `error`, and is never cancelled. A Go duration; `0` = no server bound (the join still ends when the caller's deadline or connection does). A value that does not parse is refused at startup. |

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
| `LOOMCYCLE_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`; lines below it are dropped. Every runtime line is levelled, so `warn` or `error` also hides the Info lines, "listening on" among them; fatal and startup errors are logged at `ERROR` and show at every level. An unknown value warns and uses `info`. |

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
- **A blocking tool call** — an `Interruption` waiting for a person's answer, a `Channel` await, an `Agent` wait or poll — is interrupted like any other call and fails. The resumed run may ask again, so a person can see the same interruption twice: the first one, from before the shutdown, is no longer waited on.
- **A run a background sweep started** — a memory-consolidation fan-out pass, a team subscription's walk — is cancelled once the pause is over, even if it parked (the shutdown log counts it among the paused): the sweep holds a cluster lock, a pinned database connection, until the run returns, and a run left parked would hold the shutdown until the container is killed. The next tick or sweep does the work again. A consolidation pass can still come back through a resume — the process was killed before that cancel was stored, or a snapshot taken during an operator pause was restored — and then runs outside the sweep that started it; the next tick leaves its target alone while that pass's run has not ended — for at most 4 hours while it sits parked waiting for a resume, after which a warning names the stuck run and the target is consolidated again. The hold is by consolidator agent and user: two schedules running the same consolidator agent for one user hold each other off. In a cluster, a parked run keeps the `LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER` slot its admission took until its resume gives it back; a run admitted with that cap off took none, and its resume gives nothing back ([MULTI-REPLICA.md](MULTI-REPLICA.md) item 10). On slow models, raise the bound **together with** the platform's grace period, leaving a few seconds over it: `stop_grace_period: 40s` in Docker Compose, `terminationGracePeriodSeconds: 40` in Kubernetes, `TimeoutStopSec=40` in systemd, with `LOOMCYCLE_SHUTDOWN_PAUSE_TIMEOUT_MS=30000`.

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

**The Git tool shares these hosts and limits.** Its `clone`, `fetch`, `pull` and `ls_remote` reach only `LOOMCYCLE_GIT_SOURCE_HOSTS`, under the same byte, object, time and concurrency limits; an agent needs `git_remote: [fetch]`. `LOOMCYCLE_GIT_SOURCE_MAX_FILES` also bounds how many entries a checkout, merge, pull or reset may write (with a 1 GiB byte bound); `MAX_FILE_BYTES` does not apply to the Git tool. Push and commit identity have their own settings:

| Setting | Default | Meaning |
|---|---|---|
| `LOOMCYCLE_GIT_PUSH_HOSTS` | unset: no push | hosts a push may reach, comma-separated, as `host` or `host:port`. An agent also needs `git_remote: [fetch, push]` |
| `LOOMCYCLE_GIT_PUSH_MAX_BYTES` | 100 MiB | bytes one push may send. A malformed or non-positive value refuses the boot |
| `LOOMCYCLE_GIT_AUTHOR_NAME` | the agent's name | author and committer name of every commit the Git tool makes |
| `LOOMCYCLE_GIT_AUTHOR_EMAIL` | `<agent>@loomcycle.invalid` | their email |

Every commit also carries a `Loomcycle-Run: <run id>` trailer. See `docs/TOOLS.md`, "The `Git` tool".

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


### 9d.2.3 The run inbox (files tools return)

The runtime makes one ephemeral volume of its own per run tree: **`inbox`**, at `<dynamic_root>/_ephemeral/<root_run_id>/inbox`. A file that a tool returns is kept there instead of reaching the model as bytes; this covers an MCP image/audio/blob block, base64 from a server with `binary_results: capture`, and a binary HTTP or WebFetch body. The model gets a one-line reference such as `volume:inbox:documents/report.pdf` that Read, Crypto and the `File` tool take (see [TOOLS.md](TOOLS.md), "Files tools return").

- **Requires a `dynamic_root` volume.** Without one, nothing is kept and every file becomes a refusal line. It is made on the first capture, recorded like any ephemeral volume, and purged with the tree, with the same crash sweeper. The TrueNAS app ships one (a `dynamic` volume on its own dataset, mounted at `/mnt/dynamic`); a hand-written deployment must declare one.
- **The name is reserved.** `VolumeDef op=create` refuses `inbox`, persistent or ephemeral. A tenant's existing persistent volume, or a configured `volumes:` entry, named `inbox` makes every capture refuse rather than be shadowed: rename it.
- **Runtime writes are not agent writes.** Captures, downloads and extractions are written by the runtime, so a sub-agent whose ephemeral volumes are read-only still has its results kept. Its own `File rm`/`clear` are refused, like any write.
- **Visible to operators.** The Volumes page lists a live inbox with its tenant's ephemeral volumes, and the MCP `file` tool reads, imports or copies out of a run's inbox by `run_id` (changing it needs the run's owner or an admin).
- **Caps** (refuse the boot on a bad value):

| Variable | Default | Bounds |
|---|---|---|
| `LOOMCYCLE_FILE_MAX_BYTES` | 64 MiB | one kept or downloaded file |
| `LOOMCYCLE_FILE_INBOX_MAX_BYTES` | 1 GiB | the tree's whole inbox (a resumed tree counts what it already holds) |
| `LOOMCYCLE_MCP_MAX_FRAME_BYTES` | 16 MiB | one MCP response: a stdio JSON-RPC line, or an MCP HTTP body |
| `LOOMCYCLE_FILE_BASE64_MIN_BYTES` | 4096 | the shortest base64 string a `binary_results: capture` server's text is searched for |

Since RFC FF, `/loomcycle/rfcs/files-by-reference`.

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
- **A walk runs as its promoter.** Nobody asks for a subscribed walk, so it is
  confined by whoever last promoted the team: that caller's operator-key
  restriction and isolation, and (with access control, `LOOMCYCLE_ACL`) that
  caller's rules as they stand at each walk — `run team:<name>` is asked for
  the promoter, and every member is narrowed by the promoter's rules and asked
  `spawn agent:` for them. A deny, or rules that cannot be read, starts no walk
  and leaves the message on the source. Promoting is what arms a team, so
  re-promoting is how you change whose authority it runs under. A team promoted
  before the promoter's confinement was recorded runs isolated, and one promoted
  before the promoter's identity was recorded runs without rules; both log once,
  and promoting the team again fixes either.
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
| `transcription.stt.model` (§9h) | `transcriber` — the tag is required here; an untagged alias is refused |
| `speech.voices.<language>.model` (§9i) | `synthesizer` — the tag is required here; an untagged alias is refused |

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

**Seeing them at run time.** `GET /v1/_routing` and the Web UI's routing page show both kinds under "service models":

- **The embedder**, with the alias `memory.embedder` names and what its calls have done since boot: `untried` (nothing embedded yet, so unknown), `ok` or `failing`, with counts. This is observed, not probed, because the embedder may call a host other than its provider's.
- **Each decision model**, with its provider's reachability and whether the provider lists the model. For Ollama, listed means pulled; a bare name also matches its `:latest` tag.
- Under `LOOMCYCLE_OPERATOR_KEY_RESTRICTION` a tenant sees only the decision models it can key.

See [`docs/DECISION-MODELS.md`](DECISION-MODELS.md) for the request and answer formats, the four surfaces and billing.

## 9h. Transcription — OCR and speech-to-text

> **OCR and speech-to-text both run.** OCR on `File op=extract`, speech-to-text on `File op=transcribe` (and `extract` on a recording); see the `File` tool in `docs/TOOLS.md`. The `command`, `http` and `gemini-audio` engines arrive in a later release.

Scans, photos of documents and recordings carry no text an extractor can read. The `transcription:` block names the engines that turn them into text, and what a run may spend. Absent, both engines are off.

```yaml
providers:
  whisper-local:              # any server with the OpenAI /v1/audio/transcriptions shape
    driver: openai
    base_url: http://speaches:8000/v1   # includes /v1
models:
  whisper: { provider: whisper-local, model: <the model id your server serves>, kind: transcriber }

transcription:
  ocr:
    engine: vision            # required: vision | off
    tier: middle              # default middle; resolved like the describe-images pass
    model: ""                 # optional models: alias (kind: chat); pins the model, tier is then ignored
    language: uk+ru           # default uk+ru; up to three codes joined by +
    max_pages_per_call: 100   # default 100 (never above max_pages_per_run)
    max_pages_per_run: 300    # default 300
    ocr_min_chars_per_page: 40   # default 40; below this a page counts as a scan
    max_tokens_per_page: 2000    # default 2000
    rasterize: { command: "pdftoppm -r 150 -png {in} {out_prefix}" }   # optional; must contain {in}
    prompt_template: ""       # optional; replaces the built-in OCR instruction (operator-only)
  stt:
    engine: openai-audio      # required: openai-audio | off
    provider: whisper-local   # required for openai-audio: a provider on the openai driver
    model: whisper            # required for openai-audio: an alias tagged kind: transcriber, naming a model
    language: uk              # default uk; exactly one code
    vocabulary: ""            # optional names and terms; folded to one line, at most 400 characters
    diarize: auto             # auto | on | off, default auto
    max_minutes_per_call: 120 # default 120 (never above max_minutes_per_run)
    max_minutes_per_run: 300  # default 300
    chunk_minutes: 10         # default 10 (never above max_minutes_per_call)
    chunk_overlap_seconds: 2  # default 2; 0 means the default
    timeout_s: 1800           # default 1800; one engine request and one conversion
    media: { command: "ffmpeg -nostdin -loglevel error -i {in} -ac 1 -ar 16000 -f wav {out}" }   # optional; must contain {in} and {out}
```

- `loomcycle validate` fails on an unknown key, a missing `engine`, a negative number, a per-call cap above its per-run cap, an unknown language code, an OCR `model` that is not a chat alias, an STT `provider` not on the `openai` driver, an STT `model` not tagged `kind: transcriber` (or served by another provider, or given as a `model_pattern`), an OCR helper command without `{in}`, and a `media` command without `{in}` and `{out}`.
- `command` and `http` (both engines) and `gemini-audio` (STT) are refused as not available yet.
- Whether the OCR model can read images is checked when the engine is built, not at load.
- **Speech-to-text, how a recording is sent.** A PCM WAV is cut by the runtime into `chunk_minutes` pieces under 25 MB each, every piece after the first reaching `chunk_overlap_seconds` back; the overlap's repeat is removed. With `media.command` every other format is converted to a mono 16 kHz WAV first (the converter runs without a shell, scrubbed env, on a copy in a scratch folder of the inbox), then cut. Without it, a file goes whole only if the engine reads its format, it is under 25 MB and within `max_minutes_per_call`; otherwise the call is refused naming `transcription.stt.media.command` — never truncated.
- **Minutes.** A WAV is measured from its samples (a header whose byte rate disagrees with its sample format is refused). Anything else counts at least its size at 8 kbit/s and at least its header's claim, so a header can only over-count; the counter and the ledger are then reconciled to the duration the engine reports. Minutes are reserved on the run and the run tree before the first request and given back for pieces never sent.
- **A CPU-only whisper host.** The audio request uses a client with no response-header timeout, bounded by `timeout_s` (default 30 minutes); a 10-minute piece can take several minutes on CPU. A `large-v3-turbo` model (int8) is the CPU choice, `large-v3` with a GPU; the model id is whatever the whisper server lists. In containers, the `denngubsky/loomcycle-media:<version>` image (the same binary on Debian slim with `ffmpeg` and `poppler-utils`, amd64 and arm64) carries the converter and the PDF renderer.
- Speech-to-text is priced per minute of audio: `pricing.models."<provider>/<model>": { per_minute: 0.006 }`, billed by the second, plus token prices for a model that reports tokens (e.g. `gpt-4o-transcribe`).

**Per agent.** An agent opts in to OCR and may lower the caps:

```yaml
agents:
  doc-reader:
    transcription:
      ocr: auto                # auto | always | never — default never (OCR is billed per page)
      stt: on                  # on | off — default on when the operator configured an engine
      max_pages_per_run: 50    # at most transcription.ocr.max_pages_per_run
      max_minutes_per_run: 60  # at most transcription.stt.max_minutes_per_run
```

- A cap above the operator's is **refused** at config load and at AgentDef create/fork, naming both values. At run time the lower of the two applies, so an operator who lowers a cap later still binds stored agents.
- With no engine configured, the agent's setting is accepted and has no effect.
- The block is set in yaml or the AgentDef overlay, merges per field on a fork, and is part of the agent's content hash. A run cannot override it.

**What a run reports.** Each OCR or speech-to-text pass a `File` tool call makes emits one `transcription` event: on the SSE stream, as a transcript row, on gRPC as `Event.transcription` (`TranscriptionInfo`), typed in the TS and Python clients (`AgentEvent.transcription`), and as one line in the Web UI's run views (`⟲ OCR scan.pdf · 12 pages · claude-haiku-5-5 · $0.04 · ok`). It carries the source `ref` and the `out` ref of the Markdown written, `purpose` (`ocr` | `stt`), `engine`, the `provider` and `model` called, `pages` or `seconds`, `input_tokens` and `output_tokens`, `cost` and `currency` (absent when the pair is unpriced, which is not the same as free), `status` (`ok`; `partial` when a cap or an engine error stopped it after some units, and what was written stays; `failed`), `dry_run` (an estimate: no engine was called and nothing was spent), `error` (why the pass failed or stopped early: set on `failed`, and on `partial` when a cap or an engine error stopped it; never a secret, a URL's query or the file's content) and the `run_id` whose tool call made it. Never the text. It is a record for the operator, not model input: a resumed run's conversation does not include it.

**What it costs.** OCR and speech-to-text calls land in the same usage ledger as chat calls, tagged with a `purpose` (`ocr` | `stt`; the full list is below). `GET /v1/_usage?group_by=purpose` returns one row per purpose; purpose combines with the other dimensions (`group_by=tenant,purpose`, `provider,model,purpose`, …), and the rolled-up archive keeps purposes apart, so older windows split too. Every row also carries `audio_seconds` — the audio its speech-to-text calls transcribed — and a row with none omits the key, so a chat-only report reads as before. The gRPC `UsageReport` RPC (`UsageAggregate.purpose` / `audio_seconds`), the TS `usageReport({ groupBy: ["purpose"] })` and Python `usage_report(group_by=["purpose"])` clients, and the Web UI Usage page (a purpose chip, an "audio transcribed" total and an audio column in minutes) all carry it. Allowed dimensions: `tenant,user,provider,model,source,purpose`.

**What every usage row was for, and its tier.** Every model call the runtime makes on someone's behalf is a usage row, and its `purpose` says why. *Foreground* — what a person or caller asked a model for: `chat` (an agent's own turns) and `gateway` (a call through `/v1/_llm/chat` or `/v1/chat/completions`). *Background* — what the runtime asked a model for to serve it: `consolidation` (the turns of an `internal:` agent, i.e. memory consolidation), `rerank` (a Memory or Document search's rerank), `decision` (the Decision tool and `POST /v1/_decide`), `compaction` and `recap` (summaries), `unit_generation` and `image_description` (Document jobs), and `ocr`, `stt`, `tts`. Compaction and recap summaries, unit generation and image description were not billed before this version; they now count against the token budgets like any call. A summary is charged to its run (or, for a chat's recap, to the chat's tenant and user); a Document job started from an operator route to the tenant and user whose documents it worked on. Each row also records the run's **user tier** and, on an agent's own turns and gateway calls, the **model tier** it was resolved through (`pinned` for an agent or gateway request that names its model; empty on rows from before this version and on background calls, whose model the tier did not choose). The rolled-up archive keeps both tiers apart.

## 9i. Speech — text-to-speech

> **Configuration only in this release.** The block is read and validated, so `loomcycle validate` catches a bad value now; nothing speaks yet. `File op=speak` and the `POST /v1/audio/speech` gateway route arrive in later releases.

The `speech:` block names the engine that turns text into audio, which model and voice speak each language, the output format and what a run may spend. Absent, speech is off.

```yaml
providers:
  speech-local:               # any server with the OpenAI /v1/audio/speech shape (speaches, ptts, Kokoro-FastAPI, Ollama, OpenAI)
    driver: openai
    base_url: http://speaches:8000/v1   # includes /v1
  phonon-local:
    driver: openai
    base_url: http://ptts:8880/v1
models:
  tts-uk: { provider: speech-local, model: speaches-ai/piper-uk_UA-ukrainian_tts-medium, kind: synthesizer }
  phonon: { provider: phonon-local, model: phonon, kind: synthesizer, license: commercial }

speech:
  engine: openai-speech       # required: openai-speech | off
  default_language: uk        # required for openai-speech; one code, and it must have a voices entry
  voices:                     # language → model + voice; the only list a model may choose from
    uk: { model: tts-uk, voice: ukrainian_tts }
    en: { model: phonon, voice: Hadley, alternates: [Zoey, Easton, Freya] }
  format: ogg                 # ogg (Opus) | mp3 | wav — default ogg when encode is set, else wav
  encode: { command: "ffmpeg -nostdin -loglevel error -i {in} -c:a libopus -b:a 32k {out}" }   # needed for ogg and mp3; must contain {in} and {out}
  max_chars_per_call: 4000    # default 4000; at most 4096, what one speech request accepts
  max_chars_per_run: 100000   # default 100000
  max_seconds_per_run: 3600   # default 3600
  timeout_s: 600              # default 600; one engine request and one encode
```

- A voice's `model` must be a `models:` alias tagged `kind: synthesizer` that names its `provider` and a `model` (not a `model_pattern`), on a provider using the `openai` driver. `voice` is the engine's own voice id (speaches and ptts list them at `GET /v1/audio/voices`; Ollama with `ollama show <model>`); it is not checked against the engine at load. `alternates` are the other voices a caller may pick for that language.
- Voices are per language because no single self-hosted model speaks every language well; the keys are language codes from the same list as `transcription:` (`uk`, `en`, `fr`, …).
- The runtime asks every engine for WAV and joins the pieces itself, so `wav` needs nothing. `ogg` and `mp3` go through `encode`, which runs without a shell with a scrubbed env like the transcription helpers; `format: ogg` or `mp3` without `encode` **fails config load** naming `speech.encode` — it is never silently written as WAV.
- `loomcycle validate` fails on an unknown key (in the block, a voice entry or `encode`), a missing `engine`, `openai-speech` with no voices or no `default_language` (or one with no voices entry), an unknown language code, the same language twice, a voice model of another kind or untagged, with no provider, a pattern, or off the `openai` driver, an empty voice id or one with spaces at its ends or control characters, a repeated alternate, an unknown `format`, an `encode` command without `{in}` and `{out}`, a negative number, `max_chars_per_call` above 4096 or above `max_chars_per_run`.
- **Licences.** A `models:` alias may carry `license: commercial` or `license: noncommercial` (anything else fails load). Nothing refuses a noncommercial model; a speech voice using one adds a load warning naming it, and the Settings panel will show it. The Phonon 40m voices and its multilingual model are CC BY-NC: keep them off a hosted, paid deployment.
- **Pricing.** A speech engine priced per character uses `pricing.models."<provider>/<model>": { per_million_chars: 15 }` (OpenAI `tts-1`); one priced by output time uses `per_minute`, as speech-to-text does. A self-hosted engine is free unless you set a price. A pair absent from the table is unpriced, which is not the same as free.

**Per agent.** An agent can switch speech off for itself and lower the per-run character cap:

```yaml
agents:
  briefer:
    speech:
      enabled: true            # default true when the operator configured an engine; false switches it off
      max_chars_per_run: 20000 # at most speech.max_chars_per_run
```

- A cap above the operator's is **refused** at config load and at AgentDef create/fork, naming both values. At run time the lower of the two applies, so an operator who lowers the cap later still binds stored agents.
- With no engine configured, the agent's setting is accepted and has no effect.
- The block is set in yaml or the AgentDef overlay, merges per field on a fork, and is part of the agent's content hash (an agent without it hashes as before). A run cannot override it.

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
