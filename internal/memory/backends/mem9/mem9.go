// Package mem9 is the first non-default memory.Backend (RFC I MR-4): a
// REST client that maps loomcycle's six-op Backend surface onto the
// Mem9 product (github.com/mem9-ai/mem9), an Apache-2.0 Go memory server
// with an X-API-Key-authenticated REST API.
//
// ⚠ WIRE-SHAPE HONESTY ⚠
//
// Mem9 is an EXTERNAL product. The RFC names exactly ONE concrete
// endpoint: POST {base_url}/{api_version}/search with X-API-Key auth.
// Every OTHER endpoint and every request/response JSON shape in this
// file is an ASSUMED CONTRACT — a best-effort reconstruction of a
// plausible v1alpha2 REST surface. They are NOT verified against the
// real Mem9 API.
//
// To keep the unverified surface correctable in one place, ALL wire
// details (paths + request/response structs + the response→domain
// mapping) live in the clearly-marked block at the top of this file,
// each tagged:
//
//	// ASSUMED Mem9 wire shape — verify against the real
//	// github.com/mem9-ai/mem9 v1alpha2 API before production.
//
// The CI test exercises this backend against an httptest.Server stub
// that implements THESE assumed shapes — so the tests prove the
// loomcycle-side mapping is internally consistent, NOT that it matches
// real Mem9. An operator MUST verify the shapes against their Mem9
// version before relying on this backend (see docs/MEMORY-BACKENDS.md).
//
// What IS verified and load-bearing in this file (independent of the
// wire guesses): the memory.Backend interface impl, the two tenancy
// strategies (Decision 9), RFC-F credential resolution via an injected
// resolver (Decision 10), client-side re-ranking (Decision 11), and the
// OTEL span (Decision 12). The credential (X-API-Key) is treated as a
// secret: never logged, never embedded in an error, never on a span.
package mem9

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	memory "github.com/denn-gubsky/loomcycle/internal/memory"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"go.opentelemetry.io/otel/attribute"
)

// =====================================================================
// ASSUMED Mem9 wire shape — verify against the real
// github.com/mem9-ai/mem9 v1alpha2 API before production.
//
// Everything between this banner and the next "END ASSUMED WIRE SHAPE"
// banner is the unverified REST contract. To adapt to the real Mem9
// API, change ONLY this block: the paths, the request/response structs,
// and the *Path / decode* / encode* helpers. The Backend methods below
// call these and stay shape-agnostic.
// =====================================================================

// searchPath is the ONE endpoint the RFC names concretely:
// POST {base}/{api_version}/search.
//
// ASSUMED Mem9 wire shape — verify against the real
// github.com/mem9-ai/mem9 v1alpha2 API before production.
func (b *Backend) searchPath() string {
	return b.joinPath("search")
}

// memoryItemPath addresses one memory by key:
// GET/PUT/DELETE {base}/{api_version}/memories/{key}.
//
// ASSUMED Mem9 wire shape — verify against the real
// github.com/mem9-ai/mem9 v1alpha2 API before production.
func (b *Backend) memoryItemPath(key string) string {
	return b.joinPath("memories", url.PathEscape(key))
}

// memoryCollectionPath lists memories with an optional prefix filter:
// GET {base}/{api_version}/memories?prefix=...
//
// ASSUMED Mem9 wire shape — verify against the real
// github.com/mem9-ai/mem9 v1alpha2 API before production.
func (b *Backend) memoryCollectionPath() string {
	return b.joinPath("memories")
}

// statsPath returns per-(provider, model) embedding stats:
// GET {base}/{api_version}/stats.
//
// ASSUMED Mem9 wire shape — verify against the real
// github.com/mem9-ai/mem9 v1alpha2 API before production.
func (b *Backend) statsPath() string {
	return b.joinPath("stats")
}

// wireSearchRequest is the POST /search body. Mem9 embeds server-side,
// so we send query TEXT, not a vector. The prefix scopes the search
// (shared_key_with_prefix tenancy prepends the tenant prefix here).
//
// ASSUMED Mem9 wire shape — verify against the real
// github.com/mem9-ai/mem9 v1alpha2 API before production.
type wireSearchRequest struct {
	Query  string `json:"query"`
	TopK   int    `json:"top_k"`
	Prefix string `json:"prefix,omitempty"`
}

// wireSearchResponse is the POST /search response. Mem9 returns scored
// candidates; loomcycle re-ranks + trims client-side (Decision 11).
//
// ASSUMED Mem9 wire shape — verify against the real
// github.com/mem9-ai/mem9 v1alpha2 API before production.
type wireSearchResponse struct {
	Results []wireSearchResult `json:"results"`
}

// wireSearchResult is one candidate from /search.
//
// ASSUMED Mem9 wire shape — verify against the real
// github.com/mem9-ai/mem9 v1alpha2 API before production.
type wireSearchResult struct {
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	Score     float64         `json:"score"` // cosine similarity in [0,1]
	Provider  string          `json:"embedding_provider,omitempty"`
	Model     string          `json:"embedding_model,omitempty"`
	ExpiresAt string          `json:"expires_at,omitempty"` // RFC3339, "" = none
}

// wireMemory is the GET/PUT memory item body.
//
// ASSUMED Mem9 wire shape — verify against the real
// github.com/mem9-ai/mem9 v1alpha2 API before production.
type wireMemory struct {
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	ExpiresAt string          `json:"expires_at,omitempty"` // RFC3339, "" = none
	// Embed asks Mem9 to (re)compute the server-side embedding on write.
	Embed bool `json:"embed,omitempty"`
	// EmbedText overrides the text Mem9 embeds (defaults to the value).
	EmbedText string `json:"embed_text,omitempty"`
}

// wireListResponse is the GET /memories response.
//
// ASSUMED Mem9 wire shape — verify against the real
// github.com/mem9-ai/mem9 v1alpha2 API before production.
type wireListResponse struct {
	Items     []wireMemory `json:"items"`
	Truncated bool         `json:"truncated"`
}

// wireStatsResponse is the GET /stats response.
//
// ASSUMED Mem9 wire shape — verify against the real
// github.com/mem9-ai/mem9 v1alpha2 API before production.
type wireStatsResponse struct {
	Models []struct {
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		Dimension int    `json:"dimension"`
		RowCount  int    `json:"row_count"`
	} `json:"models"`
	TotalEmbeddingBytes int64 `json:"total_embedding_bytes"`
}

// toSearchEntry maps one assumed wire result onto loomcycle's domain
// store.MemorySearchEntry. The Score is taken as cosine similarity so the
// MR-1 ranker (which expects e.Score ∈ [0,1]) operates uniformly across
// backends.
//
// ASSUMED Mem9 wire shape — verify against the real
// github.com/mem9-ai/mem9 v1alpha2 API before production.
func toSearchEntry(w wireSearchResult) store.MemorySearchEntry {
	e := store.MemorySearchEntry{Score: w.Score}
	e.Key = w.Key
	e.Value = w.Value
	e.ExpiresAt = parseWireTime(w.ExpiresAt)
	e.EmbeddedWith.Provider = w.Provider
	e.EmbeddedWith.Model = w.Model
	return e
}

// toMemoryEntry maps one assumed wire memory onto store.MemoryEntry.
//
// ASSUMED Mem9 wire shape — verify against the real
// github.com/mem9-ai/mem9 v1alpha2 API before production.
func toMemoryEntry(w wireMemory) store.MemoryEntry {
	return store.MemoryEntry{
		Key:       w.Key,
		Value:     w.Value,
		ExpiresAt: parseWireTime(w.ExpiresAt),
	}
}

// parseWireTime parses Mem9's timestamp field. An empty string (no
// expiry) yields the zero time, which the tool renders as expires_at:null.
// An unparseable value also yields the zero time (defensive — a malformed
// timestamp shouldn't fail an otherwise-valid read).
//
// ASSUMED Mem9 wire shape — verify against the real
// github.com/mem9-ai/mem9 v1alpha2 API before production.
func parseWireTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// =====================================================================
// END ASSUMED WIRE SHAPE. Everything below is verified loomcycle-side
// logic: the interface impl, tenancy, credentials, ranking, OTEL.
// =====================================================================

// Config carries everything the Backend needs that is NOT request-time
// state. Built by the tool layer (builtin.Memory.backend) from the
// resolved MemoryBackendDef + the env allowlist + ctx.
type Config struct {
	// BaseURL is the Mem9 server root (no trailing /api_version). Required.
	BaseURL string
	// APIVersion is the REST version segment. Defaults to "v1alpha2".
	APIVersion string
	// Tenancy is the resolved tenancy strategy (Decision 9). The zero
	// value (empty Kind) means key_per_tenant with no key-prefixing.
	Tenancy Tenancy
	// CredentialResolver resolves the X-API-Key for an op, per request,
	// from ctx (RFC-F UserCredentials) or the env-allowlist (Decision
	// 10). It is injected by the tool layer so this package never
	// imports internal/tools (avoids a dependency on the tool layer and
	// keeps the cycle surface minimal). MUST NOT return the key in any
	// error it produces. Required.
	CredentialResolver CredentialResolver
	// HTTPClient is the transport. When nil, New installs a client with
	// PerOpTimeout. Tests inject a client pointed at an httptest.Server.
	HTTPClient *http.Client
	// PerOpTimeout bounds a single REST call. Defaults to 10s when zero.
	PerOpTimeout time.Duration
	// BackendName is the operator's memory_backends.<name> key, used only
	// for log/OTEL identification. NOT a secret.
	BackendName string
}

// CredentialResolver resolves the X-API-Key for one op. ctx carries the
// run identity (the resolver closes over the tool layer's tools.RunIdentity
// access). It returns a typed error (NOT carrying the key) when the key
// can't be resolved — the caller surfaces it so the op fails loud or the
// fallback wrapper engages; it must NEVER fall through to an
// unauthenticated call.
type CredentialResolver func(ctx context.Context) (apiKey string, err error)

// Tenancy is the resolved per-request tenancy behavior (Decision 9). The
// tool layer builds it from the MemoryBackendDef's tenancy_strategy with
// {tenant_id} already substituted, so this package does no pattern
// interpolation — it only decides whether to prefix keys.
type Tenancy struct {
	// KeyPrefix is prepended to every key/prefix the backend sends to
	// Mem9 (shared_key_with_prefix strategy). Empty for key_per_tenant —
	// in that strategy tenant isolation comes from the per-tenant API key
	// the resolver returns, not from key-prefixing.
	KeyPrefix string
}

// Backend implements memory.Backend over Mem9's REST API.
type Backend struct {
	baseURL    string
	apiVersion string
	tenancy    Tenancy
	resolveKey CredentialResolver
	httpClient *http.Client
	name       string
}

// New builds the Mem9 backend. CredentialResolver is required (a nil
// resolver would mean an unauthenticated call — we refuse to construct
// that). BaseURL is required.
func New(cfg Config) *Backend {
	ver := cfg.APIVersion
	if ver == "" {
		ver = "v1alpha2"
	}
	hc := cfg.HTTPClient
	if hc == nil {
		to := cfg.PerOpTimeout
		if to == 0 {
			to = 10 * time.Second
		}
		hc = &http.Client{Timeout: to}
	}
	return &Backend{
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		apiVersion: ver,
		tenancy:    cfg.Tenancy,
		resolveKey: cfg.CredentialResolver,
		httpClient: hc,
		name:       cfg.BackendName,
	}
}

// joinPath builds {baseURL}/{apiVersion}/{segs...}.
func (b *Backend) joinPath(segs ...string) string {
	parts := append([]string{b.baseURL, b.apiVersion}, segs...)
	return strings.Join(parts, "/")
}

// scopedKey applies the tenancy key prefix. For key_per_tenant (empty
// prefix) the key is unchanged; for shared_key_with_prefix the tenant
// prefix is prepended. This is the tenant-isolation boundary for the
// shared-key strategy: a missing prefix here would let one tenant's
// query reach another's rows in Mem9's flat keyspace.
func (b *Backend) scopedKey(key string) string {
	return b.tenancy.KeyPrefix + key
}

// scopedPrefix applies the tenancy key prefix to a list/search prefix.
// Same isolation rationale as scopedKey: the prefix the agent supplies
// is namespaced UNDER the tenant prefix so a list/search can never
// escape the tenant's slice of the keyspace.
func (b *Backend) scopedPrefix(prefix string) string {
	return b.tenancy.KeyPrefix + prefix
}

// do performs one authenticated REST round-trip. It resolves the
// X-API-Key per call (so credential rotation + per-run creds take
// effect without reconstructing the backend), sends the request, and
// returns the response body bytes for 2xx. Non-2xx and transport errors
// become errors that NEVER contain the API key.
//
// The key is set on the header and then dropped — it is never logged and
// never placed into any error returned from here.

// maxResponseBytes caps how much of a Mem9 response we buffer. Mem9 is a
// remote trust boundary; an unbounded read would let a hostile/buggy server
// OOM the process. 16 MiB is far above any realistic memory payload.
const maxResponseBytes = 16 << 20

func (b *Backend) do(ctx context.Context, method, urlStr string, body any) ([]byte, error) {
	apiKey, err := b.resolveKey(ctx)
	if err != nil {
		// The resolver's error must not carry the key (its contract); we
		// pass it through unwrapped so a typed credential error stays
		// matchable by the caller.
		return nil, err
	}

	var rdr io.Reader
	if body != nil {
		buf, merr := json.Marshal(body)
		if merr != nil {
			return nil, fmt.Errorf("mem9: encode request: %w", merr)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, urlStr, rdr)
	if err != nil {
		return nil, fmt.Errorf("mem9: build request: %w", err)
	}
	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := b.httpClient.Do(req)
	if err != nil {
		// req.URL has no credential in it (the key is a header), so %v on a
		// transport error is secret-free.
		return nil, fmt.Errorf("mem9: request to %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bound the response read: Mem9 is a remote trust boundary, and an
	// unbounded io.ReadAll would let a malicious or buggy server OOM the
	// process with a giant body. maxResponseBytes is generous for any
	// realistic memory payload (a search result set or a single value) but
	// caps the blast radius. A body at the cap decodes normally if it's
	// valid JSON within the limit; a truly oversized one is truncated and
	// fails JSON decode in the caller (a clean error, not an OOM).
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Surface the status + a bounded body snippet. Mem9 must not echo
		// the API key in error bodies; we still cap the snippet so a
		// misbehaving server can't blow up the tool_result, and we never
		// log the request header set.
		return nil, fmt.Errorf("mem9: %s returned HTTP %d: %s", method, resp.StatusCode, snippet(respBody))
	}
	return respBody, nil
}

// Get reads one entry. A 404 maps to *store.ErrNotFound so the tool
// renders {"value": null} exactly like the in-process backend.
func (b *Backend) Get(ctx context.Context, scope store.MemoryScope, scopeID, key string) (store.MemoryEntry, error) {
	urlStr := b.memoryItemPath(b.scopedKey(scopeKey(scope, scopeID, key)))
	respBody, err := b.do(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		if isHTTPNotFound(err) {
			return store.MemoryEntry{}, &store.ErrNotFound{Kind: "memory", ID: key}
		}
		return store.MemoryEntry{}, err
	}
	var w wireMemory
	if uerr := json.Unmarshal(respBody, &w); uerr != nil {
		return store.MemoryEntry{}, fmt.Errorf("mem9: decode get response: %w", uerr)
	}
	e := toMemoryEntry(w)
	// Return the agent's key, not the wire-internal namespaced form. The
	// agent asked for `key`, so echo `key` regardless of what Mem9 echoed
	// (defends against a server that omits or reformats the key field).
	e.Key = key
	return e, nil
}

// Set writes one entry. Mem9 embeds server-side, so opts.Embed/EmbedText
// are passed to the server. A successful PUT reports Embedded=opts.Embed
// (the server did the embed); a transient embed-only failure is not
// separable from a write failure over this REST shape, so any error from
// the PUT is returned as a hard error (the fallback wrapper, if present,
// then degrades).
func (b *Backend) Set(ctx context.Context, scope store.MemoryScope, scopeID, key string, value json.RawMessage, opts memory.SetOptions) (memory.SetResult, error) {
	w := wireMemory{
		Key:       b.scopedKey(scopeKey(scope, scopeID, key)),
		Value:     value,
		Embed:     opts.Embed,
		EmbedText: opts.EmbedText,
	}
	if opts.TTL > 0 {
		w.ExpiresAt = time.Now().UTC().Add(opts.TTL).Format(time.RFC3339Nano)
	}
	if _, err := b.do(ctx, http.MethodPut, b.memoryItemPath(w.Key), w); err != nil {
		return memory.SetResult{}, err
	}
	// Mem9 owns the embedding; a 2xx means the row (and, when requested,
	// its embedding) landed.
	return memory.SetResult{Embedded: opts.Embed}, nil
}

// Delete removes one entry. A 404 is reported as existed=false (not an
// error), matching the in-process Delete contract.
func (b *Backend) Delete(ctx context.Context, scope store.MemoryScope, scopeID, key string) (bool, error) {
	urlStr := b.memoryItemPath(b.scopedKey(scopeKey(scope, scopeID, key)))
	if _, err := b.do(ctx, http.MethodDelete, urlStr, nil); err != nil {
		if isHTTPNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// List enumerates entries under (scope, scopeID) with an optional prefix.
func (b *Backend) List(ctx context.Context, scope store.MemoryScope, scopeID, prefix string, limit int) ([]store.MemoryEntry, bool, error) {
	q := url.Values{}
	q.Set("prefix", b.scopedPrefix(scopeKey(scope, scopeID, prefix)))
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	urlStr := b.memoryCollectionPath() + "?" + q.Encode()
	respBody, err := b.do(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, false, err
	}
	var lr wireListResponse
	if uerr := json.Unmarshal(respBody, &lr); uerr != nil {
		return nil, false, fmt.Errorf("mem9: decode list response: %w", uerr)
	}
	entries := make([]store.MemoryEntry, 0, len(lr.Items))
	for _, it := range lr.Items {
		e := toMemoryEntry(it)
		// Strip the tenant + scope prefix we prepended on write so the
		// agent sees the keys it used, not the wire-internal namespaced
		// form. Defensive: only strips when the prefix is actually present.
		e.Key = b.unscopeKey(scope, scopeID, e.Key)
		entries = append(entries, e)
	}
	return entries, lr.Truncated, nil
}

// Search posts the query to Mem9's /search (the ONE RFC-named endpoint),
// then re-ranks the returned candidates CLIENT-SIDE via the MR-1 ranker
// and trims to TopK (Decision 11: "external backends re-rank
// client-side"). Mem9 honors top_k but not loomcycle's hybrid formula,
// so the Backend contract — "Search returns ranked top_k" — holds
// uniformly across backends.
func (b *Backend) Search(ctx context.Context, scope store.MemoryScope, scopeID string, q memory.SearchQuery, rank memory.RankConfig, dedup memory.DedupConfig) (memory.SearchResult, error) {
	// OTEL (Decision 12, light): one span per recall with backend +
	// latency. No secrets, no query text, no transcript on the span.
	ctx, span := lcotel.Tracer().Start(ctx, "loomcycle.memory.search")
	defer span.End()
	span.SetAttributes(
		attribute.String("memory.backend", "mem9"),
		attribute.Int("memory.top_k", q.TopK),
	)
	if b.name != "" {
		span.SetAttributes(attribute.String("memory.backend_name", b.name))
	}
	start := time.Now()

	// Over-fetch a candidate pool when the rank is hybrid so recency (etc.)
	// can promote an entry the pure-cosine top-K would miss — mirrors the
	// in-process backend's pool sizing for cross-backend parity. (Dedup
	// degrades to a no-op here because the REST envelope returns no per-row
	// vectors, so it doesn't widen the pool — unlike in-process.)
	pool := q.TopK
	if !rank.IsPureSemantic() {
		pool = q.TopK * 4
	}

	// Request pool+1 so `truncated` can be detected the same way as the
	// in-process backend: a returned row beyond what the caller's top_k will
	// keep means Mem9 had more matches. Without this probe a pure-semantic
	// request (pool == top_k) could never report truncated=true even when
	// the scope held more matches — a cross-backend parity bug.
	reqBody := wireSearchRequest{
		Query:  q.QueryText,
		TopK:   pool + 1,
		Prefix: b.scopedPrefix(scopeKey(scope, scopeID, q.Prefix)),
	}
	respBody, err := b.do(ctx, http.MethodPost, b.searchPath(), reqBody)
	if err != nil {
		return memory.SearchResult{}, err
	}
	var sr wireSearchResponse
	if uerr := json.Unmarshal(respBody, &sr); uerr != nil {
		return memory.SearchResult{}, fmt.Errorf("mem9: decode search response: %w", uerr)
	}

	candidates := make([]store.MemorySearchEntry, 0, len(sr.Results))
	for _, r := range sr.Results {
		e := toSearchEntry(r)
		e.Key = b.unscopeKey(scope, scopeID, e.Key)
		candidates = append(candidates, e)
	}

	// truncated: Mem9 had more matches than the caller's top_k.
	truncated := len(candidates) > q.TopK

	// Client-side re-rank with the SAME `now` used for the rank scores so
	// the rendered score matches the ordering (same discipline as the
	// in-process backend). Dedup also runs client-side for uniform
	// semantics (Decision 11), AFTER rank and BEFORE trim. The Mem9 REST
	// envelope returns no per-row vectors (the server owns the embed +
	// cosine), so every entry has an empty Vector and DedupResults degrades
	// to a no-op here — distinct rows are kept and dropped stays 0. The
	// plumbing exists so dedup behaves identically the day Mem9 starts
	// returning vectors; until then a vector-less backend simply can't
	// dedup, which is the documented degradation, not a silent divergence.
	now := time.Now()
	ranked := memory.RankCandidates(candidates, rank, now)
	deduped, dropped := memory.DedupResults(ranked, dedup)
	if len(deduped) > q.TopK {
		deduped = deduped[:q.TopK]
	}
	rankScores := memory.ScoreAll(deduped, rank, now)

	span.SetAttributes(attribute.Float64("memory.recall_latency_ms", float64(time.Since(start).Microseconds())/1000.0))
	if dedup.Enabled {
		span.SetAttributes(attribute.Int("memory.dedup.dropped_count", dropped))
	}

	out := memory.SearchResult{
		Entries:    deduped,
		RankScores: rankScores,
		// Mem9 embeds server-side; we don't see the query vector dimension.
		// 0 is a truthful "unknown from this backend" rather than a guess.
		QueryEmbeddingDim: 0,
		Truncated:         truncated,
		DedupDropped:      dropped,
	}
	if rank.SourceFrequencyReserved() {
		out.RankNote = "source_weight and frequency_weight are reserved and contribute 0 until source/access_count tracking ships"
	}
	return out, nil
}

// Stats returns per-(provider, model) embedding stats from Mem9.
func (b *Backend) Stats(ctx context.Context, scope store.MemoryScope) (store.MemoryEmbedStats, error) {
	respBody, err := b.do(ctx, http.MethodGet, b.statsPath(), nil)
	if err != nil {
		return store.MemoryEmbedStats{}, err
	}
	var ws wireStatsResponse
	if uerr := json.Unmarshal(respBody, &ws); uerr != nil {
		return store.MemoryEmbedStats{}, fmt.Errorf("mem9: decode stats response: %w", uerr)
	}
	out := store.MemoryEmbedStats{Scope: scope, TotalEmbeddingBytes: ws.TotalEmbeddingBytes}
	for _, m := range ws.Models {
		out.Models = append(out.Models, store.MemoryEmbedModelStats{
			Provider:  m.Provider,
			Model:     m.Model,
			Dimension: m.Dimension,
			RowCount:  m.RowCount,
		})
	}
	return out, nil
}

// scopeKey namespaces the agent's key under (scope, scopeID) so Mem9's
// flat keyspace still separates the agent keyspace from the user
// keyspace, and one scope_id from another. This is loomcycle's scope
// isolation (independent of tenancy): without it, scope=agent key "x"
// and scope=user key "x" would collide in Mem9.
//
// Shape: "{scope}/{scopeID}/{key}". The tenancy KeyPrefix (when set) is
// applied OUTSIDE this, by scopedKey/scopedPrefix.
func scopeKey(scope store.MemoryScope, scopeID, key string) string {
	return string(scope) + "/" + scopeID + "/" + key
}

// unscopeKey reverses scopeKey + the tenancy prefix so the agent sees
// the key it wrote. Defensive: returns the input unchanged if the
// expected prefix isn't present (e.g. a hand-seeded Mem9 row).
func (b *Backend) unscopeKey(scope store.MemoryScope, scopeID, wireKey string) string {
	k := strings.TrimPrefix(wireKey, b.tenancy.KeyPrefix)
	scopePrefix := string(scope) + "/" + scopeID + "/"
	return strings.TrimPrefix(k, scopePrefix)
}

// snippet bounds an error-body excerpt so a misbehaving Mem9 server
// can't blow up a tool_result. 200 bytes is enough to see a JSON error.
func snippet(b []byte) string {
	const max = 200
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// isHTTPNotFound reports whether err is a do() error for an HTTP 404.
// Used by Get/Delete to map Mem9's 404 onto the loomcycle "absent"
// semantics. String-match is acceptable here: do() formats the status
// itself, so the format is owned in this file.
func isHTTPNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "HTTP 404")
}

var _ memory.Backend = (*Backend)(nil)
