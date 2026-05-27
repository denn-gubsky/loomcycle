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
| `Grep`      | `LOOMCYCLE_READ_ROOT=...` (shared with Read; v0.8.24) |
| `Glob`      | `LOOMCYCLE_READ_ROOT=...` (shared with Read; v0.8.24) |
| `NotebookEdit` | `LOOMCYCLE_WRITE_ROOT=...` (shared with Write; v0.8.24) |
| `HTTP`      | `LOOMCYCLE_HTTP_HOST_ALLOWLIST=api.example.com,...`   |
| `WebFetch`  | (same allowlist as HTTP — shared backend)             |
| `WebSearch` | `BRAVE_API_KEY=...`                                   |
| `Bash`      | `LOOMCYCLE_BASH_ENABLED=1` + `LOOMCYCLE_BASH_CWD=...` |
| `Agent`     | Always registered (server-internal); per-agent `allowed_tools` controls who can spawn. |
| `Skill`     | `LOOMCYCLE_SKILLS_ROOT=/path/to/skills` (or skills inlined per-agent via YAML `skills:` list). v0.8.22+ also consults the `skill_defs` store for DB-active overrides. |
| `SkillDef`  | Always registered (v0.8.22). Per-agent `skill_def_scopes:` YAML gate (default-deny); no extra env var. Storage shared with the rest of the substrate. |
| `Memory`    | Storage backend (SQLite default; Postgres opt-in) + per-agent `memory_scopes:` allowlist. |

Bash has additional warnings: it is **not a true sandbox** even when enabled. Run loomcycle inside a container or VM if Bash is exposed to untrusted prompts. See `internal/tools/builtin/bash.go` for the full warning.

Sandbox semantics (file tools):
- Paths must resolve **inside** the sandbox root after full `EvalSymlinks` evaluation. Symlinks pointing outside the root are refused.
- `Write` resolves the **parent** dir (target may not exist yet); `Read`, `Edit`, `Grep`, `Glob`, and `NotebookEdit` resolve the target itself.
- All file writes are atomic via tempfile + same-directory rename.
- `Grep` skips binary files via NUL-byte heuristic on the first 8 KiB; caps output at 256 KiB + `head_limit` (default 100).
- `Glob` supports `**` for recursive segment match; returns paths sorted by mtime DESC, capped at 100 results.
- `NotebookEdit` accepts `replace` / `insert` / `delete` modes; preserves all non-target cells and notebook metadata verbatim.

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

## The `Agent` tool — sub-agent spawning (v0.4.0)

The `Agent` built-in lets a parent agent spawn a child run by name:

```json
{"name": "researcher", "prompt": "Investigate X and return JSON …"}
```

**Auto-registration.** Unlike other built-ins, `Agent` is registered automatically by the HTTP server (it has to close over the server's own sub-run runner). It still respects per-agent `allowed_tools` — only agents that list `Agent` in their YAML can spawn children.

**What the child inherits from the parent (via ctx):**

- `user_id` — sub-agents bill to the same user.
- `parent_agent_id` — written to the child's run row; powers cascade-cancel.
- **Caller-authoritative host policy** (`allowed_hosts` + `web_search_filter`) — the parent's per-call host narrowing flows into the child. Without this propagation, sub-agents would silently fall back to the operator's static `LOOMCYCLE_HTTP_HOST_ALLOWLIST`, which usually excludes localhost callbacks. The fix landed in v0.4.0; see `internal/api/http/server.go runSubAgent` and `internal/tools/tool.go HostPolicy`.

**What the child does NOT inherit:**

- Tool allowlist — the child's `allowed_tools` is its YAML definition, narrowed only by the operator's enabled set. Parents cannot widen.
- Session — sub-agents get a fresh session.
- Tenant — inherited only for `user_id` purposes; multi-tenant isolation rules apply equally.

**Recursion cap.** `MaxAgentDepth = 16` by default. Deeper spawning attempts return `IsError: true` tool_results so the parent can decide to retry / fall back / give up.

**Failure mode is a tool error, not a run failure.** A sub-agent that crashes, times out, or rejects its input returns an `IsError: true` tool_result. Loomcycle does NOT tear down the parent on a child failure — the parent sees the error and decides what to do.

References: `internal/tools/builtin/agent.go`, `internal/api/http/server.go runSubAgent`, `internal/api/http/agent_subagent_test.go`.

## The `Skill` tool — Approach A (static bundling, shipped) and Approach B (dynamic, scaffolded)

**Approach A — static bundling (active in v0.4.0).**

At config-load, every directory under `LOOMCYCLE_SKILLS_ROOT` named `<skill>/SKILL.md` is read. Agents that list a skill in their YAML get the skill's body **concatenated into their system prompt** as a cacheable trusted-text block:

```yaml
agents:
  cv-adapter:
    model: smart
    allowed_tools: [Read, Write]
    skills: [voice-applier, cv-voice-applier]   # bodies merged into system prompt at config-load
    system_prompt_file: prompts/cv-adapter.md
```

Constraint: each skill's frontmatter declares its own `allowed-tools`; this list must be a **subset** of the agent's `allowed_tools`. Mismatches are rejected at config-load with a clear error. This prevents skills from advertising tools the agent can't actually invoke.

**Approach B — dynamic Skill tool (placeholder).**

The `Skill` built-in is registered when `LOOMCYCLE_SKILLS_ROOT` is set; the model can call it with `{"name": "voice-applier"}` to load a skill mid-conversation. In v0.4.0 the tool returns "unknown skill" — full Approach B implementation is v1.0 work. The hook is in place so prompts that reference the dynamic Skill tool can be authored today; they degrade gracefully (the tool reports the skill isn't available, the model continues without).

References: `internal/skills/`, `internal/tools/builtin/skill.go`.

## The `Memory` tool — persistent agent-scoped storage (v0.8.0)

The `Memory` built-in gives agents a place to write down state that survives across runs and sessions. Six operations behind one tool, discriminated by an `op` field: **`get`**, **`set`**, **`delete`**, **`list`**, **`incr`**, **`search`** (v0.9.0).

### Wire shape

```jsonc
{
  "op":     "get" | "set" | "delete" | "list" | "incr" | "search",
  "scope":  "agent" | "user",
  "key":    "string",     // get/set/delete/incr
  "value":  any,          // set
  "delta":  number,       // incr (default 1, may be negative)
  "ttl":    number,       // set/incr — seconds; absent = no expiry
  "prefix": "string",     // list/search filter
  "limit":  number,       // list cap (default 100, max 1000)
  // v0.9.0 Vector Memory:
  "embed":      bool,     // set — also store an embedding for this row
  "embed_text": "string", // set — text to embed (defaults to JSON-stringified value)
  "query":      "string", // search — text to embed and use as similarity query
  "top_k":      number    // search — max results (default 10, max 50)
}
```

Result shapes:

```jsonc
get    → { "value": <stored> | null, "expires_at": "RFC3339" | null }
set    → { "ok": true, "embedded"?: bool, "embed_warning"?: "string" }
incr   → { "value": <new int> }
delete → { "deleted": true | false }
list   → { "entries": [{"key", "value", "expires_at"}], "truncated": bool }
search → { "entries": [{"key", "value", "score", "embedded_with", "expires_at"}],
           "query_embedding_dim": number, "truncated": bool }
```

### Scopes

Two scopes ship in v0.8.0:

- **`agent`** — keyed by the yaml-declared agent name. Cross-run, **shared across users.** Use for: per-agent counters, learned heuristics, summaries the agent wants its future self to read.
- **`user`** — keyed by the run's `user_id`. Cross-agent, **per end-user.** Use for: voice/preferences, per-user notes, anything you want every agent that's allowed to read the user scope to see.

`session` and `tenant` scopes are forward-compatible (the yaml allowlist accepts new scope strings without a wire-protocol change) but not implemented in v0.8.0.

`scope_id` is **always** resolved server-side. The model picks the scope; loomcycle picks the scope_id. A model-supplied scope_id would let one user's agent run read another user's keys.

### Per-agent yaml policy

```yaml
agents:
  cv-adapter:
    allowed_tools: [Memory, Read, Write]
    memory_scopes: [agent, user]            # which scopes this agent may use
    memory_quota_bytes: 5_000_000           # per-(scope, scope_id) override (default 1 MB)
```

`memory_scopes` is a default-deny allowlist. An agent with `Memory` in `allowed_tools` but no `memory_scopes` sees a refusal on every Memory call.

`memory_quota_bytes` overrides the global `LOOMCYCLE_MEMORY_MAX_SCOPE_BYTES` for this agent only. Use higher caps for memory-heavy agents; lower for noisy agents you want kept in check.

### Operator env vars

| Env var | Default | Purpose |
|---|---|---|
| `LOOMCYCLE_MEMORY_MAX_VALUE_BYTES` | `65536` | Per-write cap on the `value` payload. 0 disables. |
| `LOOMCYCLE_MEMORY_MAX_SCOPE_BYTES` | `1048576` | Default per-(scope, scope_id) cap. 0 disables. |
| `LOOMCYCLE_MEMORY_SWEEP_MS` | `900000` (15 min) | TTL reaper goroutine cadence. 0 disables. |

### Atomic increment

`op: "incr"` is the counter primitive. If the key doesn't exist (or the existing value has already expired), the row is created starting from `0 + delta`. If the existing value isn't a JSON number, the call is refused with `wrong_type`. Optional `ttl` resets the expiry on every increment.

### TTL semantics

`ttl: 3600` on a `set` makes the row expire in one hour. Reads filter expired rows out **before** the sweeper runs them — agents never see stale values, even on a slow sweep cadence. The sweep goroutine just keeps the table bounded over the long haul.

### Vector / semantic search (v0.9.0)

The `search` op turns Memory into a retrieval substrate: agents `set` a row with `embed: true` and `embed_text: "..."`, then later `search` for rows by **semantic similarity** to a query. Exact-key lookup (`get`, `list`) is for state retrieval; semantic search is for "what have I learned that's *close to* this?"

```jsonc
// embedding-on-set
{
  "op": "set", "scope": "agent", "key": "rec1",
  "value": { "name": "Alice", "skills": ["Go", "Rust"] },
  "embed": true, "embed_text": "Alice is a Go and Rust developer"
}
// → { "ok": true, "embedded": true }

// search
{
  "op": "search", "scope": "agent",
  "query": "systems programmer",
  "top_k": 5
}
// → { "entries": [
//       { "key": "rec1", "value": {...}, "score": 0.91,
//         "embedded_with": {"provider": "openai", "model": "text-embedding-3-large"},
//         "expires_at": null }, ... ],
//     "query_embedding_dim": 3072, "truncated": false }
```

**Score** is cosine similarity in `[0, 1]` (higher = closer). Backends convert from their native distance function before returning.

**top_k** defaults to 10 and is hard-capped at 50 (RFC §6). Use the `prefix` field to scope `search` to a key-prefix the same way `list` does.

**`embedded_with`** carries the `(provider, model)` of the row's stored embedding so callers can spot rows under an older embedder before running a reembed migration.

#### Configuration

Two pieces of operator config gate vector ops:

1. **Backend** — Postgres with the `pgvector` extension is the only supported backend in v0.9.0. SQLite refuses with `vector_unsupported` (sqlite-vec ships in v0.9.1). Set `LOOMCYCLE_PGVECTOR_ENABLED=1` to opt in; loomcycle probes `pg_extension` at boot and refuses to start when the extension isn't installed.
2. **Embedder** — exactly one embedder per loomcycle instance, declared in yaml:

   ```yaml
   memory:
     embedder:
       provider: openai     # openai | gemini | anthropic (stub in v0.9.0)
       model: text-embedding-3-large
       timeout_ms: 30000    # optional; env fallback LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS
       batch_size: 100      # optional; env fallback LOOMCYCLE_MEMORY_EMBED_BATCH_SIZE
   ```

When `memory.embedder:` is unset, the `search` op and `embed: true` on `set` refuse with `embedder_not_configured`. The k/v ops are unaffected — operators not wanting semantic search don't have to configure anything.

**Anthropic embedder note:** v0.9.0 ships an Anthropic *stub* that refuses with `embedder_not_implemented`. Anthropic has no native embeddings API today; v0.9.1 will wire Voyage AI under this driver name. Use `openai` or `gemini` for v0.9.0.

#### Failure modes

| Code | When | Recovery |
|---|---|---|
| `vector_unsupported` | Backend has no vector index (SQLite, or Postgres without `LOOMCYCLE_PGVECTOR_ENABLED`). | Install pgvector + set the env var, OR drop `embed: true` / `search` from the agent's flow. |
| `embedder_not_configured` | No `memory.embedder` in yaml. | Add the yaml block, or drop vector ops. |
| `embedder_not_implemented` | Operator picked `provider: anthropic` in v0.9.0. | Switch to `openai` or `gemini`; Voyage proxy ships v0.9.1. |
| `dimension_mismatch` | Stored rows are at one dim; query embedder runs at another (typical mid-migration). | Run `POST /v1/_memory/reembed` to update stored rows. |

`embed: true` failures *after* the k/v row landed (transient embedder network failure, etc.) do NOT roll back. The response carries `embedded: false` + `embed_warning: "..."` so the agent sees the partial outcome. Permanent configuration errors (no embedder, no vector support) refuse UPFRONT — the k/v row is not written.

#### Quota accounting

Embedding bytes do NOT count toward `memory_quota_bytes`. Operators don't pay for the vector's storage in their per-scope cap — only the k/v row's key + value bytes count (RFC §8).

#### Operator admin endpoints

```
GET  /v1/_memory/embed_stats?scope=
POST /v1/_memory/reembed?scope=&scope_id=&dry_run=true|false
```

**`embed_stats`** returns per-`(provider, model, dimension)` row counts + total embedding bytes for a scope. Operators run this BEFORE a model swap to estimate impact.

**`reembed`** walks rows whose stored `(provider, model)` doesn't match the configured embedder and re-embeds them via the live embedder. `dry_run=true` (the safe default — even without the flag) returns the plan plus sample keys; `dry_run=false` commits and returns `rows_reembedded` + `rows_failed` + `failed_keys`. Partial failures are non-fatal — operators see exactly which rows to retry.

The Web UI's `/ui/memory` page exposes both as a model-distribution badge + a "reembed plan" → confirm-and-commit flow.

#### Operator env vars (v0.9.0)

| Env var | Default | Purpose |
|---|---|---|
| `LOOMCYCLE_PGVECTOR_ENABLED` | `0` | Opt in to vector support on Postgres. Boot-time `pg_extension` probe; refuses to start when missing. |
| `LOOMCYCLE_SQLITE_VEC_PATH` | (unset) | **Reserved for v0.9.1.** Path to the sqlite-vec shared library. Currently parsed but unused; SQLite vector ops always refuse in v0.9.0. |
| `LOOMCYCLE_MEMORY_EMBED_BATCH_SIZE` | `100` | Default batch size for embedder calls. Provider hard caps still apply on top. |
| `LOOMCYCLE_MEMORY_EMBED_TIMEOUT_MS` | `30000` | Per-call embedder HTTP timeout. 0 = rely on outer ctx. |

#### Snapshot integration

Snapshots round-trip embeddings: `Capture()` packs the float32 vector as base64 little-endian + writes it under each memory row's optional `embedding` field. `Restore()` decodes + writes via `MemoryEmbedSet` on the destination. Destinations without vector support drop the embedding with a warning per row (k/v always lands; operators re-embed after enabling pgvector). The envelope shape is locked at v1.0 — Phase 1 readers (v0.8.x) emit `embedding: null` for every row; Phase 2 readers (v0.9.0+) populate it. No envelope migration is needed.

### What's NOT in v0.8.0 / v0.9.0

- Cross-tenant sharing (no `tenant` scope yet).
- Append-log primitive — agents wanting an event stream write `events/<timestamp>` keys + `list` with prefix.
- Automatic eviction / LRU — quota exceeded → the write fails with `quota_exceeded`. Agents call `delete` explicitly.
- Encryption-at-rest — disk encryption is operator-config-wide, not Memory-specific. Revisit alongside v0.9.x HA work.
- Server-side schema validation — values are JSON; agents own their schemas.
- v0.9.0 vector-specific deferrals: SQLite vector backend (v0.9.1), Anthropic native embedder (v0.9.1, via Voyage), HNSW index on `memory_embeddings` (v0.9.x perf pass — requires single-dim scope), per-agent embedder override (v0.10.x), hybrid search + rerankers (post-v1).

References: `internal/store/store.go` (interface), `internal/store/sqlite/sqlite.go` + `internal/store/postgres/postgres.go` (adapters), `internal/tools/builtin/memory.go` (tool), `internal/providers/embedder.go` (embedder substrate), `internal/api/http/memory_admin.go` (admin endpoints).

## The `Channel` tool — persistent inter-agent message bus (v0.8.4)

Channels are the asynchronous decoupled handoff primitive. One agent publishes JSON payloads to a named channel; another agent subscribes and drains them with cursor-based delivery. Same storage backbone as Memory (sqlite + postgres) plus an in-process notification bus for sub-millisecond same-process subscriber wake-ups.

### Operator setup

Channels MUST be declared in the top-level `channels:` block of `loomcycle.yaml`. No auto-creation; the namespace is operator-owned.

```yaml
channels:
  findings:
    scope: agent              # cursor isolation: agent | user | global
    default_ttl: 86400        # per-message TTL fallback (seconds)
    max_messages: 10000       # bounded storage; oldest trimmed first
    semantic: queue           # informational: queue | broadcast

  alerts:
    scope: global
    default_ttl: 3600
    max_messages: 1000
    semantic: broadcast
```

Per-agent ACL via the agent yaml:

```yaml
agents:
  researcher:
    channels:
      publish:   [findings]            # exact match
      subscribe: []                    # one-way producer
  analyst:
    channels:
      publish:   []
      subscribe: [findings, alerts, findings/*]  # trailing /* prefix wildcard
```

Wildcards are anchored at the end (`findings/*` matches `findings/alpha` but NOT `findings`). Mid-string globs are rejected at config-load so a typo can't accidentally grant `*` access.

### Tool surface (what an agent sees)

```jsonc
// Publish (producer side):
{ "op": "publish", "channel": "findings", "value": {...}, "ttl": 3600 }
//   → { "message_id": "msg_...", "channel": "findings", "dropped_oldest": 0 }

// Subscribe (consumer side) — polling, optional long-poll:
{ "op": "subscribe", "channel": "findings", "max_messages": 10, "wait_ms": 0 }
//   → { "messages": [{ "id", "value", "published_at" }], "next_cursor": "msg_...", "channel": "findings" }

// Ack — explicitly commit a cursor (only needed for at-least-once via peek+ack):
{ "op": "ack", "channel": "findings", "cursor": "msg_..." }

// Peek — non-consuming read (debugging or at-least-once consumer pattern):
{ "op": "peek", "channel": "findings", "from_cursor": "cur_0", "max_messages": 10 }

// List — informational, reports this agent's allowlists:
{ "op": "list_channels" }
//   → { "publish": [...], "subscribe": [...] }
```

### Delivery semantics

**`subscribe` is at-most-once-by-default**: the tool commits `next_cursor` BEFORE returning. Agents that just loop `subscribe` march forward without tracking cursors themselves. If the loomcycle process or the agent crashes between "subscribe returned" and "agent finished processing," the batch is lost.

**For at-least-once / crash safety, use `peek` + `ack`**:

1. `peek` — read messages without advancing the cursor.
2. Process them; persist the work somewhere durable.
3. `ack` with the cursor of the last-processed message — commits the cursor.
4. Next `peek` reads from the new committed cursor.

Cursor monotonicity is enforced: `ack` with a cursor older than the currently committed one returns `cursor_regression`. Cursors are opaque strings (ULID-shaped under the hood — sortable by publish time, agents should not parse them).

### Cursor scope (mirrors Memory)

- **`scope: agent`** — cursor is per agent name. Two researcher-agent runs share a cursor on the same channel (work continues across runs). Two DIFFERENT agents subscribing to the same agent-scoped channel each maintain their own queue.
- **`scope: user`** — cursor is per `user_id`. Two runs for the same user share a cursor — the "user-stream" shape. Cross-agent cursor sharing for the same user (the canonical "researcher → analyst" hand-off pattern).
- **`scope: global`** — one shared cursor for the whole channel. Cross-tenant fan-out broadcasts. Operator declares the channel explicitly; an unintentional `global` ACL can leak across tenants.

### Long-poll subscribe

`subscribe` accepts an optional `wait_ms`. If the storage read returns empty, the tool blocks on the in-process notification bus for that channel until either a new publish lands or the timeout fires. Cap is operator-controlled (`LOOMCYCLE_CHANNELS_LONGPOLL_CAP_MS`, default 30 s). Same-process subscribers wake within microseconds of a publish; cross-process subscribers (multi-replica deployments) fall back to polling — that's deferred to v0.9.x.

### Diagnostic logging (v0.12.7)

`LOOMCYCLE_CHANNEL_DEBUG=1` enables structured per-publish + per-retry log lines used to characterise the residual subscribe-empty race the v0.8.x bounded-retry workaround mitigates. Default off — these lines are noisy on a busy install. Operators flip the flag during a load test, capture a window of logs (~20 retry-saved lines), then flip it back. The two log shapes are:

- `channel "X" publish: id=... commit_us=N pool_total=A pool_acquired=B pool_idle=C` — fires on every successful publish. `commit_us` measures the postgres `tx.Commit` duration in microseconds; the pool fields capture pgxpool state at commit time.

- `channel "X" subscribe-race-recovered attempt=N msgs=K recovery_lag_ms=Y first_read_lag_us=Z from_cursor="..." pool_total=A pool_acquired=B pool_idle=C` — fires when the bounded retry rescued a subscribe after Bus.Wait returned via the waker but the immediate re-read found no rows. `first_read_lag_us` is the diagnostic field that distinguishes the race window (waker → empty read), separate from `recovery_lag_ms` (waker → eventually-non-empty read).

A complementary `subscribe-race-exhausted` log fires when all 3 retry attempts return empty. SQLite's parity logging (commit_us only — single-writer, no pool) lets operators A/B the same workload against both backends. See `~/work/loomcycle-internal/doc-internal/channel-race-investigation.md` for the hypothesis table.

### Quotas and overflow

- Per-write payload cap: `LOOMCYCLE_CHANNELS_MAX_VALUE_BYTES` (default 64 KB). 0 disables.
- Per-channel storage cap: `max_messages` in the operator yaml. When a publish would push the count past this, the OLDEST rows are trimmed inside the same txn. Publisher never blocks. The publish result includes `dropped_oldest: N`.
- TTL reaper: periodic background goroutine deletes expired rows. Cadence `LOOMCYCLE_CHANNELS_SWEEP_MS` (default 15 min). Read paths filter expired rows at WHERE regardless of sweeper cadence.

### Sub-agent inheritance

A spawned sub-agent inherits the parent's Channel ACL via ctx (mirror of `WithMemoryPolicy` / `WithHostPolicy`). A sub-agent cannot publish to a channel the parent couldn't. This is what makes the ACL story enforceable: no escalation across the `Agent` boundary.

### Typed audit events

Every successful `publish` and every delivered message surfaces a structured event on the run's SSE stream — distinct from the surrounding `tool_call` / `tool_result` envelope so SSE consumers can build channel-activity dashboards by filtering on event Type:

| Event type | Fires on | Payload |
|---|---|---|
| `channel_publish` | Successful Channel.publish | `channel`, `message_id`, `scope`, `scope_id`, `payload_bytes`, `payload_preview` (truncated at 200 chars), `dropped_oldest` (overflow trim count) |
| `channel_delivery` | One per message in a subscribe response (incl. replay batches) | `channel`, `message_id`, `scope`, `scope_id`, `payload_bytes`, `payload_preview`, `cursor` (= the message's own id) |

Publish events count **production** (one per successful `Channel.publish`). Delivery events count **consumption** (one per message returned to a subscriber — on a `cur_0` replay, every message in the batch fires a fresh delivery event). All the same information is available in the `tool_result` envelope; the typed events exist purely to avoid the need to parse every `tool_result` JSON to detect channel activity.

The payload preview is capped at 200 UTF-8 characters with a trailing `…` when truncated; adapters that need the full payload read it from the `tool_result` envelope (which carries the untruncated JSON). `payload_bytes` is always the full untruncated byte length.

### Out of scope for v0.8.4

- Cross-process / multi-replica notification — multi-replica subscribers fall back to polling. v0.9.x.
- Dead-letter queue / redelivery-count tracking.
- Streaming subscribe (tool emits events mid-call). Polling only; long-poll covers the real-time-ish use case.
- Schema enforcement on payloads — operator declares JSON schema per channel.
- Admin API for inspecting / purging channels. Manual SQL for v0.8.4.

References: `internal/store/store.go` (interface), `internal/tools/builtin/channel.go` (tool), `internal/channels/bus.go` (notification bus), `doc-internal/rfcs/channels-tool.md` (full design RFC, gitignored).

## LocalAPI tools — OpenAPI gateway (scaffolded; not the v0.4 integration vehicle)

Operators register a local HTTP API by pointing at an OpenAPI spec in YAML:

```yaml
local_api:
  spec: openapi.yaml          # relative to this YAML's directory
  base_url: http://localhost:3000
  tool_name_prefix: jobs       # tools become jobs__<operationId>
```

At config-load, loomcycle parses the spec and registers one tool per operation. The configured prefix determines the tool names (e.g. `jobs__listProjects`, `jobs__createApplication`). Each tool's input schema is derived from the OpenAPI parameters + request body schema. Agents call them like any other tool; loomcycle forwards the request to `base_url`.

**Status (v0.4.0).** Code, parser, dispatcher wiring, and unit tests are landed (`internal/tools/localapi/`). The runtime registers LocalAPI tools at startup when `cfg.LocalAPI.SpecPath` is non-empty. The first production consumer (jobs-search-agent) chose the MCP-server pattern instead — it runs its own `/api/mcp` Streamable-HTTP server exposing typed tools (e.g., `mcp__jobs__getAgentContext`, `mcp__jobs__patchApplication`), which loomcycle consumes through the existing MCP HTTP transport. LocalAPI stays available for future consumers that prefer "wire an OpenAPI spec, get typed tools" without standing up an MCP server.

**Why this matters when it lands.** Today an agent prompt has to spell out `GET http://localhost:3000/api/agent/context` as a string the model writes. The model occasionally invents wrong hostnames or paths (the cv-batch-adapter cv-adapter children burned all their iterations guessing hostnames in May 2026). With LocalAPI: the model sees a typed tool `jobs__getAgentContext` with parameter docs, and the URL string is loomcycle's responsibility, not the model's.

**Allowlist behaviour.** LocalAPI tools are subject to the same default-deny: agents must list them in `allowed_tools` (glob `jobs__*` works). The HTTP destination (`base_url`) is NOT subject to `LOOMCYCLE_HTTP_HOST_ALLOWLIST` — operator opting into a `local_api` entry is the explicit grant.

**No SSRF defense for `base_url`.** LocalAPI is for trusted internal APIs only. If you need allowlisted external HTTP access from agents, use the `HTTP` / `WebFetch` built-ins, which carry the full SSRF protection.

References: `internal/tools/localapi/`, `cmd/loomcycle/main.go` (registration), `loomcycle.example.yaml` (commented `local_api:` example).

## Provider × tool matrix

What works with what (Gemini added in v0.7.2 alongside the existing four backends):

|              | Anthropic | OpenAI | DeepSeek | Gemini | Ollama (tool-tuned) |
|---|:---:|:---:|:---:|:---:|:---:|
| `Read` / `Write` / `Edit`  | ✅ | ✅ | ✅ | ✅ | ✅ |
| `HTTP` / `WebFetch`        | ✅ | ✅ | ✅ | ✅ | ✅ |
| `WebSearch`                | ✅ | ✅ | ✅ | ✅ | ✅ |
| `Bash`                     | ✅ | ✅ | ✅ | ✅ | ✅ |
| `Agent` (sub-agents)       | ✅ | ✅ | ✅ | ✅ | ✅ |
| `Skill` (Approach A)       | ✅ | ✅ | ✅ | ✅ | ✅ |
| `LocalAPI` (OpenAPI gateway) | ⏳ | ⏳ | ⏳ | ⏳ | ⏳ | (scaffolded — for future OpenAPI-without-MCP-server consumers) |
| MCP tools (stdio + HTTP)   | ✅ | ✅ | ✅ | ✅ | ✅ |
| Native cache_control       | ✅ | ❌ | ❌ | ❌ | ❌ |
| Parallel tool calls        | ✅ | ✅ | ✅ | ✅ | depends on model |
| Streaming text + tool_use  | ✅ | ✅ | ✅ | ✅ | ✅ |
| `Usage.Model` populated    | ✅ | ✅ (v0.6.0+) | ✅ | ✅ | ✅ |
| Effort hint translation    | ✅ thinking.budget_tokens | ✅ reasoning_effort | ✅ (via OpenAI wrapper) | ✅ thinkingConfig.thinkingBudget | ❌ |

Ollama caveat: tool calling only works on **tool-tuned** models (qwen3+, Llama 3.1 instruct variants, Qwen2.5-Instruct, Mistral Nemo Instruct). Non-tool-tuned models will silently drop the `tools` field instead of calling them — Ollama's behaviour, not the driver's. Tool_use IDs are synthesized by the loop because Ollama doesn't issue them.

DeepSeek caveat: the v0.6.0 driver wraps the OpenAI driver, so it inherits OpenAI's behaviour exactly. The `deepseek-reasoner` model emits `reasoning_content` separately from `content`; the driver surfaces it as `EventDone.Reasoning` (since v0.7.0) and now also as live `EventThinking` events (since v0.7.1).

Gemini caveat: enabled by `GEMINI_API_KEY` env (Google AI Studio key). Optional `GEMINI_BASE_URL` overrides for Vertex AI deployments. Gemini reads the model name from the URL path (not the request body), uses `x-goog-api-key` header auth, and emits `functionCall` parts instead of OpenAI-style `tool_calls`. The driver translates loomcycle roles (`user` / `assistant`) to Gemini's (`user` / `model`). Tool_use IDs are synthesized by the loop because Gemini doesn't issue them. Effort hint translates to `generationConfig.thinkingConfig.thinkingBudget` on gemini-2.5-flash / gemini-2.5-pro: `low` → 0 (disable), `medium` → 2048, `high` → 8192 (clamped to `max_tokens - 1024` when budget would equal/exceed `max_tokens`). Thinking *output* is opaque (Gemini emits a `thoughtSignature` blob, not text) — no `EventThinking` plumbing yet; will revisit when Gemini opens up a text-trace surface.

## Tool-use hooks (v0.7.x+)

Operator-supplied middleware around tool dispatch. External apps register HTTP-webhook callbacks against `(agent, tool, phase)` selectors; loomcycle invokes them around the dispatcher so the hook can rewrite the input, short-circuit with a synthetic result, or rewrite the post-tool result.

The canonical use case is **wrapping untrusted content** from `WebFetch` / `HTTP` / MCP results in trust-boundary markers so a downstream LLM treats payloads as data rather than instructions. Other shapes the seam supports: per-tool quotas, audit logs, content sanitisation, soft-deny patterns ("you tried to fetch X; here's a redacted version instead"), OTEL spans tied to tool invocations.

### Registration

```
POST /v1/hooks
{
  "owner": "jobs-search-web",       // app UID; (owner, name) is the identity
  "name":  "scan-webfetch",
  "phase": "post",                  // "pre" | "post"
  "agents": ["*"],                  // glob list; empty = ["*"]
  "tools":  ["WebFetch", "HTTP"],   // glob list; empty = ["*"]
  "callback_url": "https://jobs-search-web.local/api/hooks/scan",
  "fail_mode": "open",              // "open" (default) | "closed"
  "timeout_ms": 5000                // default 5000, ceiling 60000
}
→ 200 { "id": "hook_xxx" }

GET    /v1/hooks               // debug listing
DELETE /v1/hooks/{id}          // remove a registration
```

**Idempotency**: re-registering the same `(owner, name)` **replaces** the prior entry in-place (preserves chain ordering, mints a fresh ID). Solves the cascading-on-restart problem cleanly: an app that re-registers on its own startup never accumulates duplicates.

**No persistence** across loomcycle restart. Apps re-register on their own startup. If your app is down, your hooks aren't active — which matches reality (the app can't process callbacks anyway).

### Filtering

- `agents`: array of exact matches or `["*"]`. Empty / missing = `["*"]`.
- `tools`: array of exact matches; supports `prefix*` glob (`mcp__jobs__*`). Empty / missing = `["*"]`.
- A hook fires when **both** match. Multiple hooks can match the same call — they chain in registration order (Pre) or reverse order (Post, LIFO middleware).

### Webhook payload

**Pre** (`POST <callback_url>`):
```json
{
  "phase": "pre", "owner": "...", "hook_name": "...",
  "agent": "company-researcher", "user_id": "...", "agent_id": "...",
  "tool_call": { "id": "...", "name": "WebFetch", "input": { "url": "..." } }
}
→ { "input": {...} }                                 // rewrite
→ { "deny": { "is_error": false, "text": "..." } }   // short-circuit
→ {} or 204                                          // no change
```

**Post** (`POST <callback_url>`):
```json
{
  "phase": "post", "owner": "...", "hook_name": "...",
  "agent": "company-researcher", "user_id": "...", "agent_id": "...",
  "tool_call": { "id": "...", "name": "WebFetch", "input": {...} },
  "tool_result": { "is_error": false, "text": "...page body..." }
}
→ { "result": { "is_error": false, "text": "<untrusted>...</untrusted>" } }
→ {} or 204                                          // no change
```

### Composition

- **Pre chain**: registration order. The first non-nil `deny` short-circuits the rest of the Pre chain.
- **Post chain**: reverse registration order (LIFO middleware). Each hook sees the result the prior (inner) hook produced.
- Hooks within the chain run sequentially per-tool. The **parallel tool dispatcher** (v0.7.x) runs N tool calls in parallel; their hook chains fire in parallel too. The webhook server must handle concurrent calls.

### Failure modes

- **`fail_mode: "open"`** *(default)*: webhook timeout / 5xx / network error → original input or result passes through unchanged. Right for telemetry-shaped hooks where the hook should never block tool dispatch when the registering app is down.
- **`fail_mode: "closed"`**: webhook timeout / 5xx / network error → tool fails with `IsError: true`. Right for security-shaped hooks (injection scanners) where a down hook would let bypassed payloads through.

### Trust-boundary invariants

- Hooks run **after** the policy layer (`allowed_tools` / `allowed_hosts`). Hooks may narrow further or rewrite content. The **only** exception is the per-call host-widening capability below, which requires explicit operator opt-in.
- Hooks **cannot** tear down the agent run. The worst they can do is short-circuit one tool call with a synthetic `IsError: true` result.
- Webhook payloads include `agent_id` and `user_id` for correlation but **do NOT** include the agent's prompt or message history.

### Per-call host widening (v0.8.17)

A Pre-hook can grant the model permission to fetch a hostname that's *not* on the operator's static `LOOMCYCLE_HTTP_HOST_ALLOWLIST` and *not* on the runtime caller's `runRequest.allowed_hosts`. The grant is **per-tool-call** — scoped to exactly one `Execute()`, never cached server-side, never inherited by sub-agents.

This solves the dynamic-discovery problem: `job-searcher` finds a job posting on a site nobody pre-enumerated; the calling service's hook checks its own per-user allowlist + reputation oracle + business rules and approves the URL on the spot.

#### Opt-in (operator yaml only)

```yaml
hooks:
  permit_host_widen:
    owners: ["jobs-search-web"]
```

Or env: `LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS=jobs-search-web,company-research`

- **Exact-string match** against `Hook.Owner`. No globs.
- Without an entry here, ANY hook's `allow_hosts` field is silently dropped at the dispatcher (with a `hooks: pre ... dropping grant` WARN log + counter increment via `Dispatcher.Stats().HostWidenDenied`). The model and the runtime caller cannot enable this — only the operator yaml can.
- **Caller-authoritative mode interaction**: when `LOOMCYCLE_HTTP_CALLER_AUTHORITATIVE=true`, a permitted hook STILL widens on top of the caller's authoritative list. Operator yaml is the floor for *behaviors* (including hook delegation), not just for the static host list.

#### Wire shape

A Pre-hook returns an optional `allow_hosts` field alongside the existing `input` / `deny`:

```json
{
  "input": {...},
  "deny": {"is_error": true, "text": "..."},
  "allow_hosts": ["acme.com", ".trusted-cdn.com"]
}
```

#### Matching semantics

Intentionally **stricter** than the operator allowlist:

- `"acme.com"` (no leading dot) → **exact hostname match only**. Won't widen `careers.acme.com`.
- `".acme.com"` (leading dot) → **suffix-match** (matches `acme.com` AND any subdomain). Symmetric with the operator-list shape.

This lets cautious hook authors be surgical ("approve only the literal URL the model asked about") while supporting the broader case via the leading-dot opt-in.

#### Composition

- Multiple permitted-owner hooks contributing `allow_hosts` in one chain → the dispatcher returns the **deduplicated UNION** (case-insensitive on hostname).
- A `deny` anywhere in the chain **discards all prior** `allow_hosts` grants (we don't carry policy widenings into a denied call — confused-deputy hardening).
- Order of `AllowHosts` in the outcome is first-seen across the Pre chain.

#### Audit event

The loop emits one `EventHostWidened` per dispatched call where widening fired:

```json
{
  "type": "host_widened",
  "host_widening": {
    "tool_call_id": "lc-0-0",
    "tool_name": "WebFetch",
    "url": "https://acme.com/careers/123",
    "hook_owner": "jobs-search-web",
    "hook_name": "url-gate",
    "hosts_added": ["acme.com"]
  }
}
```

Persisted to the `events` table via `makeRecordingEmit` like every other typed event. Operators audit confused-deputy patterns by joining on `tool_call_id` and comparing the requested URL's host to the granted `HostsAdded` — if they're always identical for one owner, the hook is probably echoing model input without independent validation.

Aggregate counters via `Dispatcher.Stats()`:

```go
type DispatcherStats struct {
    HostWidenPermitted int64 // grants honoured
    HostWidenDenied    int64 // grants dropped (owner not in permit list)
}
```

#### SECURITY — confused-deputy hazard

The Pre-hook's `tool_call.input` is **model-generated and untrusted**. A naive hook that echoes `input.url`'s hostname back as `allow_hosts` literally lets the model widen its own allowlist. **Validate independently** — against the user's own preferences, a per-tenant allowlist, a domain-reputation service, your own business rules. Never trust the URL the model is asking about as authority for whether the URL should be approved.

#### Known limitations (v1)

1. **Redirect to a new unknown host within a single fetch** is NOT re-validated by the hook. The extras attached pre-dispatch cover the entire tool call (initial URL + all redirects), so a redirect to a hostname the hook *did* approve up-front works. But a redirect to a brand-new host nobody approved fails. Practical workaround: hooks for redirect-heavy use cases (job boards proxied through ATS trackers) should approve the likely redirect targets too.
2. **Dial-time private-IP block is NOT bypassable.** Even if `allow_hosts` includes `localhost`, the host's resolved private IP is still blocked unless the operator separately opted in via `HTTPPrivateHostAllowlist`. This is a separate, orthogonal trust boundary — hooks widen at the hostname layer, never at the IP layer.
3. **Sub-agents do NOT inherit a parent's widening.** Per-tool-call scope means the grant evaporates the moment Execute() returns. If a sub-agent needs the same widening, the same hook needs to be registered against the sub-agent's name (via the hook's `agents:` glob).
4. **No server-side caching.** Every WebFetch to a hostname outside the static list triggers a fresh callback. Hooks should cache client-side if their decision is stable for the user/tenant.

References: `internal/hooks/`, `internal/api/http/hooks.go` (registration routes), `internal/loop/loop.go::dispatchOneTool` (chain invocation), `internal/config/config.go::HooksConfig` (operator opt-in shape), `internal/tools/builtin/httptool.go::hostAllowedExtras` (matcher).

## The `Interruption` tool — human-in-the-loop primitive (v0.8.16)

The human bridge in the v0.8.x substrate. Three ops:

```
Interruption.ask(question, options?, context?, timeout_ms?, priority?)
  → blocks the loop; tool result carries the human's answer
Interruption.notify(message, priority?)
  → fire-and-forget message
Interruption.cancel(interruption_id)
  → agent unblocks a previously-asked question it answered itself
```

### Why "Interruption" not "Question"?

Generalises the previously-planned Question tool with a broader option set — v0.8.16 only writes `kind: question`, but the schema's `kind` column + the `_system/interrupts/*` channel namespace are forward-compatible for future `pause` / `wait_until` / `approval` kinds without reopening the design.

### Three delivery surfaces

Operator picks one via `interruption.backend:`:

| Backend | Where the human sees it | When to pick it |
|---|---|---|
| `webui` (default) | `/ui/interrupts` inbox in the embedded Web UI | Production. The cookie-authed session matches the run's `user_id`. |
| `mcp_server:<name>` | Consumer's own MCP server tool (`mcp__<name>__ask`) | When the consumer already runs an MCP server (e.g. jobs-search-agent's `/api/mcp`) and wants to integrate question rendering into its own UI. |
| `cli` | Local-dev stdin/stdout (operator runs a separate `loomcycle-interrupt-cli`-style script that subscribes to `_system/interrupts/pending` and posts the resolve endpoint) | Local development. |

The agent-facing tool surface is identical across all three; only the resolve path differs.

### Per-agent ACL

```yaml
agents:
  batch-processor:
    allowed_tools: [Read, Memory, Interruption, Context]
    interruption:
      enabled: true
      kinds: [question]      # v0.8.16 only value; future: "pause", "approval"
      max_pending: 1         # per-run cap; 0 = use operator default
```

Default-deny: missing the `interruption` block means every op returns `is_error` with a clear "not enabled" refusal. Same shape as `memory_scopes` / `agent_def_scopes` / `skill_def_scopes` / `evaluation_scopes`.

### Operator config

```yaml
interruption:
  backend: webui                     # webui | mcp_server:<name> | cli
  default_timeout_ms: 3600000        # 1h — when an `ask` doesn't pass timeout_ms
  max_timeout_ms: 86400000           # 24h — hard ceiling
  max_pending_per_run: 10            # operator global cap; agent yaml narrows
  heartbeat_interval_ms: 30000       # how often the during-block heartbeat ticks
```

The during-block heartbeat is load-bearing — without it, a question that waits an hour gets reaped by the v0.5.0 sweeper (default `StaleAfter` 10 min, calibrated for tool-dispatch durations). The Interruption tool spawns a dedicated ticker that fires `Store.UpdateHeartbeat` directly while blocked.

### System channels

The signal flow rides on the v0.8.6 `_system/*` namespace — operator declares two channels in the top-level `channels:` block:

```yaml
channels:
  _system/interrupts/pending:
    scope: user
    semantic: broadcast
    publisher: system
    default_ttl: 3600
  _system/interrupts/resolved:
    scope: user
    semantic: broadcast
    publisher: system
    default_ttl: 86400
```

The Web UI inbox subscribes to `_system/interrupts/pending` via the existing Channel subscribe path. The resolve endpoint publishes to `_system/interrupts/resolved` so external dashboards / Slack bots can render the audit trail.

### Wire surface

| Endpoint | Purpose |
|---|---|
| `POST /v1/runs/{run_id}/interrupts/{interrupt_id}/resolve` | Submit an answer. Validates options (422), expiry (410), already-terminal (409). |
| `GET /v1/runs/{run_id}/interrupts?status=<pending\|resolved\|...>` | List per-run interrupts (audit trail). |
| `GET /v1/users/{user_id}/interrupts?status=pending` | User-scoped inbox (drives the Web UI). |
| `mcp__loomcycle__interruption_resolve` | 21st LoomCycle MCP meta-tool. Lets an external orchestrator (Claude Code) act as the answerer for any pending interrupt. |

### Storage

New `interrupts` table (migration 0011). Columns: `interrupt_id`, `run_id`, `kind` (closed enum), `status` (`pending`/`resolved`/`timed_out`/`cancelled`), `question`, `options` (JSON array), `context_data`, `priority`, `answer`, `answer_meta` (JSON; future-kind discriminated), timestamps, `resolved_by`, and denormalised `user_id` / `agent_id` / `agent_name` for the listing queries.

### SSE event

`EventInterruptionPending` (type `interruption_pending`) on the run's SSE stream with `interruption: {interrupt_id, kind, question, options, context, priority, expires_at}`. Web UI consumers use this for real-time modal rendering without a follow-up fetch.

### Architecture references

- `doc-internal/rfcs/interruption-tool.md` — RFC bundling concept + 5-PR implementation plan
- `internal/tools/builtin/interruption.go` — tool implementation (Bus.Wait blocking + heartbeat ticker + ACL gate)
- `internal/api/http/server.go::handleResolveInterrupt` — resolve endpoint
- `internal/api/mcp/handlers.go::handleInterruptionResolve` — 21st MCP meta-tool

