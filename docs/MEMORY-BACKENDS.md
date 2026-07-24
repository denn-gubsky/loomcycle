# Pluggable memory backends

loomcycle's `Memory` tool stores agent- and user-scoped key/value state.
By default that state lives **in-process** — in loomcycle's own SQLite or
Postgres store, embedded for semantic search by the configured embedder.
RFC I MR-4 adds a **pluggable backend** seam so the same `Memory` tool can
route to an external memory server instead, per agent, without any agent
prompt change.

This page covers the backend model, the `MemoryBackendDef` schema, and the
first non-default backend: **Mem9**.

## Why pluggable backends (and why not a memory subsystem)

We chose a *pluggable backend* over coupling loomcycle to one memory
product. A subsystem coupling would regress loomcycle's per-user +
per-agent + per-tenant isolation (external products tend toward a flat
"one API key = one shared space" model), break the single-Go-binary
posture, and bind loomcycle's roadmap to a third party's pricing and
longevity. A pluggable backend keeps all the upside — an operator who runs
other memory-consumer products can share a memory pool across them — with
none of those couplings. The in-process backend stays the default and the
unconditional fallback.

**Thesis:** the backend is an operator deployment choice, expressed in
config; agents are backend-agnostic and never see which backend served a
recall.

## The backend model

Every key/value `Memory` op (get/set/delete/list/search) routes through
a `memory.Backend`. An agent's `memory_backend: <name>` field selects a
named backend; absent, the agent uses the operator-default (in-process).
The backend NAME is operator-resolved and stamped onto the run — it is
**never** model/tool input (same trust posture as the memory scope).

Two backend kinds ship today:

| kind        | where state lives                        | when to use |
|-------------|------------------------------------------|-------------|
| `inprocess` | loomcycle's own store + embedder (default) | single binary, no external dependency, lowest latency |
| `mem9`      | an external Mem9 REST server             | cross-runtime memory sharing; you already run Mem9 |

Backends are declared under `memory_backends:` in operator yaml, or
authored at runtime via the `MemoryBackendDef` tool (forks of the static
roots). Resolution precedence: static yaml first, then the active
substrate def.

## MemoryBackendDef schema

```yaml
memory_backends:
  <name>:
    kind: inprocess | mem9
    config:                       # kind=mem9 only
      base_url: "https://..."     # Mem9 server root (no /api_version suffix)
      api_version: "v1alpha2"     # REST version segment (default v1alpha2)
      api_key_env: "LOOMCYCLE_MEM9_..."   # env-var NAME holding the X-API-Key
    tenancy_strategy:             # kind=mem9 only; see "Tenancy" below
      kind: key_per_tenant | shared_key_with_prefix
      env_pattern: "LOOMCYCLE_MEM9_TENANT_{tenant_id}_API_KEY"  # key_per_tenant
      prefix_pattern: "tenant-{tenant_id}::"                    # shared_key_with_prefix
    fallback_on_error: inprocess  # optional — degrade to local memory on backend error
    health_check_interval_seconds: 60   # optional
```

`api_key_env` is an env-var **name**, never a plaintext key. The value is
resolved at use time, gated by the env-allowlist (see below).

## Mem9 setup recipe

> ⚠ **VERIFY THE WIRE MAPPING BEFORE PRODUCTION.** The Mem9 backend is
> implemented against the documented `v1alpha2` REST shape and a CI
> `httptest` stub that defines those shapes. Only ONE endpoint — `POST
> {base_url}/{api_version}/search` with `X-API-Key` auth — is specified
> concretely by the RFC; **every other endpoint and request/response JSON
> shape is an ASSUMED contract** reconstructed from a plausible v1alpha2
> surface. They are NOT verified against the real
> `github.com/mem9-ai/mem9` API. The wire shapes are isolated at the top
> of `internal/memory/backends/mem9/mem9.go`, each tagged `// ASSUMED Mem9
> wire shape …`. Before relying on this backend, verify those shapes
> against your Mem9 version and adjust that block (and the CI stub) if
> they differ. The CI tests prove the loomcycle-side mapping is
> internally consistent — NOT that it matches real Mem9.

### 1. Allowlist the API-key env var

The Mem9 `X-API-Key` is resolved through the **same env-allowlist** the
scheduler and webhooks use (no new credential surface). Add the env-var
name(s) to `LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST` and set the value in your
secrets layer (e.g. `.env.local`):

```
LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST=LOOMCYCLE_MEM9_MANAGED_API_KEY
# in .env.local (never commit this):
LOOMCYCLE_MEM9_MANAGED_API_KEY=...
```

A non-allowlisted or unset key is a **hard refusal**, not a silent
unauthenticated call: the op fails loud, or — if `fallback_on_error:
inprocess` is set — degrades to local memory.

### 2. Declare the backend and route an agent to it

```yaml
memory_backends:
  default:
    kind: inprocess
  mem9-managed:
    kind: mem9
    config:
      base_url: "https://api.mem9.ai"
      api_version: "v1alpha2"
      api_key_env: "LOOMCYCLE_MEM9_MANAGED_API_KEY"
    tenancy_strategy:
      kind: key_per_tenant
      env_pattern: "LOOMCYCLE_MEM9_TENANT_{tenant_id}_API_KEY"
    fallback_on_error: inprocess

agents:
  job-search-batch:
    memory_backend: mem9-managed
```

### 3. Credential resolution order

At each op, the backend resolves the `X-API-Key` in this order:

1. **Per-run credential** — `RunIdentity.UserCredentials[<api_key_env>]`
   (RFC F). The credential key is the `api_key_env` name, so a caller can
   override the env value per run without a second naming scheme.
2. **Env fallback** — `os.Getenv(<env-name>)`, allowlist-gated. The
   env-name is `api_key_env`, or — for `key_per_tenant` with an
   `env_pattern` — the pattern with `{tenant_id}` substituted.

The key is sent as the `X-API-Key` header only. It is **never** logged,
never placed in an error message, and never attached to an OTEL span.

## Tenancy

Per-tenant scoping is enforced **loomcycle-side**, not delegated to Mem9
(whose native model is a flat keyspace). Pick a strategy at admin write
time:

- **`key_per_tenant`** (default) — one Mem9 API key per tenant, resolved
  from `env_pattern` with `{tenant_id}` substituted. Tenant isolation
  comes from the distinct key; keys are NOT prefixed. This is the standard
  posture.
- **`shared_key_with_prefix`** — one shared key; every memory key is
  prefixed with `prefix_pattern` (`{tenant_id}` substituted), so tenants
  are isolated within one flat keyspace. Use when Mem9 keys are
  rate-limited or the deployment is single-tenant. A missing tenant on the
  run is a **hard error** here — an empty prefix would collapse all
  tenants into one keyspace.

> **`{tenant_id}` source (assumption).** loomcycle's run identity carries
> no dedicated tenant field today, so `{tenant_id}` is the run's
> `user_id` — which is operator/caller-authoritative (the API layer stamps
> it; it is never model input, same trust posture as the memory
> scope_id). Single-tenant deployments have one stable `user_id` and one
> resolved key. If a first-class tenant field is added to the run identity
> later, only the resolver changes — the config surface is stable.

## fallback_on_error

`fallback_on_error: inprocess` wraps the remote backend so any backend
error (network outage, auth failure, bad config) **degrades to the
in-process backend** instead of failing the run. The degradation is logged
(`memory backend ... failed ..., falling back to in-process`) and visible
on OTEL spans. A genuine "key not found" from the remote is NOT treated as
a failure — it passes through unchanged so a deletion isn't masked by a
stale local copy.

Without `fallback_on_error`, a backend error surfaces to the agent as a
tool error.

> ⚠ **`fallback_on_error` is mutually exclusive with the memory-layer
> `add` / `recall` ops** (see below). The in-process fallback is a
> key/value store and cannot honor a semantic add/recall, so a
> fallback-wrapped memory-layer backend reports `capability_unsupported`
> for those two ops — fail-closed, never a silent degrade that would
> drop facts. Use fallback for the six key/value ops, or a memory layer
> for `add`/`recall`, but not both on one backend.

## Memory layer: add / recall

Beyond the six key/value ops the Memory tool serves the `add` / `recall`
paradigm: you hand it conversation messages and let the system distil
durable facts, then answer natural-language recall queries over them.
There are no caller keys; identity is server-assigned. **The default
in-process backend now serves this natively** — `add` enqueues the
messages onto a durable queue for background consolidation; `recall` is a
semantic search over stored memories. An external memory-layer backend
may instead extract facts server-side with its own LLM.

```jsonc
// ingest a conversation — infer defaults to true (queued for consolidation)
{"op": "add", "scope": "user",
 "messages": [{"role": "user", "content": "I prefer dark mode"}]}
// → {"status": "pending"}   (async — no read-after-write guarantee)

// recall stored memories by meaning
{"op": "recall", "scope": "user", "query": "ui preferences", "top_k": 5}
// → {"facts": [{"id": "mem_ab12…", "memory": "user prefers dark mode", "score": 0.91}]}
```

`recall` needs the vector stack (an embedder + a vector-capable store);
without it, it refuses with `vector_unsupported` / `embedder_not_configured`.
`capability_unsupported` is reached only for a backend that does not
implement the memory-layer contract at all. `add` / `recall` honor the
agent's `memory_scopes` and the run's tenant exactly like the key/value
ops. See the `memory-layer` `Context.help` topic for the full op reference.

## Observability

Each `memory.search` against the Mem9 backend emits an OTEL span
(`loomcycle.memory.search`) with `memory.backend=mem9`,
`memory.top_k`, and `memory.recall_latency_ms` — so an operator sees the
network hop versus in-process latency on the existing trace dashboards. No
secrets, query text, or transcripts are placed on spans.

## When to use which

- **in-process** — the default. Choose it unless you have a specific
  reason to externalize memory. Lowest latency, no external dependency,
  full per-scope isolation, native to the single-binary deployment.
- **mem9** — choose it when you already run Mem9 (or other Mem9-consuming
  runtimes) and want a shared memory pool across them, and you have
  verified the wire mapping against your Mem9 version. Expect a network
  hop on every recall; pair with `fallback_on_error: inprocess` so a Mem9
  outage degrades gracefully.
