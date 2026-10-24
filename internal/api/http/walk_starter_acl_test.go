package http

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC EZ §1.5: a team walk started on the plane — `POST /v1/_teamdef
// op=run`, the gRPC TeamDef RPC, the MCP teamdef tool — with no enclosing
// run had no set on its run policy, so its members ran on their agents' full
// grants whatever the starter was bound to: only a walk an agent started
// carried that run's caller's set down. The walk now carries its starter's
// set, as a trigger's walk carries its owner's.

// devOperatorBearer is a tenant-operator token of the member dev, so it may
// reach the definition plane; dev's bindings are by user, so they bind it.
const devOperatorBearer = "tok-dev-operator"

func (e *aclResumeEnv) mintDevOperator(t *testing.T) {
	t.Helper()
	if _, err := e.st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: "def_dev_op", Name: "u-def_dev_op", TenantID: "acme", Subject: "dev",
		TokenHash: auth.HashToken(testTokenPepper, devOperatorBearer), AllowedScopes: []string{auth.ScopeTenant},
	}); err != nil {
		t.Fatal(err)
	}
}

// planeWalk starts probe-team over POST /v1/_teamdef as dev's operator token
// and returns the walk's run id.
func (e *aclResumeEnv) planeWalk(t *testing.T) string {
	t.Helper()
	code, out := bearerCall(t, e.ts, devOperatorBearer, "POST", "/v1/_teamdef", `{"op":"run","name":"probe-team","input":"go"}`)
	if code != 200 {
		t.Fatalf("POST /v1/_teamdef = %d: %s", code, out)
	}
	var res struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.RunID == "" {
		t.Fatalf("walk result %s: %v", out, err)
	}
	return res.RunID
}

// A walk dev starts over HTTP spawns its member narrowed by dev's list, and
// the member records dev under the http transport for a resume.
func TestPlaneTeamWalk_HTTPMembersAreNarrowedByTheStartersACL(t *testing.T) {
	e := aclResumeFixture(t, true, map[string][][]providers.Event{"worker": {probeTurn("tu_p"), endTurn()}})
	e.withProbeTeam(t)
	e.mintDevOperator(t)
	walkID := e.planeWalk(t)
	e.assertNarrowedByDev(t, e.waitProbes(t, 1)[0])
	assertMemberRecordsOwner(t, e.memberOf(t, walkID), "http")
}

// The gRPC RPC and the MCP tool reach the walk through the connector's
// TeamDef with the caller's principal on ctx: the same seam narrows them.
func TestPlaneTeamWalk_ConnectorMembersAreNarrowedByTheStartersACL(t *testing.T) {
	e := aclResumeFixture(t, true, map[string][][]providers.Event{"worker": {probeTurn("tu_c"), endTurn()}})
	e.withProbeTeam(t)
	ctx := authz.WithTransport(e.authorCtx(t), "grpc")
	res, err := e.srv.TeamDef(ctx, json.RawMessage(`{"op":"run","name":"probe-team","input":"go"}`))
	if err != nil || res.IsError {
		t.Fatalf("TeamDef op=run: %v %s", err, res.Text)
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil || out.RunID == "" {
		t.Fatalf("walk result %s: %v", res.Text, err)
	}
	e.assertNarrowedByDev(t, e.waitProbes(t, 1)[0])
	assertMemberRecordsOwner(t, e.memberOf(t, out.RunID), "grpc")
}

// The starter's set is asked `spawn agent:` for each member: an operator's
// own deny on spawning the member binds the walk it starts.
func TestPlaneTeamWalk_ADenyOnSpawningTheMemberFailsTheWalk(t *testing.T) {
	e := aclResumeFixture(t, false, map[string][][]providers.Event{"worker": {endTurn()}})
	e.withProbeTeam(t)
	e.mintDevOperator(t)
	if _, err := e.st.AclDefPut(context.Background(), store.AclDefRow{TenantID: "acme", Name: "no-spawn",
		Rules: json.RawMessage(`[{"effect":"deny","actions":["spawn"],"resources":["agent:worker"]}]`)}); err != nil {
		t.Fatal(err)
	}
	bindACL(t, e.st, "no-spawn")
	code, out := bearerCall(t, e.ts, devOperatorBearer, "POST", "/v1/_teamdef", `{"op":"run","name":"probe-team","input":"go"}`)
	if n := e.prov.callsOf("worker"); n != 0 {
		t.Errorf("the denied member made %d model call(s) (walk answered %d: %s)", n, code, out)
	}
	if !slices.ContainsFunc(e.audit.events, func(ev audit.Event) bool {
		return ev.Action == "acl_denied" && ev.ActorSubject == "dev" && ev.TargetName == "agent:worker" && ev.AclAction == "spawn"
	}) {
		t.Errorf("no spawn acl_denied audit event for dev; events: %+v", e.audit.events)
	}
}

// A walk whose ctx already carries a set — a trigger's owner's, an enclosing
// run's — keeps it: the starter's is stamped only where none is.
func TestStampWalkStarterACL_KeepsASetAlreadyOnThePolicy(t *testing.T) {
	e := aclResumeFixture(t, true, nil)
	owner := &authz.Effective{Subject: authz.Subject{TenantID: "acme", ID: "owner"}}
	ctx := tools.UpdateRunPolicy(e.authorCtx(t), func(p *tools.RunPolicy) { p.ACL = owner })
	if got := tools.PolicyOf(stampWalkStarterACL(ctx)).ACL; got != owner {
		t.Errorf("the walk's set = %+v, want the one already on its policy", got)
	}
	if got := tools.PolicyOf(stampWalkStarterACL(e.authorCtx(t))).ACL; got == nil || got.Subject.ID != "dev" {
		t.Errorf("a plane walk's set = %+v, want its starter dev's", got)
	}
}
