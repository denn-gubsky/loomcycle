package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		"/v1/_memory/backfill_embeddings?scope=user&scope_id=alice&prefix=doc.chunk:&dry_run=false", nil).
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
		"/v1/_memory/backfill_embeddings?scope=user&scope_id=alice&prefix=doc.chunk:", nil).
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
