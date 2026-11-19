package grpc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

func reasonOf(err error) (codes.Code, string, string) {
	st := status.Convert(err)
	reason := ""
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			reason = info.GetReason()
		}
	}
	return st.Code(), reason, st.Message()
}

// The RPC hands its changes and the tenant to the connector, where the rules and
// the plan live, and renders every field of what comes back; a refusal carries
// HTTP's code as its ErrorInfo reason, and an internal failure hides its text.
func TestPlanOntology_DelegatesToTheConnector(t *testing.T) {
	mc := &mockConnector{planRes: connector.PlanOntologyResult{
		Tenant: "acme", Fingerprint: "fp", Issues: []string{"i1"}, NewIssues: []string{"n1"},
		ScopesCounted: 3, ScopesSkipped: 1, Complete: false,
		Changes: []connector.OntologyPlannedChange{{
			Op: "delete", Risk: connector.OntologyRiskDestroys, Notes: []string{"note"},
			Entry:   connector.OntologyPlanEntry{ChunkID: "c1", Name: "Provision", Section: "classes", Kind: "class"},
			Subtree: []connector.OntologyPlanEntry{{ChunkID: "c2", Name: "Article", Section: "classes", Kind: "class"}},
			Counts: map[string]connector.OntologyPlanCount{"statements": {
				N: 4, Effect: connector.OntologyEffectDestroyed, TenantScope: 3, OtherScopes: 1,
				ByStatus: map[string]int{"asserted": 3, "proposed": 1}, Sample: []string{"s1", "s2"},
			}},
		}},
	}}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()

	resp, err := client.PlanOntology(context.Background(), &loomcyclepb.PlanOntologyRequest{
		Tenant: "globex",
		Changes: []*loomcyclepb.OntologyChange{
			{Op: "delete", Entry: "c1"}, {Op: "rename", Entry: "c2", Name: "Clause"},
			{Op: "move", Entry: "c3", Parent: "c4"}, {Op: "edit", Entry: "c5", Body: "b"},
		},
	})
	if err != nil {
		t.Fatalf("PlanOntology: %v", err)
	}
	want := []connector.OntologyChange{
		{Op: "delete", Entry: "c1"}, {Op: "rename", Entry: "c2", Name: "Clause"},
		{Op: "move", Entry: "c3", Parent: "c4"}, {Op: "edit", Entry: "c5", Body: "b"},
	}
	if got := mc.planReq; got.Tenant != "globex" || len(got.Changes) != len(want) {
		t.Fatalf("connector got %+v", got)
	}
	for i := range want {
		if mc.planReq.Changes[i] != want[i] {
			t.Errorf("change %d = %+v, want %+v", i, mc.planReq.Changes[i], want[i])
		}
	}
	if resp.GetTenant() != "acme" || resp.GetFingerprint() != "fp" || resp.GetScopesCounted() != 3 || resp.GetScopesSkipped() != 1 ||
		resp.GetComplete() || strings.Join(resp.GetIssues(), ",") != "i1" || strings.Join(resp.GetNewIssues(), ",") != "n1" {
		t.Errorf("response = %+v", resp)
	}
	c := resp.GetChanges()[0]
	cnt := c.GetCounts()["statements"]
	if c.GetOp() != "delete" || c.GetRisk() != "destroys" || c.GetEntry().GetChunkId() != "c1" || c.GetEntry().GetKind() != "class" ||
		len(c.GetSubtree()) != 1 || c.GetSubtree()[0].GetName() != "Article" || strings.Join(c.GetNotes(), ",") != "note" ||
		cnt.GetN() != 4 || cnt.GetEffect() != "destroyed" || cnt.GetTenantScope() != 3 || cnt.GetOtherScopes() != 1 ||
		cnt.GetByStatus()["proposed"] != 1 || strings.Join(cnt.GetSample(), ",") != "s1,s2" {
		t.Errorf("change = %+v", c)
	}

	for sentinel, want := range map[error]codes.Code{
		connector.ErrOntologyPlanInvalid:     codes.InvalidArgument,
		connector.ErrOntologyPlanForbidden:   codes.PermissionDenied,
		connector.ErrOntologyPlanNotFound:    codes.NotFound,
		connector.ErrOntologyPlanUnavailable: codes.Unavailable,
	} {
		mc.planErr = connector.WithMessage(sentinel, "why")
		_, err := client.PlanOntology(context.Background(), &loomcyclepb.PlanOntologyRequest{Changes: []*loomcyclepb.OntologyChange{{Op: "retire", Entry: "c1"}}})
		code, reason, _ := reasonOf(err)
		if code != want || reason != connector.OntologyPlanCode(sentinel) {
			t.Errorf("%v maps to %v %q, want %v %q", sentinel, code, reason, want, connector.OntologyPlanCode(sentinel))
		}
	}
	mc.planErr = errors.New("open /var/lib/secret.db: denied")
	_, err = client.PlanOntology(context.Background(), &loomcyclepb.PlanOntologyRequest{})
	if code, reason, msg := reasonOf(err); code != codes.Internal || reason != "plan_failed" || strings.Contains(msg, "/var/lib") {
		t.Errorf("an internal failure = %v %q %q, want Internal plan_failed without its text", code, reason, msg)
	}
}

// A request over HTTP's body bound is refused before the connector, as
// invalid_body, so no transport takes a larger plan than another.
func TestPlanOntology_RequestOverTheBoundIsRefused(t *testing.T) {
	mc := &mockConnector{}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()
	_, err := client.PlanOntology(context.Background(), &loomcyclepb.PlanOntologyRequest{Changes: []*loomcyclepb.OntologyChange{
		{Op: "edit", Entry: "c1", Body: strings.Repeat("a", connector.OntologyPlanMaxRequestBytes)},
	}})
	if code, reason, _ := reasonOf(err); code != codes.InvalidArgument || reason != "invalid_body" || mc.planReq.Changes != nil {
		t.Errorf("an oversized plan = %v %q, connector got %+v; want InvalidArgument invalid_body before the connector", code, reason, mc.planReq)
	}
}

// A proposal item's id reaches the connector, and a planned change's proposal id
// comes back (RFC GQ P4.3): the fields HTTP carries as id and proposal.
func TestPlanOntology_ProposalIDRoundTrips(t *testing.T) {
	mc := &mockConnector{planRes: connector.PlanOntologyResult{Changes: []connector.OntologyPlannedChange{
		{Op: "retire", Risk: connector.OntologyRiskNone, Proposal: "p1", Entry: connector.OntologyPlanEntry{ChunkID: "c1"}},
	}}}
	client, cleanup := startTestServerWithConnector(t, mc)
	defer cleanup()
	resp, err := client.PlanOntology(context.Background(), &loomcyclepb.PlanOntologyRequest{
		Changes: []*loomcyclepb.OntologyChange{{Op: connector.OntologyPlanProposal, Id: "p1"}},
	})
	if err != nil {
		t.Fatalf("PlanOntology: %v", err)
	}
	if got := mc.planReq.Changes; len(got) != 1 || got[0] != (connector.OntologyChange{Op: connector.OntologyPlanProposal, ID: "p1"}) {
		t.Errorf("connector got %+v, want the proposal item with its id", got)
	}
	if c := resp.GetChanges(); len(c) != 1 || c[0].GetProposal() != "p1" {
		t.Errorf("response changes = %+v, want proposal p1", c)
	}
}

// Who may call it on this transport: an admin and a tenant operator. Its HTTP
// route is carved out of the member path, so a member is refused here too; a
// token limited to certain agents is refused whatever its scopes.
func TestPlanOntology_IsOperatorOnly(t *testing.T) {
	const method = grpcMethodPrefix + "PlanOntology"
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
