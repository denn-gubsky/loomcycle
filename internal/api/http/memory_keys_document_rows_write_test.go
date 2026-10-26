package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The memory key routes write the keyspace Document chunk bodies and their
// derived units live in. A raw write there skips the Document tool's revision
// check and its scope grants, so these routes refuse what the Memory tool
// refuses, for every caller: no value write to a chunk body or a unit, and no
// removal of a unit or a prose body. A consolidated fact's body row carries
// provenance and stays deletable by its id.

func memoryKeyWrite(t *testing.T, s *Server, p auth.Principal, method, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	if s.cfgHolder == nil {
		s.cfgHolder = config.NewHolder(&config.Config{})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/_memory/scopes/{scope}/{scope_id}/keys/{key...}", s.handlePutMemoryEntry)
	mux.HandleFunc("DELETE /v1/_memory/scopes/{scope}/{scope_id}/keys/{key...}", s.handleDeleteMemoryEntry)
	req := httptest.NewRequest(method, "/v1/_memory/scopes/user/alice/keys/"+key, strings.NewReader(body)).
		WithContext(auth.WithPrincipal(context.Background(), p))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// everyCaller is each kind of principal the key routes admit, with no access
// rule in play: the refusal is the namespace's, not an access decision.
func everyCaller() map[string]auth.Principal {
	return map[string]auth.Principal{
		"member":          {TenantID: "A", Subject: "bob", Scopes: []string{auth.ScopeRunsCreate}},
		"tenant operator": {TenantID: "A", Subject: "op", Scopes: []string{auth.ScopeTenant}},
		"admin":           {TenantID: "A", Subject: "root", Scopes: []string{auth.ScopeAdmin}},
	}
}

func assertDocumentRowRefusal(t *testing.T, what string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"document_row"`) ||
		!strings.Contains(rec.Body.String(), "/v1/_document") {
		t.Errorf("%s: %d %s, want 400 document_row pointing at /v1/_document", what, rec.Code, rec.Body.String())
	}
}

func TestMemoryKeyPut_DocumentRowsAreRefusedForEveryCaller(t *testing.T) {
	c := newACLSearchCorpus(t)
	ctx := context.Background()
	unitKey := memrank.UnitKey(c.unit, memrank.UnitClaim, 0)
	for name, p := range everyCaller() {
		for _, key := range []string{"doc.chunk:" + c.report, "doc.chunk:" + c.fact, "doc.chunk:brand-new", unitKey, "doc.unit:brand-new:claim:0"} {
			before, _ := c.s.store.MemoryGet(ctx, "A", store.MemoryScopeUser, "alice", key)
			rec := memoryKeyWrite(t, c.s, p, http.MethodPut, key, `{"value":"overwritten"}`)
			assertDocumentRowRefusal(t, name+" PUT "+key, rec)
			after, err := c.s.store.MemoryGet(ctx, "A", store.MemoryScopeUser, "alice", key)
			if string(after.Value) != string(before.Value) || (err == nil) != (len(before.Value) > 0) {
				t.Errorf("%s PUT %s: the row changed: %s → %s", name, key, before.Value, after.Value)
			}
		}
		if rec := memoryKeyWrite(t, c.s, p, http.MethodPut, "notes/ok", `{"value":"fine"}`); rec.Code != http.StatusOK {
			t.Errorf("%s PUT an ordinary key: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestMemoryKeyDelete_ProseBodyAndUnitAreRefusedFactIsDeletable(t *testing.T) {
	ctx := context.Background()
	for name, p := range everyCaller() {
		c := newACLSearchCorpus(t)
		unitKey := memrank.UnitKey(c.unit, memrank.UnitClaim, 0)
		for _, key := range []string{"doc.chunk:" + c.report, unitKey} {
			rec := memoryKeyWrite(t, c.s, p, http.MethodDelete, key, "")
			assertDocumentRowRefusal(t, name+" DELETE "+key, rec)
			if _, err := c.s.store.MemoryGet(ctx, "A", store.MemoryScopeUser, "alice", key); err != nil {
				t.Errorf("%s DELETE %s: the row is gone: %v", name, key, err)
			}
		}
		// A consolidated fact homed in a chunk is removed by the id recall reported.
		if rec := memoryKeyWrite(t, c.s, p, http.MethodDelete, "doc.chunk:"+c.fact, ""); rec.Code != http.StatusNoContent {
			t.Errorf("%s DELETE the fact: %d %s, want 204", name, rec.Code, rec.Body.String())
		}
		var nf *store.ErrNotFound
		if _, err := c.s.store.MemoryGet(ctx, "A", store.MemoryScopeUser, "alice", "doc.chunk:"+c.fact); !errors.As(err, &nf) {
			t.Errorf("%s DELETE the fact: still there (err %v)", name, err)
		}
		// A key with no row is a no-op, as before.
		if rec := memoryKeyWrite(t, c.s, p, http.MethodDelete, "doc.chunk:never-written", ""); rec.Code != http.StatusNoContent {
			t.Errorf("%s DELETE a missing chunk key: %d %s, want 204", name, rec.Code, rec.Body.String())
		}
	}
}

// provenanceFaultStore cannot read provenance, so no chunk body can be classified.
type provenanceFaultStore struct{ store.Store }

func (provenanceFaultStore) MemoryProvenanceGet(context.Context, string, store.MemoryScope, string, string) (store.MemoryProvenance, error) {
	return store.MemoryProvenance{}, errors.New("provenance read failed")
}

// TestMemoryKeyDelete_UnclassifiableBodyIsNotRemoved — when the row cannot be
// told apart from a prose body, it is not removed.
func TestMemoryKeyDelete_UnclassifiableBodyIsNotRemoved(t *testing.T) {
	c := newACLSearchCorpus(t)
	real := c.s.store
	c.s.store = provenanceFaultStore{real}
	rec := memoryKeyWrite(t, c.s, everyCaller()["admin"], http.MethodDelete, "doc.chunk:"+c.fact, "")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("DELETE an unclassifiable body: %d %s, want 500", rec.Code, rec.Body.String())
	}
	if _, err := real.MemoryGet(context.Background(), "A", store.MemoryScopeUser, "alice", "doc.chunk:"+c.fact); err != nil {
		t.Errorf("the row was removed: %v", err)
	}
}
