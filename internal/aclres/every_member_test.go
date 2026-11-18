package aclres

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// readReq is the question EveryMemberSets' sets answer for a tenant document.
func readReq(e *authz.Effective, path string) authz.Request {
	return authz.Request{Subject: e.Subject, Action: "read", Resource: authz.Resource{Type: "document", Name: "tenant:" + path}}
}

// TestEveryMemberSets_ADenyBoundToAnyoneRestricts — a deny bound to a group, a user,
// a token, the tenant or (in config) a group restricts a document for "every
// member"; only the tenant-wide one reaches the plain member's set, whose baseline
// lets it read the rest. A retired or expired binding restricts nothing.
func TestEveryMemberSets_ADenyBoundToAnyoneRestricts(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	put := func(name, path string) {
		t.Helper()
		raw, _ := json.Marshal([]authz.Rule{{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:" + path}}})
		if _, err := st.AclDefPut(ctx, store.AclDefRow{TenantID: "acme", Name: name, Rules: raw}); err != nil {
			t.Fatal(err)
		}
	}
	bind := func(row store.AclBindingRow) store.AclBindingRow {
		t.Helper()
		row.TenantID = "acme"
		out, err := st.AclBindingPut(ctx, row)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	for name, path := range map[string]string{"g": "/g/**", "u": "/u/**", "k": "/k/**", "t": "/t/**", "r": "/r/**", "x": "/x/**", "s": "/s/**"} {
		put(name, path)
	}
	bind(store.AclBindingRow{ACLName: "g", SubjectKind: "group", SubjectID: "contractors"})
	bind(store.AclBindingRow{ACLName: "u", SubjectKind: "user", SubjectID: "bob"})
	bind(store.AclBindingRow{ACLName: "k", SubjectKind: "token", SubjectID: "def-1"})
	bind(store.AclBindingRow{ACLName: "t", SubjectKind: "tenant"})
	retired := bind(store.AclBindingRow{ACLName: "r", SubjectKind: "group", SubjectID: "contractors"})
	if _, err := st.AclBindingRetire(ctx, "acme", retired.BindingID); err != nil {
		t.Fatal(err)
	}
	bind(store.AclBindingRow{ACLName: "x", SubjectKind: "user", SubjectID: "carol", ExpiresAt: time.Now().Add(-time.Hour)})
	r := New(st, authz.ModeEnforce, 0)
	r.SetStatic(func() *authz.Static {
		return &authz.Static{Bindings: []authz.StaticBinding{{Tenant: "acme", Binding: authz.Binding{ACL: "s", SubjectKind: authz.SubjectGroup, SubjectID: "staff"}}}}
	})

	member, denies, err := r.EveryMemberSets(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{"/g/a": true, "/u/a": true, "/k/a": true, "/t/a": true, "/s/a": true,
		"/r/a": false, "/x/a": false, "/free": false} {
		if got := denies.DeniesUnattended(readReq(denies, path)); got != want {
			t.Errorf("denies %s = %v, want %v", path, got, want)
		}
	}
	for path, want := range map[string]authz.Outcome{"/t/a": authz.Denied, "/g/a": authz.Allowed, "/free": authz.Allowed} {
		if got := member.Decide(readReq(member, path)).Outcome; got != want {
			t.Errorf("plain member %s = %v, want %v", path, got, want)
		}
	}
}
