package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC EZ §1.2 — ending a walk asks `cancel team:<name>` of its team, however
// it is ended: TeamDef cancel inside a run, and the run-plane cancels outside
// one — POST /v1/runs/{run_id}/cancel and the gRPC CancelTurn RPC (both
// CancelTurn, on the walk's replica or routed to it), and MCP cancel_run by
// the walk's agent id (CancelRun).

// teamACLCtx is a tenant operator of acme judged by mode, with rules bound.
// rules nil leaves the principal with no compiled set.
func teamACLCtx(t *testing.T, mode authz.Mode, rules ...authz.Rule) context.Context {
	t.Helper()
	p := auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}}
	if rules != nil {
		eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: "acme", ID: "op"},
			Bindings: []authz.Binding{{ACL: "l", SubjectKind: authz.SubjectTenant}},
			ACLs:     map[string]authz.ACLVersion{"l": {ACL: authz.ACL{Name: "l", Rules: rules}}}, Now: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		p.Effective = eff
	}
	return authz.WithMode(auth.WithPrincipal(context.Background(), p), mode)
}

func denyCancelOf(team string) authz.Rule {
	return authz.Rule{Effect: authz.Deny, Actions: []string{"cancel"}, Resources: []string{"team:" + team}}
}

// cancelWalkAs drives POST /v1/runs/{run_id}/cancel on srv under ctx.
func cancelWalkAs(srv *Server, ctx context.Context, walkID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/runs/"+walkID+"/cancel", strings.NewReader(`{"reason":"stop"}`))
	req.SetPathValue("run_id", walkID)
	rec := httptest.NewRecorder()
	srv.handleCancelTurn(rec, req.WithContext(ctx))
	return rec
}

func wantACLRefusal(t *testing.T, rec *httptest.ResponseRecorder, status int, code, names string) {
	t.Helper()
	var body struct {
		Code     string `json:"code"`
		Action   string `json:"action"`
		Resource string `json:"resource"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != status || body.Code != code || (names != "" && body.Action+" "+body.Resource != names) {
		t.Errorf("cancel = %d %s, want %d %s naming %q", rec.Code, rec.Body, status, code, names)
	}
}

func (h *detachHarness) wantRunning(walkID string) {
	h.t.Helper()
	if row, err := h.st.GetRun(context.Background(), walkID); err != nil || row.Status != store.RunRunning {
		h.t.Fatalf("walk after a refused cancel = %+v, %v; want still running", row.Status, err)
	}
}

func TestCancelTurn_ALocalWalkAsksCancelOfItsTeam(t *testing.T) {
	h := newDetachHarness(t, false)
	h.srv.SetSteerRegistry(steer.NewRegistry(0)) // the route is served only with steering on
	walkID := h.runDetached(h.startAgent(nil, false))
	h.memberUp(walkID)

	wantACLRefusal(t, cancelWalkAs(h.srv, teamACLCtx(t, authz.ModeCompat, denyCancelOf("solo")), walkID), 403, "acl_denied", "cancel team:solo")
	// enforce with no set to decide by refuses as unavailable, not admitted.
	wantACLRefusal(t, cancelWalkAs(h.srv, teamACLCtx(t, authz.ModeEnforce), walkID), 503, "acl_unavailable", "")
	h.wantRunning(walkID)
	if _, live := h.srv.walks.get(walkID); !live {
		t.Fatal("a refused cancel stopped the walk")
	}

	if rec := cancelWalkAs(h.srv, teamACLCtx(t, authz.ModeCompat, denyCancelOf("other")), walkID); rec.Code != 200 {
		t.Fatalf("cancel by a caller denied only another team = %d %s", rec.Code, rec.Body)
	}
	if walk := h.waitEnded(walkID); walk.Status != store.RunCancelled {
		t.Errorf("walk = %s, want cancelled", walk.Status)
	}
}

func TestCancelTurn_AWalkOnAnotherReplicaAsksCancelOfItsTeam(t *testing.T) {
	c := newWalkCluster(t)
	walkID, _ := c.runWalkOnA(t)

	wantACLRefusal(t, cancelWalkAs(c.b, teamACLCtx(t, authz.ModeCompat, denyCancelOf("solo")), walkID), 403, "acl_denied", "cancel team:solo")
	c.h.wantRunning(walkID)
	if _, live := c.a.walks.get(walkID); !live {
		t.Fatal("a refused cancel routed to the owner stopped the walk")
	}

	if rec := cancelWalkAs(c.b, teamACLCtx(t, authz.ModeCompat, denyCancelOf("other")), walkID); rec.Code != 200 {
		t.Fatalf("cancel via r2 by a caller denied only another team = %d %s", rec.Code, rec.Body)
	}
	if walk := c.h.waitEnded(walkID); walk.Status != store.RunCancelled {
		t.Errorf("walk = %s, want cancelled", walk.Status)
	}
}

// MCP cancel_run addresses a run by agent id, and every walk of a team is
// filed under `team:<name>`.
func TestCancelRun_AWalkByAgentIDAsksCancelOfItsTeam(t *testing.T) {
	h := newDetachHarness(t, false)
	walkID := h.runDetached(h.startAgent(nil, false))
	h.memberUp(walkID)

	_, err := h.srv.CancelRun(teamACLCtx(t, authz.ModeCompat, denyCancelOf("solo")), teamWalkAgentPrefix+"solo", "stop")
	var ref *connector.ACLRefusal
	if !errors.As(err, &ref) || ref.Code != "acl_denied" || !strings.Contains(ref.Body, `"resource":"team:solo"`) {
		t.Fatalf("CancelRun(team:solo) by a caller denied cancel team:solo = %v, want an acl_denied refusal", err)
	}
	h.wantRunning(walkID)
	if _, err := h.srv.CancelRun(teamACLCtx(t, authz.ModeCompat, denyCancelOf("other")), teamWalkAgentPrefix+"solo", "stop"); err != nil {
		t.Errorf("CancelRun(team:solo) by a caller denied only another team: %v", err)
	}
}
