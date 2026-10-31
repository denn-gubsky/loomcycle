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

// The RPC hands its request to the connector, where the rules and the merge
// live, and renders what comes back; a refusal carries the shared code as its
// ErrorInfo reason.
func TestMergeSubjects_DelegatesToTheConnector(t *testing.T) {
	mc := &mockConnector{mergeRes: connector.MergeSubjectsResult{
		Tenant:     "acme",
		Duplicate:  connector.SubjectRef{DocumentID: "d1", RootChunkID: "r1", NaturalKey: "person:ada-king"},
		Survivor:   connector.SubjectRef{DocumentID: "d2", RootChunkID: "r2", NaturalKey: "person:ada-lovelace"},
		FactsMoved: 3, StatementsRepointed: 2, StatementsRetired: 1, StatementsLeft: 1, NotAnnounced: "why",
	}}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	resp, err := client.MergeSubjects(context.Background(), &loomcyclepb.MergeSubjectsRequest{
		Duplicate: "person:ada-king", Survivor: "d2", Tenant: "globex",
	})
	if err != nil {
		t.Fatalf("MergeSubjects: %v", err)
	}
	if got := mc.mergeReq; got.Duplicate != "person:ada-king" || got.Survivor != "d2" || got.Tenant != "globex" {
		t.Errorf("connector got %+v", got)
	}
	if resp.GetTenant() != "acme" || resp.GetDuplicate().GetRootChunkId() != "r1" || resp.GetSurvivor().GetNaturalKey() != "person:ada-lovelace" ||
		resp.GetFactsMoved() != 3 || resp.GetStatementsRepointed() != 2 || resp.GetStatementsRetired() != 1 ||
		resp.GetStatementsLeft() != 1 || resp.GetNotAnnounced() != "why" {
		t.Errorf("response = %+v", resp)
	}

	for sentinel, want := range map[error]codes.Code{
		connector.ErrSubjectMergeInvalid:       codes.InvalidArgument,
		connector.ErrSubjectMergeSame:          codes.InvalidArgument,
		connector.ErrSubjectMergeNotSubject:    codes.InvalidArgument,
		connector.ErrSubjectMergeForbidden:     codes.PermissionDenied,
		connector.ErrSubjectMergeNotInScope:    codes.NotFound,
		connector.ErrSubjectMergeAlreadyMerged: codes.FailedPrecondition,
		connector.ErrSubjectMergeConflict:      codes.FailedPrecondition,
		connector.ErrSubjectMergeUnavailable:   codes.Unavailable,
	} {
		mc.mergeErr = connector.WithMessage(sentinel, "why")
		_, err := client.MergeSubjects(context.Background(), &loomcyclepb.MergeSubjectsRequest{Duplicate: "a", Survivor: "b"})
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
		if reason != connector.SubjectMergeCode(sentinel) {
			t.Errorf("%v: reason %q, want %q", sentinel, reason, connector.SubjectMergeCode(sentinel))
		}
	}
}

// Who may call it on this transport: an admin and a tenant operator. Its HTTP
// route is carved out of the member path, so a member is refused here too; a
// token limited to certain agents is refused whatever its scopes.
func TestMergeSubjects_IsOperatorOnly(t *testing.T) {
	const method = grpcMethodPrefix + "MergeSubjects"
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
