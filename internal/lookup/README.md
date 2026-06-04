# `internal/lookup`

The canonical seam between loomcycle's static-yaml era and its v0.8+ dynamic substrate.

## Why this package exists

Loomcycle pre-v0.8 had ONE load path for `config.AgentDef`:

```
yaml → config.LoadConfig → resolveSkills / resolveAgent → cfg.Agents
```

`cfg.Agents` was the single source of truth, and `resolveSkills` applied a chain of normalizations at boot (SystemPromptBase capture, allowed-tools widening check, skill body baking).

v0.8.15 added `dynamic_agents` (RegisterAgent path). v0.8.22 added the AgentDef substrate (`agent_defs` + `agent_def_active`). Both new READ paths skipped the normalizer chain. The result was a class of bugs we paid for one PR at a time:

| PR | Symptom |
|----|---------|
| #184 | `/v1/runs` returns "unknown agent" for substrate-registered names — `lookupAgent` only consulted `cfg.Agents` + `dynamic_agents`. The fix added the third tier AND a `substrateAgentDef` adapter (because `config.AgentDef` has yaml-only tags; substrate persistence uses snake_case json — every field silently dropped on `json.Unmarshal` into `config.AgentDef`). |
| #186 | Substrate-registered agents lost their system prompt at every skill-enabled run. `resolveSkillBodiesForRun` rebuilds `SystemPrompt = SystemPromptBase + skill bodies`, and the dynamic load path left `SystemPromptBase` empty. |

This package consolidates the lookup chain + the normalizer chain + the adapter into one canonical place. The runtime contract: **a substrate-pushed AgentDef MUST produce a `config.AgentDef` byte-equivalent to the same content loaded from yaml.** Verified by `TestAgent_EquivalenceYamlVsSubstrate`.

## When to use it

Every code path that needs to resolve an agent / skill / MCP server NAME to its effective runtime def should go through this package:

```go
def, ok := lookup.Agent(ctx, s.store, s.cfg, tenantID, name)
sk, ok  := lookup.Skill(ctx, s.store, s.skillSet, name)
spec, ok := lookup.MCPServer(s.cfg, dynRegistry, name)
```

### Agent resolution precedence (RFC N — tenant axis)

`lookup.Agent` resolves within the caller's `tenantID`:

1. **(tenantID != "")** tenant-scoped dynamic — `dynamic_agents` then
   `agent_def_active`, both `WHERE tenant_id=tenantID`. A per-tenant
   registration *shadows* the shared static base by name.
2. **static `cfg.Agents`** — the shared operator base every tenant inherits.
3. **shared dynamic** — `dynamic_agents` then `agent_def_active`,
   `tenant_id=""`.

For the default tenant `""`, step 1 is skipped, so the order collapses to
**static-cfg → shared-dynamic** — byte-for-byte the pre-RFC-N behavior. A
single-tenant deployment (everything `tenant_id=""`) is unchanged. The
`tenantID` MUST come from the authoritative principal in ctx
(`auth.PrincipalFromContext` → `tools.RunIdentity` fallback → `""`), never
from a wire/request field — see `internal/api/http/server.go`'s
`tenantFromCtx`. (Skills + MCP servers still resolve globally; their tenant
axis is a follow-up.)

Don't read substrate rows directly. Don't unmarshal into `config.AgentDef` (use the json-tagged adapters). The pattern is enforced by the drift tests (`TestAgent_DriftDetection` + `TestMergedDef_DriftDetection_VsLookupSubstrateAgentDef`) — a future field added to one shape without the other fails CI.

### Coverage matrix

| Resolver | Production callers | Notes |
|----------|-------------------|-------|
| `lookup.Agent` | `api/http/server.go:lookupAgent` (covers `/v1/runs`, `/v1/messages`, sub-agent spawn). | Drives the load-bearing static-vs-substrate equalization. |
| `lookup.Skill` | `api/http/server.go:resolveSkillBodiesForRun` (slow-path bake). | Logs non-NotFound substrate errors as a side effect — operator gets a signal when a transient store hiccup forces a substrate→static fallback for one skill. |
| `lookup.MCPServer` | `/ui/library/mcp-servers` GET endpoint (when added; HTTP-only call sites). | **CANNOT** replace `cmd/loomcycle/main.go`'s pool build callback — that callback needs `Command/Args/Env/PoolSize` for stdio, which `MCPServerSpec` deliberately omits because the substrate refuses stdio at the create boundary. The stdio path stays inline-in-main.go by design. |

## Architectural pattern for new substrates

When adding a dynamic substrate for a domain type:

### 1. Read path walks the same normalizer chain as boot-time

If `resolveSkills` (or the equivalent boot-time helper for your domain) sets a derived field for the static path, the dynamic resolver MUST replicate it. Add a normalizer to this package (`NormalizeAgentDef`-style) and call it on every dynamic load.

### 2. Persistence uses explicit JSON tags

Unmarshal targets a struct WITH json tags (the `Substrate*` adapters in this package), then convert to the runtime consumer type via a single named `ToConfigDef` method. NEVER `json.Unmarshal` directly into a yaml-only struct — Go silently no-ops on every field, and you get the bug PR #184 fixed.

### 3. Equivalence tests pin the contract

Add a test that loads the SAME content via yaml AND via the substrate, then asserts field-by-field equivalence. The test is the canonical "did I forget to replicate a normalizer?" check.

### 4. Drift tests prevent future field-addition gaps

Reflection-based audit pinning every json tag in the persistence shape against an explicit `want` set. Adding a field to the substrate's `mergedDef` (or equivalent) without adding it here AND updating the `want` set fails the test — forcing a conscious decision.

## Belt-and-suspenders normalization

The package implements normalization at TWO sites for the `system_prompt_base` invariant:

| Site | When | Purpose |
|------|------|---------|
| `NormalizeAgentDef` (read-side) | Every `lookup.Agent` call | Runtime correctness floor — works on legacy rows that pre-date the write-side fix. |
| `mergedDef.normalize()` in `agentdef.go` (write-side) | Every `AgentDef.create / fork / bootstrapStatic` | On-disk correctness floor — new rows persist the right shape. |
| `BackfillAgentDefSystemPromptBase` (one-shot at boot) | Once per process boot | Migrates legacy rows so the on-disk data eventually converges. |

The read-side normalizer is the load-bearing one — production runtime correctness depends on it, regardless of what's on disk. The write-side normalizer + backfill close the on-disk data gap so snapshot/restore round-trips are clean.

## What this package does NOT do

- **Mutate.** Read-only by design. Writes go through the substrate tools (`AgentDef.create` / `SkillDef.create` / `MCPServerDef.create`).
- **Apply policy.** Allowed-tools widening checks, scope checks, etc. stay at the substrate tool layer (write-side) and the runtime dispatcher (read-side). The resolver is a pure-data shape converter.
- **Cache.** Each call hits the store. If perf becomes a concern, add caching here behind the same interface; consumers don't need to change.

## Related references

- PR #184 (lookup fallthrough + JSON adapter): the symptomatic fix.
- PR #186 (read-side normalizer): the symptomatic fix.
- `internal/config/config.go:resolveSkills` — the boot-time normalizer this package mirrors.
- `internal/tools/builtin/agentdef.go:mergedDef` — the substrate persistence shape; `SubstrateAgentDef` is its public mirror.
