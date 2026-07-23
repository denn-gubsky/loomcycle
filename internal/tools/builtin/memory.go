package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/fallback"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/inprocess"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/mem9"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/redact"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
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

	// PrivateHostAllowlist exempts specific hosts from the mem9 client's
	// dial-time private-IP SSRF block (set in main.go from
	// cfg.Env.HTTPPrivateHostAllowlist — the same operator vouch list the HTTP
	// tool uses). Empty = every private/loopback/metadata IP is refused for
	// mem9 dials, so a model-authored base_url can't reach an internal host.
	// An operator running an internal mem9 backend adds its host here.
	PrivateHostAllowlist []string

	// SqlMem is the RFC AA SQL Memory manager backing sql_query / sql_exec.
	// Nil = the SQL ops refuse with "SQL Memory is not enabled on this
	// server" (the subsystem is off by default; main.go sets it only when
	// storage.sqlmem_enabled). The k/v ops are unaffected.
	SqlMem *sqlmem.Manager

	// SqlAudit records one append-only event per sql_query / sql_exec
	// (WHO ran WHAT op against WHICH scope; the statement is redacted, or
	// omitted in metadata mode). Nil = SQL ops are not audited (best-effort:
	// an audit failure never blocks the SQL op).
	SqlAudit audit.Sink

	// SqlAuditMode is "full" (record the redacted statement) or "metadata"
	// (record op/scope/row counts only). Empty defaults to "full".
	SqlAuditMode string

	// Redactor masks operator infra-secrets out of an audited SQL statement
	// before it is written (full mode only). Nil-safe — a nil Redactor
	// leaves the statement unchanged (the validator already blocks the
	// dangerous statement shapes; redaction is defence for incidental
	// secret-bearing literals in agent SQL).
	Redactor *redact.Redactor
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
	// RFC N: resolve under the run's tenant so a tenant-private backend
	// shadows the shared base; "" tenant collapses to static→shared exactly
	// as before.
	def, ok := lookup.MemoryBackend(ctx, m.Store, m.Cfg, tools.RunIdentity(ctx).TenantID, name)
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
// mem9RequestTimeout is the per-op HTTP timeout for the guarded mem9 client.
// Matches mem9.New's own default (used when Config.HTTPClient is nil), so
// swapping in the SSRF-guarded client keeps the timeout behaviour unchanged.
const mem9RequestTimeout = 10 * time.Second

func (m *Memory) buildMem9(ctx context.Context, name string, def config.MemoryBackend) memrank.Backend {
	tenancy, prefix, err := resolveTenancy(ctx, def.TenancyStrategy)
	if err != nil {
		log.Printf("memory: memory_backend %q (mem9) tenancy unresolved: %v — using in-process fallback", name, err)
		return m.defaultBackend()
	}

	resolver := m.mem9CredentialResolver(def, def.TenancyStrategy, prefix)

	// SSRF guard: dial through the shared private-IP-blocking client so a
	// model-authored base_url cannot reach an internal/loopback/metadata host
	// and exfiltrate the operator-allowlisted X-API-Key. Blocks at DIAL time
	// (rebinding/redirect-safe); the operator's HTTP private-host allowlist
	// (m.PrivateHostAllowlist) exempts a legitimately-private mem9 host.
	b := mem9.New(mem9.Config{
		BaseURL:            def.Config.BaseURL,
		APIVersion:         def.Config.APIVersion,
		Tenancy:            tenancy,
		CredentialResolver: resolver,
		BackendName:        name,
		HTTPClient:         newSSRFGuardedClient(mem9RequestTimeout, m.PrivateHostAllowlist),
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
// TENANT SOURCE: the authoritative tools.RunIdentity(ctx).TenantID
// (RFC L). On authenticated routes this is set from the resolved
// auth.Principal's tenant — which OVERRIDES the wire tenant_id, so it is
// a real isolation boundary, not a forgeable label. Distinct subjects
// under one tenant SHARE this memory partition (they collaborate within
// the tenant); isolation is per-tenant, NOT per-user (that was the
// pre-RFC-L shape, which keyed on UserID). For legacy single-token
// deployments the principal tenant is "default"; in dev open-mode the
// tenant may be empty, in which case key_per_tenant resolves the "" key
// and shared_key_with_prefix refuses (the backstop below).
func resolveTenancy(ctx context.Context, ts config.MemoryBackendTenancy) (mem9.Tenancy, string, error) {
	tenantID := tools.RunIdentity(ctx).TenantID

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
		tenantID := tools.RunIdentity(ctx).TenantID
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
	`add / recall (memory-layer backends only): add ingests conversation messages (the backend may LLM-extract durable facts); recall is a natural-language semantic search over those facts. These require a memory-layer backend (memory_backend kind=mem9); against the default key/value store they return capability_unsupported. Unlike set/get, add does not store value-at-key and is often async — do not assume read-after-write. ` +
	`SQL Memory (a DISTINCT capability of this tool, gated separately by the agent's sql_scopes — having Memory alone does NOT grant it): sql_query runs a read-only SELECT and sql_exec runs a single DDL/DML statement (CREATE/INSERT/UPDATE/DELETE) against a per-scope SQL database SEPARATE from the key/value memory above. Pass statement (one statement, no ATTACH/PRAGMA/load_extension/multiple statements) and optional positional args for ? placeholders. scope selects the database: agent (this agent, durable), user (this end-user, durable), or run (ephemeral, dropped when the run ends). For atomic multi-step writes, sql_begin opens a transaction for the scope, subsequent sql_exec/sql_query run on it, and sql_commit / sql_rollback finish it (it auto-rolls-back if the run ends or it is abandoned). A second sql_begin while one is open NESTS a savepoint — sql_commit/sql_rollback then affect the innermost level (the outer transaction continues on rollback); each result reports the current depth (0 = closed). Requires sql_scopes on the agent AND the server-side subsystem enabled.`

const memoryInputSchema = `{
  "type": "object",
  "properties": {
    "op":         {"type": "string", "enum": ["get","set","delete","list","incr","search","merge","append_dedupe","bounded_list","add","recall","sql_query","sql_exec","sql_begin","sql_commit","sql_rollback"], "description": "Which operation to perform. Families: key/value (get,set,delete,list,incr,merge,append_dedupe,bounded_list,search); memory-layer (add,recall — mem9 backends only); SQL (sql_query,sql_exec,sql_begin,sql_commit,sql_rollback — a per-scope SQL database, gated separately by sql_scopes)."},
    "scope":      {"type": "string", "enum": ["agent","user","run"], "description": "Which keyspace/database. agent: this agent's (cross-run, cross-user). user: this end-user's (cross-agent). run: ephemeral per-run, dropped at run end — SQL ops only."},
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
    "threshold":  {"type": "number", "description": "recall-only: 0..1 relevance floor for returned facts (0 = backend default)."},
    "statement":  {"type": "string", "description": "sql_query / sql_exec: ONE SQL statement. sql_query is read-only (SELECT / WITH … SELECT); sql_exec is DDL/DML (CREATE/INSERT/UPDATE/DELETE/etc.). ATTACH, PRAGMA, load_extension, transactions, and multiple statements are refused."},
    "args":       {"type": "array", "description": "sql_query / sql_exec: positional bind parameters for ? placeholders. An element of the form {\"$embed\": \"text\"} is replaced server-side by the embedding of that text as a pgvector value (reference it with a ::vector cast, e.g. ... ORDER BY embedding <=> ?::vector); requires the postgres tier with pgvector + a configured embedder.", "items": {}},
    "timeout_ms": {"type": "integer", "description": "sql_query / sql_exec: reserved — the server-configured statement timeout is authoritative in this version."}
  },
  "required": ["op","scope"],
  "additionalProperties": false
}`

type memoryInput struct {
	Op    string `json:"op"`
	Scope string `json:"scope"`
	Key   string `json:"key,omitempty"`
	// Path (RFC AL) optionally registers/addresses this entry in the Path
	// tree. On `set`, a memory_entry dirent is registered at this path; on
	// `get`, the path resolves to the entry's key (an alternative to `key`).
	// Same (scope, scope_id) as the entry; tenant from the run identity.
	Path      string          `json:"path,omitempty"`
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

	// --- RFC AA SQL Memory (sql_query / sql_exec) ---
	// Statement is the single SQL statement to run (validated by the
	// Go-layer security floor before it reaches the driver).
	Statement string `json:"statement,omitempty"`
	// Args are positional bind parameters for `?` placeholders in Statement.
	Args []any `json:"args,omitempty"`
	// TimeoutMs is a per-call statement timeout request. RESERVED in Phase 1:
	// the operator-configured LOOMCYCLE_SQLMEM_STATEMENT_TIMEOUT_MS is
	// authoritative (a per-call override that could only ever tighten, never
	// widen, is a Phase-2 refinement). Accepted so the wire shape is stable.
	TimeoutMs int `json:"timeout_ms,omitempty"`
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
		return errResult("Memory tool: not configured (no Store backend — set LOOMCYCLE_STORAGE_BACKEND or remove Memory from the agent's tools)"), nil
	}
	var in memoryInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult(fmt.Sprintf("invalid input JSON: %s", err)), nil
	}

	// RFC AA SQL Memory ops resolve scope through resolveSqlScope (their own
	// {agent,user,run} gate keyed off SqlMemPolicy), NOT the k/v resolveScope
	// — which is gated on memory_scopes and rejects `run`. Dispatch them
	// before the k/v scope resolution so a SQL op never trips the k/v gate.
	switch in.Op {
	case "sql_query":
		return m.execSqlQuery(ctx, in)
	case "sql_exec":
		return m.execSqlExec(ctx, in)
	case "sql_begin":
		return m.execSqlBegin(ctx, in)
	case "sql_commit":
		return m.execSqlTxnFinish(ctx, in, true)
	case "sql_rollback":
		return m.execSqlTxnFinish(ctx, in, false)
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
		return errResult(fmt.Sprintf("unknown op %q (must be one of: get, set, delete, list, incr, search, merge, append_dedupe, bounded_list, add, recall, sql_query, sql_exec, sql_begin, sql_commit, sql_rollback)", in.Op)), nil
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

// resolveSqlScope is the RFC AA SQL Memory scope gate. It is SEPARATE from
// resolveScope: SQL scopes are {agent, user, run} (run has no k/v analogue),
// and the gate is the agent's sql_scopes ACL (SqlMemPolicy), not memory_scopes.
//
// Default-deny: an empty sql_scopes refuses ALL SQL. The scope_id is resolved
// SERVER-SIDE from the run context (never the wire) exactly like resolveScope:
//
//	agent → tools.AgentName(ctx)
//	user  → tools.RunIdentity(ctx).UserID
//	run   → tools.RunIdentity(ctx).RootRunID
//
// so one agent's run can never read another agent's / user's / run's SQL DB.
//
// The run scope keys off RootRunID (the TOP-LEVEL run at the root of the
// spawn tree), NOT the per-sub-run RunID: that way the whole tree shares one
// ephemeral DB, and the server's run-completion drop (which targets
// meta.RootRunID) reclaims exactly the file the tree used — keying off the
// per-sub-run id instead would orphan a sub-agent's DB (never dropped) and
// hide the parent's tables from a child granted `run`. Mirrors how RFC AH
// ephemeral volumes scope to RootRunID.
func (m *Memory) resolveSqlScope(ctx context.Context, requested string) (scope, scopeID string, err error) {
	if requested == "" {
		return "", "", fmt.Errorf("missing required field: scope (one of: agent, user, run)")
	}
	pol := tools.SqlMemPolicy(ctx)
	if len(pol.AllowedScopes) == 0 {
		return "", "", fmt.Errorf("Memory tool: this agent has no sql_scopes configured — add `sql_scopes: [agent]` (and/or user, run) to the agent yaml")
	}
	if !contains(pol.AllowedScopes, requested) {
		return "", "", fmt.Errorf("Memory tool: sql scope %q not in this agent's sql_scopes %v", requested, pol.AllowedScopes)
	}
	switch requested {
	case "agent":
		name := tools.AgentName(ctx)
		if name == "" {
			return "", "", fmt.Errorf("Memory tool: sql scope=agent requires a yaml-declared agent (no agent name on the run context)")
		}
		return "agent", name, nil
	case "user":
		uid := tools.RunIdentity(ctx).UserID
		if uid == "" {
			return "", "", fmt.Errorf("Memory tool: sql scope=user requires a user_id on the run (caller must supply user_id when starting the run)")
		}
		return "user", uid, nil
	case "run":
		// RootRunID roots the spawn tree; fall back to RunID for a run started
		// outside the volume-aware run-start path (RootRunID unset there).
		ident := tools.RunIdentity(ctx)
		rid := ident.RootRunID
		if rid == "" {
			rid = tools.RunID(ctx)
		}
		if rid == "" {
			return "", "", fmt.Errorf("Memory tool: sql scope=run requires an active run (no run id on the context)")
		}
		return "run", rid, nil
	default:
		return "", "", fmt.Errorf("Memory tool: unknown sql scope %q (want one of: agent, user, run)", requested)
	}
}

// execSqlQuery runs a read-only SELECT against the resolved scope DB.
func (m *Memory) execSqlQuery(ctx context.Context, in memoryInput) (tools.Result, error) {
	scope, scopeID, err := m.resolveSqlScope(ctx, in.Scope)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if m.SqlMem == nil {
		return errResult("SQL Memory is not enabled on this server (set storage.sqlmem_enabled / LOOMCYCLE_SQLMEM_ENABLED=1)"), nil
	}
	if strings.TrimSpace(in.Statement) == "" {
		return errResult("sql_query: missing required field: statement"), nil
	}
	args, aerr := m.resolveEmbedArgs(ctx, in.Args)
	if aerr != nil {
		m.auditSql(ctx, "sql_query", scope, scopeID, in.Statement, 0, 0, aerr)
		return errResult(fmt.Sprintf("sql_query: %s", aerr)), nil
	}
	key := sqlmem.ScopeKey{Tenant: sqlScopeTenant(ctx), Scope: scope, ScopeID: scopeID}
	txnID := currentSqlTxnID(ctx, scope, scopeID)
	start := time.Now()
	var res *sqlmem.QueryResult
	var qerr error
	if txnID != "" && m.SqlMem.InTxn(txnID) {
		res, qerr = m.SqlMem.QueryTxn(ctx, txnID, in.Statement, args)
	} else {
		res, qerr = m.SqlMem.Query(ctx, key, in.Statement, args)
	}
	durMs := time.Since(start).Milliseconds()
	if qerr != nil {
		m.auditSql(ctx, "sql_query", scope, scopeID, in.Statement, 0, durMs, qerr)
		return errResult(fmt.Sprintf("sql_query: %s", qerr)), nil
	}
	m.auditSql(ctx, "sql_query", scope, scopeID, in.Statement, int64(len(res.Rows)), durMs, nil)
	return okJSON(map[string]any{
		"columns":   res.Columns,
		"rows":      res.Rows,
		"truncated": res.Truncated,
	})
}

// execSqlExec runs a DDL/DML statement against the resolved scope DB.
func (m *Memory) execSqlExec(ctx context.Context, in memoryInput) (tools.Result, error) {
	scope, scopeID, err := m.resolveSqlScope(ctx, in.Scope)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if m.SqlMem == nil {
		return errResult("SQL Memory is not enabled on this server (set storage.sqlmem_enabled / LOOMCYCLE_SQLMEM_ENABLED=1)"), nil
	}
	if strings.TrimSpace(in.Statement) == "" {
		return errResult("sql_exec: missing required field: statement"), nil
	}
	args, aerr := m.resolveEmbedArgs(ctx, in.Args)
	if aerr != nil {
		m.auditSql(ctx, "sql_exec", scope, scopeID, in.Statement, 0, 0, aerr)
		return errResult(fmt.Sprintf("sql_exec: %s", aerr)), nil
	}
	// Per-agent quota override wins over the manager default when > 0.
	quota := tools.SqlMemPolicy(ctx).QuotaBytes
	key := sqlmem.ScopeKey{Tenant: sqlScopeTenant(ctx), Scope: scope, ScopeID: scopeID}
	txnID := currentSqlTxnID(ctx, scope, scopeID)
	start := time.Now()
	var res *sqlmem.ExecResult
	var xerr error
	if txnID != "" && m.SqlMem.InTxn(txnID) {
		res, xerr = m.SqlMem.ExecTxn(ctx, txnID, in.Statement, args, quota)
	} else {
		res, xerr = m.SqlMem.Exec(ctx, key, in.Statement, args, quota)
	}
	durMs := time.Since(start).Milliseconds()
	if xerr != nil {
		m.auditSql(ctx, "sql_exec", scope, scopeID, in.Statement, 0, durMs, xerr)
		return errResult(fmt.Sprintf("sql_exec: %s", xerr)), nil
	}
	m.auditSql(ctx, "sql_exec", scope, scopeID, in.Statement, res.RowsAffected, durMs, nil)
	return okJSON(map[string]any{
		"rows_affected":  res.RowsAffected,
		"last_insert_id": res.LastInsertID,
	})
}

// sqlScopeTenant returns the tenant partition for a DURABLE SQL Memory scope.
// Unlike the k/v store (which accepts an empty tenant as a valid partition), SQL
// Memory sanitizes the tenant into a filesystem path (sqlite) / postgres
// identifier (postgres) and so cannot key on "". In open mode / legacy-token
// deployments the run carries no authoritative tenant (TenantID==""), which would
// otherwise fail durable agent/user ops with "empty scope identifier";
// canonicalize it to "default" — the value keyPath's docs assume and the
// documented manual `tenant_id: default` workaround used (so data is continuous
// across both). A real (non-empty) tenant is used verbatim; the run scope is not
// tenant-keyed, so this is a no-op there.
func sqlScopeTenant(ctx context.Context) string {
	if t := tools.RunIdentity(ctx).TenantID; t != "" {
		return t
	}
	return "default"
}

// currentSqlTxnID returns the explicit-transaction registry key for the current
// run + scope, or "" when there is no run id on the context (then SQL ops
// auto-commit). Keyed off the run-tree root so run-completion cleanup reclaims
// any open transaction.
func currentSqlTxnID(ctx context.Context, scope, scopeID string) string {
	rid := tools.RunIdentity(ctx).RootRunID
	if rid == "" {
		rid = tools.RunID(ctx)
	}
	if rid == "" {
		return ""
	}
	return sqlmem.BuildTxnID(rid, scope, scopeID)
}

// execSqlBegin opens an explicit (multi-call) transaction for the resolved
// scope. Subsequent sql_exec/sql_query on this scope (in this run) run on it
// until sql_commit / sql_rollback.
func (m *Memory) execSqlBegin(ctx context.Context, in memoryInput) (tools.Result, error) {
	scope, scopeID, err := m.resolveSqlScope(ctx, in.Scope)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if m.SqlMem == nil {
		return errResult("SQL Memory is not enabled on this server (set storage.sqlmem_enabled / LOOMCYCLE_SQLMEM_ENABLED=1)"), nil
	}
	rid := tools.RunIdentity(ctx).RootRunID
	if rid == "" {
		rid = tools.RunID(ctx)
	}
	if rid == "" {
		return errResult("sql_begin: an explicit transaction requires an active run"), nil
	}
	txnID := sqlmem.BuildTxnID(rid, scope, scopeID)
	key := sqlmem.ScopeKey{Tenant: sqlScopeTenant(ctx), Scope: scope, ScopeID: scopeID}
	start := time.Now()
	depth, berr := m.SqlMem.BeginTxn(ctx, txnID, rid, key)
	durMs := time.Since(start).Milliseconds()
	if berr != nil {
		m.auditSql(ctx, "sql_begin", scope, scopeID, "", 0, durMs, berr)
		return errResult(fmt.Sprintf("sql_begin: %s", berr)), nil
	}
	m.auditSql(ctx, "sql_begin", scope, scopeID, "", 0, durMs, nil)
	// depth is the nesting level after this begin (1 = root txn; 2+ = a nested
	// SAVEPOINT level — Phase 3b).
	return okJSON(map[string]any{"ok": true, "depth": depth})
}

// execSqlTxnFinish commits (commit=true) or rolls back the open transaction for
// the resolved scope.
func (m *Memory) execSqlTxnFinish(ctx context.Context, in memoryInput, commit bool) (tools.Result, error) {
	op := "sql_rollback"
	if commit {
		op = "sql_commit"
	}
	scope, scopeID, err := m.resolveSqlScope(ctx, in.Scope)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if m.SqlMem == nil {
		return errResult("SQL Memory is not enabled on this server (set storage.sqlmem_enabled / LOOMCYCLE_SQLMEM_ENABLED=1)"), nil
	}
	txnID := currentSqlTxnID(ctx, scope, scopeID)
	if txnID == "" {
		return errResult(fmt.Sprintf("%s: an explicit transaction requires an active run", op)), nil
	}
	start := time.Now()
	var depth int
	var ferr error
	if commit {
		depth, ferr = m.SqlMem.CommitTxn(txnID)
	} else {
		depth, ferr = m.SqlMem.RollbackTxn(txnID)
	}
	durMs := time.Since(start).Milliseconds()
	if ferr != nil {
		m.auditSql(ctx, op, scope, scopeID, "", 0, durMs, ferr)
		return errResult(fmt.Sprintf("%s: %s", op, ferr)), nil
	}
	m.auditSql(ctx, op, scope, scopeID, "", 0, durMs, nil)
	// depth is the nesting level AFTER this op: a nested level was closed (still
	// >0) or the whole transaction committed/rolled back (0). Phase 3b.
	return okJSON(map[string]any{"ok": true, "depth": depth})
}

// embedDirective reports whether a bind arg is an {"$embed": "<text>"} directive
// (RFC AA Phase 3c) — the server-side embedding form that keeps a raw vector out
// of the model context.
func embedDirective(arg any) (string, bool) {
	mp, ok := arg.(map[string]any)
	if !ok || len(mp) != 1 {
		return "", false
	}
	v, ok := mp["$embed"]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// resolveEmbedArgs replaces every {"$embed": "<text>"} bind arg with the
// pgvector text of that text's embedding, so the agent never handles a raw
// N-float vector. All directives in one statement embed in ONE Embed call.
// Returns args unchanged when none are present; refuses (typed) when an embedder
// or the vector-capable tier is missing.
func (m *Memory) resolveEmbedArgs(ctx context.Context, args []any) ([]any, error) {
	var texts []string
	var slots []int
	for i, a := range args {
		if txt, ok := embedDirective(a); ok {
			if strings.TrimSpace(txt) == "" {
				return nil, fmt.Errorf("$embed directive has empty text")
			}
			texts = append(texts, txt)
			slots = append(slots, i)
		}
	}
	if len(texts) == 0 {
		return args, nil
	}
	if m.Embedder == nil {
		return nil, fmt.Errorf("$embed requires a configured embedder (set memory.embedder)")
	}
	if m.SqlMem == nil || !m.SqlMem.VectorsEnabled() {
		return nil, fmt.Errorf("$embed requires vector columns — the postgres tier with pgvector installed in the sqlmem_ext schema (see docs/SQL_MEMORY.md)")
	}
	vecs, err := m.Embedder.Embed(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	if len(vecs) != len(texts) {
		return nil, fmt.Errorf("embedder returned %d vectors for %d texts", len(vecs), len(texts))
	}
	out := make([]any, len(args))
	copy(out, args)
	for j, i := range slots {
		out[i] = encodePgvector(vecs[j])
	}
	return out, nil
}

// encodePgvector formats a float32 vector as pgvector's text wire form
// "[1,2,3]" (the agent casts it to ::vector). Mirrors the main store's encoder.
func encodePgvector(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// auditSql records one append-only SQL Memory audit event. Best-effort: a
// nil sink is skipped, and a Record error is logged but NEVER blocks the op
// (audit is observability, not a transaction participant). In "full" mode
// the statement is REDACTED before recording (operator infra-secrets out);
// in "metadata" mode the statement is omitted entirely.
func (m *Memory) auditSql(ctx context.Context, op, scope, scopeID, statement string, rows, durMs int64, opErr error) {
	if m.SqlAudit == nil {
		return
	}
	ident := tools.RunIdentity(ctx)
	ev := audit.Event{
		ActorTenant:   ident.TenantID,
		ActorSubject:  ident.UserID,
		Action:        op,
		SqlOp:         op,
		SqlScope:      scope,
		SqlScopeID:    scopeID,
		SqlRows:       rows,
		SqlDurationMs: durMs,
	}
	if opErr != nil {
		ev.SqlError = opErr.Error()
	}
	if m.sqlAuditMode() == "full" {
		if m.Redactor != nil {
			ev.SqlStatement = m.Redactor.String(statement)
		} else {
			ev.SqlStatement = statement
		}
	}
	if err := m.SqlAudit.Record(ev); err != nil {
		log.Printf("sqlmem: audit record (%s scope=%s) failed: %v", op, scope, err)
	}
}

// sqlAuditMode normalizes the audit mode; empty defaults to "full".
func (m *Memory) sqlAuditMode() string {
	if m.SqlAuditMode == "metadata" {
		return "metadata"
	}
	return "full"
}

func (m *Memory) execGet(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) (tools.Result, error) {
	if in.Path != "" && in.Key == "" {
		key, err := m.resolveMemoryPath(ctx, scope, scopeID, in.Path)
		if err != nil {
			return errResult("get: " + err.Error()), nil
		}
		in.Key = key
	}
	if in.Key == "" {
		return errResult("get: missing required field: key (or path)"), nil
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

// coreBlockKeyPrefix is the reserved KV namespace for RFC BL P1 core memory
// blocks (single source of truth in the memrank package, shared with the HTTP
// injection reader): a block labeled <label> is stored at `core/<label>`.
const coreBlockKeyPrefix = memrank.CoreBlockKeyPrefix

// enforceCoreBlockWrite gates an agent's Memory `set` to a reserved
// core/<label> key against the run's resolved core-block policy (RFC BL P1):
//
//   - a read_only block → refuse the write (operator-authored; the agent may
//     read the value via injection but never overwrite it).
//   - a block with limit_bytes > 0 → refuse a value over the per-block cap
//     (mirrors the per-scope quota refusal).
//
// A write to a core/<label> key with NO matching declared block passes through
// as a normal key. scope is the already-resolved store scope; the policy
// block's Scope ("agent"/"user"/"tenant") must match it, so a user-scope block
// can't gate an agent-scope write of the same label. The policy is
// operator-resolved on ctx — never model-supplied.
func enforceCoreBlockWrite(ctx context.Context, scope store.MemoryScope, key string, valueLen int) error {
	if !strings.HasPrefix(key, coreBlockKeyPrefix) {
		return nil
	}
	label := strings.TrimPrefix(key, coreBlockKeyPrefix)
	for _, b := range tools.CoreBlocksPolicy(ctx).Blocks {
		if b.Label != label || b.Scope != string(scope) {
			continue
		}
		if b.ReadOnly {
			return fmt.Errorf("Memory.set: core block %q (scope=%s) is read_only — operator-authored; agent writes are refused", label, scope)
		}
		if b.LimitBytes > 0 && valueLen > b.LimitBytes {
			return fmt.Errorf("Memory.set: core block %q (scope=%s) value %d bytes exceeds limit_bytes %d", label, scope, valueLen, b.LimitBytes)
		}
		return nil
	}
	return nil
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
	// Fail fast on a malformed path before writing the value (RFC AL).
	if in.Path != "" {
		if _, err := normalizePath(in.Path); err != nil {
			return errResult("set: " + err.Error()), nil
		}
	}
	if m.MaxValueBytes > 0 && len(in.Value) > m.MaxValueBytes {
		return errResult(fmt.Sprintf("set: value (%d bytes) exceeds max %d bytes", len(in.Value), m.MaxValueBytes)), nil
	}
	// RFC BL P1: a write to a reserved `core/<label>` key is gated by the
	// agent's core-block config — read_only refuses the write entirely,
	// limit_bytes caps it (mirroring the quota refusal below). Undeclared
	// core/* keys pass through as normal memory.
	if err := enforceCoreBlockWrite(ctx, scope, in.Key, len(in.Value)); err != nil {
		return errResult(err.Error()), nil
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
	// RFC AL — register a Path-tree dirent for this entry. The k/v value is
	// already durable; a dirent failure is surfaced as a warning so the value
	// isn't lost (the agent can retry the path). The path was validated at the
	// top of execSet, so this only fails on a store fault.
	if in.Path != "" {
		if err := m.registerMemoryDirent(ctx, scope, scopeID, in.Key, in.Path); err != nil {
			resp["path_warning"] = fmt.Sprintf("value set but path registration failed: %s", err)
		} else {
			resp["path"] = in.Path
		}
	}
	return okJSON(resp)
}

// resolveMemoryPath resolves a Path-tree path to the memory key it names,
// within the entry's own (scope, scope_id) tree. tenant from the run identity.
func (m *Memory) resolveMemoryPath(ctx context.Context, scope store.MemoryScope, scopeID, rawPath string) (string, error) {
	if m.Store == nil {
		return "", fmt.Errorf("path addressing requires a Store backend")
	}
	canonical, err := normalizePath(rawPath)
	if err != nil {
		return "", err
	}
	parent, name, isRoot := splitPath(canonical)
	if isRoot {
		return "", fmt.Errorf("path may not be the root")
	}
	row, err := m.Store.DirentGet(ctx, tools.RunIdentity(ctx).TenantID, string(scope), scopeID, parent, name)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return "", fmt.Errorf("no such path: %s", canonical)
		}
		return "", err
	}
	if row.Kind != "memory_entry" {
		return "", fmt.Errorf("path %s is a %s, not a memory entry", canonical, row.Kind)
	}
	var ref struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(row.ResourceRef, &ref)
	if ref.Key == "" {
		return "", fmt.Errorf("path %s has no memory key", canonical)
	}
	return ref.Key, nil
}

// registerMemoryDirent registers (upserts) a memory_entry dirent naming this
// k/v entry at the given path, in the entry's own (scope, scope_id) tree.
func (m *Memory) registerMemoryDirent(ctx context.Context, scope store.MemoryScope, scopeID, key, rawPath string) error {
	if m.Store == nil {
		return fmt.Errorf("path addressing requires a Store backend")
	}
	canonical, err := normalizePath(rawPath)
	if err != nil {
		return err
	}
	parent, name, isRoot := splitPath(canonical)
	if isRoot {
		return fmt.Errorf("path may not be the root")
	}
	ref, _ := json.Marshal(map[string]any{"scope": string(scope), "scope_id": scopeID, "key": key, "facet": "kv"})
	_, err = m.Store.DirentCreate(ctx, store.DirentRow{
		TenantID: tools.RunIdentity(ctx).TenantID, Scope: string(scope), ScopeID: scopeID,
		ParentPath: parent, Name: name, Kind: "memory_entry", ResourceRef: ref,
	})
	return err
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
			"score":      r.Score,           // raw cosine similarity — NEVER touched by hybrid fusion or ranking (stable across searches)
			"rank_score": res.RankScores[i], // computed rank the result was ordered by: fused-semantic (RRF) + recency + frequency
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
	// RFC BL: the run's authoritative tenant partitions base memory. Server-
	// sourced from RunIdentity (never tool input) — the same isolation key the
	// sqlmem/dirent paths in this file already use.
	next, err := m.Store.MemoryIncrement(ctx, tools.RunIdentity(ctx).TenantID, scope, scopeID, in.Key, delta, ttl)
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
	// RFC BL: partition by the run's authoritative tenant (server-sourced).
	final, err := m.Store.MemoryAtomicUpdate(ctx, tools.RunIdentity(ctx).TenantID, scope, scopeID, in.Key, ttl,
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
	// RFC BL: partition by the run's authoritative tenant (server-sourced).
	final, err := m.Store.MemoryAtomicUpdate(ctx, tools.RunIdentity(ctx).TenantID, scope, scopeID, in.Key, ttl,
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
	// RFC BL: partition by the run's authoritative tenant (server-sourced).
	final, err := m.Store.MemoryAtomicUpdate(ctx, tools.RunIdentity(ctx).TenantID, scope, scopeID, in.Key, ttl,
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
