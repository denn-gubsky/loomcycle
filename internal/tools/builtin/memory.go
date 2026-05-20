package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

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
}

const memoryDescription = `Persistent key/value storage scoped to this agent or end-user. ` +
	`Survives across runs and sessions. Use for: counters, summaries, voice/preferences, ` +
	`learned facts, notes for your future self. ` +
	`Operations: get, set, delete, list, incr, search. ` +
	`Scope is "agent" (this agent's keyspace, shared across users) or "user" (this end-user's keyspace, shared across agents). ` +
	`Values are JSON. Optional TTL is in seconds. ` +
	`v0.9.0: pass embed=true with embed_text on set to enable semantic search; use op=search with query to find rows by similarity.`

const memoryInputSchema = `{
  "type": "object",
  "properties": {
    "op":         {"type": "string", "enum": ["get","set","delete","list","incr","search"], "description": "Which operation to perform."},
    "scope":      {"type": "string", "enum": ["agent","user"], "description": "Which keyspace: this agent's (cross-run, cross-user) or this user's (cross-agent)."},
    "key":        {"type": "string", "description": "The entry's key. Required for get / set / delete / incr."},
    "value":      {"description": "The JSON value to store. Required for set."},
    "delta":      {"type": "integer", "description": "Increment delta for incr (default 1, may be negative)."},
    "ttl":        {"type": "integer", "description": "Optional time-to-live in seconds. Applies to set and incr; 0 means no expiry."},
    "prefix":     {"type": "string", "description": "Optional key prefix filter for list / search."},
    "limit":      {"type": "integer", "description": "Maximum entries to return for list (default 100)."},
    "embed":      {"type": "boolean", "description": "v0.9.0 set-only: when true, also generates and stores an embedding so this row is reachable via op=search."},
    "embed_text": {"type": "string", "description": "v0.9.0 set-only: the text to embed when embed=true. Defaults to the JSON-stringified value when omitted."},
    "query":      {"type": "string", "description": "v0.9.0 search-only: the text to embed and use as the similarity query."},
    "top_k":      {"type": "integer", "description": "v0.9.0 search-only: max results (default 10, max 50)."}
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
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: get, set, delete, list, incr, search)", in.Op)), nil
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
	entry, err := m.Store.MemoryGet(ctx, scope, scopeID, in.Key)
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
	ttl := time.Duration(in.TTL) * time.Second
	if err := m.Store.MemorySet(ctx, scope, scopeID, in.Key, in.Value, ttl); err != nil {
		return errResult(fmt.Sprintf("set: %s", err)), nil
	}

	// v0.9.0 Vector Memory: when embed=true, generate an embedding
	// alongside the k/v write. Failures here DO NOT roll back the
	// base row — the operator can re-embed later via the admin
	// endpoint. We surface a warning in the response so the model
	// sees the partial-write outcome and can decide whether to
	// retry. Quota math already excluded embedding bytes above.
	resp := map[string]any{"ok": true}
	if in.Embed {
		if err := m.persistEmbedding(ctx, scope, scopeID, in); err != nil {
			// Tool returns ok=true (k/v landed) but embedded=false +
			// warning so the model sees the partial outcome.
			log.Printf("memory.set: embed failed for (scope=%s, key=%s): %v", scope, in.Key, err)
			resp["embedded"] = false
			resp["embed_warning"] = err.Error()
		} else {
			resp["embedded"] = true
		}
	}
	return okJSON(resp)
}

// persistEmbedding embeds the supplied text (or the JSON-stringified
// value when embed_text is empty) and writes the embedding row.
// Returns nil on success; the caller surfaces failures as a
// non-fatal warning rather than refusing the whole set.
func (m *Memory) persistEmbedding(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) error {
	if m.Embedder == nil {
		return errors.New(store.ErrEmbedderNotConfigured.Msg)
	}
	if !m.Store.SupportsVectors() {
		return errors.New(store.ErrVectorUnsupported.Msg)
	}
	text := in.EmbedText
	if text == "" {
		// Fall back to the JSON-stringified value. Useful for
		// agents that store small text snippets directly — they
		// don't have to repeat the text in both `value` and
		// `embed_text`.
		text = string(in.Value)
	}
	vecs, err := m.Embedder.Embed(ctx, []string{text})
	if err != nil {
		return fmt.Errorf("embed: %w", err)
	}
	if len(vecs) != 1 {
		return fmt.Errorf("embed: got %d vectors, want 1", len(vecs))
	}
	emb := store.MemoryEmbedding{
		Provider:  m.Embedder.Provider(),
		Model:     m.Embedder.Model(),
		Dimension: len(vecs[0]),
		Vector:    vecs[0],
		EmbedText: text,
		CreatedAt: time.Now().UTC(),
	}
	return m.Store.MemoryEmbedSet(ctx, scope, scopeID, in.Key, emb)
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
	if !m.Store.SupportsVectors() {
		return errResult(store.ErrVectorUnsupported.Msg), nil
	}
	if m.Embedder == nil {
		return errResult(store.ErrEmbedderNotConfigured.Msg), nil
	}
	if in.Query == "" {
		return errResult("search: missing required field: query"), nil
	}
	topK := in.TopK
	if topK <= 0 {
		topK = 10
	}
	if topK > 50 {
		topK = 50
	}

	// Embed the query text. Failures here are the embedder's
	// problem — surface them directly so operators see exactly
	// what went wrong.
	vecs, err := m.Embedder.Embed(ctx, []string{in.Query})
	if err != nil {
		return errResult(fmt.Sprintf("search: embed query: %s", err)), nil
	}
	if len(vecs) != 1 {
		return errResult(fmt.Sprintf("search: embed query: got %d vectors, want 1", len(vecs))), nil
	}
	queryVec := vecs[0]

	results, err := m.Store.MemoryEmbedSearch(ctx, scope, scopeID, in.Prefix, queryVec, topK)
	if err != nil {
		// ErrDimensionMismatch is the user-actionable one — operators
		// swap embedder models and forget to re-embed. Surface it as a
		// clear refusal with the admin-endpoint migration hint.
		if errors.Is(err, store.ErrDimensionMismatch) || errors.Is(err, store.ErrVectorUnsupported) {
			return errResult(err.Error()), nil
		}
		// Also catch the typed-Code form; the dimension-mismatch
		// helper in the postgres adapter wraps the message with the
		// concrete dims so we need to recognise it by Code, not by
		// exact value comparison.
		var me *store.MemoryError
		if errors.As(err, &me) && me.Code == store.ErrDimensionMismatch.Code {
			return errResult(err.Error()), nil
		}
		return errResult(fmt.Sprintf("search: %s", err)), nil
	}

	entries := make([]map[string]any, 0, len(results))
	for _, r := range results {
		entries = append(entries, map[string]any{
			"key":   r.Key,
			"value": r.Value,
			"score": r.Score,
			"embedded_with": map[string]any{
				"provider": r.EmbeddedWith.Provider,
				"model":    r.EmbeddedWith.Model,
			},
			"expires_at": expiresAtRFC3339(r.ExpiresAt),
		})
	}
	return okJSON(map[string]any{
		"entries":             entries,
		"query_embedding_dim": len(queryVec),
		"truncated":           len(entries) == topK, // soft-truncation hint; reader can re-search with higher top_k
	})
}

func (m *Memory) execDelete(ctx context.Context, scope store.MemoryScope, scopeID string, in memoryInput) (tools.Result, error) {
	if in.Key == "" {
		return errResult("delete: missing required field: key"), nil
	}
	deleted, err := m.Store.MemoryDelete(ctx, scope, scopeID, in.Key)
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
	entries, truncated, err := m.Store.MemoryList(ctx, scope, scopeID, in.Prefix, limit)
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
	entries, truncated, err := m.Store.MemoryList(ctx, scope, scopeID, "", listCap)
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
