package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/breakpoints"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// seedRun creates a session + run the tenant gate will accept.
func seedRun(t *testing.T, srv *Server) string {
	t.Helper()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "agent-x", "alice")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "agent-x", UserID: "alice"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return run.ID
}

func armedFrom(t *testing.T, body string) []string {
	t.Helper()
	var out struct {
		RunID string   `json:"run_id"`
		Armed []string `json:"armed"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return out.Armed
}

// TestHandleBreakpoints_ArmsAWalkThatIsAlreadyRunning is the whole point: the
// walk registered with no breakpoints, and an operator arms one afterwards.
func TestHandleBreakpoints_ArmsAWalkThatIsAlreadyRunning(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRun(t, srv)

	// A walk starts with NOTHING armed — the ordinary case.
	set, release, err := srv.breakpointReg.Open(runID, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if set.Armed("wave", breakpoints.BeforeDispatch) {
		t.Fatal("a walk with no run argument started armed")
	}

	rec := doJSON(t, srv, "PUT", "/v1/runs/"+runID+"/breakpoints", `{"breakpoints":["wave:before_dispatch"]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := armedFrom(t, rec.Body.String()); len(got) != 1 || got[0] != "wave:before_dispatch" {
		t.Errorf("armed = %v", got)
	}
	// The LIVE set the walk is consulting changed — not a copy.
	if !set.Armed("wave", breakpoints.BeforeDispatch) {
		t.Error("the walk's own set did not see the arming")
	}
	if set.Armed("wave", breakpoints.Review) {
		t.Error("a phase-qualified spec armed another phase too")
	}
}

// TestHandleBreakpoints_GetReadsBackCanonically: a caller sees what the walk
// will do, not an echo of its shorthand.
func TestHandleBreakpoints_GetReadsBackCanonically(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRun(t, srv)
	_, release, _ := srv.breakpointReg.Open(runID, []string{"wave", "wave:review"}, 0, nil)
	defer release()

	rec := doJSON(t, srv, "GET", "/v1/runs/"+runID+"/breakpoints", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	got := armedFrom(t, rec.Body.String())
	if len(got) != 2 || got[0] != "wave:before_dispatch" || got[1] != "wave:review" {
		t.Errorf("armed = %v, want the bare form spelled out, canonical and sorted", got)
	}
}

// TestHandleBreakpoints_EmptyListTurnsDebugOff — the canvas's off switch.
func TestHandleBreakpoints_EmptyListTurnsDebugOff(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRun(t, srv)
	set, release, _ := srv.breakpointReg.Open(runID, []string{"wave"}, 0, nil)
	defer release()

	rec := doJSON(t, srv, "PUT", "/v1/runs/"+runID+"/breakpoints", `{"breakpoints":[]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if len(armedFrom(t, rec.Body.String())) != 0 || set.Armed("wave", breakpoints.BeforeDispatch) {
		t.Error("the empty list did not disarm")
	}
}

// assertArmedIsEmptyList checks the raw body, not a decode: json.Unmarshal
// reads null and [] into the same empty slice, and null is what crashed the
// walk pane.
func assertArmedIsEmptyList(t *testing.T, what, body string) {
	t.Helper()
	if !strings.Contains(body, `"armed":[]`) {
		t.Errorf("%s: body = %s, want \"armed\":[]", what, body)
	}
}

// TestHandleBreakpoints_NothingArmedReadsAsEmptyList: the ordinary walk —
// started with no breakpoints — reports an empty list, not null.
func TestHandleBreakpoints_NothingArmedReadsAsEmptyList(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRun(t, srv)
	_, release, err := srv.breakpointReg.Open(runID, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	rec := doJSON(t, srv, "GET", "/v1/runs/"+runID+"/breakpoints", "")
	if rec.Code != 200 {
		t.Fatalf("GET = %d; body=%s", rec.Code, rec.Body.String())
	}
	assertArmedIsEmptyList(t, "GET", rec.Body.String())

	// A PUT carrying only the deadline leaves the arming — still nothing.
	rec = doJSON(t, srv, "PUT", "/v1/runs/"+runID+"/breakpoints", `{"review_ttl_seconds":60}`)
	if rec.Code != 200 {
		t.Fatalf("PUT deadline = %d; body=%s", rec.Code, rec.Body.String())
	}
	assertArmedIsEmptyList(t, "PUT deadline only", rec.Body.String())
}

// TestHandleBreakpoints_DisarmingEverythingRespondsEmptyList: the off switch
// answers with the set it left, which is empty, not null.
func TestHandleBreakpoints_DisarmingEverythingRespondsEmptyList(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRun(t, srv)
	_, release, err := srv.breakpointReg.Open(runID, []string{"wave", "wave:review"}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	rec := doJSON(t, srv, "PUT", "/v1/runs/"+runID+"/breakpoints", `{"breakpoints":[]}`)
	if rec.Code != 200 {
		t.Fatalf("PUT = %d; body=%s", rec.Code, rec.Body.String())
	}
	assertArmedIsEmptyList(t, "PUT []", rec.Body.String())
}

// TestHandleBreakpoints_RejectedPutKeepsThePreviousArming: an operator fixing a
// typo must not discover they have also disarmed what was working.
func TestHandleBreakpoints_RejectedPutKeepsThePreviousArming(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRun(t, srv)
	set, release, _ := srv.breakpointReg.Open(runID, []string{"wave:before_dispatch"}, 0, nil)
	defer release()

	rec := doJSON(t, srv, "PUT", "/v1/runs/"+runID+"/breakpoints", `{"breakpoints":["review","wave:typo"]}`)
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !set.Armed("wave", breakpoints.BeforeDispatch) {
		t.Error("a rejected PUT disarmed the previous set")
	}
	if set.Armed("review", breakpoints.BeforeDispatch) {
		t.Error("a rejected PUT partially applied")
	}
}

// TestHandleBreakpoints_NoLiveWalkIs404: an arming that appeared to succeed and
// then never paused anything is the worst outcome for a debugger, so a run with
// no walk in flight fails loudly. A cross-tenant run folds into the SAME 404 —
// run_ids are not secret, so the gate must not become an existence oracle.
func TestHandleBreakpoints_NoLiveWalkIs404(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRun(t, srv) // a real run, but no walk registered

	for _, method := range []string{"GET", "PUT"} {
		rec := doJSON(t, srv, method, "/v1/runs/"+runID+"/breakpoints", `{"breakpoints":["wave"]}`)
		if rec.Code != 404 {
			t.Errorf("%s with no live walk = %d, want 404; body=%s", method, rec.Code, rec.Body.String())
		}
	}
	// An unknown run id is the same 404, not a different code that would say
	// "this run does not exist".
	rec := doJSON(t, srv, "GET", "/v1/runs/run-nope/breakpoints", "")
	if rec.Code != 404 {
		t.Errorf("unknown run = %d, want the same opaque 404", rec.Code)
	}
}

// TestBreakpointPhases_MatchTheRegistrys pins the two spellings of the phase
// against each other. The registry is a leaf and cannot import teamrun, so the
// constants are declared twice; if they ever diverge, every arming would
// silently stop matching and no other test would notice.
func TestBreakpointPhases_MatchTheRegistrys(t *testing.T) {
	if string(teamrun.BeforeDispatch) != breakpoints.BeforeDispatch {
		t.Errorf("before_dispatch spelled %q in teamrun and %q in breakpoints",
			teamrun.BeforeDispatch, breakpoints.BeforeDispatch)
	}
	if string(teamrun.Review) != breakpoints.Review {
		t.Errorf("review spelled %q in teamrun and %q in breakpoints",
			teamrun.Review, breakpoints.Review)
	}
	// And the adapter actually bridges them.
	set, _ := breakpoints.NewSet([]string{"wave:review"})
	var src teamrun.BreakpointSource = liveBreakpoints{set}
	if !src.Armed("wave", teamrun.Review) {
		t.Error("the adapter did not translate the phase")
	}
	if src.Armed("wave", teamrun.BeforeDispatch) {
		t.Error("the adapter armed the wrong phase")
	}
}

// Arming the removed pause live is refused, with the replacement named, and the
// arming the walk already had is left as it was.
func TestHandleBreakpoints_TheRemovedPauseIsRefused(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRun(t, srv)
	set, release, err := srv.breakpointReg.Open(runID, []string{"wave:review"}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	rec := doJSON(t, srv, "PUT", "/v1/runs/"+runID+"/breakpoints", `{"breakpoints":["wave:after_collection"]}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "wave:review") {
		t.Errorf("status = %d body=%s, want 400 naming \"wave:review\"", rec.Code, rec.Body.String())
	}
	if !set.Armed("wave", breakpoints.Review) {
		t.Error("a refused arming disarmed what the walk had")
	}
}

// An isolated member cannot read or change another user's walk arming — a
// disarm would release that user's members held for review. It is the same
// opaque 404 as a walk that is not there; the owner is served.
func TestHandleBreakpoints_AnIsolatedMemberCannotReachAColleaguesWalk(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRunInTenant(t, srv.store, "acme", "alice", "team:triage")
	set, release, err := srv.breakpointReg.Open(runID, []string{"wave:review"}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	call := func(p auth.Principal, method, body string) int {
		req := httptest.NewRequest(method, "/v1/runs/"+runID+"/breakpoints", strings.NewReader(body))
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
		req.SetPathValue("run_id", runID)
		rec := httptest.NewRecorder()
		if method == "PUT" {
			srv.handlePutRunBreakpoints(rec, req)
		} else {
			srv.handleGetRunBreakpoints(rec, req)
		}
		return rec.Code
	}
	bob := auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeUser}}
	for _, method := range []string{"GET", "PUT"} {
		if code := call(bob, method, `{"breakpoints":[]}`); code != 404 {
			t.Errorf("isolated colleague %s = %d, want the opaque 404", method, code)
		}
	}
	if !set.Armed("wave", breakpoints.Review) {
		t.Fatal("a refused PUT disarmed the walk")
	}
	alice := auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeUser}}
	if code := call(alice, "PUT", `{"breakpoints":[]}`); code != 200 || set.Armed("wave", breakpoints.Review) {
		t.Errorf("owner PUT = %d, armed after = %v; want 200 and disarmed", code, set.Armed("wave", breakpoints.Review))
	}
}

// targetsTeam is a starter "wave" feeding an agent "draft", judged by a
// consolidator "judge": one state a wave breakpoint can arm, one only review
// can, and one neither can.
const targetsTeam = `{"entry":"wave","states":[` +
	`{"state":"wave","handler":{"kind":"starter","source":{"channel":"c1"},"fanout":{"agent":"agent-x","per":"message"},"sink":{"channel":"c2"}}},` +
	`{"state":"draft","handler":{"kind":"agent","agent":"agent-x"}},` +
	`{"state":"judge","handler":{"kind":"consolidator","agent":"agent-x"}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"wave","to":"draft","on":"success"},{"from":"draft","to":"judge","on":"success"},` +
	`{"from":"judge","to":"done","on":"success"}]}`

// openWalkOnTargetsTeam opens the armed set the way TeamDef op=run does: under
// the walk's run id, with the walk's own target check.
func openWalkOnTargetsTeam(t *testing.T, srv *Server, runID string, seed []string) (*breakpoints.Set, func()) {
	t.Helper()
	def, err := teamgraph.Parse([]byte(targetsTeam))
	if err != nil {
		t.Fatal(err)
	}
	targets := func(spec string) error { return teamrun.CheckBreakpointTargets(def, "triage", []string{spec}) }
	_, release, err := srv.openTeamBreakpoints(tools.WithRunID(context.Background(), runID), seed, 0, targets)
	if err != nil {
		t.Fatal(err)
	}
	set, ok := srv.breakpointReg.Get(runID)
	if !ok {
		t.Fatal("the walk's set is not registered under its run id")
	}
	return set, release
}

// TestHandleBreakpoints_PutRefusesAnUnknownStateAndKeepsTheArming: the start of
// a walk refuses a breakpoint on a state its team does not have; the live
// re-arm used to accept it, and it was silently never hit.
func TestHandleBreakpoints_PutRefusesAnUnknownStateAndKeepsTheArming(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRun(t, srv)
	set, release := openWalkOnTargetsTeam(t, srv, runID, []string{"wave"})
	defer release()

	rec := doJSON(t, srv, "PUT", "/v1/runs/"+runID+"/breakpoints", `{"breakpoints":["wave:review","wvae"]}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid_breakpoint") ||
		!strings.Contains(rec.Body.String(), `has no state \"wvae\"`) {
		t.Fatalf("status = %d, want 400 invalid_breakpoint naming the state; body=%s", rec.Code, rec.Body.String())
	}
	if !set.Armed("wave", breakpoints.BeforeDispatch) || set.Armed("wave", breakpoints.Review) {
		t.Errorf("a refused PUT changed the arming: %v", set.List())
	}
}

// TestHandleBreakpoints_PutRefusesAWaveBreakpointOnAnAgentState: an agent state
// dispatches no wave, so a pause armed on it would never be reached.
func TestHandleBreakpoints_PutRefusesAWaveBreakpointOnAnAgentState(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRun(t, srv)
	set, release := openWalkOnTargetsTeam(t, srv, runID, nil)
	defer release()

	for _, spec := range []string{"draft", "draft:before_dispatch"} {
		rec := doJSON(t, srv, "PUT", "/v1/runs/"+runID+"/breakpoints", `{"breakpoints":["`+spec+`"]}`)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "only a starter") {
			t.Errorf("%s: status = %d, want 400 naming the kind; body=%s", spec, rec.Code, rec.Body.String())
		}
	}
	if len(set.List()) != 0 {
		t.Errorf("a refused PUT armed %v", set.List())
	}
}

// TestHandleBreakpoints_PutAcceptsAStarterTheWalkHas — the check refuses only
// what could never be hit.
func TestHandleBreakpoints_PutAcceptsAStarterTheWalkHas(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRun(t, srv)
	set, release := openWalkOnTargetsTeam(t, srv, runID, nil)
	defer release()

	rec := doJSON(t, srv, "PUT", "/v1/runs/"+runID+"/breakpoints", `{"breakpoints":["wave","wave:review"]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !set.Armed("wave", breakpoints.BeforeDispatch) || !set.Armed("wave", breakpoints.Review) {
		t.Errorf("armed = %v", set.List())
	}
}

// TestHandleBreakpoints_PutArmsReviewOnAnAgentState: review reaches an agent
// state's member, so a live re-arm may name one.
func TestHandleBreakpoints_PutArmsReviewOnAnAgentState(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRun(t, srv)
	set, release := openWalkOnTargetsTeam(t, srv, runID, nil)
	defer release()

	rec := doJSON(t, srv, "PUT", "/v1/runs/"+runID+"/breakpoints", `{"breakpoints":["draft:review"]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !set.Armed("draft", breakpoints.Review) {
		t.Errorf("armed = %v", set.List())
	}
}

// TestHandleBreakpoints_PutRefusesReviewOnAConsolidator: its answer is the
// walk's verdict on the work, not work to review — refused live as at start.
func TestHandleBreakpoints_PutRefusesReviewOnAConsolidator(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRun(t, srv)
	set, release := openWalkOnTargetsTeam(t, srv, runID, []string{"draft:review"})
	defer release()

	rec := doJSON(t, srv, "PUT", "/v1/runs/"+runID+"/breakpoints", `{"breakpoints":["judge:review"]}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "is a consolidator") {
		t.Fatalf("status = %d, want 400 naming the consolidator; body=%s", rec.Code, rec.Body.String())
	}
	if !set.Armed("draft", breakpoints.Review) || set.Armed("judge", breakpoints.Review) {
		t.Errorf("a refused PUT changed the arming: %v", set.List())
	}
}
