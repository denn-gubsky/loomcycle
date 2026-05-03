# Tools and tool policy

loomcycle exposes tools to agents through a **two-layer default-deny model**: every tool is disabled at the operator layer until env-configured, and every agent gets zero tools at the agent layer until `allowed_tools` is set in YAML. Both layers must say "yes" before a tool reaches the model.

This document explains the model end-to-end and what it means for operators wiring up new agents or new tools.

## The two layers

```
┌──────────────────────────────────────────────────────────────┐
│ Layer 1 — Operator (server-wide)                             │
│   Built-ins refuse every call when their env is unset.       │
│   MCP servers must be declared in mcp_servers in YAML, with  │
│   optional per-server allowed_tools narrowing.               │
└──────────────────────┬───────────────────────────────────────┘
                       │ (registered + enabled tools)
                       ▼
┌──────────────────────────────────────────────────────────────┐
│ Layer 2 — Agent (per-run)                                    │
│   Agent's allowed_tools in YAML lists the names exposed.     │
│   Empty / missing list → zero tools, full stop.              │
│   Caller's request body may further narrow (intersection).   │
└──────────────────────┬───────────────────────────────────────┘
                       │ (effective tool set)
                       ▼
                   Dispatcher
```

Both layers are intersection-only: nothing the model sees can ever exceed what the operator allowed at layer 1, and nothing exceeds what the agent definition allowed at layer 2.

## Layer 1 — operator-side: enabling tools

### Built-ins

Each built-in is registered into the dispatcher at process startup but **refuses every call** until its sandbox is configured via env. The model will see the tool's spec and may try to call it, but every call comes back with `{"is_error": true, "text": "<tool> is not configured ..."}`. This deliberate behaviour means the model gets a clear "not configured" signal it can self-correct from, instead of `unknown tool`.

| Tool        | Enabled by                                            |
|-------------|-------------------------------------------------------|
| `Read`      | `LOOMCYCLE_READ_ROOT=/path/to/sandbox`                |
| `Write`     | `LOOMCYCLE_WRITE_ROOT=/path/to/sandbox`               |
| `Edit`      | `LOOMCYCLE_WRITE_ROOT=/path/to/sandbox` (shared)      |
| `HTTP`      | `LOOMCYCLE_HTTP_HOST_ALLOWLIST=api.example.com,...`   |
| `WebFetch`  | (same allowlist as HTTP — shared backend)             |
| `WebSearch` | `BRAVE_API_KEY=...`                                   |
| `Bash`      | `LOOMCYCLE_BASH_ENABLED=1` + `LOOMCYCLE_BASH_CWD=...` |

Bash has additional warnings: it is **not a true sandbox** even when enabled. Run loomcycle inside a container or VM if Bash is exposed to untrusted prompts. See `internal/tools/builtin/bash.go` for the full warning.

Sandbox semantics (file tools):
- Paths must resolve **inside** the sandbox root after full `EvalSymlinks` evaluation. Symlinks pointing outside the root are refused.
- `Write` resolves the **parent** dir (target may not exist yet); `Read` and `Edit` resolve the target itself.
- All file writes are atomic via tempfile + same-directory rename.

SSRF semantics (network tools):
- `HostAllowlist` is **suffix-anchored** at a dot boundary: an entry `example.com` matches `example.com` and `api.example.com` but not `evilexample.com`.
- After DNS resolution, **private IPs are hard-blocked regardless of allowlist** — RFC1918, loopback, link-local (including the AWS/GCP metadata service at `169.254.169.254`), multicast, and IPv6 ULAs. This defeats DNS rebinding.
- Redirects re-validate the destination against the same allowlist on every hop.

### MCP servers

MCP servers are declared in YAML under `mcp_servers`. Each server's tools are registered as `mcp__{server}__{tool}` after the server's `tools/list` discovery completes.

Per-server narrowing at the operator layer:

```yaml
mcp_servers:
  brave-search:
    transport: stdio
    command: npx
    args: [-y, "@modelcontextprotocol/server-brave-search"]
    env: { BRAVE_API_KEY: "${BRAVE_API_KEY}" }
    # Operator-level filter: only these of the server's tools are
    # registered AT ALL. Others are invisible to every agent.
    allowed_tools: [brave_web_search]
```

Without `allowed_tools` on the server entry, every tool the server advertises is registered. With it, only the listed tools are registered, and the rest are dropped before any agent can ever request them.

## Layer 2 — agent-side: exposing tools

Each agent definition lists which tools it can access. The list is **anchored**: anything not on it is invisible.

```yaml
agents:
  cv-adapter:
    model: smart
    allowed_tools:
      - Read
      - Edit
      - "mcp__brave-search__*"   # glob — every brave-search tool that's registered
    system_prompt_file: ./prompts/cv-adapter.md

  ats-filter:
    model: local
    allowed_tools: [Read]         # only reads files
```

If you forget `allowed_tools`, the agent gets zero tools. The startup log will warn:

```
note: agent "myagent" has no allowed_tools — it will see zero tools (intentional default-deny; add allowed_tools to its YAML to expose tools)
```

This is a feature, not a bug. New agents start with no privileges; you grant them.

### Glob support

Entries ending in `*` match by prefix. The canonical use is exposing all tools from one MCP server: `"mcp__brave-search__*"`. Built-in names are short and unambiguous so globs aren't usually needed for them, but the same machinery applies.

### Caller-side narrowing

The HTTP request can carry `allowed_tools` in the body to further narrow the agent's set on a single run:

```http
POST /v1/runs
{
  "agent": "cv-adapter",
  "allowed_tools": ["Read"],         // narrowing — only Read for THIS run
  "segments": [...]
}
```

The effective set is the **intersection**: agent allowlist ∩ caller allowlist. The caller can shrink the agent's privileges for one run; it can never widen them.

Use cases:
- Multi-step pipelines where stage 1 only reads, stage 2 also writes.
- A web UI that exposes a "safe mode" that disables `Bash` even when the agent is normally allowed it.

## Per-request URL allowlist (HTTP / WebFetch / WebSearch)

The operator's `LOOMCYCLE_HTTP_HOST_ALLOWLIST` is a security floor — every host the agent might ever need to reach. For a single run, callers usually want to constrain further: this run is about scraping job listings from `linkedin.com` and `indeed.com`, that one is about reading from `bbc.co.uk`. Per-request narrowing exists for exactly this.

```http
POST /v1/runs
{
  "agent": "cv-adapter",
  "allowed_hosts": ["linkedin.com", "indeed.com"],
  "web_search_filter": "drop",       // optional; defaults to "drop"
  "segments": [...]
}
```

### Three-state `allowed_hosts`

| Field state           | Effective allowlist                                  |
|-----------------------|------------------------------------------------------|
| omitted (`null`)      | Operator's full static list (no narrowing)           |
| `[]` (empty array)    | **Deny all** — every HTTP / WebFetch call refuses    |
| `["a.com", "b.com"]`  | Operator list ∩ caller list                          |

Distinguishing `null` from `[]` matters: omitting the field means "I don't have an opinion, use the operator's list"; sending `[]` means "this run does no network calls".

### Intersection-only — caller can shrink, never widen

If the operator's static allowlist is `["api.linkedin.com"]` and the caller asks for `["evil.example", "api.linkedin.com"]`, the effective list is `["api.linkedin.com"]` — `evil.example` is silently dropped. The operator's policy is the floor; nothing the caller does can lift it. Suffix matching applies on both sides: `api.linkedin.com` is allowed under operator entry `linkedin.com`.

### Trust boundary

`allowed_hosts` MUST come from the trusted upstream caller, never from the model. The model can read its own `system` prompt and request body content but cannot construct a /v1/runs request. As long as the application calling loomcycle is the one supplying `allowed_hosts`, the threat model holds. **Don't pipe model-generated text into `allowed_hosts`** — that turns the policy into something the prompt can manipulate.

### `web_search_filter` for WebSearch

Brave Search returns whatever it found. With per-request narrowing in place, the model sees URLs it can't actually fetch, which wastes context tokens. Two options:

| `web_search_filter` | Brave behaviour preserved | Model sees results outside `allowed_hosts`? |
|---------------------|---------------------------|----------------------------------------------|
| `"drop"` (default when `allowed_hosts` is set) | Yes (Brave is paid) | No — non-matching results are filtered out, indices renumber |
| `"keep"`            | Yes                       | Yes — full result list passes through; caller filters downstream |

`drop` is the default because the typical agent is already context-constrained and showing it URLs it can't follow up on is wasteful. `keep` is for callers that want to see what Brave returned before applying their own narrowing logic (e.g. a UI that displays "we found N results on these other hosts too").

Filter mode is ignored when `allowed_hosts` is `null`.

### Session continuation

Continuations on `/v1/sessions/{id}/messages` re-supply `allowed_hosts` and `web_search_filter` per call. The list is **not** persisted on the session. This keeps "what hosts can this turn reach?" answerable from the request alone — no implicit state to chase. A continuation can change (typically narrow) the list as the conversation evolves.

### What this doesn't cover

- **Path-level filtering** (`example.com/api/v1/*` only) — out of scope; operators wanting that wire an HTTP MCP gateway in front.
- **Per-host method limits** (`linkedin.com` GET-only) — same reasoning.
- **Quota / rate limits per host** — orthogonal; belongs in a v0.4 hooks/observability story.

## What "default-deny" means in practice

| Situation                                                 | Result                                |
|-----------------------------------------------------------|---------------------------------------|
| Operator hasn't set `LOOMCYCLE_WRITE_ROOT`                | `Write`/`Edit` calls refuse with a clear message. |
| Agent omits `allowed_tools`                               | The model sees zero tools.            |
| Agent includes a tool the operator hasn't enabled         | The tool is registered (visible) but every call refuses. |
| Caller's request lists a tool the agent doesn't allow     | That tool is silently dropped from the run's set. |
| Agent globs `mcp__foo__*` but no such server is declared  | No tools — server isn't registered.   |

## Design rationale

**Why register tools the operator hasn't enabled?** So the model sees `{tool} is not configured` instead of `unknown tool`. The first is a signal the model can self-correct from ("oh, I shouldn't try that here"); the second is a confusing dead end.

**Why default-deny at the agent layer?** Adding a new built-in shouldn't silently grant every existing agent new privileges. With default-deny, new tools require explicit agent opt-in.

**Why allow caller narrowing but not widening?** The agent definition is the trust contract between operator and the runtime; the caller is upstream code that can reduce trust for one call but not raise it. This matches the principle that policy decisions flow inward (operator → agent → caller) and trust never compounds in the other direction.

## Testing the policy

Unit tests in `internal/tools/policy/policy_test.go` cover the matching rules (exact, glob, intersection).

Integration tests in `internal/api/http/server_test.go`:
- `TestAgentWithNoAllowedToolsSeesZeroTools` — empty allowlist → zero tools at the dispatcher.
- `TestAgentWithExplicitAllowSeeTool` — positive control: allow-listed tool reaches the model.

Empirical proof of the security invariant: inverting the default in `filterTools` (so empty allowlist exposes every tool) makes the first test fail. The test is what stops a future refactor from accidentally inverting the policy.
