package http

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// RFC HI P2: a client's lease on a page (§2), behind the real auth
// middleware, and the parked run it keeps (D4).

const leasePrefix = "/v1/workspaces/exposure-leases/"

// leaseServer is the state fixture (alice's pages A and B, bob's C, the
// exposure routes on one mux) with leases bounded at 8 h.
func leaseServer(t *testing.T) (*ticketFixture, *httptest.Server) {
	t.Helper()
	tf, ts := stateServer(t)
	tf.f.m.SetExpose(lifecycle.ExposeLimits{Enabled: true, MaxLease: 8 * time.Hour})
	return tf, ts
}

func leaseCall(t *testing.T, ts *httptest.Server, method, bearer, id, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+leasePrefix+id, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func leaseEndOf(t *testing.T, body string) time.Time {
	t.Helper()
	var out struct {
		LeaseExpiresAt time.Time `json:"lease_expires_at"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.LeaseExpiresAt.IsZero() {
		t.Fatalf("lease answer %q: %v", body, err)
	}
	return out.LeaseExpiresAt
}

// The page's own user takes a lease and the state reports it, as the page's
// deadline too; a shorter renewal keeps the later end; DELETE ends it.
func TestExposureLease_TakenRenewedAndReleasedByThePagesOwnUser(t *testing.T) {
	_, ts := leaseServer(t)
	before := time.Now()
	code, body := leaseCall(t, ts, "POST", "alice-token", testExposureID, `{"ttl_s": 7200}`)
	if code != http.StatusOK {
		t.Fatalf("lease = %d %s", code, body)
	}
	end := leaseEndOf(t, body)
	if end.Before(before.Add(2*time.Hour)) || end.After(time.Now().Add(2*time.Hour)) {
		t.Errorf("lease end = %v, want now + 2h", end)
	}
	code, body = stateGet(t, ts, "alice-token", exposurePrefix+testExposureID)
	s := decodeState(t, body)
	if code != http.StatusOK || s.LeaseExpiresAt == nil || !s.LeaseExpiresAt.Equal(end) || s.ExpiresAt == nil || !s.ExpiresAt.Equal(end) {
		t.Errorf("state = %d %+v, want lease_expires_at and expires_at %v", code, s, end)
	}
	if code, body := leaseCall(t, ts, "POST", "alice-token", testExposureID, `{"ttl_s": 60}`); code != http.StatusOK || !leaseEndOf(t, body).Equal(end) {
		t.Errorf("a 60s renewal = %d %s, want the 2h lease kept (%v)", code, body, end)
	}
	if code, body := leaseCall(t, ts, "DELETE", "alice-token", testExposureID, ""); code != http.StatusNoContent {
		t.Fatalf("release = %d %s", code, body)
	}
	if _, body := stateGet(t, ts, "alice-token", exposurePrefix+testExposureID); decodeState(t, body).LeaseExpiresAt != nil {
		t.Errorf("after release the state reads %s, want no lease", body)
	}
}

// G38 acceptance through the routes: a leased page no request used for an
// hour is still served after the sweep; released, the next sweep closes it
// idle and it is gone.
func TestExposureLease_ALeasedPageIsServedPastItsIdleTimeout(t *testing.T) {
	tf, ts := leaseServer(t)
	ctx := context.Background()
	if code, body := leaseCall(t, ts, "POST", "alice-token", testExposureID, `{"ttl_s": 7200}`); code != http.StatusOK {
		t.Fatalf("lease = %d %s", code, body)
	}
	// An hour passes with no request: well past the 15m idle timeout.
	if err := tf.f.st.WorkspaceExposureTouch(ctx, "acme", testExposureID, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, err := tf.f.m.SweepExposures(ctx); n != 0 || err != nil {
		t.Fatalf("sweep = %d, %v; want the leased page kept", n, err)
	}
	if code, body := stateGet(t, ts, "alice-token", pagePath); code != http.StatusOK || body != "hello from the workspace" {
		t.Errorf("the leased page an hour unused = %d %q, want served", code, body)
	}
	// That request was a use: put the last use back an hour.
	if err := tf.f.st.WorkspaceExposureTouch(ctx, "acme", testExposureID, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if code, _ := leaseCall(t, ts, "DELETE", "alice-token", testExposureID, ""); code != http.StatusNoContent {
		t.Fatalf("release = %d", code)
	}
	if n, _ := tf.f.m.SweepExposures(ctx); n != 1 {
		t.Fatalf("sweep after release closed %d, want the page", n)
	}
	_, body := stateGet(t, ts, "alice-token", exposurePrefix+testExposureID)
	if s := decodeState(t, body); s.State != exposureStateClosed || s.ClosedReason != store.WorkspaceExposureClosedIdle {
		t.Errorf("state after release and sweep = %+v, want closed idle", s)
	}
	if code, _ := stateGet(t, ts, "alice-token", pagePath); code != http.StatusNotFound {
		t.Errorf("the closed page = %d, want 404", code)
	}
}

// Another user's page, an unknown id, a closed page and a caller naming no
// user all get the unknown id's answer, taking and releasing; nothing is
// written.
func TestExposureLease_AnythingButTheUsersLivePageIsUnknown(t *testing.T) {
	tf, ts := leaseServer(t)
	ctx := context.Background()
	if _, err := tf.f.st.WorkspaceExposureClose(ctx, "acme", exposureB, store.WorkspaceExposureClosedUnexposed, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, unknownBody := stateGet(t, ts, "alice-token", exposurePrefix+"ffffffffffffffffffffffff")
	for name, c := range map[string]struct{ bearer, id string }{
		"bob on alice's page": {"bob-token", testExposureID},
		"alice on bob's page": {"alice-token", exposureC},
		"unknown":             {"alice-token", "ffffffffffffffffffffffff"},
		"closed":              {"alice-token", exposureB},
		"the legacy token":    {"legacy-secret", testExposureID},
		"a malformed page id": {"alice-token", "nothex"},
	} {
		for _, m := range []string{"POST", "DELETE"} {
			code, body := leaseCall(t, ts, m, c.bearer, c.id, `{"ttl_s": 60}`)
			if code != http.StatusNotFound || body != unknownBody {
				t.Errorf("%s %s = %d %q, want the unknown id's 404 %q", m, name, code, body, unknownBody)
			}
		}
	}
	for _, id := range []string{testExposureID, exposureB, exposureC} {
		if e, _ := tf.f.st.WorkspaceExposureGet(ctx, "acme", id); e.LeaseExpiresAt != nil {
			t.Errorf("a refused lease was written on %s: %v", id, e.LeaseExpiresAt)
		}
	}
}

// ttl_s is 1 to max_lease_s: over the bound, missing, not positive, or not a
// whole number is a 400, the bound named; checked before the page is looked
// up, so another user's page answers it the same.
func TestExposureLease_TTLOverTheBoundIsRefusedNamingIt(t *testing.T) {
	_, ts := leaseServer(t)
	for _, body := range []string{`{"ttl_s": 28801}`, `{"ttl_s": 0}`, `{"ttl_s": -5}`, `{}`, `{"ttl_s": 9223372036854775807}`} {
		for _, id := range []string{testExposureID, exposureC} {
			code, out := leaseCall(t, ts, "POST", "alice-token", id, body)
			if code != http.StatusBadRequest || !strings.Contains(out, "28800") || !strings.Contains(out, "max_lease_s") {
				t.Errorf("%s on %s = %d %s, want 400 naming max_lease_s 28800", body, id, code, out)
			}
		}
	}
	for _, body := range []string{`{"ttl_s": 1.5}`, `ttl=60`, ``} {
		if code, out := leaseCall(t, ts, "POST", "alice-token", testExposureID, body); code != http.StatusBadRequest {
			t.Errorf("body %q = %d %s, want 400", body, code, out)
		}
	}
}

// A Web UI cookie never takes or ends a lease, nor does a request with no
// bearer header: a page of another site cannot keep pages alive in the
// user's name.
func TestExposureLease_NeverTakenByACookie(t *testing.T) {
	tf, _ := leaseServer(t)
	for _, c := range []struct {
		method string
		h      http.HandlerFunc
	}{{"POST", tf.s.handleExposureLease}, {"DELETE", tf.s.handleExposureLeaseRelease}} {
		for name, cookie := range map[string]bool{"cookie": true, "no header": false} {
			r := httptest.NewRequest(c.method, leasePrefix+testExposureID, strings.NewReader(`{"ttl_s": 60}`))
			r.SetPathValue("id", testExposureID)
			ctx := auth.WithPrincipal(r.Context(), *alice)
			if cookie {
				r.Header.Set("Authorization", "Bearer alice-token")
				ctx = withUIAuth(ctx, uiAuth{})
			}
			r = r.WithContext(ctx)
			w := httptest.NewRecorder()
			c.h(w, r)
			if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "bearer_required") {
				t.Errorf("%s by %s = %d %s, want 401 bearer_required", c.method, name, w.Code, w.Body.String())
			}
		}
	}
	if e, _ := tf.f.st.WorkspaceExposureGet(context.Background(), "acme", testExposureID); e.LeaseExpiresAt != nil {
		t.Errorf("a cookie wrote a lease: %v", e.LeaseExpiresAt)
	}
}

// max_lease_s 0 turns leases off: 403 leases_off, for any id (it says
// nothing of a page), and no run is given a lease hook.
func TestExposureLease_RefusedWhenLeasesAreOff(t *testing.T) {
	tf, ts := leaseServer(t)
	tf.f.m.SetExpose(lifecycle.ExposeLimits{Enabled: true})
	for _, id := range []string{testExposureID, "ffffffffffffffffffffffff"} {
		for _, m := range []string{"POST", "DELETE"} {
			if code, body := leaseCall(t, ts, m, "alice-token", id, `{"ttl_s": 60}`); code != http.StatusForbidden || !strings.Contains(body, "leases_off") {
				t.Errorf("%s on %s = %d %s, want 403 leases_off", m, id, code, body)
			}
		}
	}
	if tf.s.idleExtendFor("run1") != nil {
		t.Error("a run got a lease hook with leases off")
	}
}

// leaseRunExposures is the real bookkeeping with every page's dial sent to
// one test application.
type leaseRunExposures struct {
	*lifecycle.Manager
	app *httptest.Server
}

func (x leaseRunExposures) DialExposure(ctx context.Context, _ store.WorkspaceRow, _ int) (io.ReadWriteCloser, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", x.app.Listener.Addr().String())
}

// G38 acceptance, D4: a parked interactive run whose page holds a live lease
// is still parked after its idle timeout, and the page answers; once the
// lease ends, the run ends idle and its page goes with its workspace.
func TestExposureLease_ALeasedPageKeepsItsParkedRun(t *testing.T) {
	agents := map[string]config.AgentDef{"builder": {Model: "stub-model", SystemPrompt: "b", Tools: []string{"Workspace"}, Workspace: wsAll}}
	f := newWSRunFixture(t, agents, &scriptedProvider{scripts: [][]providers.Event{
		wsCall("t1", `{"op":"open","name":"work"}`), wsDone}})
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "chart") }))
	t.Cleanup(app.Close)
	f.mgr.SetExpose(lifecycle.ExposeLimits{Enabled: true, MaxLease: 8 * time.Hour})
	f.srv.SetWorkspaceExposures(leaseRunExposures{Manager: f.mgr, app: app}, config.WorkspaceExposeConfig{MaxConnections: 8,
		RequestsPerSecond: 100, BytesPerSecond: 1 << 30, MaxMessageBytes: 1 << 20, IdleTimeoutS: 900, SocketLifetimeS: 3600})
	const idle = 300 * time.Millisecond
	f.srv.cfg().Env.InteractiveIdleTimeout = idle

	in := f.input("builder")
	in.Interactive = true
	cb := f.callbacks()
	runIDs := make(chan string, 1)
	cb.OnRegistered = func(_, runID, _, _ string) { runIDs <- runID }
	done := make(chan error, 1)
	go func() { done <- f.srv.RunOnce(context.Background(), in, cb) }()
	runID := <-runIDs
	waitUntil(t, "the open result", func() bool { return len(f.results()) >= 1 })
	ctx := context.Background()
	rows, err := f.st.WorkspaceListByOwnerRun(ctx, "acme", runID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("the run's workspaces = %v, %v", rows, err)
	}
	const page = "abcdefabcdefabcdefabcdef"
	if _, err := f.st.WorkspaceExposureCreate(ctx, store.WorkspaceExposure{ID: page, TenantID: "acme", UserID: "bob",
		WorkspaceID: rows[0].ID, Port: 8000, Kind: store.WorkspaceExposurePage, RunID: runID}, 2); err != nil {
		t.Fatal(err)
	}
	leaseEnd, err := f.mgr.LeaseExposure(ctx, "acme", "bob", page, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(3 * idle) // well past the run's idle timeout, within the lease
	select {
	case err := <-done:
		t.Fatalf("the run ended %v before its page's lease did: %v", time.Until(leaseEnd), err)
	default:
	}
	if run, _ := f.st.GetRun(ctx, runID); run.Status != store.RunRunning {
		t.Errorf("the leased run is %q, want still running (parked)", run.Status)
	}
	r := httptest.NewRequest("GET", exposurePrefix+page+"/", nil)
	r.Header.Set("Authorization", "Bearer lc_test")
	r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{"runs:read"}}))
	r.SetPathValue("id", page)
	r.SetPathValue("path", "")
	w := httptest.NewRecorder()
	f.srv.handleWorkspaceExposure(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "chart" {
		t.Errorf("the leased page past the run's idle timeout = %d %q, want served", w.Code, w.Body.String())
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not end after its page's lease")
	}
	if time.Now().Before(leaseEnd) {
		t.Errorf("the run ended %v before its page's lease", time.Until(leaseEnd))
	}
	run := waitRunStatus(t, f.st, runID, store.RunCompleted)
	if run.StopReason != loop.StopReasonIdleTimeout {
		t.Errorf("stop reason = %q, want %q", run.StopReason, loop.StopReasonIdleTimeout)
	}
	waitUntil(t, "the page to close with its workspace", func() bool {
		e, err := f.st.WorkspaceExposureGet(ctx, "acme", page)
		return err == nil && !e.Live() && e.ClosedReason == store.WorkspaceExposureClosedReleased
	})
}

// A top-level chat restored after a restart waits for its operator as a live
// one does, and a leased page of its workspace keeps it past its idle
// timeout the same way, until the lease ends.
func TestExposureLease_ALeasedPageKeepsARestoredParkedRun(t *testing.T) {
	srv, _, _, run := parkedRunFixture(t, true)
	const idle = 200 * time.Millisecond
	srv.cfg().Env.InteractiveIdleTimeout = idle
	ctx := context.Background()
	m := lifecycle.New(nil, srv.store, lifecycle.Limits{}, nil)
	m.SetExpose(lifecycle.ExposeLimits{Enabled: true, MaxLease: 8 * time.Hour})
	srv.SetWorkspaceExposures(m, config.WorkspaceExposeConfig{MaxConnections: 8, RequestsPerSecond: 100, BytesPerSecond: 1 << 30,
		MaxMessageBytes: 1 << 20, IdleTimeoutS: 900, SocketLifetimeS: 3600})
	if _, err := srv.store.WorkspaceCreate(ctx, store.WorkspaceRow{ID: "ws_r", TenantID: run.TenantID, UserID: "alice", Name: "web",
		WorkerID: "wk1", Engine: "host", OwnerRunID: run.ID, RootRunID: run.ID, State: store.WorkspaceReady}); err != nil {
		t.Fatal(err)
	}
	const page = "fedcbafedcbafedcbafedcba"
	if _, err := srv.store.WorkspaceExposureCreate(ctx, store.WorkspaceExposure{ID: page, TenantID: run.TenantID, UserID: "alice",
		WorkspaceID: "ws_r", Port: 8000, Kind: store.WorkspaceExposurePage}, 2); err != nil {
		t.Fatal(err)
	}
	leaseEnd, err := srv.store.WorkspaceExposureExtendLease(ctx, run.TenantID, page, time.Now().Add(1500*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if n, warns := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("resumed %d, want 1 (warnings: %v)", n, warns)
	}
	time.Sleep(4 * idle)
	if got, _ := srv.store.GetRun(ctx, run.ID); got.Status != store.RunRunning {
		t.Fatalf("the restored run with a leased page is %q past its idle timeout, want still parked", got.Status)
	}
	got := waitRunStatus(t, srv.store, run.ID, store.RunCompleted)
	if got.StopReason != loop.StopReasonIdleTimeout || time.Now().Before(leaseEnd) {
		t.Errorf("stop reason = %q at %v before the lease's end; want %q after it", got.StopReason, time.Until(leaseEnd), loop.StopReasonIdleTimeout)
	}
}
