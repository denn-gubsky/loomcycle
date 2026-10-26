package http

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// The key routes read the same rows by key: GET one entry, and the keys list.
// A denied document's chunk body, a fact homed in it and its derived units are
// held back from them as from the search.

func memoryKeyRequest(t *testing.T, s *Server, p auth.Principal, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/_memory/scopes/{scope}/{scope_id}/keys", s.handleListMemoryEntries)
	mux.HandleFunc("GET /v1/_memory/scopes/{scope}/{scope_id}/keys/{key...}", s.handleGetMemoryEntry)
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(auth.WithPrincipal(context.Background(), p))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestMemoryKeyRoutes_ACLDeniedDocumentRowsAreAbsent(t *testing.T) {
	c := newACLSearchCorpus(t)
	unitKey := memrank.UnitKey(c.unit, memrank.UnitClaim, 0)
	missing := memoryKeyRequest(t, c.s, auth.Principal{TenantID: "A", Subject: "op", Scopes: []string{auth.ScopeAdmin}},
		"/v1/_memory/scopes/user/alice/keys/doc.chunk:nope")
	for name, p := range limitedCallers(t) {
		for _, key := range []string{"doc.chunk:" + c.report, "doc.chunk:" + c.fact, "doc.chunk:" + c.unit, unitKey} {
			rec := memoryKeyRequest(t, c.s, p, "/v1/_memory/scopes/user/alice/keys/"+key)
			if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "supplier audit") {
				t.Errorf("%s GET %s: %d %s, want 404", name, key, rec.Code, rec.Body.String())
			}
			// The refusal is the missing key's, so it does not say the row exists.
			want := strings.ReplaceAll(missing.Body.String(), "doc.chunk:nope", key)
			if rec.Body.String() != want {
				t.Errorf("%s GET %s: body %s, want the missing-key body %s", name, key, rec.Body.String(), want)
			}
		}
		for _, key := range []string{"doc.chunk:" + c.norm, "notes/n1", "facts/f1"} {
			if rec := memoryKeyRequest(t, c.s, p, "/v1/_memory/scopes/user/alice/keys/"+key); rec.Code != http.StatusOK {
				t.Errorf("%s GET %s: %d %s, want 200", name, key, rec.Code, rec.Body.String())
			}
		}
		for _, q := range []string{"", "?prefix=doc.", "?prefix=doc.unit:", "?include_embedding_metadata=true"} {
			rec := memoryKeyRequest(t, c.s, p, "/v1/_memory/scopes/user/alice/keys"+q)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s list%s: %d %s", name, q, rec.Code, rec.Body.String())
			}
			for _, id := range []string{c.report, c.fact, c.unit} {
				if strings.Contains(rec.Body.String(), id) {
					t.Errorf("%s list%s: names the read-denied chunk %s: %s", name, q, id, rec.Body.String())
				}
			}
			if q != "?prefix=doc.unit:" && !strings.Contains(rec.Body.String(), c.norm) {
				t.Errorf("%s list%s: the readable norm %s is missing: %s", name, q, c.norm, rec.Body.String())
			}
		}
	}
}

func TestMemoryKeyRoutes_UnrestrictedCallerAnswerIsUnchanged(t *testing.T) {
	c := newACLSearchCorpus(t)
	admin := auth.Principal{TenantID: "A", Subject: "op", Scopes: []string{auth.ScopeAdmin}}
	unlimited := auth.Principal{TenantID: "A", Subject: "bob", Scopes: []string{auth.ScopeRunsCreate}, Effective: allowDocumentsSet(t, "bob")}
	for _, path := range []string{
		"/v1/_memory/scopes/user/alice/keys",
		"/v1/_memory/scopes/user/alice/keys?include_embedding_metadata=true",
		"/v1/_memory/scopes/user/alice/keys/doc.chunk:" + c.report,
		"/v1/_memory/scopes/user/alice/keys/" + memrank.UnitKey(c.unit, memrank.UnitClaim, 0),
	} {
		plain := memoryKeyRequest(t, c.s, admin, path)
		if plain.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, plain.Code, plain.Body.String())
		}
		if strings.HasSuffix(path, "/keys") && !strings.Contains(plain.Body.String(), c.report) {
			t.Errorf("%s: an unrestricted caller lost the report chunk: %s", path, plain.Body.String())
		}
		if got := memoryKeyRequest(t, c.s, unlimited, path); !bytes.Equal(got.Body.Bytes(), plain.Body.Bytes()) {
			t.Errorf("%s: a set that denies nothing changed the answer:\n got %s\nwant %s", path, got.Body.String(), plain.Body.String())
		}
	}
}
