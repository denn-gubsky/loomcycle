package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// sessionlessRunStore reads every run back with no session, the row shape the
// run-row gates skipped their ownership check for.
type sessionlessRunStore struct {
	store.Store
}

func (s sessionlessRunStore) GetRun(ctx context.Context, id string) (store.Run, error) {
	run, err := s.Store.GetRun(ctx, id)
	run.SessionID = ""
	return run, err
}

// isolatedMember is a member of acme confined to its own runs.
func isolatedMember(subject string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "acme", Subject: subject, Scopes: []string{auth.ScopeRunsCreate, auth.ScopeUser}})
}

// The review verb read the run through the tenant store and confined an
// isolated member through the run's session — so a sessionless row let one
// rule on a colleague's held run.
func TestReviewRun_SessionlessRunRefusesAnIsolatedColleague(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	s.SetSteerRegistry(steer.NewRegistry(0))
	runID := seedRunInTenant(t, st, "acme", "alice", "a_held")
	q, dereg := s.steerReg.Register(steer.Entry{RunID: runID, UserID: "alice"})
	defer dereg()
	payload, _ := json.Marshal(providers.Event{Type: providers.EventAwaitingReview,
		AwaitingReview: &providers.AwaitingReviewEventInfo{Round: 1}})
	if err := st.AppendEvent(context.Background(), runID, string(providers.EventAwaitingReview), payload); err != nil {
		t.Fatal(err)
	}
	s.store = sessionlessRunStore{Store: st}

	if _, err := s.ReviewRun(isolatedMember("bob"), runID, "approve", "", "api"); !errors.Is(err, connector.ErrRunNotInFlight) {
		t.Errorf("isolated colleague's verdict: err = %v, want the opaque not-in-flight", err)
	}
	select {
	case m := <-q:
		t.Fatalf("a colleague's verdict reached the run: %+v", m)
	default:
	}
	if _, err := s.ReviewRun(isolatedMember("alice"), runID, "approve", "", "api"); err != nil {
		t.Fatalf("owner's verdict: %v", err)
	}
	if m := <-q; m.Kind != steer.KindApprove {
		t.Errorf("owner's verdict delivered %+v, want an approval", m)
	}
}

// The breakpoint gate had the same session-only confinement.
func TestHandleBreakpoints_SessionlessWalkRefusesAnIsolatedColleague(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRunInTenant(t, srv.store, "acme", "alice", "team:triage")
	_, release, err := srv.breakpointReg.Open(runID, []string{"wave:review"}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	srv.store = sessionlessRunStore{Store: srv.store}

	get := func(ctx context.Context) int {
		req := httptest.NewRequest("GET", "/v1/runs/"+runID+"/breakpoints", strings.NewReader(""))
		req = req.WithContext(ctx)
		req.SetPathValue("run_id", runID)
		rec := httptest.NewRecorder()
		srv.handleGetRunBreakpoints(rec, req)
		return rec.Code
	}
	if code := get(isolatedMember("bob")); code != 404 {
		t.Errorf("isolated colleague = %d, want the opaque 404", code)
	}
	if code := get(isolatedMember("alice")); code != 200 {
		t.Errorf("owner = %d, want 200", code)
	}
}

// A run another replica owns is gated by its row here; a sessionless row
// skipped the confinement, so an isolated member could steer or retune a
// colleague's run from any replica but the owner.
func TestSteer_SessionlessRemoteRunRefusesAnIsolatedColleague(t *testing.T) {
	srv, cleanup := clusterFixture(t)
	defer cleanup()
	reg := steer.NewRegistry(2)
	reg.SetClusterSteerer(&fakeCluster{})
	srv.SetSteerRegistry(reg)
	runID := clusterRun(t, srv, "acme", "r2")
	srv.store = sessionlessRunStore{Store: srv.store}

	if _, err := srv.runForSteer(isolatedMember("bob"), runID); !errors.Is(err, connector.ErrRunNotInFlight) {
		t.Errorf("isolated colleague: err = %v, want the opaque not-in-flight", err)
	}
	if run, err := srv.runForSteer(isolatedMember("alice"), runID); err != nil || run.ID != runID {
		t.Errorf("owner: %q, %v; want the run", run.ID, err)
	}
}
