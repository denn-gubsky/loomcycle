package grpc

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// The RPC hands its request to the connector, where the rules and the audit
// live, and renders what comes back.
func TestRetentionOverride_DelegatesToTheConnector(t *testing.T) {
	mc := &mockConnector{retentionRes: connector.RetentionOverrideResult{
		Cleared:   true,
		Overrides: []connector.RetentionOverride{{Tenant: "acme", Agent: "judge", Until: "2026-10-12T18:00:00Z", SetBy: "ops", CreatedAt: "2026-10-12T16:00:00Z"}},
	}}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	resp, err := client.RetentionOverride(context.Background(), &loomcyclepb.RetentionOverrideRequest{
		Op: "set", Tenant: "globex", Agent: "judge", Until: "2026-10-12T18:00:00Z",
	})
	if err != nil {
		t.Fatalf("RetentionOverride: %v", err)
	}
	want := time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC)
	if got := mc.retentionReq; got.Op != "set" || got.Tenant != "globex" || got.Agent != "judge" || !got.Until.Equal(want) {
		t.Errorf("connector got %+v", got)
	}
	if len(resp.GetOverrides()) != 1 || !resp.GetCleared() {
		t.Fatalf("response = %+v", resp)
	}
	if o := resp.GetOverrides()[0]; o.GetTenant() != "acme" || o.GetAgent() != "judge" || o.GetUntil() != "2026-10-12T18:00:00Z" || o.GetSetBy() != "ops" || o.GetCreatedAt() != "2026-10-12T16:00:00Z" {
		t.Errorf("entry = %+v", o)
	}

	// An until that is not a time never reaches the connector.
	mc.retentionReq = connector.RetentionOverrideRequest{}
	_, err = client.RetentionOverride(context.Background(), &loomcyclepb.RetentionOverrideRequest{Op: "set", Agent: "judge", Until: "tomorrow"})
	if status.Code(err) != codes.InvalidArgument || mc.retentionReq.Op != "" {
		t.Errorf("a bad until: err = %v, connector got %+v", err, mc.retentionReq)
	}

	for sentinel, want := range map[error]codes.Code{
		connector.ErrRetentionOverrideInvalid:          codes.InvalidArgument,
		connector.ErrRetentionOverrideForbidden:        codes.PermissionDenied,
		connector.ErrRetentionOverrideAuditUnavailable: codes.Unavailable,
		connector.ErrRetentionOverrideStoreUnavailable: codes.FailedPrecondition,
	} {
		mc.retentionErr = connector.WithMessage(sentinel, "why")
		_, err := client.RetentionOverride(context.Background(), &loomcyclepb.RetentionOverrideRequest{Op: "list"})
		if status.Code(err) != want {
			t.Errorf("%v maps to %v, want %v", sentinel, status.Code(err), want)
		}
	}
}

// Who may call it on this transport: an admin and a tenant operator. Its HTTP
// routes are carved out of the member path, so a member is refused here too; a
// token limited to certain agents is refused whatever its scopes.
func TestRetentionOverride_IsOperatorOnly(t *testing.T) {
	const method = grpcMethodPrefix + "RetentionOverride"
	if got := requiredScopeForRPC(method); got != auth.ScopeTenant {
		t.Fatalf("required scope = %q, want %q", got, auth.ScopeTenant)
	}
	as := func(scopes []string, r auth.Restrictions) context.Context {
		return auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "s", Scopes: scopes, Restrictions: r})
	}
	for what, tc := range map[string]struct {
		ctx  context.Context
		want bool
	}{
		"an admin":           {as([]string{auth.ScopeAdmin}, auth.Restrictions{}), true},
		"a tenant operator":  {as([]string{auth.ScopeTenant}, auth.Restrictions{}), true},
		"a member":           {as([]string{auth.ScopeRunsCreate, auth.ScopeRunsRead}, auth.Restrictions{}), false},
		"an isolated user":   {as([]string{auth.ScopeUser}, auth.Restrictions{}), false},
		"a restricted token": {as([]string{auth.ScopeTenant}, auth.Restrictions{Agents: []string{"judge"}}), false},
	} {
		if err := enforceScope(tc.ctx, method); (err == nil) != tc.want {
			t.Errorf("%s: enforceScope = %v, want allowed=%v", what, err, tc.want)
		}
	}
}
