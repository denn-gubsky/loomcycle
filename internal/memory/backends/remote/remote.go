// Package remote implements memory.Backend by HTTP-proxying the six data
// operations to a PEER loomcycle instance's /v1/_memory/* surface. It is the
// first faithful external backend for the RFC I seam: unlike an LLM-extract
// memory *product*, another loomcycle is a faithful flat KV+vector store
// (synchronous Set->Get, real Stats, real vector Search), so it plugs in behind
// the same six methods with no paradigm mismatch.
//
// "Embeds server-side": the peer owns embedding. This backend never touches a
// local Embedder — Set forwards embed=true and the peer embeds; Search sends
// query text and the peer runs the vector search. (RFC CD Part B.)
//
// Connection policy (the SSRF-guarded HTTP client, the credential-env
// allowlist) is INJECTED by the factory (internal/tools/builtin), so this
// package stays free of config/netguard/os coupling and is unit-testable
// against an httptest peer.
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	memory "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Options configures a remote backend. The factory fills these from a resolved
// config.MemoryBackend.
type Options struct {
	// BaseURL is the peer's origin, e.g. "https://peer.example:8787". A
	// trailing slash is tolerated.
	BaseURL string
	// APIVersion is reserved; the data API is /v1 today.
	APIVersion string
	// DefaultAPIKeyEnv is the env-var NAME whose value is the peer bearer,
	// used when tenancy is "" (one shared credential).
	DefaultAPIKeyEnv string
	// TenancyKind is "" (one shared credential) or "key_per_tenant" (a
	// distinct credential per local tenant, its env name from EnvPattern).
	// "shared_key_with_prefix" is refused — see New.
	TenancyKind string
	// EnvPattern is the key_per_tenant env-name template containing {tenant_id}.
	EnvPattern string
	// KeyResolver maps an env-var NAME to its value, allowlist-gated. The
	// factory supplies config.EnvNameCredentialSafe + os.Getenv. A nil resolver
	// (or an empty resolved env name) means "no auth" — an unauthenticated peer.
	KeyResolver func(envName string) (string, error)
	// HTTPClient is the SSRF-guarded client the factory built. Required.
	HTTPClient *http.Client
}

// Backend is a memory.Backend that proxies to a peer's /v1/_memory/*.
type Backend struct {
	base         string
	defAPIKeyEnv string
	tenancyKind  string
	envPattern   string
	keyResolver  func(string) (string, error)
	http         *http.Client
}

var _ memory.Backend = (*Backend)(nil)

// New validates the options and builds the backend.
func New(o Options) (*Backend, error) {
	if o.BaseURL == "" {
		return nil, fmt.Errorf("remote memory backend: base_url is required")
	}
	if o.TenancyKind == "shared_key_with_prefix" {
		// The peer search API (POST /v1/_memory/search) has no key-prefix
		// parameter, so a namespaced search could not be scoped to one
		// tenant's prefix — it would leak other tenants' rows into a search
		// result. Refuse loudly rather than half-honor it. Supported tenancy:
		// "" (one shared credential) and key_per_tenant (a distinct peer
		// credential/tenant per local tenant).
		return nil, fmt.Errorf("remote memory backend: tenancy_strategy=shared_key_with_prefix is not supported (use key_per_tenant)")
	}
	if o.HTTPClient == nil {
		return nil, fmt.Errorf("remote memory backend: HTTPClient is required")
	}
	return &Backend{
		base:         strings.TrimRight(o.BaseURL, "/"),
		defAPIKeyEnv: o.DefaultAPIKeyEnv,
		tenancyKind:  o.TenancyKind,
		envPattern:   o.EnvPattern,
		keyResolver:  o.KeyResolver,
		http:         o.HTTPClient,
	}, nil
}

// authHeader resolves the peer bearer for this call. For key_per_tenant the
// env-var name is derived from the LOCAL tenant (run identity), so the mapping
// happens per-call. Returns "" for an unauthenticated peer.
func (b *Backend) authHeader(ctx context.Context) (string, error) {
	envName := b.defAPIKeyEnv
	if b.tenancyKind == "key_per_tenant" && b.envPattern != "" {
		envName = strings.ReplaceAll(b.envPattern, "{tenant_id}", tools.RunIdentity(ctx).TenantID)
	}
	if envName == "" || b.keyResolver == nil {
		return "", nil
	}
	tok, err := b.keyResolver(envName)
	if err != nil {
		return "", err // contains only the env-var NAME, never the value
	}
	return "Bearer " + tok, nil
}

func (b *Backend) Get(ctx context.Context, scope store.MemoryScope, scopeID, key string) (store.MemoryEntry, error) {
	status, raw, err := b.request(ctx, http.MethodGet, b.keysURL(scope, scopeID, key), nil)
	if err != nil {
		return store.MemoryEntry{}, err
	}
	switch status {
	case http.StatusOK:
		var r struct {
			Entry store.MemoryEntry `json:"entry"`
		}
		if uerr := json.Unmarshal(raw, &r); uerr != nil {
			return store.MemoryEntry{}, decodeErr(uerr)
		}
		return r.Entry, nil
	case http.StatusNotFound:
		return store.MemoryEntry{}, &store.ErrNotFound{Kind: "memory", ID: key}
	default:
		return store.MemoryEntry{}, statusErr("get", status, raw)
	}
}

func (b *Backend) Set(ctx context.Context, scope store.MemoryScope, scopeID, key string, value json.RawMessage, opts memory.SetOptions) (memory.SetResult, error) {
	// NOTE: opts.Provenance cannot round-trip — the peer PUT body carries no
	// provenance field. A consolidation write's provenance is therefore dropped
	// over a remote backend (a documented limitation of the HTTP data API).
	body := putBody{Value: value, Embed: opts.Embed}
	if opts.TTL > 0 {
		body.TTLSeconds = int(opts.TTL / time.Second)
	}
	status, raw, err := b.request(ctx, http.MethodPut, b.keysURL(scope, scopeID, key), body)
	if err != nil {
		return memory.SetResult{}, err
	}
	if status != http.StatusOK {
		return memory.SetResult{}, statusErr("set", status, raw)
	}
	var r struct {
		Embedded     bool   `json:"embedded"`
		EmbedWarning string `json:"embed_warning"`
	}
	_ = json.Unmarshal(raw, &r)
	return memory.SetResult{Embedded: r.Embedded, EmbedWarning: r.EmbedWarning}, nil
}

func (b *Backend) Delete(ctx context.Context, scope store.MemoryScope, scopeID, key string) (bool, error) {
	status, raw, err := b.request(ctx, http.MethodDelete, b.keysURL(scope, scopeID, key), nil)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusNoContent, http.StatusOK:
		// The peer DELETE is idempotent and returns no existence signal, so we
		// report existed=true on success. A remote backend cannot cheaply know
		// whether a row was present without a racy pre-read.
		return true, nil
	default:
		return false, statusErr("delete", status, raw)
	}
}

func (b *Backend) List(ctx context.Context, scope store.MemoryScope, scopeID, prefix string, limit int) ([]store.MemoryEntry, bool, error) {
	u := b.keysURL(scope, scopeID, "")
	q := url.Values{}
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	status, raw, err := b.request(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, false, err
	}
	if status != http.StatusOK {
		return nil, false, statusErr("list", status, raw)
	}
	var r struct {
		Entries   []store.MemoryEntry `json:"entries"`
		Truncated bool                `json:"truncated"`
	}
	if uerr := json.Unmarshal(raw, &r); uerr != nil {
		return nil, false, decodeErr(uerr)
	}
	return r.Entries, r.Truncated, nil
}

func (b *Backend) Search(ctx context.Context, scope store.MemoryScope, scopeID string, q memory.SearchQuery, rank memory.RankConfig, dedup memory.DedupConfig) (memory.SearchResult, error) {
	req := searchReq{
		Query:   q.QueryText,
		Scope:   string(scope),
		ScopeID: scopeID,
		TopK:    q.TopK,
		Rank:    rankPtr(rank),   // omitted when zero so the peer uses its default ranking
		Dedup:   dedupPtr(dedup), // (a zero config forwarded as-is would degrade the peer's rank)
	}
	for _, s := range q.Sources {
		req.Sources = append(req.Sources, string(s))
	}
	status, raw, err := b.request(ctx, http.MethodPost, b.base+"/v1/_memory/search", req)
	if err != nil {
		return memory.SearchResult{}, err
	}
	if status != http.StatusOK {
		return memory.SearchResult{}, typedStatusErr("search", status, raw)
	}
	var r searchResp
	if uerr := json.Unmarshal(raw, &r); uerr != nil {
		return memory.SearchResult{}, decodeErr(uerr)
	}
	out := memory.SearchResult{
		QueryEmbeddingDim: r.QueryEmbeddingDim,
		Truncated:         r.Truncated,
		// We forwarded the selector to a loomcycle peer, whose /v1/_memory/search
		// applies it (same code path). Honest to report applied only when we
		// actually asked (the "false is the zero value on purpose" contract).
		SourcesApplied: len(q.Sources) > 0,
	}
	for _, e := range r.Entries {
		var se store.MemorySearchEntry
		se.Key = e.Key
		se.Value = e.Value
		se.Score = e.Score
		se.EmbeddedWith.Provider = e.EmbeddedWith.Provider
		se.EmbeddedWith.Model = e.EmbeddedWith.Model
		// Reconstruct Origin so memory.Class(entry) reproduces the peer's kind:
		// a "fact" needs a non-empty provenance origin; a "note" has none; a
		// "document" is classified by its reserved key prefix regardless.
		if e.Kind == "fact" {
			se.Origin = "remote"
		}
		out.Entries = append(out.Entries, se)
		out.RankScores = append(out.RankScores, e.RankScore)
	}
	return out, nil
}

func (b *Backend) Stats(ctx context.Context, scope store.MemoryScope) (store.MemoryEmbedStats, error) {
	u := b.base + "/v1/_memory/embed_stats?scope=" + url.QueryEscape(string(scope))
	status, raw, err := b.request(ctx, http.MethodGet, u, nil)
	if err != nil {
		return store.MemoryEmbedStats{}, err
	}
	if status != http.StatusOK {
		return store.MemoryEmbedStats{}, typedStatusErr("stats", status, raw)
	}
	var r struct {
		Models              []store.MemoryEmbedModelStats `json:"models"`
		TotalEmbeddingBytes int64                         `json:"total_embedding_bytes"`
	}
	if uerr := json.Unmarshal(raw, &r); uerr != nil {
		return store.MemoryEmbedStats{}, decodeErr(uerr)
	}
	return store.MemoryEmbedStats{Scope: scope, Models: r.Models, TotalEmbeddingBytes: r.TotalEmbeddingBytes}, nil
}

// keysURL builds .../v1/_memory/scopes/{scope}/{scope_id}/keys[/{key...}],
// escaping each segment while PRESERVING the key's slashes (the peer's
// {key...} route treats them as path separators).
func (b *Backend) keysURL(scope store.MemoryScope, scopeID, key string) string {
	sid := scopeID
	if sid == "" {
		// tenant scope forces scope_id="" server-side, but the route still
		// needs a segment; any placeholder works.
		sid = "_"
	}
	var sb strings.Builder
	sb.WriteString(b.base)
	sb.WriteString("/v1/_memory/scopes/")
	sb.WriteString(url.PathEscape(string(scope)))
	sb.WriteByte('/')
	sb.WriteString(url.PathEscape(sid))
	sb.WriteString("/keys")
	if key != "" {
		sb.WriteByte('/')
		sb.WriteString(escapeKeyPath(key))
	}
	return sb.String()
}

func escapeKeyPath(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// request performs one HTTP call and returns (status, body, transport-error).
// The Authorization header carries the bearer; the URL never does, so a logged
// error (which includes method + url) is secret-free.
func (b *Backend) request(ctx context.Context, method, u string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("remote memory backend: encode request: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	authz, err := b.authHeader(ctx)
	if err != nil {
		return 0, nil, err
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("remote memory backend: %s %s: %w", method, u, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, raw, nil
}

// --- wire shapes (mirror the peer's HTTP structs) ---

type putBody struct {
	Value      json.RawMessage `json:"value"`
	Embed      bool            `json:"embed,omitempty"`
	TTLSeconds int             `json:"ttl_seconds,omitempty"`
}

type searchReq struct {
	Query   string              `json:"query"`
	Scope   string              `json:"scope"`
	ScopeID string              `json:"scope_id,omitempty"`
	TopK    int                 `json:"top_k,omitempty"`
	Rank    *memory.RankConfig  `json:"rank,omitempty"`
	Dedup   *memory.DedupConfig `json:"dedup,omitempty"`
	Sources []string            `json:"sources,omitempty"`
}

type searchResp struct {
	Entries []struct {
		Key          string          `json:"key"`
		Value        json.RawMessage `json:"value"`
		Score        float64         `json:"score"`
		RankScore    float64         `json:"rank_score"`
		EmbeddedWith struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"embedded_with"`
		Kind    string `json:"kind"`
		ChunkID string `json:"chunk_id"`
	} `json:"entries"`
	QueryEmbeddingDim int  `json:"query_embedding_dim"`
	Truncated         bool `json:"truncated"`
}

// --- error helpers ---

func decodeErr(err error) error {
	return fmt.Errorf("remote memory backend: decode response: %w", err)
}

// bodyErr extracts a {code,error} envelope from a peer error body.
func bodyErr(raw []byte) (code, msg string) {
	var e struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil {
		return e.Code, e.Error
	}
	return "", ""
}

// statusErr renders a generic error for a non-2xx (Get/Set/Delete/List): the
// fallback wrapper degrades on any of these.
func statusErr(op string, status int, raw []byte) error {
	code, msg := bodyErr(raw)
	if msg != "" {
		return fmt.Errorf("remote memory backend: %s failed (%d %s): %s", op, status, code, msg)
	}
	return fmt.Errorf("remote memory backend: %s failed with status %d", op, status)
}

// typedStatusErr maps a peer {code} error to a *store.MemoryError so
// Search/Stats honor the interface contract (vector_unsupported etc. render
// unchanged when the backend is used without a fallback wrapper).
func typedStatusErr(op string, status int, raw []byte) error {
	code, msg := bodyErr(raw)
	if code != "" {
		if msg == "" {
			msg = fmt.Sprintf("remote memory backend: %s: %s", op, code)
		}
		return &store.MemoryError{Code: code, Msg: msg}
	}
	return statusErr(op, status, raw)
}

// rankPtr / dedupPtr return nil for a zero config so the request omits it and
// the peer applies its default ranking (a zero config forwarded verbatim would
// zero every weight and produce a degenerate order).
var (
	zeroRankJSON, _  = json.Marshal(memory.RankConfig{})
	zeroDedupJSON, _ = json.Marshal(memory.DedupConfig{})
)

func rankPtr(r memory.RankConfig) *memory.RankConfig {
	if b, _ := json.Marshal(r); bytes.Equal(b, zeroRankJSON) {
		return nil
	}
	return &r
}

func dedupPtr(d memory.DedupConfig) *memory.DedupConfig {
	if b, _ := json.Marshal(d); bytes.Equal(b, zeroDedupJSON) {
		return nil
	}
	return &d
}
