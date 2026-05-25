# Provider configuration

loomcycle dispatches LLM calls through a pluggable provider matrix. Each provider has its own driver (`internal/providers/<id>/`) and is registered at boot. Operators select a provider per-agent (yaml `provider:` field) or per-tier (`user_tiers.<tier>.tiers.<level>[].provider`).

This page covers operator-facing setup for each provider. The substrate, resolver, and tier-fallback design live in `docs/CONFIGURATION.md`.

## Supported providers

| ID | Auth | Endpoint | Status |
|---|---|---|---|
| `anthropic` | API key (`ANTHROPIC_API_KEY`) | `https://api.anthropic.com` | Production. The default choice. |
| `openai` | API key (`OPENAI_API_KEY`) | `https://api.openai.com` | Production. |
| `deepseek` | API key (`DEEPSEEK_API_KEY`) | `https://api.deepseek.com` | Production. |
| `gemini` | API key (`GEMINI_API_KEY`) | `https://generativelanguage.googleapis.com` | Production. |
| `ollama` | Bearer (`OLLAMA_API_KEY`) | `https://ollama.com` (configurable via `OLLAMA_CLOUD_BASE_URL`) | Production. Hosted ollama.com. |
| `ollama-local` | None (local trust) | `OLLAMA_BASE_URL` (default `http://localhost:11434`) | Production. Local-network Ollama. |
| `anthropic-oauth-dev` | OAuth subscription | `https://api.anthropic.com` | **Opt-in. Research/dev only.** Reverse-engineered Anthropic subscription billing via Claude Code's OAuth flow. See below. |

## `anthropic-oauth-dev` — Anthropic subscription billing (research/dev only)

> **Read the risk section before enabling.** This provider authenticates against the operator's Claude Pro/Max subscription via a reverse-engineered OAuth flow (Pi's `pi-ai` package, github.com/earendil-works/pi, 51K stars, is the reference). It is **not officially endorsed by Anthropic**, operates in a vendor-policy gray zone, and carries account-revocation risk.

### When to use it

Research workloads at scale: self-evolution experiments, agentic-team load testing, multi-iteration coding-task experiments. Concrete pain it solves: a 100-iteration self-evolution cycle costs $750-$3,750 at API-key Sonnet pricing; a $200/month MAX subscription absorbs the same workload.

### When NOT to use it

- **Production deployment of any kind.** Server-hosted instances stay on API-key billing.
- **Multi-tenant workloads.** One operator, one Claude subscription. Multi-tenant deployments must use API-key Anthropic.
- **Anything customer-facing.** The OAuth path can break at any time (Anthropic changes the auth surface; subscription revoked; quota exhausted). Customer-facing workloads need API-key SLAs.

### Prerequisites

1. **Active Claude Pro or Max subscription** (`claude.ai/settings/billing`). The OAuth scope set requests `user:inference` — only subscriptions with inference quota work.
2. **`LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED=1`** in your environment. Without this, the provider is absent from the resolver matrix and the `loomcycle anthropic` CLI subcommands refuse to run.
3. **A browser on the same machine** as loomcycle, OR `--manual` mode (operator pastes the authorize URL into a browser on a different machine, then pastes the callback URL back into the terminal).

### One-time login

```sh
export LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED=1
loomcycle anthropic login
```

This:
1. Generates a PKCE pair (S256 challenge).
2. Starts a localhost callback server on `127.0.0.1:53692` (configurable via `LOOMCYCLE_ANTHROPIC_OAUTH_CALLBACK_PORT`).
3. Opens your browser at `https://claude.ai/oauth/authorize?...` — you authorize loomcycle's access to your subscription.
4. Receives the auth code via the callback, exchanges it for access + refresh tokens at `https://platform.claude.com/v1/oauth/token`.
5. Persists tokens at `~/.config/loomcycle/anthropic-oauth.json` (chmod 0600 enforced).

A background goroutine refreshes the access token every 30 seconds, rotating proactively 5 minutes before expiry. The refresh token can be long-lived (Anthropic rotates it when it pleases; loomcycle persists whatever comes back).

### Status check

```sh
loomcycle anthropic status
```

Prints the token file path, whether tokens are loaded, the access token's expiry, the granted scope, and the obtainedAt timestamp. Also surfaces a warning if the token file's permissions have drifted from 0600.

### Logout

```sh
loomcycle anthropic logout
```

Deletes the token file. Idempotent — safe to run when no tokens are stored.

### Agent configuration

```yaml
agents:
  research-coder:
    description: "Heavy-volume coding-task agent for load testing"
    provider: anthropic-oauth-dev
    model: claude-sonnet-4-6
    allowed_tools:
      - Read
      - Write
      - Edit
      - Bash
      - Grep
      - Glob
      - Memory       # exposed via the mcp__loomcycle__memory wire name
      - Channel      # exposed via the mcp__loomcycle__channel wire name
      - Agent        # exposed via the mcp__loomcycle__agent wire name (parallel_spawn etc.)
    max_tokens: 8192
    max_iterations: 64
```

**All loomcycle tools are admissible under OAuth-dev.** The 10-tool Claude-Code canonical overlap (`Read`, `Write`, `Edit`, `Bash`, `Grep`, `Glob`, `NotebookEdit`, `WebFetch`, `WebSearch`, `Skill`) and any real MCP tool (`mcp__<server>__<tool>` from `mcp_servers:` yaml or `MCPServerDef` substrate) pass through unchanged. Loomcycle's substrate primitives (`Memory`, `Channel`, `Agent`, `AgentDef`, `SkillDef`, `MCPServerDef`, `Evaluation`, `Interruption`, `Context`, `HTTP`) are exposed under the `mcp__loomcycle__*` wire mask — to Anthropic's subscription-billing layer they look like tools from an MCP server registered as "loomcycle"; to the loomcycle dispatcher they continue to work exactly as today.

### Tier configuration (with API-key fallback)

```yaml
user_tiers:
  research:
    provider_priority: [anthropic-oauth-dev, anthropic]
    tiers:
      low:    [{provider: anthropic-oauth-dev, model: claude-haiku-4-5-20251001}]
      middle: [{provider: anthropic-oauth-dev, model: claude-sonnet-4-6}]
      high:
        - {provider: anthropic-oauth-dev, model: claude-sonnet-4-6}
        - {provider: anthropic, model: claude-sonnet-4-6}    # API-key fallback when subscription quota is exhausted
    fallback_on_error: true
```

With `fallback_on_error: true`, subscription-quota-exhausted errors trigger a fallback to API-key Anthropic for the affected request. Research workloads can run primarily on subscription billing and bleed over to API credits only when the MAX quota hits its ceiling.

### Risk acknowledgement

By enabling `LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED=1`, you acknowledge:

1. **Reverse-engineered flow.** loomcycle's OAuth-dev provider uses Claude Code's OAuth `client_id` (publicly visible in Pi's open-source code at 51K stars; the constant is in `internal/providers/anthropic_oauth_dev/client_id.go`). Anthropic has not officially endorsed third-party use of this flow.
2. **Subscription-terms gray zone.** Anthropic's MAX/Pro subscription terms historically restrict programmatic use outside their official SDK. Pi has been operating publicly since 2025-08 without takedown, suggesting Anthropic tolerates this in practice — but tolerance is not endorsement.
3. **Account revocation risk.** Anthropic can flag accounts using non-Claude-Code clients at any time. Operators using OAuth-dev accept the risk that their MAX subscription may be limited or revoked if Anthropic's detection systems flag the pattern.
4. **Drift exposure.** Anthropic can change the OAuth flow, the beta marker, or the required User-Agent at any time. When that happens, OAuth-dev breaks until a hotfix lands. Operators with `LOOMCYCLE_CLAUDE_CODE_VERSION=<override>` can self-patch the User-Agent string for the duration.
5. **Single-machine deployment.** Tokens live in `~/.config/loomcycle/anthropic-oauth.json`. There is no support for shared token stores, multi-replica synchronization, or server-side token mounting. Multi-tenant or server-hosted deployments must use API-key Anthropic.

If any of these are unacceptable for your workload, use the `anthropic` provider (API key) instead.

### Drift detection

Auth-surface drift surfaces as one of two patterns:

- **Refresh failures**: the background goroutine logs `anthropic-oauth-dev: refresh failed: ...` on every 30s tick. Persistent failures across multiple ticks mean the refresh endpoint changed or the refresh token was revoked.
- **400 INVALID_REQUEST on Messages calls**: Anthropic's auth surface rejected the request. Most common cause: the User-Agent's pinned Claude Code version is too old.

When the second pattern hits:

```sh
# Self-patch until a hotfix lands:
export LOOMCYCLE_CLAUDE_CODE_VERSION=2.1.80   # whatever Pi's pinned to today
loomcycle ...
```

Check Pi's source (github.com/earendil-works/pi → `packages/ai/src/providers/anthropic.ts`) for their current pinned version.

When refresh fails persistently:

```sh
loomcycle anthropic logout
loomcycle anthropic login
```

### How it works (architecturally)

The OAuth-dev driver wraps the production `internal/providers/anthropic` driver. Two layers sit on top:

1. **HTTP transport** (`oauthTransport` in `driver.go`): strips `x-api-key` from outgoing requests, adds `Authorization: Bearer <current access token>`, appends `claude-code-20250219,oauth-2025-04-20` to `anthropic-beta`, sets `user-agent: claude-cli/<version>`.
2. **Tool-name mask** (`loomcycle_mask.go`): rewrites loomcycle-only built-in tool names (Memory / Channel / etc.) to `mcp__loomcycle__<name>` on outbound, reverses on inbound. To Anthropic this looks like an MCP server registered as "loomcycle"; to the loomcycle dispatcher the tools work exactly as today.

Everything else (SSE parsing, retry on 429, rate-limit honouring, cache_control breakpoint placement, effort hint translation) is inherited from the production driver. The OAuth-dev path is a thin shim — when Anthropic adds a Messages API feature, the production driver gains support first and the OAuth-dev path inherits it for free.

### Multi-replica HA — not supported

The OAuth-dev provider is single-operator, single-machine. There's no token-sync mechanism, no shared refresh state. Multi-replica deployments must use API-key Anthropic. This is enforced by the design (tokens in `~/.config/loomcycle/`), not by a runtime check — operators who attempt to mount the token file across replicas will get refresh-token-rotation races and inconsistent state.

### References

- Pi (`earendil-works/pi`, 51K stars): the source of the OAuth client_id + flow shape.
- The internal RFC at `~/work/loomcycle-internal/doc-internal/rfcs/anthropic-oauth-dev.md` (locked 2026-05-19) documents the full design + decisions.
- `Context.help(topic="loomcycle")` from inside an agent prompt for the in-substrate description.
