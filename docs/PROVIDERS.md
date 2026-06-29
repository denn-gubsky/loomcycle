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

> ### ⚠ NO GUARANTEES — READ BEFORE ENABLING
>
> The Anthropic OAuth flow used by this provider is **not an official integration**. It is the product of reverse-engineering done by the Pi agent team (github.com/earendil-works/pi, 51K stars, open-source) and replicated here by the loomcycle team. We do our best to mimic Claude Code's wire shape — tool names, system prompt format, headers, beta markers — so loomcycle's LLM calls pass through Anthropic's subscription-billing detection. **We cannot guarantee this will continue to work.** Anthropic can change the auth flow, the wire shape, the version pinning, or the detection heuristics at any time and the integration will break until a hotfix lands.
>
> **You are running this against your own Claude Pro/Max subscription. Anthropic's subscription terms historically restrict programmatic use outside the official Anthropic SDK. You — the operator — are solely responsible for any consequences with Anthropic if they ever object to this use, including account flagging, rate-limiting, or subscription revocation.** loomcycle (the project) and its maintainers carry no warranty, no liability, and provide no support guarantees for the OAuth-dev path. If your account is affected, the resolution path is between you and Anthropic; we cannot intervene.
>
> By setting `LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED=1` and running `loomcycle anthropic login`, you acknowledge that:
>
> 1. The OAuth flow is reverse-engineered, not officially endorsed by Anthropic.
> 2. Anthropic may flag, limit, or revoke your subscription if they object to non-Claude-Code clients using OAuth tokens.
> 3. The integration may stop working at any time as Anthropic updates their auth surface; loomcycle will ship hotfixes when feasible but offers no SLA.
> 4. All risk and any consequences with Anthropic are yours to manage. loomcycle disclaims responsibility for account, billing, or terms-of-service outcomes.
> 5. If you cannot accept these terms, **do not enable this provider** — use the production `anthropic` provider (API key) instead.
>
> This feature exists because it is genuinely useful for research workloads where API-key billing would be prohibitively expensive. It is not a free lunch.

### When to use it

Research workloads at scale: self-evolution experiments, agentic-team load testing, multi-iteration coding-task experiments. Concrete pain it solves: a 100-iteration self-evolution cycle costs $750-$3,750 at API-key Sonnet pricing; a $200/month MAX subscription absorbs the same workload.

### When NOT to use it

- **Production deployment of any kind.** Server-hosted instances stay on API-key billing.
- **Multi-tenant workloads.** One operator, one Claude subscription. Multi-tenant deployments must use API-key Anthropic.
- **Anything customer-facing.** The OAuth path can break at any time (Anthropic changes the auth surface; subscription revoked; quota exhausted). Customer-facing workloads need API-key SLAs.

### Prerequisites

1. **Active Claude Pro or Max subscription** (`claude.ai/settings/billing`). The OAuth scope set requests `user:inference` — only subscriptions with inference quota work.
2. **`LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED=1`** in your environment. Without this, the provider is absent from the resolver matrix and the `loomcycle anthropic` CLI subcommands refuse to run.
3. **A browser on the same machine as loomcycle.** Cross-machine login is not supported in v0.11.9 — the callback server binds to `127.0.0.1` on the loomcycle host, so the browser must redirect to that same loopback. Headless environments (CI, server-without-display) can use `--manual`: loomcycle prints the authorize URL instead of auto-opening a browser, and the operator opens that URL in a browser **on the same machine** (e.g. via SSH X-forwarding, a VNC session, or a graphical session that doesn't expose a default-browser hook to `xdg-open`).

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
loomcycle anthropic status            # local token-file metadata only
loomcycle anthropic status --probe    # ALSO verify server-side validity
```

Prints the token file path, whether tokens are loaded, the access token's expiry, the granted scope, and the obtainedAt timestamp. Also surfaces a warning if the token file's permissions have drifted from 0600.

By default every field is **local token-file metadata** — the token can read "valid / expires in 6h" while Anthropic has already revoked it server-side, so plain `status` prints `Server-side: not checked`. Pass **`--probe`** (alias `--verify`) to confirm against Anthropic: it attempts a token refresh (free — token endpoint, no inference billing), reporting `✓ valid` (exit 0) or `✗ INVALID` (exit 1) with the reason. A successful probe **rotates and persists** a fresh token, so it also heals/extends the session.

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

### Risk details

The headline disclaimer above is the contract. This section is the operational detail.

* **Reverse-engineered flow.** The Claude Code OAuth `client_id` constant in `internal/providers/anthropic_oauth_dev/client_id.go` is the same one Pi publishes; loomcycle did not negotiate this access path with Anthropic. The wire shape — beta markers, User-Agent string, system-prompt prepend, tool-name canonicalization, MCP-name masking — exists to look like Claude Code so Anthropic's subscription billing accepts the request. None of this is guaranteed to keep working.
* **Subscription-terms posture.** Anthropic's MAX/Pro subscription terms historically restrict programmatic use outside their official SDK. Pi has been operating publicly since 2025-08 without takedown, suggesting Anthropic tolerates this in practice — but tolerance is not endorsement, and tolerance can be withdrawn at any time without notice.
* **No SLA, no warranty, no liability.** loomcycle is Apache-2.0 software. The OAuth-dev path is opt-in and explicitly described as "research/dev only" precisely so this expectation is clear. If you need contractually-guaranteed access to Claude, you must use the official API-key path (`provider: anthropic`) — that's between you and Anthropic and they support it.
* **Single-machine deployment, by design.** Tokens live at `~/.config/loomcycle/anthropic-oauth.json` (chmod 0600). There is no support for shared token stores, multi-replica synchronization, or server-side token mounting. Multi-tenant or server-hosted deployments must use API-key Anthropic. This is enforced by design — to limit the blast radius if Anthropic's detection trips.
* **Drift exposure.** Anthropic can change the OAuth flow, the beta marker, the required User-Agent string, or the validation of any wire-shape element at any time. When that happens, OAuth-dev breaks until a hotfix lands. Operators with `LOOMCYCLE_CLAUDE_CODE_VERSION=<override>` can self-patch the User-Agent string for the duration; other drift may require a code update.

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

## Image / vision input (RFC AT)

loomcycle accepts image input as a content block on a `user`-role segment, over
**every transport** (HTTP, gRPC, MCP, the TS + Python adapters). It is additive
and backward-compatible — text-only callers are unchanged, and there is no new
endpoint.

### Sending an image

A caller adds an `image` block alongside text blocks in a `user` segment of
`POST /v1/runs` (or `POST /v1/sessions/{id}/messages` for a continuation turn):

```json
{
  "agent": "vision-agent",
  "segments": [{
    "role": "user",
    "content": [
      {"type": "trusted-text", "text": "What's in this picture?"},
      {"type": "image", "media_type": "image/png", "data": "<base64 bytes, no data: prefix>"}
    ]
  }]
}
```

- **`media_type`** must be one of `image/png`, `image/jpeg`, `image/gif`,
  `image/webp` (the common denominator across all vision providers).
- **`data`** is the raw base64 of the image bytes — **no `data:` prefix**. There
  is deliberately **no URL form**: accepting a URL would make loomcycle fetch
  arbitrary hosts (SSRF). The OpenAI driver builds the `data:` URI internally.
- Images are valid only in `user`-role segments.

### Per-transport

The same `image` block is accepted on every transport; only the representation
of the bytes differs (JSON has no byte type, proto does):

| Transport | How to send an image |
|---|---|
| **HTTP** (`POST /v1/runs`, `/v1/sessions/{id}/messages`) | `data` = base64 string (as above) |
| **MCP server** (`spawn_run` / `spawn_runs`) | a `segments` arg with the same JSON block — `data` = base64 string |
| **TS** (`@loomcycle/client`) | `PromptContent` `image` variant: `{ type:"image", media_type, data }` (base64 string) on `runStreaming` / `continueSession` |
| **gRPC** (`Run` / `Continue`) | `PromptContentBlock.media_type` (string) + **`data` (bytes)** — raw image bytes, NOT base64; the server encodes at the boundary |
| **Python** (`pip install loomcycle`, gRPC) | a segment dict `{"type":"image","media_type":...,"data": b"...raw bytes..."}` |

gRPC/Python carry the raw bytes natively (no ~33% base64 inflation); HTTP/MCP/TS
carry base64 strings. All converge on the same internal representation.

### Per-provider serialization

Each driver serializes the block natively — no conversion to text:

| Provider | Wire form |
|---|---|
| `anthropic` (+ `anthropic-oauth-dev`) | `{"type":"image","source":{"type":"base64","media_type","data"}}` |
| `openai` | user message `content` becomes the array form with an `image_url` part holding a `data:` URI |
| `gemini` | a part with `inlineData: {mimeType, data}` |
| `ollama` / `ollama-local` | the user message's `images: [base64]` field |
| `deepseek` | **not supported** — DeepSeek text models reject images |

### The capability gate

A provider advertises `SupportsVision`; the loop refuses an image sent to a
text-only provider/model **before** the call, emitting a clear error event
("model X on provider Y does not support image input") rather than letting the
image be silently dropped or the provider return an opaque 400. This makes a
provider-fallback onto a text-only model fail loudly. Per-model nuance (a legacy
text-only model on an otherwise vision-capable provider — `claude-2`/
`claude-instant`, `gpt-3.5*`, the original `gpt-4`/`gpt-4-32k` snapshots) is
refined inside the driver; unknown models default to supported (a wrong guess
surfaces as a provider error, never a silent drop). For Ollama the model must be
a vision model (llava, llama3.2-vision, …) — that's the operator's choice.

### Request body cap

Inline base64 makes requests larger, so the run-ingest body cap (`POST /v1/runs`
and `POST /v1/sessions/{id}/messages`) defaults to **16 MiB** and is tunable via
`LOOMCYCLE_MAX_REQUEST_BYTES` (bytes). An over-cap request returns
`413 Request Entity Too Large`. Consumers should resample images to a sane max
edge (~1568px) before sending to keep them small and bound token cost.

### Caveats

- The gRPC `PromptContentBlock` carries `media_type` + `data` (bytes) but still
  omits `kind` (the untrusted-block source label) — gRPC callers can send images
  and trusted/untrusted text, but not a custom untrusted `kind`.
- **No image output** (generation), no audio/video.
- A vision model can be influenced by text rendered *inside* an image
  (prompt-injection-via-image) — inherent to any vision system, not defended at
  the wire.
