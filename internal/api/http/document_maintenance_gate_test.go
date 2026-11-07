package http

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A tenant operator runs the document maintenance passes on its own tenant, as
// it runs the memory sweeps: the route gate no longer stops it at the /v1/_*
// admin catch-all before the tenant-confining handler. A member is still
// refused, and the projection stays operator-admin.
func TestDocumentMaintenance_ATenantOperatorPassesTheGateAMemberDoesNot(t *testing.T) {
	_, ts, st := restrictedFixtureServer(t)
	const opBearer = "tenant-operator-secret"
	if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: "def_op", Name: "op", TenantID: "acme", Subject: "op",
		TokenHash: auth.HashToken(testTokenPepper, opBearer), AllowedScopes: []string{auth.ScopeTenant},
	}); err != nil {
		t.Fatal(err)
	}
	refusedByGate := func(code int, body string) bool {
		return code == 403 && strings.Contains(body, "insufficient_scope")
	}
	for _, route := range []string{"describe_images", "reindex", "derive_units"} {
		path := "/v1/_document/" + route + "?scope=tenant"
		if code, body := bearerCall(t, ts, opBearer, "POST", path, ""); refusedByGate(code, body) {
			t.Errorf("tenant operator %s: refused by the scope gate: %d %s", route, code, body)
		}
		if code, body := bearerCall(t, ts, freeBearer, "POST", path, ""); code != 403 {
			t.Errorf("member %s: %d %s, want 403", route, code, body)
		}
	}
	if code, body := bearerCall(t, ts, opBearer, "POST", "/v1/_document/project?scope=tenant", ""); !refusedByGate(code, body) {
		t.Errorf("tenant operator project: %d %s, want the admin gate's 403", code, body)
	}
}
