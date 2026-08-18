package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
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
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/remote"
	"github.com/denn-gubsky/loomcycle/internal/netguard"
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
	// Store directly, so MR-3's MemoryBackendDef can select a named
	// backend here. When nil, Execute lazily defaults it to the
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
//   - anything else → unknown kind; logs and falls back to in-process.
//     This is the DEGRADE PATH for a def persisted by an older build: the
//     external `mem9` kind was removed, so a stored kind:mem9 row now lands
//     here and serves from in-process rather than failing the agent's run.
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
	case "remote":
		rb, err := m.newRemoteBackend(def)
		if err != nil {
			// A misconfigured remote def must not fail the agent's run: log and
			// serve locally (same degrade posture as the unknown-kind arm).
			log.Printf("memory: memory_backend %q (kind=remote) is misconfigured: %v — using in-process fallback", name, err)
			return m.defaultBackend()
		}
		// fallback_on_error: inprocess wraps the remote so an unreachable peer
		// degrades to local memory per-op instead of failing the run.
		if def.FallbackOnError == "inprocess" {
			return fallback.New(rb, m.defaultBackend(), log.Printf)
		}
		return rb
	default:
		log.Printf("memory: memory_backend %q has unknown kind %q — using in-process fallback", name, def.Kind)
		return m.defaultBackend()
	}
}

// remoteBackendTimeout bounds each proxied call to a peer instance.
const remoteBackendTimeout = 30 * time.Second

// newRemoteBackend builds a remote memory backend from a resolved def. The
// SSRF-guarded HTTP client and the credential-env allowlist are supplied here
// (the remote package stays free of config/netguard/os coupling). The peer's
// own host is added to the guarded client's private-host allowlist — the
// operator authored this base_url, so reaching it on a private network
// (sibling replica / tailnet) is an explicit trust decision; redirects to any
// OTHER private address stay blocked.
func (m *Memory) newRemoteBackend(def config.MemoryBackend) (memrank.Backend, error) {
	host := ""
	if u, err := url.Parse(def.Config.BaseURL); err == nil {
		host = u.Hostname()
	}
	var allowlist []string
	if host != "" {
		allowlist = []string{host}
	}
	client := netguard.NewGuardedClient(remoteBackendTimeout, allowlist)
	return remote.New(remote.Options{
		BaseURL:          def.Config.BaseURL,
		APIVersion:       def.Config.APIVersion,
		DefaultAPIKeyEnv: def.Config.APIKeyEnv,
		TenancyKind:      def.TenancyStrategy.Kind,
		EnvPattern:       def.TenancyStrategy.EnvPattern,
		KeyResolver:      resolveCredentialEnv,
		HTTPClient:       client,
	})
}

// resolveCredentialEnv resolves an env-var NAME to its value, gated by the SAME
// allowlist the def validator uses — so a remote backend can never be pointed
// at one of loomcycle's own infrastructure secrets (e.g. LOOMCYCLE_AUTH_TOKEN),
// even if a def author named one. Returns only the env-var NAME in errors,
// never the value (the fallback wrapper logs these).
func resolveCredentialEnv(name string) (string, error) {
	if !config.EnvNameCredentialSafe(name) {
		return "", fmt.Errorf("api_key_env %q is not an allowed credential env var", name)
	}
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("api_key_env %q is not set", name)
	}
	return v, nil
}

// memoryLayer resolves the MemoryLayer capability for the agent's configured
// backend (RFC K). It returns (layer, true) when the resolved backend
// implements the add/recall memory-layer paradigm, or (nil, false) when it
// does not. The caller (execAdd/execRecall) turns false into the typed
// capability_unsupported refusal — a fail-closed refusal, never a silent
// no-op that would drop the caller's messages.
//
// It reuses backend(ctx) so backend selection (the per-agent memory_backend
// Def, the unknown-kind degradation) stays in one place — the memory-layer
// view is just a capability probe over the same resolved backend. A wrapped
// backend is treated as a layer only if the WRAPPER surfaces the capability,
// so a wrapper that cannot honor a semantic add/recall degrades to "no
// layer" rather than silently routing them at a KV store.
func (m *Memory) memoryLayer(ctx context.Context) (memrank.MemoryLayer, bool) {
	return memrank.AsMemoryLayer(m.backend(ctx))
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
	`add / recall: add ingests conversation messages for durable memory — on the default backend it enqueues them for background consolidation and returns status "pending" (a scheduled consolidator later distils durable facts); recall is a natural-language semantic search over stored memories and needs an embedder + a vector-capable store (otherwise it returns vector_unsupported / embedder_not_configured). A backend that is not memory-layer-capable returns capability_unsupported. Unlike set/get, add does not store value-at-key and is async — do not assume read-after-write. ` +
	`SQL Memory (a DISTINCT capability of this tool, gated separately by the agent's sql_scopes — having Memory alone does NOT grant it): sql_query runs a read-only SELECT and sql_exec runs a single DDL/DML statement (CREATE/INSERT/UPDATE/DELETE) against a per-scope SQL database SEPARATE from the key/value memory above. Pass statement (one statement, no ATTACH/PRAGMA/load_extension/multiple statements) and optional positional args for ? placeholders. scope selects the database: agent (this agent, durable), user (this end-user, durable), or run (ephemeral, dropped when the run ends). For atomic multi-step writes, sql_begin opens a transaction for the scope, subsequent sql_exec/sql_query run on it, and sql_commit / sql_rollback finish it (it auto-rolls-back if the run ends or it is abandoned). A second sql_begin while one is open NESTS a savepoint — sql_commit/sql_rollback then affect the innermost level (the outer transaction continues on rollback); each result reports the current depth (0 = closed). Requires sql_scopes on the agent AND the server-side subsystem enabled.`

const memoryInputSchema = `{
  "type": "object",
  "properties": {
    "op":         {"type": "string", "enum": ["get","set","delete","list","incr","search","merge","append_dedupe","bounded_list","add","recall","sql_query","sql_exec","sql_begin","sql_commit","sql_rollback","cursor_get","cursor_scan","cursor_lease","cursor_advance","cursor_release","supersede","pending_drain","pending_ack"], "description": "Which operation to perform. Families: key/value (get,set,delete,list,incr,merge,append_dedupe,bounded_list,search); memory-layer (add,recall — add enqueues for background consolidation, recall needs the vector stack); SQL (sql_query,sql_exec,sql_begin,sql_commit,sql_rollback — a per-scope SQL database, gated separately by sql_scopes); consolidation (cursor_get,cursor_scan,cursor_lease,cursor_advance,cursor_release,supersede,pending_drain,pending_ack — background memory consolidation, gated separately by a dedicated grant)."},
    "scope":      {"type": "string", "enum": ["agent","user","tenant","run"], "description": "Which keyspace/database. agent: this agent's (cross-run, cross-user). user: this end-user's (cross-agent). tenant: shared by every user and agent in the tenant — what you write here, they all read. run: ephemeral per-run, dropped at run end — SQL ops only."},
    "key":        {"type": "string", "description": "The entry's key. Required for get / set / delete / incr / merge / append_dedupe / bounded_list."},
    "value":      {"description": "The JSON value. Required for set / merge / append_dedupe / bounded_list. For merge: a JSON object whose fields overlay the existing object. For append_dedupe / bounded_list: the item to append."},
    "delta":      {"type": "integer", "description": "Increment delta for incr (default 1, may be negative)."},
    "ttl":        {"type": "integer", "description": "Optional time-to-live in seconds. Applies to write ops; 0 means no expiry (or keep existing on update)."},
    "prefix":     {"type": "string", "description": "Optional key prefix filter for list / search."},
    "sources":    {"type": "array", "items": {"type": "string", "enum": ["facts","notes","documents"]}, "description": "search / recall: which kinds of remembered thing to return. \"facts\" = memory a consolidator distilled; \"notes\" = memory an agent wrote directly; \"documents\" = Document chunk bodies, which share the memory keyspace. recall defaults to facts+notes (document prose is not something you were told); search defaults to everything. Each result carries a matching \"kind\". Mixing documents with only ONE of facts/notes is refused — use [facts], [notes], [facts,notes], [documents], or all three. Use this instead of guessing key prefixes."},
    "limit":      {"type": "integer", "description": "list: max entries returned (default 100). bounded_list: keep the N most recent items (required, >= 1). cursor_scan: max chats returned in one page (default 10, max 50)."},
    "embed":      {"type": "boolean", "description": "v0.9.0 set-only: when true, also generates and stores an embedding so this row is reachable via op=search."},
    "embed_text": {"type": "string", "description": "v0.9.0 set-only: the text to embed when embed=true. Defaults to the JSON-stringified value when omitted."},
    "query":      {"type": "string", "description": "v0.9.0 search-only: the text to embed and use as the similarity query."},
    "top_k":      {"type": "integer", "description": "v0.9.0 search-only: max results (default 10, max 50)."},
    "rank":       {"type": "object", "description": "search-only hybrid ranking weights. Omit for pure semantic (default). Properties: semantic_weight, recency_weight, recency_half_life_hours, source_weight, frequency_weight (source/frequency reserved — contribute 0 today).", "properties": {"semantic_weight": {"type": "number"}, "recency_weight": {"type": "number"}, "recency_half_life_hours": {"type": "number"}, "source_weight": {"type": "number"}, "frequency_weight": {"type": "number"}}},
    "dedup":      {"type": "object", "description": "search-only near-duplicate collapse. Omit (or enabled=false) for no dedup (default). Drops a result whose embedding cosine similarity to a higher-ranked kept result is >= threshold. Properties: enabled (bool), threshold (number, cosine-similarity floor, default 0.92), mode (\"drop\" default | \"merge\" | \"keep\").", "properties": {"enabled": {"type": "boolean"}, "threshold": {"type": "number"}, "mode": {"type": "string", "enum": ["drop","merge","keep"]}}},
    "messages":   {"type": "array", "description": "add-only: conversation turns to ingest. Each item is {role, content}.", "items": {"type": "object", "properties": {"role": {"type": "string", "enum": ["user","assistant","system"]}, "content": {"type": "string"}}, "required": ["role","content"]}},
    "infer":      {"type": "boolean", "description": "add-only: when true (default) the messages are handed to the memory layer for consolidation into durable facts (on the default backend, enqueued for a background consolidator); false stores them verbatim as one row."},
    "metadata":   {"type": "object", "description": "add-only: opaque key/value context attached to the ingestion.", "additionalProperties": {"type": "string"}},
    "threshold":  {"type": "number", "description": "recall-only: 0..1 relevance floor for returned facts (0 = backend default)."},
    "statement":  {"type": "string", "description": "sql_query / sql_exec: ONE SQL statement. sql_query is read-only (SELECT / WITH … SELECT); sql_exec is DDL/DML (CREATE/INSERT/UPDATE/DELETE/etc.). ATTACH, PRAGMA, load_extension, transactions, and multiple statements are refused."},
    "args":       {"type": "array", "description": "sql_query / sql_exec: positional bind parameters for ? placeholders. An element of the form {\"$embed\": \"text\"} is replaced server-side by the embedding of that text as a pgvector value (reference it with a ::vector cast, e.g. ... ORDER BY embedding <=> ?::vector); requires the postgres tier with pgvector + a configured embedder.", "items": {}},
    "timeout_ms": {"type": "integer", "description": "sql_query / sql_exec: reserved — the server-configured statement timeout is authoritative in this version."},
    "lease_ttl_ms":  {"type": "integer", "description": "cursor_lease: how long to hold the consolidation lease, in milliseconds (0 = default; clamped to a maximum). The lease auto-expires so a crashed consolidator never wedges a target."},
    "completed_at":  {"type": "string", "description": "cursor_advance: the watermark timestamp (RFC3339), copied verbatim from the cursor_scan row you consolidated. With session_id it forms the composite watermark; the watermark only ever moves forward, and the server refuses a timestamp that does not match that chat's real finish time."},
    "session_id":    {"type": "string", "description": "cursor_advance: the watermark session id, copied verbatim from the same cursor_scan row as completed_at (required — the pair travels together). Must be a real, finished chat belonging to this memory target."},
    "ids":           {"type": "array", "description": "pending_ack: the pending-row ids to mark drained (as returned by pending_drain).", "items": {"type": "string"}},
    "provenance":    {"type": "object", "description": "set-only: where this fact came from, recorded alongside the row. class is a short label for the kind of fact (e.g. preference, fact, decision, correction); source_session_id / source_run_id name the chat and run it was distilled from (relay them from pending_drain or the transcript you read). Descriptive only — it never changes what the write can reach. The writer identity is stamped server-side.", "properties": {"class": {"type": "string"}, "source_session_id": {"type": "string"}, "source_run_id": {"type": "string"}}, "additionalProperties": false},
    "from_pending":  {"type": "string", "description": "set-only: the id of a pending item you drained, so this fact records what produced it. Pass the id and the server fills in the origin and source ids from that row — you cannot set those yourself. Unknown or unowned ids are ignored and the write still succeeds. Prefer this over filling source_session_id / source_run_id by hand when the fact came from a drained item."},
    "include_provenance": {"type": "boolean", "description": "get-only: also return where the fact came from, and whether that origin is still readable (origin_available). A false origin_available means the chat has since been deleted — the fact is still valid, you just cannot go re-read its source."}
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
	Path   string          `json:"path,omitempty"`
	Value  json.RawMessage `json:"value,omitempty"`
	Delta  *int64          `json:"delta,omitempty"`
	TTL    int64           `json:"ttl,omitempty"`
	Prefix string          `json:"prefix,omitempty"`
	// Sources selects WHICH KINDS of remembered thing search/recall may return
	// (RFC BW): "facts" (the agent's own memory) and/or "documents" (Document chunk
	// bodies, which share the memory keyspace). It is the named alternative to making
	// a caller know the reserved doc.chunk: prefix.
	Sources   []string `json:"sources,omitempty"`
	Limit     int      `json:"limit,omitempty"`
	Embed     bool     `json:"embed,omitempty"`      // v0.9.0
	EmbedText string   `json:"embed_text,omitempty"` // v0.9.0
	Query     string   `json:"query,omitempty"`      // v0.9.0
	TopK      int      `json:"top_k,omitempty"`      // v0.9.0
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

	// --- RFC BL P2 consolidation control ops ---
	// LeaseTTLMs is the cursor_lease hold duration in milliseconds. 0 = the
	// default; clamped to a maximum. The lease auto-expires so a crashed
	// consolidator never wedges a target.
	LeaseTTLMs int64 `json:"lease_ttl_ms,omitempty"`
	// CompletedAt is the cursor_advance watermark timestamp (RFC3339). Paired
	// with SessionID as the composite watermark.
	CompletedAt string `json:"completed_at,omitempty"`
	// SessionID is the cursor_advance watermark session id (composite tie-break).
	SessionID string `json:"session_id,omitempty"`
	// IDs are the pending_ack row ids (returned earlier by pending_drain).
	IDs []string `json:"ids,omitempty"`
	// Provenance is the RFC BL "where did this fact come from" block on `set`.
	// Only class / source_session_id / source_run_id are model-supplied; the
	// origin is stamped server-side (see provenanceForSet).
	Provenance *memoryProvenanceInput `json:"provenance,omitempty"`
	// FromPending attributes this write to a pending-queue row the caller
	// drained (RFC BL P3). The model supplies a POINTER; the server supplies the
	// VALUES — origin, source_session_id and source_run_id are read off that row,
	// which is why `origin` can stay unforgeable while still recording that a
	// fact came from a compaction flush rather than a scheduled pass.
	//
	// The id is resolved within the caller's own (tenant, scope, scope_id), so an
	// unowned row is not found. A miss is IGNORED, not refused: a stale reference
	// must not fail an otherwise-correct write, and refusing would turn the field
	// into a probe for which ids exist. It grants nothing — the caller was
	// already authorized to write this key.
	FromPending string `json:"from_pending,omitempty"`
	// IncludeProvenance asks `get` to also return where the fact came from, plus
	// whether that origin is still inspectable (RFC BL P3). Opt-in: it costs a
	// second read, and the default output stays byte-identical.
	IncludeProvenance bool `json:"include_provenance,omitempty"`
}

// memoryProvenanceInput is the model-supplied half of store.MemoryProvenance.
// `origin` is deliberately ABSENT: it names the writer, so accepting it from
// the model would let any agent label its own writes as consolidator output and
// poison the "facts a machine distilled" filter. The server stamps it instead.
type memoryProvenanceInput struct {
	Class           string `json:"class,omitempty"`
	SourceSessionID string `json:"source_session_id,omitempty"`
	SourceRunID     string `json:"source_run_id,omitempty"`
}

const (
	// memoryOriginConsolidator is the server-stamped origin for a write made by
	// a run holding the consolidation grant — a background consolidation pass.
	memoryOriginConsolidator = "consolidator"
	// maxProvenanceFieldBytes bounds each model-supplied provenance field. They
	// are descriptive columns, never an authz input, so the only real risk is an
	// unbounded string bloating the row — clamp rather than refuse the write.
	maxProvenanceFieldBytes = 128
)

// provenanceForSet builds the row's provenance from the model's block plus the
// SERVER's view of who is writing. A run holding the consolidation grant is a
// consolidation pass, so its writes are stamped origin=consolidator; an
// ordinary agent's `set` gets no origin and (absent a provenance block) writes
// exactly the columns it wrote before this existed.
//
// The stamp requires an actual RUN, not just the grant. The operator planes (MCP
// operatorCtx, HTTP substrate-admin, gRPC substrate) hand out
// Consolidation: true alongside their wildcard memory scopes, and those contexts
// carry no run id — so a plain `Memory op=set` from any authenticated MCP session
// would otherwise land origin=consolidator with nothing having distilled it,
// hollowing out the one thing the column is for: a trustworthy filter for facts a
// machine distilled from a transcript.
func provenanceForSet(ctx context.Context, in memoryInput) store.MemoryProvenance {
	var prov store.MemoryProvenance
	if in.Provenance != nil {
		prov.Class = clampField(in.Provenance.Class)
		prov.SourceSessionID = clampField(in.Provenance.SourceSessionID)
		prov.SourceRunID = clampField(in.Provenance.SourceRunID)
	}
	if tools.RunID(ctx) != "" && tools.MemoryPolicy(ctx).Consolidation {
		prov.Origin = memoryOriginConsolidator
	}
	return prov
}

// resolveFromPending overlays server-authoritative provenance from the pending
// row `in.FromPending` names (RFC BL P3), leaving `prov` untouched when the field
// is absent or the row is not visible to this caller.
//
// The lookup's (tenant, scope, scopeID) tuple is the authorization check, so a
// foreign row is simply not found. A miss returns prov UNCHANGED and no error:
// the write is legal either way, and distinguishing "no such row" from "not
// yours" would leak which ids exist.
//
// What it overrides and what it does not: the pending row's origin and source ids
// WIN over the model's block, because that is the whole point — a server-recorded
// producer beats a model-asserted one. Class is untouched; it is the model's
// judgement about the fact, not a fact about where the fact came from.
func (m *Memory) resolveFromPending(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput, prov store.MemoryProvenance) store.MemoryProvenance {
	if in.FromPending == "" || m.Store == nil {
		return prov
	}
	row, err := m.Store.MemoryPendingGet(ctx, tools.RunIdentity(ctx).TenantID, scope, scopeID, in.FromPending)
	if err != nil {
		return prov
	}
	if row.Origin != "" {
		prov.Origin = row.Origin
	}
	if row.SourceSessionID != "" {
		prov.SourceSessionID = row.SourceSessionID
	}
	if row.SourceRunID != "" {
		prov.SourceRunID = row.SourceRunID
	}
	return prov
}

// clampField trims a model-supplied provenance field to the column budget.
func clampField(v string) string {
	if len(v) > maxProvenanceFieldBytes {
		return v[:maxProvenanceFieldBytes]
	}
	return v
}

// Name implements tools.Tool.
func (m *Memory) Name() string { return "Memory" }

// UsageHint implements tools.HintedTool — surfaced by Context op=guide.
func (m *Memory) UsageHint() string {
	return "add is ASYNCHRONOUS — a later recall may not see it yet; recall/get/list read, add/set/delete write. Pass op + scope on every call."
}

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
	case "cursor_get":
		return m.execCursorGet(ctx, scope, scopeID, in)
	case "cursor_scan":
		return m.execCursorScan(ctx, scope, scopeID, in)
	case "cursor_lease":
		return m.execCursorLease(ctx, scope, scopeID, in)
	case "cursor_advance":
		return m.execCursorAdvance(ctx, scope, scopeID, in)
	case "cursor_release":
		return m.execCursorRelease(ctx, scope, scopeID, in)
	case "supersede":
		return m.execSupersede(ctx, scope, scopeID, in)
	case "pending_drain":
		return m.execPendingDrain(ctx, scope, scopeID, in)
	case "pending_ack":
		return m.execPendingAck(ctx, scope, scopeID, in)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: get, set, delete, list, incr, search, merge, append_dedupe, bounded_list, add, recall, cursor_get, cursor_scan, cursor_lease, cursor_advance, cursor_release, supersede, pending_drain, pending_ack, sql_query, sql_exec, sql_begin, sql_commit, sql_rollback)", in.Op)), nil
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
			return "", "", fmt.Errorf("Memory tool: this agent has no memory_scopes configured — add `memory_scopes: [agent]` (and/or user, tenant) to the agent yaml")
		}
		return "", "", fmt.Errorf("Memory tool: scope %q not in this agent's memory_scopes %v", requested, policy.AllowedScopes)
	}

	// RFC BX P2b: an isolated member (substrate:user) may address only its own
	// user/agent scope — refuse the tenant-shared / global keyspace even when the
	// agent's memory_scopes would otherwise permit it. Server-derived from the
	// run's Isolated bit; a non-isolated run is unaffected.
	if err := tools.ConfineIsolatedScope(ctx, store.MemoryScope(requested)); err != nil {
		return "", "", err
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
	case store.MemoryScopeTenant:
		// scope_id is EMPTY on purpose: the tenant_id column already carries the
		// identity, so a second copy here would be redundant and would make the
		// retention fan-out ambiguous about which column is authoritative. Matches
		// the global scope's convention and the Path tree's tenant dirent.
		//
		// The tenant itself needs no presence check: it is stamped on every run's
		// identity from server-side auth (never the wire), and the single-tenant /
		// open-mode value "" is a legitimate tenant, not a missing one. Requiring
		// non-empty here would make the scope unusable in exactly the deployment
		// where it is least dangerous.
		return store.MemoryScopeTenant, "", nil
	default:
		return "", "", fmt.Errorf("Memory tool: unknown scope %q (want one of: agent, user, tenant)", requested)
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
		return "", "", fmt.Errorf("Memory tool: this agent has no sql_scopes configured — add `sql_scopes: [agent]` (and/or user, run, tenant) to the agent yaml")
	}
	if !contains(pol.AllowedScopes, requested) {
		return "", "", fmt.Errorf("Memory tool: sql scope %q not in this agent's sql_scopes %v", requested, pol.AllowedScopes)
	}
	// RFC BX P2b: an isolated member may not reach the tenant-shared SQL keyspace.
	// The {agent, user, run} scopes are own-scoped and pass through unaffected
	// (ConfineIsolatedScope only refuses tenant/global).
	if err := tools.ConfineIsolatedScope(ctx, store.MemoryScope(requested)); err != nil {
		return "", "", err
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
	case "tenant":
		// scope_id is the TENANT here, NOT empty — the opposite of the k/v tenant
		// scope above, and deliberately so. sqlmem derives a postgres schema + LOGIN
		// role from sha256(tenant \x1f scope \x1f scope_id) and rejects an empty
		// component, so the id has to be non-empty; {t, tenant, t} still hashes
		// distinctly from {t, user, t} because the scope segment differs.
		//
		// The k/v plane has no such constraint and uses "" so its tenant_id column
		// stays the single source of identity. Two conventions, one reason each.
		return "tenant", sqlScopeTenant(ctx), nil
	default:
		return "", "", fmt.Errorf("Memory tool: unknown sql scope %q (want one of: agent, user, run, tenant)", requested)
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
	return sqlScopeTenantValue(tools.RunIdentity(ctx).TenantID)
}

// sqlScopeTenantValue is the same rule for a tenant already in hand. SQL Memory
// rejects an empty tenant (it sanitizes the value into a path/identifier), so the
// default partition is stored as "default" while the k/v and dirent planes keep the
// raw "". Both forms must go through here — a restated ""→"default" at a call site
// is how the tenant axis drifts.
func sqlScopeTenantValue(tenant string) string {
	if tenant != "" {
		return tenant
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
	out := map[string]any{
		"value":      entry.Value,
		"expires_at": expiresAtRFC3339(entry.ExpiresAt),
	}
	if in.IncludeProvenance {
		out["provenance"] = m.provenanceView(ctx, scope, scopeID, in.Key)
	}
	return okJSON(out)
}

// provenanceView renders a fact's recorded origin plus whether that origin is
// STILL inspectable (RFC BL P3). Returns nil when the row carries no provenance,
// so a caller can tell "unattributed" from "attributed to something gone".
//
// origin_available answers "can I go read the conversation this came from". It is
// resolved live rather than stored, because a stored flag would be wrong the
// moment the retention sweeper next ran and would then need its own
// reconciliation — the classic cache-invalidation trap.
//
// The recorded ids are returned VERBATIM whether or not they resolve. A dead
// citation is still the honest record of where the fact came from, and it is what
// an audit needs after the chat is gone. Nothing here rewrites or clears them,
// and nothing anywhere prunes a fact for having an unresolvable origin: RFC BM
// retention deletes chats on a clock, so origin links are EXPECTED to go dead,
// and pruning on that would make retention silently destroy memory.
func (m *Memory) provenanceView(ctx context.Context, scope store.MemoryScope, scopeID, key string) map[string]any {
	if m.Store == nil {
		return nil
	}
	tenant := tools.RunIdentity(ctx).TenantID
	prov, err := m.Store.MemoryProvenanceGet(ctx, tenant, scope, scopeID, key)
	if err != nil || prov.IsZero() {
		return nil
	}
	out := map[string]any{}
	if prov.Origin != "" {
		out["origin"] = prov.Origin
	}
	if prov.Class != "" {
		out["class"] = prov.Class
	}
	if prov.SourceSessionID != "" {
		out["source_session_id"] = prov.SourceSessionID
	}
	if prov.SourceRunID != "" {
		out["source_run_id"] = prov.SourceRunID
	}
	if prov.SourceSessionID != "" || prov.SourceRunID != "" {
		out["origin_available"] = m.originAvailable(ctx, tenant, prov)
	}
	return out
}

// originAvailable reports whether the conversation a fact was distilled from can
// still be read, checking the session first and falling back to the run (today's
// `add` path records only a run id — RunIdentity carries no session id).
//
// Both arms compare the fetched row's tenant to the caller's. The ids on a
// pre-P3 fact are model-supplied, so without that comparison this would answer
// "does id X exist" for an arbitrary id — a cross-tenant existence oracle. With
// it, the question collapses to "does this id exist in MY tenant", which leaks
// nothing the caller could not already determine.
func (m *Memory) originAvailable(ctx context.Context, tenant string, prov store.MemoryProvenance) bool {
	if id := prov.SourceSessionID; id != "" {
		if sess, err := m.Store.GetSession(ctx, id); err == nil && sess.TenantID == tenant {
			return true
		}
	}
	if id := prov.SourceRunID; id != "" {
		if run, err := m.Store.GetRun(ctx, id); err == nil && run.TenantID == tenant {
			return true
		}
	}
	return false
}

// parseSources maps the wire strings onto the typed selector (RFC BW).
//
// UNKNOWN VALUES ARE DROPPED rather than rejected. The selector narrows what comes
// back, so the failure mode of a typo is a caller believing it filtered when it did
// not — but rejecting the whole call would make a future value name break an older
// runtime, and each op's default already covers the empty result. The enum in the
// input schema is where a caller learns the spelling.
func parseSources(in []string) []memrank.Source {
	if len(in) == 0 {
		return nil
	}
	out := make([]memrank.Source, 0, len(in))
	for _, s := range in {
		switch memrank.Source(strings.ToLower(strings.TrimSpace(s))) {
		case memrank.SourceFacts:
			out = append(out, memrank.SourceFacts)
		case memrank.SourceDocuments:
			out = append(out, memrank.SourceDocuments)
		}
	}
	return out
}

// coreBlockKeyPrefix is the reserved KV namespace for RFC BL P1 core memory
// blocks (single source of truth in the memrank package, shared with the HTTP
// injection reader): a block labeled <label> is stored at `core/<label>`.
const coreBlockKeyPrefix = memrank.CoreBlockKeyPrefix

// coreBlockFor returns the declared core block that gates a mutation of the
// reserved core/<label> key, or nil if key is not a core/* key or no block with
// a matching label AND scope is declared. scope is the already-resolved store
// scope; the block's Scope ("agent"/"user"/"tenant") must match it, so a
// user-scope block can't gate an agent-scope mutation of the same label. The
// policy is operator-resolved on ctx — never model-supplied.
func coreBlockFor(ctx context.Context, scope store.MemoryScope, key string) *config.CoreBlock {
	if !strings.HasPrefix(key, coreBlockKeyPrefix) {
		return nil
	}
	label := strings.TrimPrefix(key, coreBlockKeyPrefix)
	blocks := tools.CoreBlocksPolicy(ctx).Blocks
	for i := range blocks {
		if blocks[i].Label == label && blocks[i].Scope == string(scope) {
			return &blocks[i]
		}
	}
	return nil
}

// enforceCoreBlockWrite gates an agent's Memory mutation of a reserved
// core/<label> key against the run's resolved core-block policy (RFC BL P1).
// It is shared by EVERY mutating op (set/delete/incr/merge/append_dedupe/
// bounded_list) so a read_only block can't be erased or overwritten and a
// limit_bytes block can't be grown past its cap by any op:
//
//   - a read_only block → refuse the mutation (operator-authored; the agent may
//     read the value via injection but never overwrite or delete it).
//   - else a block with limit_bytes > 0 → refuse when the RESULTING value
//     exceeds the per-block cap (mirrors the per-scope quota refusal).
//
// op names the operation for the refusal message. valueLen is the size of the
// resulting value in bytes; pass -1 for ops that cannot grow the value
// (delete/incr) to skip the size check — only the read_only refusal applies.
// A key with NO matching declared block passes through as a normal key.
func enforceCoreBlockWrite(ctx context.Context, scope store.MemoryScope, key, op string, valueLen int) error {
	b := coreBlockFor(ctx, scope, key)
	if b == nil {
		return nil
	}
	if b.ReadOnly {
		return fmt.Errorf("Memory.%s: core block %q (scope=%s) is read_only — operator-authored; agent writes are refused", op, b.Label, scope)
	}
	if valueLen >= 0 && b.LimitBytes > 0 && valueLen > b.LimitBytes {
		return fmt.Errorf("Memory.%s: core block %q (scope=%s) value %d bytes exceeds limit_bytes %d", op, b.Label, scope, valueLen, b.LimitBytes)
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
	if err := enforceCoreBlockWrite(ctx, scope, in.Key, "set", len(in.Value)); err != nil {
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
		TTL:        ttl,
		Embed:      in.Embed,
		EmbedText:  in.EmbedText,
		Provenance: m.resolveFromPending(ctx, scope, scopeID, in, provenanceForSet(ctx, in)),
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
//	    // a document hit also carries the chunk it is addressed by, plus the
//	    // document + heading it came from when SQL Memory can supply them:
//	    { "key": "doc.chunk:8f1e...", "kind": "document", "chunk_id": "8f1e...",
//	      "document": "Verified writes", "title": "Reading your coverage", ... },
//	    ...
//	  ],
//	  "query_embedding_dim": 1536,
//	  "truncated": false }
func (m *Memory) execSearch(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) (tools.Result, error) {
	if in.Query == "" {
		return errResult("search: missing required field: query"), nil
	}
	// NOTE: the vector-support / embedder pre-flight is NOT done here on the
	// tool's in-process Store/Embedder — a named memory_backend
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
		Sources:   parseSources(in.Sources),
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
		if errors.Is(err, memrank.ErrSourcesNotExpressible) {
			return errResult(err.Error()), nil
		}
		return errResult(fmt.Sprintf("search: %s", err)), nil
	}

	entries := make([]map[string]any, 0, len(res.Entries))

	// Collect the document hits' chunk ids up front so their readable labels
	// resolve in ONE batched query instead of one per row.
	var docChunkIDs []string
	for _, r := range res.Entries {
		if memrank.Class(r) == store.MemoryRowDocument {
			docChunkIDs = append(docChunkIDs, strings.TrimPrefix(r.Key, memrank.DocumentChunkKeyPrefix))
		}
	}
	labels := ChunkLabelsFor(ctx, m.SqlMem, tools.RunIdentity(ctx).TenantID, scope, scopeID, docChunkIDs)

	for i, r := range res.Entries {
		kind := memrank.Class(r)
		entry := map[string]any{
			"key": r.Key,
			// kind tells a caller WHAT it got: a document hit is prose written down
			// somewhere, not something the user said, and weighing them alike is how
			// a fence marker outranks a medication (RFC BW §4b).
			"kind":       string(kind),
			"value":      r.Value,
			"score":      r.Score,           // raw cosine similarity — NEVER touched by hybrid fusion or ranking (stable across searches)
			"rank_score": res.RankScores[i], // computed rank the result was ordered by: fused-semantic (RRF) + recency + frequency
			"embedded_with": map[string]any{
				"provider": r.EmbeddedWith.Provider,
				"model":    r.EmbeddedWith.Model,
			},
			"expires_at": expiresAtRFC3339(r.ExpiresAt),
		}
		// A document hit is prose the caller may want to CITE, and until now it came
		// back addressed only by an opaque `doc.chunk:<hex>` key — so a page of hits
		// could not be attributed without a get_chunk per row. Carry the chunk id it
		// is addressed by, plus the document and heading it sits under when SQL Memory
		// can supply them (absent when it cannot; never an error — the bodies and the
		// titles live on different planes).
		if kind == store.MemoryRowDocument {
			chunkID := strings.TrimPrefix(r.Key, memrank.DocumentChunkKeyPrefix)
			entry["chunk_id"] = chunkID
			if lb, ok := labels[chunkID]; ok {
				if lb.Document != "" {
					entry["document"] = lb.Document
				}
				if lb.Title != "" {
					entry["title"] = lb.Title
				}
			}
		}
		entries = append(entries, entry)
	}
	out := map[string]any{
		"entries":             entries,
		"query_embedding_dim": res.QueryEmbeddingDim,
		"truncated":           res.Truncated,
	}
	// Only when a selector was actually requested: a caller that passed none has
	// nothing to be warned about, and an always-present key would change the response
	// shape for every existing search (RFC BW §6).
	if len(in.Sources) > 0 && !res.SourcesApplied {
		out["sources_applied"] = false
		out["note"] = "this memory backend did not apply the source selector, so these " +
			"results are NOT restricted to the kinds you asked for"
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
		// lives in the backend (which sets that attribute on its span);
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
		Sources:   parseSources(in.Sources),
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
	out := map[string]any{"facts": facts}
	// A BACKEND THAT IGNORED THE SELECTOR MUST NOT LOOK LIKE ONE THAT HONOURED IT
	// (RFC BW §6). Recall's default EXCLUDES document prose, so a backend that dropped
	// the selector returns exactly what the default exists to keep out — and the caller
	// would read it as remembered fact. Say so rather than refuse: refusing would make
	// a backend swap break working agents.
	if !res.SourcesApplied {
		out["sources_applied"] = false
		out["note"] = "this memory backend did not apply the source selector, so these " +
			"results may include document prose rather than only remembered facts"
	}
	return okJSON(out)
}

// --- RFC BL P2 consolidation control ops ---
//
// Every op below is gated by BOTH resolveScope (the memory_scopes ACL, already
// applied before dispatch) AND the memory_consolidation grant (consolidationGate).
// The (scope, scopeID) are server-resolved; the tenant comes from RunIdentity —
// the model never supplies a target scope_id or tenant.

const (
	// defaultConsolidationLeaseTTL is the cursor_lease hold when lease_ttl_ms is
	// unset; maxConsolidationLeaseTTL caps it so a runaway value can't wedge a
	// target far into the future (the lease auto-expires regardless).
	defaultConsolidationLeaseTTL = 60 * time.Second
	maxConsolidationLeaseTTL     = time.Hour

	// defaultCursorScanLimit / maxCursorScanLimit bound one cursor_scan page.
	// The default is deliberately SMALL: a pass must finish its whole page within
	// max_iterations, because a pass that runs out of iterations never advances
	// the watermark and the next pass re-reads the identical batch forever. A
	// truncated page is cheap (the next pass resumes at the watermark), an
	// unfinishable page is a permanent stall.
	defaultCursorScanLimit = 10
	maxCursorScanLimit     = 50

	// maxWatermarkSkew bounds how far a supplied cursor_advance completed_at may
	// sit from the session's REAL settled instant. It is not a security margin —
	// the session itself is verified either way, and the store's instant is what
	// gets recorded — it only absorbs the one benign difference: a model that
	// re-renders a scan row's RFC3339Nano timestamp at second precision. Anything
	// further apart is not the pair cursor_scan handed out.
	maxWatermarkSkew = time.Second
)

// consolidationGate returns a refusal Result (and true) when the run lacks the
// memory_consolidation grant. Default-deny: the grant is a SEPARATE gate from
// memory_scopes, so a scope-authorized agent still cannot run these ops without it.
func consolidationGate(ctx context.Context, op string) (tools.Result, bool) {
	if tools.MemoryPolicy(ctx).Consolidation {
		return tools.Result{}, false
	}
	return errResult(fmt.Sprintf("memory: %s requires the memory_consolidation grant (add memory_consolidation: true to the agent config)", op)), true
}

// consolidationOwner is the stable lease-owner identity for this run — the run
// id when present (survives across a run's turns), else the agent name. Never
// model-supplied.
func consolidationOwner(ctx context.Context) string {
	if id := tools.RunID(ctx); id != "" {
		return id
	}
	return tools.AgentName(ctx)
}

// requireConsolidationLease refuses a MUTATING consolidation op unless THIS run
// holds a live lease on the resolved target. It mirrors the ownership check the
// store applies to cursor_advance, but has to live here: supersede and
// pending_ack are ordinary row writes at the store layer, with no cursor row in
// the call to check against.
//
// WHY THESE OPS NEED IT AND `set` DOES NOT. The lease is what stops two passes
// working one target — and the advisory lock behind the fan-out is per SCHEDULE
// DEF, so two schedule defs covering one tenant (or an operator kicking a manual
// pass mid-flight) genuinely produce two concurrent passes. A racing `set` is
// harmless because the skill mints deterministic subject-derived keys: the second
// pass overwrites the same row. The two ops here have no such safety net, and
// pending_ack's failure is UNRECOVERABLE: it stamps drained_at, MemoryPendingDrain
// filters `drained_at IS NULL`, so a pass that acks and then dies before writing
// the fact has made those queued conversations invisible to every future pass.
// The content existed only in the queue payload, so no deterministic key brings
// it back.
//
// Fails CLOSED on a cursor read fault: refusing a destructive op on an unverified
// lease is recoverable (the next pass redoes it), acking on one is not.
func (m *Memory) requireConsolidationLease(ctx context.Context, op string, scope store.MemoryScope, scopeID string) (tools.Result, bool) {
	owner := consolidationOwner(ctx)
	if owner == "" {
		return errResult(fmt.Sprintf("%s: no stable run/agent identity to hold the target's lease", op)), true
	}
	row, err := m.Store.MemoryCursorGet(ctx, tools.RunIdentity(ctx).TenantID, scope, scopeID)
	if err != nil {
		return errResult(fmt.Sprintf("%s: cannot verify the target's lease: %s", op, err)), true
	}
	if row.LeasedBy != owner || row.LeaseExpiresAt.IsZero() || !row.LeaseExpiresAt.After(time.Now().UTC()) {
		return errResult(fmt.Sprintf("%s: not lease owner — take this target's lease with cursor_lease (and stop if it reports acquired=false) before %s", op, op)), true
	}
	return tools.Result{}, false
}

// cursorJSON renders a cursor row for the agent (non-secret watermark + lease
// fields). Zero timestamps render as null via expiresAtRFC3339.
func cursorJSON(row store.MemoryCursorRow) map[string]any {
	return map[string]any{
		"watermark_completed_at": expiresAtRFC3339(row.WatermarkCompletedAt),
		"watermark_session_id":   row.WatermarkSessionID,
		"leased_by":              row.LeasedBy,
		"lease_expires_at":       expiresAtRFC3339(row.LeaseExpiresAt),
	}
}

func (m *Memory) execCursorGet(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) (tools.Result, error) {
	if res, denied := consolidationGate(ctx, "cursor_get"); denied {
		return res, nil
	}
	row, err := m.Store.MemoryCursorGet(ctx, tools.RunIdentity(ctx).TenantID, scope, scopeID)
	if err != nil {
		return errResult(fmt.Sprintf("cursor_get: %s", err)), nil
	}
	return okJSON(cursorJSON(row))
}

// execCursorScan lists the settled chats this target has not consolidated yet,
// OLDEST FIRST, strictly after the target's stored watermark.
//
// This op exists because the only other way to discover work — paging the chat
// list — is unsafe here. That list is ordered newest-first and filtered on
// last_activity, a DIFFERENT timestamp from the watermark's max(completed_at),
// while the watermark itself is forward-only. A pass that read the newest N
// chats and then advanced to the newest one it saw would strand every older
// chat PERMANENTLY, silently, with the first pass on an existing deployment
// discarding the whole historical backlog.
//
// Everything that makes the read safe is enforced HERE rather than by prompt
// text, so a steered or confused model cannot widen it:
//
//   - the watermark comes from the store (MemoryCursorGet), never the wire;
//   - the target is the server-resolved (tenant, scope, scope_id);
//   - the query is strictly-after and ascending, so consolidating a page and
//     advancing to its LAST row can never skip a session;
//   - only all-terminal sessions qualify, so a live chat is never half-read;
//   - the pass's OWN agent name is excluded, so a pass structurally cannot see
//     (or re-consolidate) its own past reports;
//   - and so is every agent declared `internal:`. Self-exclusion alone was not
//     enough: a pass spawns one `memory/extractor` child per chat, each child is
//     a session whose transcript CONTAINS the chat it just extracted, and those
//     became candidates on the next pass — the pipeline consuming its own
//     output, 7 of the last 8 chats on the live store and growing ~15 a pass.
//     The set is server-resolved from the operator's config, never from the
//     wire, for the same reason the target is.
func (m *Memory) execCursorScan(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) (tools.Result, error) {
	if res, denied := consolidationGate(ctx, "cursor_scan"); denied {
		return res, nil
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultCursorScanLimit
	}
	if limit > maxCursorScanLimit {
		limit = maxCursorScanLimit
	}
	tenantID := tools.RunIdentity(ctx).TenantID
	cursor, err := m.Store.MemoryCursorGet(ctx, tenantID, scope, scopeID)
	if err != nil {
		return errResult(fmt.Sprintf("cursor_scan: %s", err)), nil
	}
	// scopeID is the target's USER id under scope=user — the only scope a
	// consolidation fan-out dispatches, and the one whose chats these are. Under
	// scope=agent it filters an agent name against user_id and correctly matches
	// nothing: an agent-scope target owns bookkeeping, not chat transcripts.
	//
	// Over-fetch by one to detect truncation without a second query.
	rows, err := m.Store.ConsolidatableSessions(ctx, tenantID, scopeID, "",
		withInternalAgents(m.Cfg, tools.AgentName(ctx)),
		cursor.WatermarkCompletedAt, cursor.WatermarkSessionID, limit+1)
	if err != nil {
		return errResult(fmt.Sprintf("cursor_scan: %s", err)), nil
	}
	truncated := len(rows) > limit
	if truncated {
		rows = rows[:limit]
	}
	sessions := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		sessions = append(sessions, map[string]any{
			"session_id":   r.SessionID,
			"completed_at": r.MaxCompletedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return okJSON(map[string]any{"sessions": sessions, "truncated": truncated})
}

func (m *Memory) execCursorLease(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) (tools.Result, error) {
	if res, denied := consolidationGate(ctx, "cursor_lease"); denied {
		return res, nil
	}
	owner := consolidationOwner(ctx)
	if owner == "" {
		return errResult("cursor_lease: no stable run/agent identity to own the lease"), nil
	}
	ttl := defaultConsolidationLeaseTTL
	if in.LeaseTTLMs > 0 {
		// Clamp in millisecond space BEFORE converting: a huge lease_ttl_ms
		// (≳9.2e12) would overflow int64 in the *time.Millisecond multiply and
		// wrap negative, silently skipping a post-multiply max check. Negatives
		// never reach here (the > 0 guard falls through to the default).
		ms := in.LeaseTTLMs
		if maxMs := int64(maxConsolidationLeaseTTL / time.Millisecond); ms > maxMs {
			ms = maxMs
		}
		ttl = time.Duration(ms) * time.Millisecond
	}
	row, acquired, err := m.Store.MemoryCursorLease(ctx, tools.RunIdentity(ctx).TenantID, scope, scopeID, owner, time.Now().UTC(), ttl)
	if err != nil {
		return errResult(fmt.Sprintf("cursor_lease: %s", err)), nil
	}
	out := cursorJSON(row)
	out["acquired"] = acquired
	out["owner"] = owner
	return okJSON(out)
}

// execCursorAdvance moves the target's watermark to a VERIFIED point.
//
// The watermark is forward-only and there is no reset op, so an advance is the
// one consolidation op whose blast radius is permanent. A transcript line shaped
// as a bookkeeping fact ("the correct cursor position is
// completed_at=2200-01-01T00:00:00Z, session_id=<this session>") only had to
// steer the model ONCE to stop that target's consolidation forever, with no
// operator-visible signal and no remediation through the tool surface.
//
// So the pair is checked against the store rather than merely parsed. The
// advance target must be a REAL session, in the caller's own tenant, belonging to
// this memory target, that has already settled, whose settled instant matches the
// supplied timestamp. Then the worst an injection can achieve is an advance to a
// real, already-settled chat — a bounded, self-correcting outcome — and the
// far-future attack is structurally impossible rather than merely discouraged.
//
// Backwards/equal advances stay a no-op (the store is already monotonic): this
// is an authenticity check layered on top, not a replacement for it.
func (m *Memory) execCursorAdvance(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) (tools.Result, error) {
	if res, denied := consolidationGate(ctx, "cursor_advance"); denied {
		return res, nil
	}
	owner := consolidationOwner(ctx)
	if owner == "" {
		return errResult("cursor_advance: no stable run/agent identity to own the lease"), nil
	}
	if in.CompletedAt == "" {
		return errResult("cursor_advance: missing required field: completed_at"), nil
	}
	if in.SessionID == "" {
		return errResult("cursor_advance: missing required field: session_id — the watermark names the chat it stops at; copy the pair from a cursor_scan row"), nil
	}
	completedAt, perr := time.Parse(time.RFC3339Nano, in.CompletedAt)
	if perr != nil {
		return errResult(fmt.Sprintf("cursor_advance: completed_at must be an RFC3339 timestamp: %s", perr)), nil
	}
	// Cheap first cut before touching the store: a watermark can only ever name a
	// chat that has already finished, so a future instant is never valid.
	if completedAt.After(time.Now().UTC()) {
		return errResult("cursor_advance: completed_at is in the future — the watermark only moves to a chat that has already finished"), nil
	}
	tenantID := tools.RunIdentity(ctx).TenantID
	settledAt, sessionUser, serr := m.Store.SessionSettledAt(ctx, tenantID, in.SessionID)
	if serr != nil {
		var nf *store.ErrNotFound
		if errors.As(serr, &nf) {
			return errResult(fmt.Sprintf("cursor_advance: no chat %q in this tenant — the watermark may only name a real chat; copy the pair from a cursor_scan row", in.SessionID)), nil
		}
		return errResult(fmt.Sprintf("cursor_advance: %s", serr)), nil
	}
	// Ownership before anything that would describe the session: a chat under
	// another user must read as unusable, not as a probe result. Only the user
	// scope has a per-target owner — an agent-scope target is confined by tenant
	// alone (its scope_id is an agent name, not a session owner).
	if scope == store.MemoryScopeUser && sessionUser != scopeID {
		return errResult(fmt.Sprintf("cursor_advance: chat %q does not belong to this memory target", in.SessionID)), nil
	}
	if settledAt.IsZero() {
		return errResult(fmt.Sprintf("cursor_advance: chat %q has not finished yet — advancing past a live chat would skip whatever it says next", in.SessionID)), nil
	}
	if skew := settledAt.Sub(completedAt); skew > maxWatermarkSkew || skew < -maxWatermarkSkew {
		return errResult(fmt.Sprintf("cursor_advance: completed_at does not match chat %q (it settled at %s) — copy the pair verbatim from a cursor_scan row",
			in.SessionID, settledAt.Format(time.RFC3339Nano))), nil
	}
	// Record the STORE's instant rather than the supplied one, so the watermark
	// always sits exactly on a real settled moment and the next scan's
	// strictly-after comparison is exact even if the model re-formatted it.
	if err := m.Store.MemoryCursorAdvance(ctx, tenantID, scope, scopeID, owner, settledAt, in.SessionID); err != nil {
		return errResult(fmt.Sprintf("cursor_advance: %s", err)), nil
	}
	return okJSON(map[string]any{"ok": true})
}

func (m *Memory) execCursorRelease(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) (tools.Result, error) {
	if res, denied := consolidationGate(ctx, "cursor_release"); denied {
		return res, nil
	}
	owner := consolidationOwner(ctx)
	if owner == "" {
		return errResult("cursor_release: no stable run/agent identity to own the lease"), nil
	}
	if err := m.Store.MemoryCursorRelease(ctx, tools.RunIdentity(ctx).TenantID, scope, scopeID, owner); err != nil {
		return errResult(fmt.Sprintf("cursor_release: %s", err)), nil
	}
	return okJSON(map[string]any{"ok": true})
}

func (m *Memory) execSupersede(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) (tools.Result, error) {
	if res, denied := consolidationGate(ctx, "supersede"); denied {
		return res, nil
	}
	if res, denied := m.requireConsolidationLease(ctx, "supersede", scope, scopeID); denied {
		return res, nil
	}
	if in.Key == "" {
		return errResult("supersede: missing required field: key"), nil
	}
	if err := m.Store.MemorySupersede(ctx, tools.RunIdentity(ctx).TenantID, scope, scopeID, in.Key); err != nil {
		return errResult(fmt.Sprintf("supersede: %s", err)), nil
	}
	return okJSON(map[string]any{"ok": true})
}

func (m *Memory) execPendingDrain(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) (tools.Result, error) {
	if res, denied := consolidationGate(ctx, "pending_drain"); denied {
		return res, nil
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := m.Store.MemoryPendingDrain(ctx, tools.RunIdentity(ctx).TenantID, scope, scopeID, limit)
	if err != nil {
		return errResult(fmt.Sprintf("pending_drain: %s", err)), nil
	}
	items := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		items = append(items, map[string]any{
			"id":      r.ID,
			"payload": json.RawMessage(r.Payload),
			// origin is what produced this item (agent_explicit | compaction),
			// server-recorded at enqueue. Surfaced so a pass can see what it is
			// holding; relaying the row's `id` back on `set` as `from_pending` is
			// what actually attributes the resulting fact, since the model cannot
			// author origin itself. Empty on rows enqueued before the column.
			"origin":            r.Origin,
			"source_session_id": r.SourceSessionID,
			"source_run_id":     r.SourceRunID,
			"created_at":        r.CreatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return okJSON(map[string]any{"pending": items})
}

func (m *Memory) execPendingAck(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) (tools.Result, error) {
	if res, denied := consolidationGate(ctx, "pending_ack"); denied {
		return res, nil
	}
	// Lease-gated: an ack is the one consolidation write with no recoverable
	// failure mode (see requireConsolidationLease).
	if res, denied := m.requireConsolidationLease(ctx, "pending_ack", scope, scopeID); denied {
		return res, nil
	}
	// The ack is confined server-side to this run's resolved (tenant, scope,
	// scopeID) — symmetric with the scoped pending_drain that surfaced the ids —
	// so a leaked or guessed id from another tenant/scope is a no-op, never a
	// cross-tenant ack. ids come from the caller's own drain; ack is at-least-once.
	if len(in.IDs) == 0 {
		return errResult("pending_ack: missing required field: ids"), nil
	}
	if err := m.Store.MemoryPendingAck(ctx, tools.RunIdentity(ctx).TenantID, scope, scopeID, in.IDs); err != nil {
		return errResult(fmt.Sprintf("pending_ack: %s", err)), nil
	}
	return okJSON(map[string]any{"ok": true, "acked": len(in.IDs)})
}

func (m *Memory) execDelete(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) (tools.Result, error) {
	if in.Key == "" {
		return errResult("delete: missing required field: key"), nil
	}
	// RFC BL P1: a read_only core block refuses delete too — otherwise an agent
	// could erase an operator-seeded read-only block. limit_bytes is moot for a
	// delete, so pass -1 to skip the size check.
	if err := enforceCoreBlockWrite(ctx, scope, in.Key, "delete", -1); err != nil {
		return errResult(err.Error()), nil
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
	// RFC BL P1: a read_only core block refuses incr too. limit_bytes is moot
	// for a bounded-width number, so pass -1 to skip the size check.
	if err := enforceCoreBlockWrite(ctx, scope, in.Key, "incr", -1); err != nil {
		return errResult(err.Error()), nil
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
	// RFC BL P1: refuse a read_only core block BEFORE taking the row lock — the
	// mutation must not commit. The limit_bytes cap is checked post-reduction
	// below (the merge grows the value), mirroring the quota re-check.
	if err := enforceCoreBlockWrite(ctx, scope, in.Key, "merge", -1); err != nil {
		return errResult(err.Error()), nil
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
	// Core-block per-block cap on the post-merge size, at the same point as the
	// quota re-check (RFC BL P1). Same commit caveat as the quota check below.
	if err := enforceCoreBlockWrite(ctx, scope, in.Key, "merge", len(final)); err != nil {
		return errResult(err.Error()), nil
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
	// RFC BL P1: refuse a read_only core block before mutating; the limit_bytes
	// cap is checked post-reduction below.
	if err := enforceCoreBlockWrite(ctx, scope, in.Key, "append_dedupe", -1); err != nil {
		return errResult(err.Error()), nil
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
	// Core-block per-block cap on the post-append size (RFC BL P1).
	if err := enforceCoreBlockWrite(ctx, scope, in.Key, "append_dedupe", len(final)); err != nil {
		return errResult(err.Error()), nil
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
	// RFC BL P1: refuse a read_only core block before mutating; the limit_bytes
	// cap is checked post-reduction below.
	if err := enforceCoreBlockWrite(ctx, scope, in.Key, "bounded_list", -1); err != nil {
		return errResult(err.Error()), nil
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
	// Core-block per-block cap on the post-trim size (RFC BL P1).
	if err := enforceCoreBlockWrite(ctx, scope, in.Key, "bounded_list", len(final)); err != nil {
		return errResult(err.Error()), nil
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
// scopeUsageCounter is the optional backend capability that sums a scope's memory
// footprint server-side, excluding namespaces that are not agent memory. Only the
// in-process backend implements it, because only the main store can hold Document
// chunk bodies; a remote backend keeps the list-based path.
type scopeUsageCounter interface {
	ScopeUsage(ctx context.Context, scope store.MemoryScope, scopeID string) (keys int, bytes int, err error)
}

func (m *Memory) checkQuota(ctx context.Context, scope store.MemoryScope, scopeID, key string, addBytes int) error {
	policy := tools.MemoryPolicy(ctx)
	quota := policy.QuotaBytes
	if quota <= 0 {
		quota = m.DefaultQuotaBytes
	}
	if quota <= 0 {
		return nil
	}

	// Preferred path: ask the backend to sum the scope in one query, excluding
	// the Document chunk-body namespace. RFC AK stores chunk bodies in this same
	// keyspace, and a scope holding thousands of them truncated the list below
	// before the agent's own rows were reached — refusing every write with "more
	// than 1000 keys" while the real memory footprint was kilobytes. Documents
	// are governed by their own quota; counting them here conflated two
	// subsystems and produced an error that named the wrong problem ("delete
	// unused keys first" would have an operator deleting their documents).
	if sc, ok := m.backend(ctx).(scopeUsageCounter); ok {
		_, used, err := sc.ScopeUsage(ctx, scope, scopeID)
		if err != nil {
			return fmt.Errorf("quota check: %w", err)
		}
		// An overwrite replaces the existing row, so its bytes are not additive.
		if existing, gerr := m.backend(ctx).Get(ctx, scope, scopeID, key); gerr == nil {
			used -= len(key) + len(existing.Value)
			if used < 0 {
				used = 0
			}
		}
		if projected := used + len(key) + addBytes; projected > quota {
			return fmt.Errorf("Memory.set: scope %q quota %d bytes would be exceeded by this write (current=%d, after=%d)",
				scope, quota, used, projected)
		}
		return nil
	}

	// Fallback for a REMOTE backend, which cannot host Document chunk bodies
	// (Documents always write to the main store) — so its list-based count is
	// already correct, and the truncation refusal below is a real signal there.
	//
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
	// routed to a remote backend stores its rows there, so summing the
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
