package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// backfillEmbedder is a deterministic stub; the vector content is irrelevant here,
// only whether rows were embedded at all.
type backfillEmbedder struct{ calls int }

func (e *backfillEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls += len(texts)
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}
func (e *backfillEmbedder) Dimension() int   { return 4 }
func (e *backfillEmbedder) Model() string    { return "stub" }
func (e *backfillEmbedder) Provider() string { return "test" }

// TestBackfillEmbeddings_UnsupportedTierSaysSoRatherThanReportingZero is what
// actually runs on the sqlite test store, and it is the more important assertion.
//
// sqlite has no vector support, so the candidate query refuses. The endpoint must
// SURFACE that refusal: reporting "0 candidates" would read as "nothing to
// backfill, you are done" to an operator whose 3,114 chunks are all unembedded.
// A capability that is absent and a corpus that is already swept must not look
// alike.
func TestBackfillEmbeddings_UnsupportedTierSaysSoRatherThanReportingZero(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	emb := &backfillEmbedder{}
	srv.embedder = emb

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/v1/_memory/backfill_embeddings?scope=user&scope_id=alice&tenant=&prefix=doc.chunk:&dry_run=false", nil).
		WithContext(auth.WithPrincipal(context.Background(), auth.Principal{
			Subject: "root", Scopes: []string{auth.ScopeAdmin},
		}))
	srv.handleMemoryBackfillEmbeddings(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 on a tier without vectors; body: %s",
			rec.Code, rec.Body.String())
	}
	var e map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e["code"] != "backfill_unavailable" {
		t.Errorf("code = %v, want backfill_unavailable", e["code"])
	}
	// And nothing was embedded — a refused tier must not have spent embedder calls.
	if emb.calls != 0 {
		t.Errorf("embedder called %d times on a tier that cannot store the result", emb.calls)
	}
}

// TestBackfillEmbeddings_DefaultsToDryRun covers the safety default: an operator
// typing a bare `curl -X POST` gets a preview, not thousands of embedder calls
// against a metered provider. Matches /v1/_memory/reembed's posture.
//
// SKIPS rather than silently passing on a tier without vectors — the sqlite test
// store refuses at the candidate query, so the 200 path is unreachable there. A
// visible skip is the honest outcome; asserting nothing and reporting PASS is how
// a suite comes to look like coverage it does not have.
func TestBackfillEmbeddings_DefaultsToDryRun(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	emb := &backfillEmbedder{}
	srv.embedder = emb

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		// ?tenant= is EXPLICIT: an admin naming no tenant at all is refused as
		// ambiguous (see TestBackfillEmbeddings_AdminMustNameATenant), and this test
		// is about the dry-run default rather than about tenant resolution.
		"/v1/_memory/backfill_embeddings?scope=user&scope_id=alice&tenant=&prefix=doc.chunk:", nil).
		WithContext(auth.WithPrincipal(context.Background(), auth.Principal{
			Subject: "root", Scopes: []string{auth.ScopeAdmin},
		}))
	srv.handleMemoryBackfillEmbeddings(rec, req)

	if rec.Code == http.StatusServiceUnavailable {
		t.Skip("this store tier has no vector support, so the dry-run path is " +
			"unreachable here; the refusal is covered by " +
			"TestBackfillEmbeddings_UnsupportedTierSaysSoRatherThanReportingZero")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var resp memoryBackfillResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.DryRun {
		t.Error("an omitted dry_run performed a LIVE backfill")
	}
	if emb.calls != 0 {
		t.Errorf("a dry run called the embedder %d times", emb.calls)
	}
}

// TestBackfillEmbeddings_RefusesWithoutAnEmbedder — the endpoint's whole job needs
// one, and reporting 0 candidates would look like success.
func TestBackfillEmbeddings_RefusesWithoutAnEmbedder(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	srv.embedder = nil
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/v1/_memory/backfill_embeddings?scope=user&scope_id=alice", nil).
		WithContext(auth.WithPrincipal(context.Background(), auth.Principal{
			Subject: "root", Scopes: []string{auth.ScopeAdmin},
		}))
	srv.handleMemoryBackfillEmbeddings(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestBackfillEmbeddings_ValidatesScope — scope_id is required because a backfill
// with an empty one would sweep whatever the store treats as the empty scope,
// which is not what any operator means.
func TestBackfillEmbeddings_ValidatesScope(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	srv.embedder = &backfillEmbedder{}
	for _, q := range []string{
		"?scope=bogus&scope_id=alice",
		"?scope=user",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/_memory/backfill_embeddings"+q, nil).
			WithContext(auth.WithPrincipal(context.Background(), auth.Principal{
				Subject: "root", Scopes: []string{auth.ScopeAdmin},
			}))
		srv.handleMemoryBackfillEmbeddings(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, rec.Code)
		}
	}
}

// TestEmbedTextForRow_UnwrapsAChunkBody — a chunk body is a JSON envelope, and
// /v1/_memory/reembed embeds row.Value verbatim. Doing that here would index the
// literal tokens `body` and `fields`, which for a short chunk could outweigh the
// prose itself.
func TestEmbedTextForRow_UnwrapsAChunkBody(t *testing.T) {
	body := `{"body":"SAVEPOINT nesting is LIFO.","fields":null}`
	got := embedTextForRow(store.MemoryEntry{Key: "doc.chunk:x", Value: json.RawMessage(body)})
	if got != "SAVEPOINT nesting is LIFO." {
		t.Errorf("got %q, want the unwrapped body", got)
	}
	// An ordinary row keeps the existing behaviour — its whole value is the text.
	plain := embedTextForRow(store.MemoryEntry{Key: "memory/fact/x", Value: json.RawMessage(`"a fact"`)})
	if plain != `"a fact"` {
		t.Errorf("ordinary row = %q, want the raw value (unchanged behaviour)", plain)
	}
}

// pagingStore is a store whose vector half is real enough to exercise the sweep:
// it honours the afterKey cursor and records what was embedded. Everything else
// delegates to the embedded store.
//
// Needed because the sqlite test tier has no vector support, so the live embedding
// path — where the starvation bug lived — is unreachable on it.
type pagingStore struct {
	store.Store
	keys     []string // sorted; the scope's memory keys
	bodies   map[string]string
	embedded map[string]bool
}

func newPagingStore(base store.Store, bodies map[string]string) *pagingStore {
	p := &pagingStore{Store: base, bodies: bodies, embedded: map[string]bool{}}
	for k := range bodies {
		p.keys = append(p.keys, k)
	}
	sort.Strings(p.keys)
	return p
}

func (p *pagingStore) MemoryEmbedListMissing(_ context.Context, _ string, _ store.MemoryScope,
	_, keyPrefix, afterKey string, limit int) ([]store.MemoryEntry, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000 // clamp, never reset to a smaller default
	}
	var out []store.MemoryEntry
	for _, k := range p.keys {
		if p.embedded[k] || !strings.HasPrefix(k, keyPrefix) || k <= afterKey {
			continue
		}
		out = append(out, store.MemoryEntry{
			Key:   k,
			Value: json.RawMessage(`{"body":` + strconv.Quote(p.bodies[k]) + `}`),
		})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (p *pagingStore) MemoryEmbedSet(_ context.Context, _ string, _ store.MemoryScope,
	_, key string, _ store.MemoryEmbedding) error {
	p.embedded[key] = true
	return nil
}

// TestBackfillEmbeddings_PagesPastRowsItCannotEmbed is the regression test for a
// bug found by running the sweep on a live 3,143-chunk scope.
//
// A row with no body text never gains an embedding, so it stays a candidate
// forever. When `limit` bounded how many rows were LOOKED AT, those rows piled up
// at the front of the key order until they filled the whole window and throughput
// reached zero with thousands of rows still unembedded — the residue obeys
// R' = R + p·(limit − R), whose fixed point is R = limit. Observed decay per
// 200-row window: 189 / 179 / 169 / 160 / 147.
//
// FAIL-BEFORE: make the handler read only the first page (drop the paging loop) and
// this reports embedded=0 with 5 embeddable rows sitting just past the empties.
func TestBackfillEmbeddings_PagesPastRowsItCannotEmbed(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	emb := &backfillEmbedder{}
	srv.embedder = emb

	// The first `limit` rows by key are ALL unembeddable; the real work sits behind
	// them. This is the shape a document scope actually has (roots and headings sort
	// among the bodies), concentrated so a small limit reproduces it.
	bodies := map[string]string{}
	for i := 0; i < 4; i++ {
		bodies[fmt.Sprintf("doc.chunk:a%02d", i)] = "" // no text to embed, ever
	}
	for i := 0; i < 5; i++ {
		bodies[fmt.Sprintf("doc.chunk:b%02d", i)] = fmt.Sprintf("body number %d", i)
	}
	ps := newPagingStore(srv.store, bodies)
	srv.store = ps

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/v1/_memory/backfill_embeddings?scope=user&scope_id=alice&tenant=&prefix=doc.chunk:&limit=4&dry_run=false", nil).
		WithContext(auth.WithPrincipal(context.Background(), auth.Principal{
			Subject: "root", Scopes: []string{auth.ScopeAdmin},
		}))
	srv.handleMemoryBackfillEmbeddings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)

	// limit=4 bounds how many are EMBEDDED. All four unembeddable rows come first, so
	// a single-window implementation embeds nothing at all.
	if n, _ := got["embedded"].(float64); n != 4 {
		t.Errorf("embedded = %v, want 4 — the sweep must page PAST the %d rows it "+
			"cannot embed, or it starves; full body: %s", got["embedded"], 4, rec.Body.String())
	}
	if n, _ := got["skipped_empty"].(float64); n != 4 {
		t.Errorf("skipped_empty = %v, want 4 — rows with no text must be reported, not "+
			"silently passed over", got["skipped_empty"])
	}
	// One embeddable row is left over, so the caller must be told to come back.
	if got["more"] != true {
		t.Errorf("more = %v, want true with a 5th embeddable row outstanding", got["more"])
	}
	if emb.calls != 4 {
		t.Errorf("embedder called %d times, want 4 — an empty body must not reach the "+
			"embedder", emb.calls)
	}
}

// TestBackfillEmbeddings_AdminMustNameATenant — memory rows are keyed on the
// tenant, so an admin token that names none resolves to the DEFAULT tenant and the
// sweep reports a truthful-looking "candidates: 0" against a tenant the operator
// never meant, leaving the intended one untouched. Verified live on a three-tenant
// deployment before this refusal existed. Mirrors the erasure, directory and
// orphan-repair surfaces.
func TestBackfillEmbeddings_AdminMustNameATenant(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	srv.embedder = &backfillEmbedder{}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/v1/_memory/backfill_embeddings?scope=user&scope_id=alice&prefix=doc.chunk:", nil).
		WithContext(auth.WithPrincipal(context.Background(), auth.Principal{
			Subject: "root", Scopes: []string{auth.ScopeAdmin},
		}))
	srv.handleMemoryBackfillEmbeddings(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when an admin names no tenant; body: %s",
			rec.Code, rec.Body.String())
	}
	var e map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e["code"] != "tenant_required" {
		t.Errorf("code = %v, want tenant_required", e["code"])
	}
	// An EXPLICIT empty tenant is a legitimate target (the default partition), so it
	// must NOT be refused — otherwise the default tenant becomes unreachable.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost,
		"/v1/_memory/backfill_embeddings?scope=user&scope_id=alice&tenant=&prefix=doc.chunk:", nil).
		WithContext(auth.WithPrincipal(context.Background(), auth.Principal{
			Subject: "root", Scopes: []string{auth.ScopeAdmin},
		}))
	srv.handleMemoryBackfillEmbeddings(rec2, req2)
	if rec2.Code == http.StatusBadRequest && strings.Contains(rec2.Body.String(), "tenant_required") {
		t.Error("an explicit ?tenant= (the default partition) was refused; that makes the " +
			"default tenant unsweepable")
	}
}
