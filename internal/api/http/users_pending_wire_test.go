package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// "pending" is a store status only a gate sets and only approval clears: the
// users CRUD refuses it on create and on update.
func TestHandleUsersCRUD_PendingStatusIsNotSettable(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	acme := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	rec := httptest.NewRecorder()
	s.handleCreateUser(rec, bodyReq("POST", "/v1/_users", `{"subject":"bob","status":"pending"}`, acme, "", ""))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create pending = %d, want 400", rec.Code)
	}
	if err := st.UserCreate(context.Background(), store.UserRow{TenantID: "acme", Subject: "carol", AccessMode: "isolated", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	s.handleUpdateUser(rec, bodyReq("PATCH", "/v1/_users/carol", `{"status":"pending"}`, acme, "subject", "carol"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("patch to pending = %d, want 400", rec.Code)
	}
}
