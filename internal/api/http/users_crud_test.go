package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// bodyReq builds a request carrying a JSON body + a stamped principal (what
// the middleware does in production). key/val set a path value when non-empty
// (Go 1.22+ req.SetPathValue, since these handlers read r.PathValue).
func bodyReq(method, target, body string, p auth.Principal, key, val string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if key != "" {
		req.SetPathValue(key, val)
	}
	return req.WithContext(auth.WithPrincipal(req.Context(), p))
}

// A substrate:tenant operator manages ONLY its own tenant's users: create
// stamps the principal's authoritative tenant, and update/delete of another
// tenant's user is an opaque 404 that leaves the target untouched.
func TestHandleUsersCRUD_TenantOperatorConfined(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}

	// A user in tenant "other", seeded directly, is the cross-tenant target.
	if err := st.UserCreate(ctx, store.UserRow{TenantID: "other", Subject: "carol", AccessMode: "tenant", Status: "active"}); err != nil {
		t.Fatalf("seed other/carol: %v", err)
	}

	// create — tenant is server-derived (never from the body); created_by is
	// the principal subject.
	rec := httptest.NewRecorder()
	s.handleCreateUser(rec, bodyReq("POST", "/v1/_users", `{"subject":"alice","display_name":"Alice","access_mode":"isolated"}`, acme, "", ""))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status %d: %s", rec.Code, rec.Body.String())
	}
	var created wireUserRecord
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.TenantID != "acme" || created.Subject != "alice" || created.AccessMode != "isolated" || created.CreatedBy != "ops" {
		t.Errorf("created = %+v, want acme/alice/isolated created_by=ops", created)
	}

	// duplicate → 409.
	rec = httptest.NewRecorder()
	s.handleCreateUser(rec, bodyReq("POST", "/v1/_users", `{"subject":"alice"}`, acme, "", ""))
	if rec.Code != http.StatusConflict {
		t.Errorf("duplicate create = %d, want 409", rec.Code)
	}

	// bad access_mode → 400.
	rec = httptest.NewRecorder()
	s.handleCreateUser(rec, bodyReq("POST", "/v1/_users", `{"subject":"x","access_mode":"wat"}`, acme, "", ""))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad access_mode = %d, want 400", rec.Code)
	}

	// cross-tenant update → opaque 404; other/carol untouched.
	rec = httptest.NewRecorder()
	s.handleUpdateUser(rec, bodyReq("PATCH", "/v1/_users/carol", `{"status":"disabled"}`, acme, "subject", "carol"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant update = %d, want 404", rec.Code)
	}
	if carol, _ := st.UserGet(ctx, "other", "carol"); carol.Status != "active" {
		t.Errorf("other/carol.status = %q after acme's cross-tenant PATCH, want active (untouched)", carol.Status)
	}

	// own-tenant update — changes apply, unspecified fields preserved.
	rec = httptest.NewRecorder()
	s.handleUpdateUser(rec, bodyReq("PATCH", "/v1/_users/alice", `{"status":"disabled","display_name":"Alice A."}`, acme, "subject", "alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("own update status %d: %s", rec.Code, rec.Body.String())
	}
	var updated wireUserRecord
	_ = json.Unmarshal(rec.Body.Bytes(), &updated)
	if updated.Status != "disabled" || updated.DisplayName != "Alice A." || updated.AccessMode != "isolated" {
		t.Errorf("updated = %+v, want status+name changed, access_mode preserved", updated)
	}

	// cross-tenant delete → opaque 404; other/carol survives.
	rec = httptest.NewRecorder()
	s.handleDeleteUser(rec, bodyReq("DELETE", "/v1/_users/carol", "", acme, "subject", "carol"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant delete = %d, want 404", rec.Code)
	}
	if _, err := st.UserGet(ctx, "other", "carol"); err != nil {
		t.Errorf("other/carol gone after acme's cross-tenant DELETE: %v", err)
	}

	// own delete → 204; a second delete → 404.
	rec = httptest.NewRecorder()
	s.handleDeleteUser(rec, bodyReq("DELETE", "/v1/_users/alice", "", acme, "subject", "alice"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("own delete = %d, want 204", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.handleDeleteUser(rec, bodyReq("DELETE", "/v1/_users/alice", "", acme, "subject", "alice"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", rec.Code)
	}
}

// GET /v1/_users merges the users-table rows over the run-derived activity:
// a subject in both is enriched, a run-only subject is registered:false with
// empty record fields, and a registered subject with no runs still appears.
func TestHandleListUsers_MergesTableAndRunRows(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()

	// alice + bob have runs in tenant acme.
	sessA, _ := st.CreateSession(ctx, "acme", "echo", "alice")
	_, _ = st.CreateRun(ctx, sessA.ID, store.RunIdentity{AgentID: "a1", UserID: "alice", TenantID: "acme"})
	sessB, _ := st.CreateSession(ctx, "acme", "echo", "bob")
	_, _ = st.CreateRun(ctx, sessB.ID, store.RunIdentity{AgentID: "a2", UserID: "bob", TenantID: "acme"})

	// alice is ALSO registered (merge); carol is registered with NO runs.
	if err := st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "alice", DisplayName: "Alice", AccessMode: "isolated", Status: "active"}); err != nil {
		t.Fatalf("register alice: %v", err)
	}
	if err := st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: "carol", DisplayName: "Carol", AccessMode: "tenant", Status: "disabled"}); err != nil {
		t.Fatalf("register carol: %v", err)
	}

	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	rec := httptest.NewRecorder()
	s.handleListUsers(rec, principalReq("GET", "/v1/_users", acme))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Users []wireUserSummary `json:"users"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	byID := map[string]wireUserSummary{}
	for _, u := range resp.Users {
		byID[u.UserID] = u
	}

	// alice: activity AND record fields merged onto one row.
	if a := byID["alice"]; !a.Registered || a.DisplayName != "Alice" || a.AccessMode != "isolated" || a.TotalCount != 1 {
		t.Errorf("alice = %+v, want registered w/ record fields AND total=1", a)
	}
	// bob: runs only — not registered, empty record fields.
	if b := byID["bob"]; b.Registered || b.DisplayName != "" || b.AccessMode != "" || b.TotalCount != 1 {
		t.Errorf("bob = %+v, want registered=false, empty record fields, total=1", b)
	}
	// carol: registered only — appears with zero activity.
	if c := byID["carol"]; !c.Registered || c.Status != "disabled" || c.TotalCount != 0 {
		t.Errorf("carol = %+v, want registered w/ status=disabled AND zero activity", c)
	}
}

// The POST /v1/_users gate is tenant-operator-scoped while GET stays open to
// any authenticated principal; the /v1/_users/ prefix (PATCH/DELETE) is
// tenant-operator-scoped too (RFC BX P2a).
func TestRequiredScopeFor_UsersCRUDGate(t *testing.T) {
	if got := requiredScopeFor("GET", "/v1/_users"); got != "" {
		t.Errorf("GET /v1/_users = %q, want \"\" (any authenticated)", got)
	}
	if got := requiredScopeFor("POST", "/v1/_users"); got != auth.ScopeTenant {
		t.Errorf("POST /v1/_users = %q, want %q", got, auth.ScopeTenant)
	}
	if got := requiredScopeFor("PATCH", "/v1/_users/alice"); got != auth.ScopeTenant {
		t.Errorf("PATCH /v1/_users/{subject} = %q, want %q", got, auth.ScopeTenant)
	}
	if got := requiredScopeFor("DELETE", "/v1/_users/alice"); got != auth.ScopeTenant {
		t.Errorf("DELETE /v1/_users/{subject} = %q, want %q", got, auth.ScopeTenant)
	}
}
