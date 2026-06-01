package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/fallback"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/inprocess"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/mem9"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Memory is the v0.8.0 built-in tool that exposes persistent
// agent-scoped key/value storage to the model.
//
// Five operations, discriminated by the `op` field:
//
//	get     — read one entry by key
//	set     — write/overwrite one entry, optional TTL (seconds)
//	delete  — remove one entry; returns whether it existed
//	list    — enumerate keys with an optional prefix filter
//	incr    — atomic add over a JSON-number value (counter primitive)
//
// scope_id is RESOLVED SERVER-SIDE based on the agent's run context —
// the model picks the SCOPE (agent vs user); loomcycle picks the
// SCOPE_ID:
//
//   - scope=agent → yaml agent name from tools.AgentName(ctx)
//   - scope=user  → user_id from tools.RunIdentity(ctx)
//
// This split is non-negotiable: a model-supplied scope_id would let
// one user's agent run read another user's keys.
//
// Quota enforcement happens at write time (set / incr). The agent's
// per-yaml `memory_quota_bytes` overrides the global default; both
// land via tools.MemoryPolicy(ctx).
type Memory struct {
	// Store is the persistence backend. Required; nil disables the
	// tool entirely (every call returns an is_error tool_result with
	// a "Memory not configured" message — operators see one clear
	// failure rather than panics).
	Store store.Store

	// MaxValueBytes caps a single write's value size (the Set / Incr
	// payload). 0 = no per-write cap. Sourced from
	// LOOMCYCLE_MEMORY_MAX_VALUE_BYTES.
	MaxValueBytes int

	// DefaultQuotaBytes is the per-(scope, scope_id) byte cap when
	// the agent yaml doesn't override it. 0 = no cap. Sourced from
	// LOOMCYCLE_MEMORY_MAX_SCOPE_BYTES.
	DefaultQuotaBytes int

	// Embedder is the v0.9.0 Vector Memory provider. Nil = vector ops
	// refuse with ErrEmbedderNotConfigured ("set memory.embedder in
	// operator yaml"); the k/v ops continue to work unchanged.
	//
	// Wired into main.go at boot from cfg.Memory.Embedder. Late-bound
	// like Store so the Memory tool can be constructed before
	// embedder construction (the order matters when operators have
	// memory.embedder unset — we don't want a nil dereference in
	// boot ordering).
	Embedder providers.Embedder

	// Backend is the RFC I (MR-2) pluggability seam. The data ops
	// (get/set/delete/list/search) route through it instead of calling
	// Store directly, so MR-3's MemoryBackendDef + MR-4's Mem9 backend
	// can plug in here. When nil, Execute lazily defaults it to the
	// in-process backend wrapping Store + Embedder — so a tool
	// constructed with only Store + Embedder set (as the tests and any
	// pre-MR-2 caller do) behaves identically. main.go sets this
	// explicitly post-store.
	//
	// Incr, quota math, and the reducer ops (merge/append_dedupe/
	// bounded_list) stay on Store directly — they are not part of the
	// six-op Backend surface (see internal/memory/backend.go).
	Backend memrank.Backend

	// Cfg is the operator config, used to resolve a per-agent
	// memory_backend NAME to its MemoryBackendDef via lookup.MemoryBackend
	// (RFC I MR-3b). Set in main.go (memoryTool.Cfg = cfg). When nil, the
	// per-agent routing path can't resolve named backends and every agent
	// falls back to the operator-default backend — the pre-MR-3b behavior.
	Cfg *config.Config

	// EnvAllowlist gates which env vars the mem9 backend may read for its
	// X-API-Key (RFC I MR-4 / Decision 10). Set in main.go from
	// cfg.Env.SchedulerEnvAllowlist — the same allowlist the scheduler +
	// webhooks use, so there's no new credential surface. An empty / nil
	// allowlist means NO env var is readable: a mem9 backend whose key
	// comes from env then refuses to construct and the agent falls back to
	// in-process (a non-allowlisted key must never produce a silent
	// unauthenticated call).
	EnvAllowlist map[string]bool
}

// backend resolves the memrank.Backend the data ops route through,
// honoring the agent's per-run memory_backend NAME (RFC I MR-3b).
//
// The backend name comes from tools.MemoryPolicy(ctx).Backend — which is
// stamped from the operator-resolved agent config — and is NEVER
// model/tool input. Same trust posture as MemoryScopes: the model picks
// the scope, the operator picks the backend.
//
// Resolution:
//   - "" (no per-agent backend) → the operator-default backend. This is
//     the pre-MR-3b path and stays byte-identical: m.Backend if set, else
//     a lazily-constructed in-process backend wrapping Store + Embedder.
//   - a named backend → resolved via lookup.MemoryBackend (static
//     memory_backends yaml OR a dynamic MemoryBackendDef). A name that
//     resolves to nothing degrades to the operator-default backend with a
//     log: dynamic Defs may not exist at config-load time, so a missing
//     name must NOT fail — it falls back.
//
// kind dispatch on a resolved Def:
//   - "" / "inprocess" → fresh in-process backend (cheap, stateless).
//   - "mem9" → NOT yet wired (lands in MR-4). Logs and falls back to
//     in-process. The Def's fallback_on_error field is honored in MR-4
//     once mem9 can actually fail at runtime; until then fallback is the
//     unconditional default.
//   - anything else → unknown kind; logs and falls back to in-process.
func (m *Memory) backend(ctx context.Context) memrank.Backend {
	name := tools.MemoryPolicy(ctx).Backend
	if name == "" {
		return m.defaultBackend()
	}
	def, ok := lookup.MemoryBackend(ctx, m.Store, m.Cfg, name)
	if !ok {
		log.Printf("memory: memory_backend %q not found — using operator-default backend", name)
		return m.defaultBackend()
	}
	switch def.Kind {
	case "", "inprocess":
		return inprocess.New(m.Store, m.Embedder)
	case "mem9":
		return m.buildMem9(ctx, name, def)
	default:
		log.Printf("memory: memory_backend %q has unknown kind %q — using in-process fallback", name, def.Kind)
		return m.defaultBackend()
	}
}

// memoryLayer resolves the MemoryLayer capability for the agent's configured
// backend (RFC K). It returns (layer, true) when the resolved backend
// implements the add/recall memory-layer paradigm, or (nil, false) when it
// does not — e.g. the default in-process KV+vector backend, which is not a
// memory layer. The caller (execAdd/execRecall) turns false into the typed
// capability_unsupported refusal.
//
// It reuses backend(ctx) so backend selection (the per-agent memory_backend
// Def, the fallback wrapper, the unknown-name degradation) stays in one
// place — the memory-layer view is just a capability probe over the same
// resolved backend. A fallback-wrapped backend is treated as a layer only if
// the wrapper itself surfaces the capability; today the in-process fallback
// is not a layer, so a mem9 layer wrapped for fallback degrades to "no layer"
// rather than silently routing add/recall to in-process KV (which can't
// honor them) — fail-closed.
func (m *Memory) memoryLayer(ctx context.Context) (memrank.MemoryLayer, bool) {
	return memrank.AsMemoryLayer(m.backend(ctx))
}

// buildMem9 constructs the RFC I MR-4 Mem9 REST backend for a resolved
// kind=mem9 Def, wraps it in the fallback backend when the Def opts into
// fallback_on_error=inprocess, and degrades to the operator-default
// backend on any CONSTRUCTION error (e.g. an unresolvable / non-allowlisted
// key, or a missing tenant). A construction failure must never fail the
// agent — it logs and falls back, the same posture as an unresolved name.
//
// Credentials are resolved PER OP by the injected CredentialResolver, not
// here; this only validates that the Def is structurally constructible.
func (m *Memory) buildMem9(ctx context.Context, name string, def config.MemoryBackend) memrank.Backend {
	tenancy, prefix, err := resolveTenancy(ctx, def.TenancyStrategy)
	if err != nil {
		log.Printf("memory: memory_backend %q (mem9) tenancy unresolved: %v — using in-process fallback", name, err)
		return m.defaultBackend()
	}

	resolver := m.mem9CredentialResolver(def, def.TenancyStrategy, prefix)

	b := mem9.New(mem9.Config{
		BaseURL:            def.Config.BaseURL,
		APIVersion:         def.Config.APIVersion,
		Tenancy:            tenancy,
		CredentialResolver: resolver,
		BackendName:        name,
	})

	// fallback_on_error=inprocess wraps the remote backend so a Mem9
	// outage degrades to local memory instead of failing the run.
	//
	// SCOPE OF FALLBACK (RFC K caveat): fallback applies ONLY to the six KV
	// ops. The fallback.Backend wrapper does NOT implement MemoryLayer (it
	// would have to fake add/recall against KV — the lobotomization RFC K
	// rejects), so a mem9 backend wrapped here will FAIL the
	// AsMemoryLayer assertion in memoryLayer(): add/recall then return
	// capability_unsupported even though the underlying mem9 IS a layer.
	// That is fail-closed-correct — the in-process KV fallback cannot honor
	// a semantic add/recall, so degrading to it would be wrong. The
	// trade-off: fallback_on_error and memory-layer add/recall are mutually
	// exclusive for the same backend.
	if def.FallbackOnError == "inprocess" {
		return fallback.New(b, m.defaultBackend(), log.Printf)
	}
	return b
}

// resolveTenancy maps a MemoryBackendDef tenancy_strategy onto the mem9
// package's resolved Tenancy + the tenant_id used for env-pattern
// resolution. {tenant_id} is substituted from the run's tenant.
//
// TENANT SOURCE (assumption, surfaced loudly): loomcycle's
// RunIdentityValue carries no dedicated tenant field, so we use
// tools.RunIdentity(ctx).UserID as {tenant_id}. The UserID is
// operator/caller-authoritative (the API layer stamps it; it is NEVER
// model input — same trust posture as the Memory scope_id), so using it
// as the tenant partition key does not open a model-controlled
// cross-tenant path. The RFC's worked example uses per-tenant user
// identities (alice@tenant-a, bob@tenant-b); single-tenant deployments
// have one stable UserID and one resolved key. If a first-class tenant
// field lands on RunIdentityValue later, change ONLY this function.
func resolveTenancy(ctx context.Context, ts config.MemoryBackendTenancy) (mem9.Tenancy, string, error) {
	tenantID := tools.RunIdentity(ctx).UserID

	switch ts.Kind {
	case "", "key_per_tenant":
		// No key-prefixing — tenant isolation comes from the per-tenant
		// API key the resolver returns. No prefix in the keyspace.
		return mem9.Tenancy{}, tenantID, nil
	case "shared_key_with_prefix":
		// One shared key; tenant isolation comes ENTIRELY from prefixing
		// every key with the per-tenant prefix, so the {tenant_id} token is
		// mandatory. An empty or token-less prefix_pattern resolves to
		// KeyPrefix="" (a no-op in scopedKey/scopedPrefix), collapsing every
		// tenant into one flat keyspace — a cross-tenant read+write leak.
		// This is the RUNTIME BACKSTOP: the Def validator and the static-
		// config check reject it earlier, but resolveTenancy refuses
		// unconditionally so NO configuration path (substrate fork OR
		// hand-written yaml, which skips Def validation) can ever reach the
		// leaky no-prefix state. (Contrast key_per_tenant, which legitimately
		// carries no prefix because isolation rests on a distinct per-tenant
		// API key.)
		prefix := ts.PrefixPattern
		if !strings.Contains(prefix, "{tenant_id}") {
			return mem9.Tenancy{}, "", fmt.Errorf("shared_key_with_prefix requires prefix_pattern to contain {tenant_id} (got %q); an empty or token-less prefix would collapse all tenants into one keyspace", prefix)
		}
		if tenantID == "" {
			return mem9.Tenancy{}, "", fmt.Errorf("shared_key_with_prefix needs {tenant_id} but the run carries no tenant (user_id)")
		}
		prefix = strings.ReplaceAll(prefix, "{tenant_id}", tenantID)
		return mem9.Tenancy{KeyPrefix: prefix}, tenantID, nil
	default:
		return mem9.Tenancy{}, "", fmt.Errorf("unknown tenancy_strategy.kind %q", ts.Kind)
	}
}

// mem9CredentialResolver builds the per-op X-API-Key resolver for a mem9
// Def (RFC I MR-4 / Decision 10). Resolution order, evaluated per op:
//
//  1. RFC-F per-run credential: tools.RunIdentity(ctx).UserCredentials[<key>]
//     where <key> is the Def's config.api_key_env name (the documented
//     convention — the operator's env-var name doubles as the credential
//     key, so a caller passing {"<API_KEY_ENV>": "..."} on the run
//     overrides the env value without a second naming scheme).
//  2. Env fallback: os.Getenv(envName), where envName is the api_key_env
//     (key_per_tenant) or the tenancy env_pattern with {tenant_id}
//     substituted. The env var MUST be on the EnvAllowlist; a
//     non-allowlisted or unset key returns an error so the op fails loud
//     (or the fallback wrapper engages). NEVER a silent unauthenticated
//     call.
//
// The resolver closes over the tool layer's tools.RunIdentity so the
// mem9 package needs no dependency on internal/tools. The returned error
// NEVER contains the key value.
func (m *Memory) mem9CredentialResolver(def config.MemoryBackend, ts config.MemoryBackendTenancy, _ string) mem9.CredentialResolver {
	return func(ctx context.Context) (string, error) {
		// Determine the env-var name to read. key_per_tenant may use the
		// tenancy env_pattern (per-tenant key); otherwise the static
		// api_key_env. Re-resolve the tenant per call so a long-lived
		// resolver always reflects the current run's identity.
		tenantID := tools.RunIdentity(ctx).UserID
		envName := def.Config.APIKeyEnv
		if ts.Kind == "key_per_tenant" && ts.EnvPattern != "" {
			envName = strings.ReplaceAll(ts.EnvPattern, "{tenant_id}", tenantID)
		}

		// 1. RFC-F per-run credential keyed by the api_key_env name.
		if cred, ok := tools.RunIdentity(ctx).UserCredentials[def.Config.APIKeyEnv]; ok && cred != "" {
			return cred, nil
		}

		// 2. Env fallback, allowlist-gated.
		if envName == "" {
			return "", fmt.Errorf("mem9: no api_key_env configured and no per-run credential supplied")
		}
		if !m.EnvAllowlist[envName] {
			// Reference the env-var NAME only — never the value. The name
			// is not a secret; the value would be.
			return "", fmt.Errorf("mem9: env var %q not in allowlist (add it to LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST)", envName)
		}
		v := os.Getenv(envName)
		if v == "" {
			return "", fmt.Errorf("mem9: env var %q is unset or empty", envName)
		}
		return v, nil
	}
}

// defaultBackend returns the operator-default backend: the explicitly-set
// m.Backend if present, else a lazily-constructed in-process backend
// wrapping Store + Embedder. Constructed per-call so a late-bound Embedder
// is always reflected; the in-process backend is a thin stateless wrapper
// so this is cheap. This is the pre-MR-3b behavior, preserved verbatim.
func (m *Memory) defaultBackend() memrank.Backend {
	if m.Backend != nil {
		return m.Backend
	}
	return inprocess.New(m.Store, m.Embedder)
}

const memoryDescription = `Persistent key/value storage scoped to this agent or end-user. ` +
	`Survives across runs and sessions. Use for: counters, summaries, voice/preferences, ` +
	`learned facts, notes for your future self. ` +
	`Operations: get, set, delete, list, incr, search, merge, append_dedupe, bounded_list. ` +
	`Scope is "agent" (this agent's keyspace, shared across users) or "user" (this end-user's keyspace, shared across agents). ` +
	`Values are JSON. Optional TTL is in seconds. ` +
	`v0.9.0: pass embed=true with embed_text on set to enable semantic search; use op=search with query to find rows by similarity. ` +
	`v0.12.x: merge / append_dedupe / bounded_list are atomic reducers — use them instead of get-modify-set when concurrent updates are possible. ` +
	`add / recall (memory-layer backends only): add ingests conversation messages (the backend may LLM-extract durable facts); recall is a natural-language semantic search over those facts. These require a memory-layer backend (memory_backend kind=mem9); against the default key/value store they return capability_unsupported. Unlike set/get, add does not store value-at-key and is often async — do not assume read-after-write.`

const memoryInputSchema = `{
  "type": "object",
  "properties": {
    "op":         {"type": "string", "enum": ["get","set","delete","list","incr","search","merge","append_dedupe","bounded_list","add","recall"], "description": "Which operation to perform."},
    "scope":      {"type": "string", "enum": ["agent","user"], "description": "Which keyspace: this agent's (cross-run, cross-user) or this user's (cross-agent)."},
    "key":        {"type": "string", "description": "The entry's key. Required for get / set / delete / incr / merge / append_dedupe / bounded_list."},
    "value":      {"description": "The JSON value. Required for set / merge / append_dedupe / bounded_list. For merge: a JSON object whose fields overlay the existing object. For append_dedupe / bounded_list: the item to append."},
    "delta":      {"type": "integer", "description": "Increment delta for incr (default 1, may be negative)."},
    "ttl":        {"type": "integer", "description": "Optional time-to-live in seconds. Applies to write ops; 0 means no expiry (or keep existing on update)."},
    "prefix":     {"type": "string", "description": "Optional key prefix filter for list / search."},
    "limit":      {"type": "integer", "description": "list: max entries returned (default 100). bounded_list: keep the N most recent items (required, >= 1)."},
    "embed":      {"type": "boolean", "description": "v0.9.0 set-only: when true, also generates and stores an embedding so this row is reachable via op=search."},
    "embed_text": {"type": "string", "description": "v0.9.0 set-only: the text to embed when embed=true. Defaults to the JSON-stringified value when omitted."},
    "query":      {"type": "string", "description": "v0.9.0 search-only: the text to embed and use as the similarity query."},
    "top_k":      {"type": "integer", "description": "v0.9.0 search-only: max results (default 10, max 50)."},
    "rank":       {"type": "object", "description": "search-only hybrid ranking weights. Omit for pure semantic (default). Properties: semantic_weight, recency_weight, recency_half_life_hours, source_weight, frequency_weight (source/frequency reserved — contribute 0 today).", "properties": {"semantic_weight": {"type": "number"}, "recency_weight": {"type": "number"}, "recency_half_life_hours": {"type": "number"}, "source_weight": {"type": "number"}, "frequency_weight": {"type": "number"}}},
    "dedup":      {"type": "object", "description": "search-only near-duplicate collapse. Omit (or enabled=false) for no dedup (default). Drops a result whose embedding cosine similarity to a higher-ranked kept result is >= threshold. Properties: enabled (bool), threshold (number, cosine-similarity floor, default 0.92), mode (\"drop\" default | \"merge\" | \"keep\").", "properties": {"enabled": {"type": "boolean"}, "threshold": {"type": "number"}, "mode": {"type": "string", "enum": ["drop","merge","keep"]}}},
    "messages":   {"type": "array", "description": "add-only (memory-layer backends): conversation turns to ingest. Each item is {role, content}.", "items": {"type": "object", "properties": {"role": {"type": "string", "enum": ["user","assistant","system"]}, "content": {"type": "string"}}, "required": ["role","content"]}},
    "infer":      {"type": "boolean", "description": "add-only: when true (default) the memory-layer backend LLM-extracts durable facts from the messages; false stores them verbatim."},
    "metadata":   {"type": "object", "description": "add-only: opaque key/value context attached to the ingestion.", "additionalProperties": {"type": "string"}},
    "threshold":  {"type": "number", "description": "recall-only: 0..1 relevance floor for returned facts (0 = backend default)."}
  },
  "required": ["op","scope"],
  "additionalProperties": false
}`

type memoryInput struct {
	Op        string          `json:"op"`
	Scope     string          `json:"scope"`
	Key       string          `json:"key,omitempty"`
	Value     json.RawMessage `json:"value,omitempty"`
	Delta     *int64          `json:"delta,omitempty"`
	TTL       int64           `json:"ttl,omitempty"`
	Prefix    string          `json:"prefix,omitempty"`
	Limit     int             `json:"limit,omitempty"`
	Embed     bool            `json:"embed,omitempty"`      // v0.9.0
	EmbedText string          `json:"embed_text,omitempty"` // v0.9.0
	Query     string          `json:"query,omitempty"`      // v0.9.0
	TopK      int             `json:"top_k,omitempty"`      // v0.9.0
	// Rank is the RFC I hybrid-ranking weight block for `search`. Nil =
	// pure semantic (today's behavior). See memrank.RankConfig.
	Rank *memrank.RankConfig `json:"rank,omitempty"`
	// Dedup is the RFC I (MR-5) search-time dedup block for `search`. Nil =
	// dedup disabled (today's behavior, zero regression). See
	// memrank.DedupConfig.
	Dedup *memrank.DedupConfig `json:"dedup,omitempty"`

	// Messages is the RFC K `add` payload — conversation turns the
	// memory-layer backend ingests (and optionally LLM-extracts into facts).
	Messages []memrank.LayerMessage `json:"messages,omitempty"`
	// Infer controls server-side fact extraction on `add` (RFC K). Pointer
	// so an omitted value defaults to true (the memory-layer paradigm) while
	// `false` opts into verbatim storage. Nil = default-true.
	Infer *bool `json:"infer,omitempty"`
	// Metadata is opaque context attached to an `add` ingestion (RFC K).
	Metadata map[string]string `json:"metadata,omitempty"`
	// Threshold is the 0..1 relevance floor for `recall` (RFC K). 0 = the
	// backend's default.
	Threshold float64 `json:"threshold,omitempty"`
}

// Name implements tools.Tool.
func (m *Memory) Name() string { return "Memory" }

// Description implements tools.Tool.
func (m *Memory) Description() string { return memoryDescription }

// InputSchema implements tools.Tool.
func (m *Memory) InputSchema() json.RawMessage { return json.RawMessage(memoryInputSchema) }

// Execute implements tools.Tool. The full request → result mapping
// lives here; helpers below normalise scope/scope_id and surface
// errors as model-readable tool_results.
func (m *Memory) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if m.Store == nil {
		return errResult("Memory tool: not configured (no Store backend — set LOOMCYCLE_STORAGE_BACKEND or remove Memory from the agent's allowed_tools)"), nil
	}
	var in memoryInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult(fmt.Sprintf("invalid input JSON: %s", err)), nil
	}
	scope, scopeID, err := m.resolveScope(ctx, in.Scope)
	if err != nil {
		return errResult(err.Error()), nil
	}

	switch in.Op {
	case "get":
		return m.execGet(ctx, scope, scopeID, in)
	case "set":
		return m.execSet(ctx, scope, scopeID, in)
	case "delete":
		return m.execDelete(ctx, scope, scopeID, in)
	case "list":
		return m.execList(ctx, scope, scopeID, in)
	case "incr":
		return m.execIncr(ctx, scope, scopeID, in)
	case "search":
		return m.execSearch(ctx, scope, scopeID, in)
	case "merge":
		return m.execMerge(ctx, scope, scopeID, in)
	case "append_dedupe":
		return m.execAppendDedupe(ctx, scope, scopeID, in)
	case "bounded_list":
		return m.execBoundedList(ctx, scope, scopeID, in)
	case "add":
		return m.execAdd(ctx, scope, scopeID, in)
	case "recall":
		return m.execRecall(ctx, scope, scopeID, in)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: get, set, delete, list, incr, search, merge, append_dedupe, bounded_list, add, recall)", in.Op)), nil
	}
}

// resolveScope validates the requested scope against the agent's
// MemoryPolicy and resolves scope_id from ctx. Returns a typed scope
// + the runtime-supplied scope_id, or a refusal error suitable for
// surfacing as a tool_result.
func (m *Memory) resolveScope(ctx context.Context, requested string) (store.MemoryScope, string, error) {
	policy := tools.MemoryPolicy(ctx)
	if requested == "" {
		return "", "", fmt.Errorf("missing required field: scope")
	}
	if !contains(policy.AllowedScopes, requested) {
		if len(policy.AllowedScopes) == 0 {
			return "", "", fmt.Errorf("Memory tool: this agent has no memory_scopes configured — add `memory_scopes: [agent]` (or [user], or both) to the agent yaml")
		}
		return "", "", fmt.Errorf("Memory tool: scope %q not in this agent's memory_scopes %v", requested, policy.AllowedScopes)
	}

	switch store.MemoryScope(requested) {
	case store.MemoryScopeAgent:
		name := tools.AgentName(ctx)
		if name == "" {
			return "", "", fmt.Errorf("Memory tool: scope=agent requires a yaml-declared agent (no agent name on the run context)")
		}
		return store.MemoryScopeAgent, name, nil
	case store.MemoryScopeUser:
		ident := tools.RunIdentity(ctx)
		if ident.UserID == "" {
			return "", "", fmt.Errorf("Memory tool: scope=user requires a user_id on the run (caller must supply user_id when starting the run)")
		}
		return store.MemoryScopeUser, ident.UserID, nil
	default:
		return "", "", fmt.Errorf("Memory tool: unknown scope %q (only agent / user are supported in v0.8.0)", requested)
	}
}

func (m *Memory) execGet(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) (tools.Result, error) {
	if in.Key == "" {
		return errResult("get: missing required field: key"), nil
	}
	entry, err := m.backend(ctx).Get(ctx, scope, scopeID, in.Key)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return okJSON(map[string]any{"value": nil, "expires_at": nil})
		}
		return errResult(fmt.Sprintf("get: %s", err)), nil
	}
	return okJSON(map[string]any{
		"value":      entry.Value,
		"expires_at": expiresAtRFC3339(entry.ExpiresAt),
	})
}

func (m *Memory) execSet(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) (tools.Result, error) {
	if in.Key == "" {
		return errResult("set: missing required field: key"), nil
	}
	if len(in.Value) == 0 {
		return errResult("set: missing required field: value"), nil
	}
	if !json.Valid(in.Value) {
		return errResult("set: value is not valid JSON"), nil
	}
	if m.MaxValueBytes > 0 && len(in.Value) > m.MaxValueBytes {
		return errResult(fmt.Sprintf("set: value (%d bytes) exceeds max %d bytes", len(in.Value), m.MaxValueBytes)), nil
	}
	// Quota math charges only the k/v row's key + value bytes;
	// embeddings are excluded per RFC §8. Operators don't pay for
	// the vector's storage in their per-scope cap.
	if err := m.checkQuota(ctx, scope, scopeID, in.Key, len(in.Value)); err != nil {
		return errResult(err.Error()), nil
	}

	// The embed orchestration (pre-flight config refusal, k/v write,
	// best-effort embedding with a non-fatal warning) lives in the
	// Backend now (RFC I MR-2). A permanent misconfiguration is returned
	// as a typed *store.MemoryError BEFORE any k/v write — render it bare,
	// matching the pre-MR-2 upfront-refusal message. Any other error is a
	// genuine k/v write failure — render it with the "set:" prefix.
	ttl := time.Duration(in.TTL) * time.Second
	res, err := m.backend(ctx).Set(ctx, scope, scopeID, in.Key, in.Value, memrank.SetOptions{
		TTL:       ttl,
		Embed:     in.Embed,
		EmbedText: in.EmbedText,
	})
	if err != nil {
		if errors.Is(err, store.ErrEmbedderNotConfigured) || errors.Is(err, store.ErrVectorUnsupported) {
			return errResult(err.Error()), nil
		}
		return errResult(fmt.Sprintf("set: %s", err)), nil
	}

	resp := map[string]any{"ok": true}
	if in.Embed {
		if res.Embedded {
			resp["embedded"] = true
		} else {
			// Transient embedder failure after the k/v row landed; the
			// row stands, the agent sees the partial-write outcome and can
			// re-embed via the admin endpoint.
			log.Printf("memory.set: embed failed for (scope=%s, key=%s): %s", scope, in.Key, res.EmbedWarning)
			resp["embedded"] = false
			resp["embed_warning"] = res.EmbedWarning
		}
	}
	return okJSON(resp)
}

// execSearch implements the v0.9.0 Memory.search op. Refuses with
// a typed error when the backend has no vector support OR when no
// embedder is configured. Embeds the query, runs the cosine
// similarity search via the Store, and returns the ranked rows.
//
// JSON response shape (matches the RFC's verification example):
//
//	{ "entries": [
//	    { "key": "...", "value": <json>, "score": 0.91,
//	      "embedded_with": {"provider": "openai", "model": "..."} },
//	    ...
//	  ],
//	  "query_embedding_dim": 1536,
//	  "truncated": false }
func (m *Memory) execSearch(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) (tools.Result, error) {
	if in.Query == "" {
		return errResult("search: missing required field: query"), nil
	}
	// NOTE: the vector-support / embedder pre-flight is NOT done here on the
	// tool's in-process Store/Embedder — a named memory_backend (e.g. mem9)
	// can serve search remotely with no local vector support or embedder.
	// The resolved backend returns the typed refusal (ErrVectorUnsupported /
	// ErrEmbedderNotConfigured), which the error handler below renders with
	// the same message. (execSet delegates its pre-flight the same way.)
	topK := in.TopK
	if topK <= 0 {
		topK = 10
	}
	if topK > 50 {
		topK = 50
	}

	// RFC I hybrid ranking. Nil rank block = pure semantic = today's
	// behavior (zero regression).
	rankCfg := memrank.DefaultRankConfig()
	if in.Rank != nil {
		rankCfg = *in.Rank
	}

	// RFC I (MR-5) search-time dedup. Nil dedup block = disabled = today's
	// behavior (zero regression). A model can opt in per search.
	var dedupCfg memrank.DedupConfig
	if in.Dedup != nil {
		dedupCfg = *in.Dedup
	}

	// The data path (embed query → over-fetch cosine pool → re-rank →
	// dedup → trim → score) lives in the Backend now (RFC I MR-2/MR-5). The
	// upfront validation above stays on the tool so the refusal ordering /
	// messages are byte-identical to pre-MR-2.
	res, err := m.backend(ctx).Search(ctx, scope, scopeID, memrank.SearchQuery{
		QueryText: in.Query,
		Prefix:    in.Prefix,
		TopK:      topK,
	}, rankCfg, dedupCfg)
	if err != nil {
		// ErrDimensionMismatch is the user-actionable one — operators
		// swap embedder models and forget to re-embed. Surface it as a
		// clear refusal with the admin-endpoint migration hint.
		// errors.Is works on backend-constructed *MemoryError values
		// thanks to MemoryError.Is(target) comparing by Code.
		if errors.Is(err, store.ErrDimensionMismatch) ||
			errors.Is(err, store.ErrVectorUnsupported) ||
			errors.Is(err, store.ErrEmbedderNotConfigured) {
			return errResult(err.Error()), nil
		}
		return errResult(fmt.Sprintf("search: %s", err)), nil
	}

	entries := make([]map[string]any, 0, len(res.Entries))
	for i, r := range res.Entries {
		entries = append(entries, map[string]any{
			"key":        r.Key,
			"value":      r.Value,
			"score":      r.Score,           // cosine similarity (unchanged field)
			"rank_score": res.RankScores[i], // hybrid score this result was ordered by
			"embedded_with": map[string]any{
				"provider": r.EmbeddedWith.Provider,
				"model":    r.EmbeddedWith.Model,
			},
			"expires_at": expiresAtRFC3339(r.ExpiresAt),
		})
	}
	out := map[string]any{
		"entries":             entries,
		"query_embedding_dim": res.QueryEmbeddingDim,
		"truncated":           res.Truncated,
	}
	// Surface dedup_dropped ONLY when the caller opted into dedup. A search
	// with no `dedup` block keeps the pre-MR-5 response shape byte-for-byte
	// (zero regression): the key is absent rather than always-zero. When
	// dedup is on, the count is always present (including 0) so the agent
	// can tell "dedup ran, found nothing" from "dedup wasn't requested."
	if in.Dedup != nil && in.Dedup.Enabled {
		out["dedup_dropped"] = res.DedupDropped
		// Observability: the RFC's memory.dedup.dropped_count (Decision 12)
		// is an OTEL span attribute. loomcycle's only OTEL substrate today
		// lives in the mem9 backend (which sets that attribute on its span);
		// the in-process path has no span here yet (broader OTEL is planned
		// for v0.9.x — see CLAUDE.md). Until that lands, mirror the repo's
		// current observability idiom (log.Printf) so operators can still
		// see dedup activity on the in-process path.
		if res.DedupDropped > 0 {
			log.Printf("memory.search: dedup dropped %d near-duplicate entries (scope=%s, mode=%s)",
				res.DedupDropped, scope, dedupModeOrDefault(in.Dedup.Mode))
		}
	}
	// Don't silently ignore a non-zero source/frequency weight — those
	// terms are reserved (contribute 0 today). Surface a note instead.
	if res.RankNote != "" {
		out["rank_note"] = res.RankNote
	}
	return okJSON(out)
}

// execAdd implements the RFC K `add` op: ingest conversation messages into a
// memory-layer backend (which may LLM-extract durable facts). Refuses with
// capability_unsupported when the resolved backend is not a memory layer
// (e.g. the default in-process KV+vector backend) — the same fail-closed
// posture as the search op's vector_unsupported refusal.
func (m *Memory) execAdd(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) (tools.Result, error) {
	layer, ok := m.memoryLayer(ctx)
	if !ok {
		return errResult(store.ErrCapabilityUnsupported.Msg), nil
	}
	if len(in.Messages) == 0 {
		return errResult("add: missing required field: messages (a non-empty array of {role, content})"), nil
	}
	totalContentBytes := 0
	for i, msg := range in.Messages {
		if msg.Content == "" {
			return errResult(fmt.Sprintf("add: messages[%d] has empty content", i)), nil
		}
		totalContentBytes += len(msg.Content)
	}
	// Bound the ingest the same way execSet bounds a value. The layer.Add path
	// POSTs the full messages array to the (possibly remote) memory backend
	// with no request-body cap, and the async-extracted facts are never charged
	// to the per-scope quota (RFC §8 excludes embedding bytes) — so without
	// this an agent could push an unbounded conversation payload through `add`
	// as a free, unaccounted egress / amplification channel. We cap the raw
	// ingest bytes; the byte cap on the input is the proportionate guard since
	// the server assigns extracted-fact storage asynchronously and out of band.
	if m.MaxValueBytes > 0 && totalContentBytes > m.MaxValueBytes {
		return errResult(fmt.Sprintf("add: messages content (%d bytes) exceeds max %d bytes", totalContentBytes, m.MaxValueBytes)), nil
	}
	// infer defaults to true — the memory-layer paradigm is LLM fact
	// extraction; an operator opts into verbatim storage with infer:false.
	infer := true
	if in.Infer != nil {
		infer = *in.Infer
	}
	res, err := layer.Add(ctx, scope, scopeID, in.Messages, memrank.AddOptions{
		Infer:    infer,
		Metadata: in.Metadata,
	})
	if err != nil {
		return errResult(fmt.Sprintf("add: %s", err)), nil
	}
	out := map[string]any{
		// status is "pending" (async ingest still extracting) or "done".
		// A memory-layer add is frequently async, so the agent should NOT
		// assume read-after-write — recall may not see the facts yet.
		"status": res.Status.String(),
	}
	if res.EventID != "" {
		out["event_id"] = res.EventID
	}
	return okJSON(out)
}

// execRecall implements the RFC K `recall` op: natural-language semantic
// search over a memory-layer backend's extracted facts. Refuses with
// capability_unsupported when the resolved backend is not a memory layer.
func (m *Memory) execRecall(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) (tools.Result, error) {
	layer, ok := m.memoryLayer(ctx)
	if !ok {
		return errResult(store.ErrCapabilityUnsupported.Msg), nil
	}
	if in.Query == "" {
		return errResult("recall: missing required field: query"), nil
	}
	topK := in.TopK
	if topK <= 0 {
		topK = 10
	}
	if topK > 50 {
		topK = 50
	}
	res, err := layer.Recall(ctx, scope, scopeID, memrank.RecallQuery{
		Query:     in.Query,
		TopK:      topK,
		Threshold: in.Threshold,
	})
	if err != nil {
		return errResult(fmt.Sprintf("recall: %s", err)), nil
	}
	facts := make([]map[string]any, 0, len(res.Facts))
	for _, f := range res.Facts {
		fact := map[string]any{
			"id":     f.ID, // server-assigned; opaque to loomcycle, NOT a caller key
			"memory": f.Memory,
			"score":  f.Score,
		}
		if len(f.Metadata) > 0 {
			fact["metadata"] = f.Metadata
		}
		facts = append(facts, fact)
	}
	return okJSON(map[string]any{"facts": facts})
}

func (m *Memory) execDelete(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) (tools.Result, error) {
	if in.Key == "" {
		return errResult("delete: missing required field: key"), nil
	}
	deleted, err := m.backend(ctx).Delete(ctx, scope, scopeID, in.Key)
	if err != nil {
		return errResult(fmt.Sprintf("delete: %s", err)), nil
	}
	return okJSON(map[string]any{"deleted": deleted})
}

func (m *Memory) execList(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) (tools.Result, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		// Hard cap to keep one tool_result from blowing past a
		// model's context window. Operators who really want more
		// should paginate via the prefix.
		limit = 1000
	}
	entries, truncated, err := m.backend(ctx).List(ctx, scope, scopeID, in.Prefix, limit)
	if err != nil {
		return errResult(fmt.Sprintf("list: %s", err)), nil
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"key":        e.Key,
			"value":      e.Value,
			"expires_at": expiresAtRFC3339(e.ExpiresAt),
		})
	}
	return okJSON(map[string]any{
		"entries":   out,
		"truncated": truncated,
	})
}

func (m *Memory) execIncr(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) (tools.Result, error) {
	if in.Key == "" {
		return errResult("incr: missing required field: key"), nil
	}
	delta := int64(1)
	if in.Delta != nil {
		delta = *in.Delta
	}
	// Quota check is approximate for incr — the on-disk value's text
	// representation is bounded by 20 bytes (max int64 width). Charge
	// 32 bytes for safety; a counter row is negligible relative to
	// any sane scope cap.
	if err := m.checkQuota(ctx, scope, scopeID, in.Key, 32); err != nil {
		return errResult(err.Error()), nil
	}
	ttl := time.Duration(in.TTL) * time.Second
	next, err := m.Store.MemoryIncrement(ctx, scope, scopeID, in.Key, delta, ttl)
	if err != nil {
		if errors.Is(err, store.ErrMemoryWrongType) {
			return errResult("incr: existing value is not a JSON number — use set with a number, or delete first"), nil
		}
		return errResult(fmt.Sprintf("incr: %s", err)), nil
	}
	return okJSON(map[string]any{"value": next})
}

// execMerge deep-merges a JSON OBJECT into the existing value. The
// existing value must be a JSON object (or absent — treated as
// empty object). Incoming value must also be a JSON object. Fields
// in the incoming object overlay the existing fields; nested objects
// recurse; non-object values (arrays, scalars, null) at any level
// replace the existing value at that path.
//
// Atomic via MemoryAtomicUpdate — concurrent merges on the same key
// serialise cleanly. The pattern that justifies this op vs a
// get/modify/set sequence at the tool layer: two agents merging
// different fields into the same profile object would otherwise lose
// one update.
func (m *Memory) execMerge(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) (tools.Result, error) {
	if in.Key == "" {
		return errResult("merge: missing required field: key"), nil
	}
	if len(in.Value) == 0 {
		return errResult("merge: missing required field: value"), nil
	}
	if !json.Valid(in.Value) {
		return errResult("merge: value is not valid JSON"), nil
	}
	// Validate the incoming value is an object up-front so we refuse
	// before taking the row lock.
	var incoming map[string]any
	if err := json.Unmarshal(in.Value, &incoming); err != nil {
		return errResult("merge: value must be a JSON object"), nil
	}
	if m.MaxValueBytes > 0 && len(in.Value) > m.MaxValueBytes {
		return errResult(fmt.Sprintf("merge: value (%d bytes) exceeds max %d bytes", len(in.Value), m.MaxValueBytes)), nil
	}

	ttl := time.Duration(in.TTL) * time.Second
	final, err := m.Store.MemoryAtomicUpdate(ctx, scope, scopeID, in.Key, ttl,
		func(existing json.RawMessage) (json.RawMessage, error) {
			base := map[string]any{}
			if len(existing) > 0 {
				if err := json.Unmarshal(existing, &base); err != nil {
					// Existing row is not a JSON object — refuse;
					// merge into a non-object would silently replace.
					return nil, fmt.Errorf("existing value is not a JSON object (use set to overwrite)")
				}
			}
			out := deepMerge(base, incoming)
			b, err := json.Marshal(out)
			if err != nil {
				return nil, fmt.Errorf("encode merged value: %w", err)
			}
			if m.MaxValueBytes > 0 && len(b) > m.MaxValueBytes {
				return nil, fmt.Errorf("merged value (%d bytes) exceeds max %d bytes", len(b), m.MaxValueBytes)
			}
			return b, nil
		})
	if err != nil {
		return errResult(fmt.Sprintf("merge: %s", err)), nil
	}
	// Quota check AFTER the merge — the post-merge size is what we
	// charge against. checkQuota's existing-row subtraction means a
	// merge that grows the row by N bytes costs N additional bytes.
	if err := m.checkQuota(ctx, scope, scopeID, in.Key, len(final)); err != nil {
		// Roll back by rewriting the old value? We can't from here
		// — the atomic update already committed. Documentation says
		// quota is approximate; surface the error so the agent sees
		// the over-cap state and can delete.
		return errResult(err.Error()), nil
	}
	return okJSON(map[string]any{"value": final})
}

// execAppendDedupe appends an item to a JSON ARRAY at the key. If the
// item is already in the array (by JSON-equality), the call is a
// no-op and `appended: false` is returned. Atomic so two agents
// appending the same value concurrently produce exactly one entry.
//
// The existing value must be a JSON array (or absent — treated as
// empty array). Items can be any JSON value.
func (m *Memory) execAppendDedupe(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) (tools.Result, error) {
	if in.Key == "" {
		return errResult("append_dedupe: missing required field: key"), nil
	}
	if len(in.Value) == 0 {
		return errResult("append_dedupe: missing required field: value"), nil
	}
	if !json.Valid(in.Value) {
		return errResult("append_dedupe: value is not valid JSON"), nil
	}

	ttl := time.Duration(in.TTL) * time.Second
	appended := false
	final, err := m.Store.MemoryAtomicUpdate(ctx, scope, scopeID, in.Key, ttl,
		func(existing json.RawMessage) (json.RawMessage, error) {
			var arr []json.RawMessage
			if len(existing) > 0 {
				if err := json.Unmarshal(existing, &arr); err != nil {
					return nil, fmt.Errorf("existing value is not a JSON array (use set to overwrite)")
				}
			}
			// JSON-equality dedupe — compare canonicalised forms so
			// {a:1,b:2} equals {b:2,a:1}.
			incomingCanon, err := canonicaliseJSON(in.Value)
			if err != nil {
				return nil, fmt.Errorf("canonicalise incoming: %w", err)
			}
			for _, existingItem := range arr {
				cc, err := canonicaliseJSON(existingItem)
				if err == nil && bytesEqual(cc, incomingCanon) {
					// Already present — return existing unchanged.
					return existing, nil
				}
			}
			appended = true
			arr = append(arr, json.RawMessage(in.Value))
			b, err := json.Marshal(arr)
			if err != nil {
				return nil, fmt.Errorf("encode appended array: %w", err)
			}
			if m.MaxValueBytes > 0 && len(b) > m.MaxValueBytes {
				return nil, fmt.Errorf("array (%d bytes) exceeds max %d bytes", len(b), m.MaxValueBytes)
			}
			return b, nil
		})
	if err != nil {
		return errResult(fmt.Sprintf("append_dedupe: %s", err)), nil
	}
	// Quota check on the final size; same caveat as execMerge — the
	// store has already committed by here.
	if err := m.checkQuota(ctx, scope, scopeID, in.Key, len(final)); err != nil {
		return errResult(err.Error()), nil
	}
	return okJSON(map[string]any{"appended": appended, "value": final})
}

// execBoundedList appends an item to a JSON ARRAY at the key and
// trims to the most recent `limit` entries. Older entries (front of
// the array) are dropped. Useful for event logs / recent-activity
// buffers / sliding-window features.
//
// Unlike append_dedupe, this op does NOT dedupe — every call appends.
// The order is insertion order; the trim drops from the head.
func (m *Memory) execBoundedList(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) (tools.Result, error) {
	if in.Key == "" {
		return errResult("bounded_list: missing required field: key"), nil
	}
	if len(in.Value) == 0 {
		return errResult("bounded_list: missing required field: value"), nil
	}
	if !json.Valid(in.Value) {
		return errResult("bounded_list: value is not valid JSON"), nil
	}
	if in.Limit < 1 {
		return errResult("bounded_list: limit must be >= 1"), nil
	}
	// Hard cap to keep one row from blowing past the model context.
	if in.Limit > 10000 {
		return errResult("bounded_list: limit must be <= 10000"), nil
	}

	ttl := time.Duration(in.TTL) * time.Second
	var droppedCount int
	final, err := m.Store.MemoryAtomicUpdate(ctx, scope, scopeID, in.Key, ttl,
		func(existing json.RawMessage) (json.RawMessage, error) {
			var arr []json.RawMessage
			if len(existing) > 0 {
				if err := json.Unmarshal(existing, &arr); err != nil {
					return nil, fmt.Errorf("existing value is not a JSON array (use set to overwrite)")
				}
			}
			arr = append(arr, json.RawMessage(in.Value))
			if len(arr) > in.Limit {
				droppedCount = len(arr) - in.Limit
				arr = arr[droppedCount:]
			}
			b, err := json.Marshal(arr)
			if err != nil {
				return nil, fmt.Errorf("encode bounded list: %w", err)
			}
			if m.MaxValueBytes > 0 && len(b) > m.MaxValueBytes {
				return nil, fmt.Errorf("array (%d bytes) exceeds max %d bytes", len(b), m.MaxValueBytes)
			}
			return b, nil
		})
	if err != nil {
		return errResult(fmt.Sprintf("bounded_list: %s", err)), nil
	}
	if err := m.checkQuota(ctx, scope, scopeID, in.Key, len(final)); err != nil {
		return errResult(err.Error()), nil
	}
	return okJSON(map[string]any{"dropped": droppedCount, "value": final})
}

// deepMerge overlays `overlay` onto `base`, recursing into nested
// maps. Non-map values at any level replace the base value (no
// concat for arrays — replace; no add for numbers — replace). Used
// by execMerge.
func deepMerge(base, overlay map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(overlay))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		// If both sides are objects, recurse. Otherwise overlay wins.
		if baseSub, baseOk := out[k].(map[string]any); baseOk {
			if overlaySub, overlayOk := v.(map[string]any); overlayOk {
				out[k] = deepMerge(baseSub, overlaySub)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// canonicaliseJSON produces a deterministic encoding of `raw` so that
// semantically-equal JSON values compare byte-equal. Object keys are
// sorted; whitespace is removed. Used by execAppendDedupe for the
// "already in the array" check.
func canonicaliseJSON(raw json.RawMessage) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v) // Go's encoder sorts map keys by default.
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// checkQuota verifies that adding `addBytes` to the (scope, scopeID)
// keyspace would not exceed the per-agent quota (or global default).
// Existing-key writes are charged the *additional* bytes only — we
// look up the current row, subtract its size, and compare.
//
// Returns nil when the write is permitted. Returns
// store.ErrMemoryQuotaExceeded (wrapped) when not.
//
// 0 quota = "no cap" (matches the env var convention for "feature
// disabled"). The check short-circuits then.
func (m *Memory) checkQuota(ctx context.Context, scope store.MemoryScope, scopeID, key string, addBytes int) error {
	policy := tools.MemoryPolicy(ctx)
	quota := policy.QuotaBytes
	if quota <= 0 {
		quota = m.DefaultQuotaBytes
	}
	if quota <= 0 {
		return nil
	}

	// Sum existing bytes (key + value) across the whole scope. The
	// list call is expected to be small for a well-behaved agent — a
	// noisy agent that writes thousands of keys hits the quota cap
	// long before this loop becomes an issue.
	//
	// For a scope of 1 MB at 64 KB/value that's at most ~16 rows in
	// the worst case; in practice scopes hold a handful of summary
	// keys. If we ever need to scale this, we'll add a cached
	// per-(scope, scope_id) byte counter via a SQL trigger.
	//
	// We treat truncation (>1000 keys in scope) as a quota refusal:
	// undercounting would let a thousand-tiny-key agent slip past the
	// cap silently. An agent that hits this limit should `delete`
	// rows before writing more, or operators should bump the quota.
	const listCap = 1000
	// List through the RESOLVED backend, not the in-process store: an agent
	// routed to a remote backend (mem9) stores its rows there, so summing the
	// local store would measure ~0 used bytes and let the per-scope
	// memory_quota_bytes cap silently never apply. backend(ctx) is the
	// in-process default (which wraps m.Store) when no remote backend is
	// configured, so this is equivalent for the common case.
	entries, truncated, err := m.backend(ctx).List(ctx, scope, scopeID, "", listCap)
	if err != nil {
		return fmt.Errorf("quota check: %w", err)
	}
	if truncated {
		return fmt.Errorf("Memory.set: scope %q has more than %d keys; quota check cannot run accurately — delete unused keys first",
			scope, listCap)
	}
	used := 0
	for _, e := range entries {
		used += len(e.Key) + len(e.Value)
		if e.Key == key {
			// Subtract the existing row's bytes — we'll re-add
			// the new size below to compute the post-write total.
			used -= len(e.Key) + len(e.Value)
		}
	}
	projected := used + len(key) + addBytes
	if projected > quota {
		return fmt.Errorf("Memory.set: scope %q quota %d bytes would be exceeded by this write (current=%d, after=%d)",
			scope, quota, used, projected)
	}
	return nil
}

// okJSON marshals v as the tool_result text. JSON marshalling is
// expected to succeed (every code path constructs a map with safe
// values); on failure we surface the error as a tool_result so the
// model sees a coherent message rather than a panic.
func okJSON(v any) (tools.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return errResult(fmt.Sprintf("encode result: %s", err)), nil
	}
	return tools.Result{Text: string(b)}, nil
}

func errResult(msg string) tools.Result {
	return tools.Result{IsError: true, Text: msg}
}

// expiresAtRFC3339 returns nil for the zero time and an RFC3339 string
// otherwise. The wire shape is stable across set/get/list — operators
// debugging a Memory issue see consistent timestamps.
func expiresAtRFC3339(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// dedupModeOrDefault renders the dedup mode for the log line, defaulting
// to "drop" so an empty mode (the common case) logs informatively.
func dedupModeOrDefault(mode string) string {
	if mode == "" {
		return "drop"
	}
	return mode
}

func contains(haystack []string, needle string) bool {
	needle = strings.TrimSpace(needle)
	for _, s := range haystack {
		if strings.TrimSpace(s) == needle {
			return true
		}
	}
	return false
}

var _ tools.Tool = (*Memory)(nil)
