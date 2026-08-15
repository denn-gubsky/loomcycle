package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// TestMemoryChanges_AuthGate pins the locked decision: both change feeds are
// substrate:tenant AND carved out of member access (not member-accessible).
func TestMemoryChanges_AuthGate(t *testing.T) {
	for _, p := range []string{"/v1/_memory/changes", "/v1/_document/changes"} {
		if got := requiredScopeFor(http.MethodGet, p); got != auth.ScopeTenant {
			t.Errorf("%s scope = %q, want %s", p, got, auth.ScopeTenant)
		}
		if tenantMemberAccessible(http.MethodGet, p) {
			t.Errorf("%s must NOT be member-accessible (memberCarveOut)", p)
		}
	}
}

// TestMemoryChanges_SSEStreamsAndFilters seeds one memory + one document change
// and asserts each feed streams only its own family (tenant-scoped).
func TestMemoryChanges_SSEStreamsAndFilters(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.AppendMemoryChange(ctx, store.MemoryChange{Type: store.MemoryChangeSet, Scope: store.MemoryScopeAgent, ScopeID: "a1", Key: "k1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendMemoryChange(ctx, store.MemoryChange{Type: store.DocumentChangeUpdated, Scope: store.MemoryScopeAgent, ScopeID: "a1", ChunkID: "CID"}); err != nil {
		t.Fatal(err)
	}

	srv := &Server{store: st, cfg: &config.Config{}}

	mem := runSSEOnce(t, srv.handleMemoryChanges, "/v1/_memory/changes")
	if !strings.Contains(mem, `"memory.set"`) || !strings.Contains(mem, `"k1"`) {
		t.Errorf("memory feed missing its change: %s", mem)
	}
	if strings.Contains(mem, `"document.chunk.updated"`) {
		t.Errorf("memory feed leaked a document change: %s", mem)
	}

	doc := runSSEOnce(t, srv.handleDocumentChanges, "/v1/_document/changes")
	if !strings.Contains(doc, `"document.chunk.updated"`) || !strings.Contains(doc, `"CID"`) {
		t.Errorf("document feed missing its change: %s", doc)
	}
	if strings.Contains(doc, `"memory.set"`) {
		t.Errorf("document feed leaked a memory change: %s", doc)
	}
}

// runSSEOnce invokes an SSE handler with a short-lived context: it drains the
// already-present changes on entry, then returns when the context times out.
func runSSEOnce(t *testing.T, h http.HandlerFunc, path string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Body.String()
}
