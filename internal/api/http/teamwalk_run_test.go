package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/turncancel"
)

// TestOpenTeamWalkRun_GrantsWhatThePauseMachineryNeeds is the regression for
// the defect this whole change exists to fix.
//
// loomboard drives loomcycle over POST /v1/_teamdef, which dispatches under
// substrateAdminCtx — a plane with NO run id and NO Interruption policy. A
// breakpoint armed there could not be answered, and a pause that cannot be
// answered fails safe to ABORT, so arming one killed the walk instead of
// pausing it. Both halves are asserted because either one alone still aborts.
func TestOpenTeamWalkRun_GrantsWhatThePauseMachineryNeeds(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()

	// The plane the canvas actually arrives on.
	base := substrateAdminCtx(context.Background())
	if tools.RunID(base) != "" {
		t.Fatal("the substrate plane already had a run id — this test no longer covers the gap")
	}
	if tools.InterruptionPolicy(base).Enabled {
		t.Fatal("the substrate plane already granted Interruption — this test no longer covers the gap")
	}

	walkCtx, runID, finish, err := srv.openTeamWalkRun(base, "triage", false)
	if err != nil {
		t.Fatalf("openTeamWalkRun: %v", err)
	}
	if runID == "" {
		t.Fatal("no run id — the breakpoint set and the ask are both keyed on it")
	}
	if got := tools.RunID(walkCtx); got != runID {
		t.Errorf("ctx run id = %q, want %q", got, runID)
	}
	pol := tools.InterruptionPolicy(walkCtx)
	if !pol.Enabled {
		t.Error("Interruption is not enabled on the walk ctx — every pause would error, and an unanswerable pause ABORTS the walk")
	}
	if len(pol.Kinds) != 1 || pol.Kinds[0] != "question" {
		t.Errorf("Interruption kinds = %v, want just [question] — a breakpoint asks nothing else", pol.Kinds)
	}

	// The run is real and addressable, filed under the team.
	run, err := srv.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("the walk's run row is not readable: %v", err)
	}
	if run.AgentID != teamWalkAgentPrefix+"triage" {
		t.Errorf("run agent = %q, want %q", run.AgentID, teamWalkAgentPrefix+"triage")
	}
	if run.Status != store.RunRunning {
		t.Errorf("run status = %q, want running", run.Status)
	}

	// And finishing records the outcome, so a failed walk is never left looking
	// like one still going.
	finish("", context.DeadlineExceeded)
	run, err = srv.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun after finish: %v", err)
	}
	if run.Status != store.RunFailed {
		t.Errorf("status after a failed walk = %q, want failed", run.Status)
	}
	// The message the walk failed with reaches the row, so a failed walk is
	// never indistinguishable from one still going.
	if run.ErrorMsg == "" {
		t.Error("the walk's error was not recorded on the run")
	}
}

// TestOpenTeamWalkRun_DetachSurvivesTheRequestCtx: a detached walk must keep
// the request's ctx VALUES (auth principal, tenant, admission) while no longer
// dying when the handler returns — the same trade an interactive run makes.
func TestOpenTeamWalkRun_DetachSurvivesTheRequestCtx(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()

	reqCtx, cancel := context.WithCancel(substrateAdminCtx(context.Background()))
	walkCtx, _, _, err := srv.openTeamWalkRun(reqCtx, "triage", true)
	if err != nil {
		t.Fatalf("openTeamWalkRun: %v", err)
	}
	cancel() // the handler returns
	if walkCtx.Err() != nil {
		t.Fatal("a detached walk ctx died with the request — the walk would be killed the moment the caller got its run id")
	}
	// Values survive: the identity the walk spawns its members under.
	if tools.RunIdentity(walkCtx).AgentID == "" {
		t.Error("the detached ctx lost its run identity")
	}

	// A NON-detached walk keeps the request's lifetime, so a disconnect still
	// tears it down.
	reqCtx2, cancel2 := context.WithCancel(substrateAdminCtx(context.Background()))
	walkCtx2, _, _, err := srv.openTeamWalkRun(reqCtx2, "triage", false)
	if err != nil {
		t.Fatalf("openTeamWalkRun: %v", err)
	}
	cancel2()
	if walkCtx2.Err() == nil {
		t.Error("a synchronous walk outlived its request ctx")
	}
}

func tenantOperatorCtx(tenant string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: tenant, Subject: "op", Scopes: []string{auth.ScopeTenant},
	})
}

// A running walk could not be stopped: the run-id cancel route refused it as
// not interactive, the agents route cannot address `team:<name>`, and only a
// breakpoint's own `abort` answer reached it. Cancelling by the run id a
// detached start returned must now end the walk — and every run it spawned,
// since they all run under the walk ctx — and record it as cancelled.
func TestCancelTurn_StopsALiveTeamWalk(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()

	walkCtx, runID, finish, err := srv.openTeamWalkRun(substrateAdminCtx(tenantOperatorCtx("acme")), "triage", true)
	if err != nil {
		t.Fatalf("openTeamWalkRun: %v", err)
	}
	member, stopMember := context.WithCancel(walkCtx) // a run the walk spawned
	defer stopMember()

	stopped, parked, err := srv.CancelTurn(tenantOperatorCtx("acme"), runID, "operator stop")
	if err != nil || !stopped || parked {
		t.Fatalf("CancelTurn = (stopped %v, parked %v, %v), want (true, false, nil) — a walk is ended, never parked", stopped, parked, err)
	}
	if !errors.Is(context.Cause(walkCtx), cancel.ErrCancelledByAPI) {
		t.Fatalf("walk ctx cause = %v, want the API-cancel cause", context.Cause(walkCtx))
	}
	if member.Err() == nil {
		t.Error("a run the walk spawned kept running after the walk was cancelled")
	}

	finish("", walkCtx.Err()) // the walk returns with its ctx error
	run, err := srv.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != store.RunCancelled {
		t.Errorf("status = %q, want cancelled — a deliberate stop is not a failure", run.Status)
	}
	if run.StopReason != "operator stop" {
		t.Errorf("stop reason = %q, want the operator's reason", run.StopReason)
	}

	// Once finished it is no longer a live walk, so a second cancel is not
	// reported as having stopped anything.
	if stopped, _, _ := srv.CancelTurn(tenantOperatorCtx("acme"), runID, ""); stopped {
		t.Error("a finished walk was reported stopped again")
	}
}

// Run ids are not secret, so the walk path needs the same ownership gate the
// turn-cancel path has: another tenant gets the opaque not-in-flight answer and
// the walk keeps running.
func TestCancelTurn_AnotherTenantCannotStopAWalk(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()

	walkCtx, runID, finish, err := srv.openTeamWalkRun(substrateAdminCtx(tenantOperatorCtx("acme")), "triage", true)
	if err != nil {
		t.Fatalf("openTeamWalkRun: %v", err)
	}
	defer finish("", nil)

	stopped, _, err := srv.CancelTurn(tenantOperatorCtx("other"), runID, "")
	if stopped || !errors.Is(err, connector.ErrRunNotInFlight) {
		t.Errorf("cross-tenant CancelTurn = (stopped %v, %v), want (false, ErrRunNotInFlight)", stopped, err)
	}
	if walkCtx.Err() != nil {
		t.Error("another tenant's cancel stopped the walk")
	}
}

// The route the canvas calls, end to end through the handler: 200 with
// stopped:true, parked:false, where it used to answer 409 not_interactive.
func TestHandleCancelTurn_StopsATeamWalk(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	srv.turnCancelReg = turncancel.NewRegistry()
	srv.steerReg = steer.NewRegistry(8)

	walkCtx, runID, finish, err := srv.openTeamWalkRun(substrateAdminCtx(tenantOperatorCtx("acme")), "triage", true)
	if err != nil {
		t.Fatalf("openTeamWalkRun: %v", err)
	}
	defer finish("", nil)

	req := httptest.NewRequest("POST", "/v1/runs/"+runID+"/cancel", strings.NewReader(`{"reason":"stop"}`))
	req.SetPathValue("run_id", runID)
	req = req.WithContext(tenantOperatorCtx("acme"))
	rec := httptest.NewRecorder()
	srv.handleCancelTurn(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["stopped"] != true || body["parked"] != false {
		t.Errorf("body = %v, want stopped:true parked:false", body)
	}
	if walkCtx.Err() == nil {
		t.Error("the route answered 200 but the walk is still running")
	}
}
