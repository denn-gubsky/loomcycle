package remote

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	memory "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// stubPeer is a minimal in-memory loomcycle /v1/_memory/* surface for testing
// the remote backend end-to-end over real HTTP.
type stubPeer struct {
	mux    *http.ServeMux
	data   map[string]json.RawMessage // "scope/scope_id/key" -> value
	lastAu string                     // last Authorization header seen
}

func newStubPeer() *stubPeer {
	p := &stubPeer{mux: http.NewServeMux(), data: map[string]json.RawMessage{}}
	key := func(r *http.Request) string {
		return r.PathValue("scope") + "/" + r.PathValue("scope_id") + "/" + r.PathValue("key")
	}
	p.mux.HandleFunc("GET /v1/_memory/scopes/{scope}/{scope_id}/keys/{key...}", func(w http.ResponseWriter, r *http.Request) {
		p.lastAu = r.Header.Get("Authorization")
		v, ok := p.data[key(r)]
		if !ok {
			writeJSON(w, 404, map[string]string{"code": "not_found", "error": "no such key"})
			return
		}
		writeJSON(w, 200, map[string]any{"scope": r.PathValue("scope"), "scope_id": r.PathValue("scope_id"),
			"entry": map[string]any{"key": r.PathValue("key"), "value": v, "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z"}})
	})
	p.mux.HandleFunc("PUT /v1/_memory/scopes/{scope}/{scope_id}/keys/{key...}", func(w http.ResponseWriter, r *http.Request) {
		p.lastAu = r.Header.Get("Authorization")
		var body struct {
			Value json.RawMessage `json:"value"`
			Embed bool            `json:"embed"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		p.data[key(r)] = body.Value
		writeJSON(w, 200, map[string]any{"scope": r.PathValue("scope"), "scope_id": r.PathValue("scope_id"),
			"key": r.PathValue("key"), "embedded": body.Embed})
	})
	p.mux.HandleFunc("DELETE /v1/_memory/scopes/{scope}/{scope_id}/keys/{key...}", func(w http.ResponseWriter, r *http.Request) {
		delete(p.data, key(r))
		w.WriteHeader(204)
	})
	p.mux.HandleFunc("GET /v1/_memory/scopes/{scope}/{scope_id}/keys", func(w http.ResponseWriter, r *http.Request) {
		prefix := r.URL.Query().Get("prefix")
		base := r.PathValue("scope") + "/" + r.PathValue("scope_id") + "/"
		entries := []map[string]any{}
		for k, v := range p.data {
			if !strings.HasPrefix(k, base) {
				continue
			}
			bare := strings.TrimPrefix(k, base)
			if prefix != "" && !strings.HasPrefix(bare, prefix) {
				continue
			}
			entries = append(entries, map[string]any{"key": bare, "value": v,
				"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z"})
		}
		writeJSON(w, 200, map[string]any{"scope": r.PathValue("scope"), "scope_id": r.PathValue("scope_id"),
			"entries": entries, "truncated": false})
	})
	p.mux.HandleFunc("POST /v1/_memory/search", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"scope": "agent", "scope_id": "a1", "query_embedding_dim": 3, "truncated": false,
			"entries": []map[string]any{{
				"key": "k/1", "value": json.RawMessage(`{"v":1}`), "score": 0.9, "rank_score": 0.8,
				"embedded_with": map[string]string{"provider": "p", "model": "m"}, "kind": "fact",
			}}})
	})
	p.mux.HandleFunc("GET /v1/_memory/embed_stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"scope": r.URL.Query().Get("scope"), "tenant": "", "total_embedding_bytes": 123,
			"models": []map[string]any{{"provider": "p", "model": "m", "dimension": 3, "row_count": 5}}})
	})
	return p
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func newTestBackend(t *testing.T, srv *httptest.Server, o Options) *Backend {
	t.Helper()
	o.BaseURL = srv.URL
	if o.HTTPClient == nil {
		o.HTTPClient = srv.Client() // unguarded — hits httptest loopback
	}
	b, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func TestRemoteBackend_RoundTrip(t *testing.T) {
	peer := newStubPeer()
	srv := httptest.NewServer(peer.mux)
	defer srv.Close()
	b := newTestBackend(t, srv, Options{})
	ctx := context.Background()

	// Set (a multi-segment key exercises the slash-preserving URL escaping).
	if _, err := b.Set(ctx, store.MemoryScopeAgent, "a1", "k/1", json.RawMessage(`{"v":1}`), memory.SetOptions{}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Get round-trips the value.
	e, err := b.Get(ctx, store.MemoryScopeAgent, "a1", "k/1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(e.Value) != `{"v":1}` {
		t.Errorf("Get value = %s, want {\"v\":1}", e.Value)
	}
	// Get on a missing key yields *store.ErrNotFound.
	_, err = b.Get(ctx, store.MemoryScopeAgent, "a1", "missing")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("Get(missing) err = %v, want *store.ErrNotFound", err)
	}
	// List returns the entry with its bare key.
	entries, trunc, err := b.List(ctx, store.MemoryScopeAgent, "a1", "k/", 10)
	if err != nil || trunc || len(entries) != 1 || entries[0].Key != "k/1" {
		t.Fatalf("List = %+v trunc=%v err=%v", entries, trunc, err)
	}
	// Delete then Get -> not found.
	if existed, derr := b.Delete(ctx, store.MemoryScopeAgent, "a1", "k/1"); derr != nil || !existed {
		t.Fatalf("Delete existed=%v err=%v", existed, derr)
	}
	if _, err := b.Get(ctx, store.MemoryScopeAgent, "a1", "k/1"); !errors.As(err, &nf) {
		t.Errorf("Get after Delete err = %v, want ErrNotFound", err)
	}
	// Stats maps the peer's model rows.
	st, err := b.Stats(ctx, store.MemoryScopeAgent)
	if err != nil || st.TotalEmbeddingBytes != 123 || len(st.Models) != 1 || st.Models[0].RowCount != 5 {
		t.Fatalf("Stats = %+v err=%v", st, err)
	}
}

func TestRemoteBackend_Search(t *testing.T) {
	peer := newStubPeer()
	srv := httptest.NewServer(peer.mux)
	defer srv.Close()
	b := newTestBackend(t, srv, Options{})

	res, err := b.Search(context.Background(), store.MemoryScopeAgent, "a1",
		memory.SearchQuery{QueryText: "q", TopK: 5, Sources: []memory.Source{memory.SourceFacts}},
		memory.RankConfig{}, memory.DedupConfig{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Key != "k/1" {
		t.Fatalf("Search entries = %+v", res.Entries)
	}
	if res.Entries[0].Score != 0.9 || len(res.RankScores) != 1 || res.RankScores[0] != 0.8 {
		t.Errorf("Search score/rank = %v / %v", res.Entries[0].Score, res.RankScores)
	}
	if res.QueryEmbeddingDim != 3 {
		t.Errorf("QueryEmbeddingDim = %d, want 3", res.QueryEmbeddingDim)
	}
	// We forwarded a non-empty Sources to a loomcycle peer -> SourcesApplied.
	if !res.SourcesApplied {
		t.Errorf("SourcesApplied = false, want true (a selector was forwarded)")
	}
	// kind=fact reconstructs a non-empty Origin so the tool classifies it as a fact.
	if res.Entries[0].Origin == "" {
		t.Errorf("Origin empty; a fact should reconstruct a non-empty origin")
	}
}

func TestRemoteBackend_KeyPerTenantAuth(t *testing.T) {
	peer := newStubPeer()
	srv := httptest.NewServer(peer.mux)
	defer srv.Close()
	var askedFor string
	b := newTestBackend(t, srv, Options{
		TenancyKind: "key_per_tenant",
		EnvPattern:  "LOOMCYCLE_PEER_{tenant_id}_KEY",
		KeyResolver: func(name string) (string, error) { askedFor = name; return "tok-" + name, nil },
	})
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "acme"})

	if _, err := b.Set(ctx, store.MemoryScopeAgent, "a1", "k", json.RawMessage(`1`), memory.SetOptions{}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if askedFor != "LOOMCYCLE_PEER_acme_KEY" {
		t.Errorf("resolver asked for %q, want LOOMCYCLE_PEER_acme_KEY", askedFor)
	}
	if peer.lastAu != "Bearer tok-LOOMCYCLE_PEER_acme_KEY" {
		t.Errorf("peer saw Authorization %q", peer.lastAu)
	}
}

func TestRemoteBackend_DeadPeerErrors(t *testing.T) {
	// A closed server -> every op returns a transport error (which the fallback
	// wrapper degrades on). 127.0.0.1:1 is not listening.
	b, err := New(Options{BaseURL: "http://127.0.0.1:1", HTTPClient: http.DefaultClient})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, gerr := b.Get(context.Background(), store.MemoryScopeAgent, "a1", "k"); gerr == nil {
		t.Errorf("Get against a dead peer returned nil error")
	}
}

func TestRemoteBackend_RejectsSharedPrefix(t *testing.T) {
	_, err := New(Options{BaseURL: "http://peer", TenancyKind: "shared_key_with_prefix", HTTPClient: http.DefaultClient})
	if err == nil || !strings.Contains(err.Error(), "shared_key_with_prefix") {
		t.Errorf("New(shared_key_with_prefix) err = %v, want a refusal", err)
	}
}
