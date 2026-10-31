package grpc

import (
	"context"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// The RPC hands its request to the connector, where the rules and the binding
// live, and renders what comes back; a refusal carries the shared code as its
// ErrorInfo reason.
func TestBindSubjectPrincipal_DelegatesToTheConnector(t *testing.T) {
	mc := &mockConnector{bindRes: connector.BindSubjectPrincipalResult{
		Tenant: "acme", DocumentID: "d1", RootChunkID: "r1", NaturalKey: "person:ada", Path: "/facts/ada",
		Principal: "u-ada", Previous: "u-old", Changed: true,
	}}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	resp, err := client.BindSubjectPrincipal(context.Background(), &loomcyclepb.BindSubjectPrincipalRequest{
		Subject: "person:ada", Principal: "u-ada", Tenant: "globex",
	})
	if err != nil {
		t.Fatalf("BindSubjectPrincipal: %v", err)
	}
	if got := mc.bindReq; got.Subject != "person:ada" || got.Principal != "u-ada" || got.Tenant != "globex" {
		t.Errorf("connector got %+v", got)
	}
	if resp.GetTenant() != "acme" || resp.GetDocumentId() != "d1" || resp.GetRootChunkId() != "r1" || resp.GetNaturalKey() != "person:ada" ||
		resp.GetPath() != "/facts/ada" || resp.GetPrincipal() != "u-ada" || resp.GetPrevious() != "u-old" || !resp.GetChanged() {
		t.Errorf("response = %+v", resp)
	}

	for sentinel, want := range map[error]codes.Code{
		connector.ErrSubjectBindInvalid:           codes.InvalidArgument,
		connector.ErrSubjectBindNotSubject:        codes.InvalidArgument,
		connector.ErrSubjectBindForbidden:         codes.PermissionDenied,
		connector.ErrSubjectBindNotFound:          codes.NotFound,
		connector.ErrSubjectBindPrincipalNotFound: codes.NotFound,
		connector.ErrSubjectBindConflict:          codes.FailedPrecondition,
		connector.ErrSubjectBindUnavailable:       codes.Unavailable,
	} {
		mc.bindErr = connector.WithMessage(sentinel, "why")
		_, err := client.BindSubjectPrincipal(context.Background(), &loomcyclepb.BindSubjectPrincipalRequest{Subject: "a", Principal: "b"})
		st := status.Convert(err)
		if st.Code() != want {
			t.Errorf("%v maps to %v, want %v", sentinel, st.Code(), want)
		}
		reason := ""
		for _, d := range st.Details() {
			if info, ok := d.(*errdetails.ErrorInfo); ok {
				reason = info.GetReason()
			}
		}
		if reason != connector.SubjectBindCode(sentinel) || reason == "" {
			t.Errorf("%v: reason %q, want %q", sentinel, reason, connector.SubjectBindCode(sentinel))
		}
	}
}

// Who may call it on this transport: an admin and a tenant operator. Its HTTP
// route is carved out of the member path, so a member is refused here too; a
// token limited to certain agents is refused whatever its scopes.
func TestBindSubjectPrincipal_IsOperatorOnly(t *testing.T) {
	const method = grpcMethodPrefix + "BindSubjectPrincipal"
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
