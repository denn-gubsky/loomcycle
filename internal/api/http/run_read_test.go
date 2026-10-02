package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// GET /v1/runs/{run_id} is the run read addressed by the run itself. The agent
// read cannot do this job for a team walk: every walk of a team is filed under
// the one agent id `team:<name>`, which that route rejects (the colon) and
// which would name only the latest walk if it did not.

// runReadServer is an open-mode server (no auth configured), so the
// middleware passes a request's ctx through untouched and each test can put
// the principal it is asserting about on the request itself. Requests go
// through the real mux, so a missing route fails the test rather than
// compiling away.
func runReadServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:   map[string]config.AgentDef{"writer": {Model: "stub-model", SystemPrompt: "write"}},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "runread.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: &numberedProvider{}}, []tools.Tool{}, concurrency.New(4, 4, 100*time.Millisecond), st)
	return srv, srv.Mux()
}

// walkAs opens a team walk filed under (tenant, user), the identity a caller's
// run ctx carries.
func walkAs(t *testing.T, srv *Server, tenant, user, team string) (string, func(builtin.WalkEnd)) {
	t.Helper()
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{
		UserID: user, TenantID: tenant, AgentID: "caller",
	})
	_, runID, finish, err := srv.openTeamWalkRun(ctx, builtin.WalkRunSpec{Name: team, Detach: true})
	if err != nil {
		t.Fatalf("openTeamWalkRun: %v", err)
	}
	return runID, finish
}

func principalCtx(tenant, subject string, scopes ...string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: tenant, Subject: subject, Scopes: scopes,
	})
}

// getRun issues GET /v1/runs/{runID} through the mux as the principal on ctx
// (nil = open mode) and decodes a 200 body.
func getRun(t *testing.T, mux http.Handler, ctx context.Context, runID string) (int, agentResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/"+runID, nil)
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var body agentResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v: %s", err, rec.Body)
		}
	}
	return rec.Code, body
}

func TestGetRun_ReadsAWalkByItsOwnRunID(t *testing.T) {
	srv, mux := runReadServer(t)
	runID, finish := walkAs(t, srv, "acme", "alice", "triage")
	op := principalCtx("acme", "op", auth.ScopeTenant)

	code, got := getRun(t, mux, op, runID)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/runs/%s = %d, want 200", runID, code)
	}
	if got.RunID != runID || got.AgentID != teamWalkAgentPrefix+"triage" {
		t.Errorf("read (run %q, agent %q), want (%q, %q)", got.RunID, got.AgentID, runID, teamWalkAgentPrefix+"triage")
	}
	if got.Status != store.RunRunning || !got.Live {
		t.Errorf("a walk in flight read as (status %q, live %v), want (running, true) — a walk is not in the cancel registry, so live must come from the walk table",
			got.Status, got.Live)
	}

	finish(builtin.WalkEnd{FinalText: "the walk's answer"})
	_, got = getRun(t, mux, op, runID)
	if got.Status != store.RunCompleted || got.Live {
		t.Errorf("a finished walk read as (status %q, live %v), want (completed, false)", got.Status, got.Live)
	}
	var res struct {
		FinalText string `json:"final_text"`
	}
	if err := json.Unmarshal(got.Result, &res); err != nil || res.FinalText != "the walk's answer" {
		t.Errorf("result = %s, want the walk's final text", got.Result)
	}
}

// Two walks of one team share the agent id `team:triage`; each must be read by
// its own run id, and the agent route cannot address either.
func TestGetRun_TwoWalksOfOneTeamEachReadByTheirOwnRunID(t *testing.T) {
	srv, mux := runReadServer(t)
	first, finishFirst := walkAs(t, srv, "acme", "alice", "triage")
	second, finishSecond := walkAs(t, srv, "acme", "alice", "triage")
	defer finishSecond(builtin.WalkEnd{})
	finishFirst(builtin.WalkEnd{FinalText: "first answer"})

	_, a := getRun(t, mux, nil, first)
	_, b := getRun(t, mux, nil, second)
	if a.RunID != first || a.Status != store.RunCompleted {
		t.Errorf("first walk read as (run %q, status %q), want (%q, completed)", a.RunID, a.Status, first)
	}
	if b.RunID != second || b.Status != store.RunRunning || !b.Live {
		t.Errorf("second walk read as (run %q, status %q, live %v), want (%q, running, true)", b.RunID, b.Status, b.Live, second)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/agents/"+teamWalkAgentPrefix+"triage", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("GET /v1/agents/team:triage = %d; this test documents that the agent route cannot address a walk (400)", rec.Code)
	}
}

func TestGetRun_AnotherTenantGetsTheOpaque404(t *testing.T) {
	srv, mux := runReadServer(t)
	runID, finish := walkAs(t, srv, "acme", "alice", "triage")
	defer finish(builtin.WalkEnd{})

	missing, _ := getRun(t, mux, principalCtx("other", "op", auth.ScopeTenant), "r_does_not_exist")
	if code, _ := getRun(t, mux, principalCtx("other", "op", auth.ScopeTenant), runID); code != http.StatusNotFound || code != missing {
		t.Errorf("another tenant's read = %d, want 404 — the same answer an unknown id gets (%d)", code, missing)
	}
	if code, _ := getRun(t, mux, principalCtx("other", "root", auth.ScopeAdmin), runID); code != http.StatusOK {
		t.Errorf("admin read across tenants = %d, want 200", code)
	}
}

// An isolated member (substrate:user alone) reads only its own runs: the read
// carries the run's answer, spec and draft.
func TestGetRun_IsolatedMemberCannotReadAnotherUsersRun(t *testing.T) {
	srv, mux := runReadServer(t)
	runID, finish := walkAs(t, srv, "acme", "alice", "triage")
	defer finish(builtin.WalkEnd{})

	if code, _ := getRun(t, mux, principalCtx("acme", "bob", auth.ScopeUser), runID); code != http.StatusNotFound {
		t.Errorf("isolated member reading another user's run = %d, want 404", code)
	}
	if code, _ := getRun(t, mux, principalCtx("acme", "alice", auth.ScopeUser), runID); code != http.StatusOK {
		t.Errorf("isolated member reading its own run = %d, want 200", code)
	}
	if code, _ := getRun(t, mux, principalCtx("acme", "op", auth.ScopeTenant), runID); code != http.StatusOK {
		t.Errorf("tenant operator reading a member's run = %d, want 200", code)
	}
}

func TestGetRun_UnknownIDIs404AndAMalformedOneIs400(t *testing.T) {
	_, mux := runReadServer(t)
	if code, _ := getRun(t, mux, nil, "r_nope"); code != http.StatusNotFound {
		t.Errorf("unknown run id = %d, want 404", code)
	}
	// The 404 body is JSON a client can decode for its code.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/runs/r_nope", nil))
	var body struct{ Code, Error string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code != "unknown_run_id" {
		t.Errorf("404 body %s = (%+v, %v), want JSON with code unknown_run_id", rec.Body, body, err)
	}
	if code, _ := getRun(t, mux, nil, "team:triage"); code != http.StatusBadRequest {
		t.Errorf("malformed run id = %d, want 400", code)
	}
}

// live is about THIS run. An agent id's cancel-registry entry belongs to
// whichever of its runs is in flight, so an older run of the same agent id
// must not read as live because a newer one is.
func TestGetRun_LiveIsTheRunNotItsAgentID(t *testing.T) {
	srv, mux := runReadServer(t)
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "writer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	old, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_shared", UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	_, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	if err := srv.cancelReg.Register(cancel.Entry{AgentID: "a_shared", RunID: "r_newer"}, stop); err != nil {
		t.Fatal(err)
	}
	if _, got := getRun(t, mux, nil, old.ID); got.Live {
		t.Error("an older run read as live because its agent id's newer run is registered")
	}
	srv.cancelReg.Deregister("a_shared")
	if err := srv.cancelReg.Register(cancel.Entry{AgentID: "a_shared", RunID: old.ID}, stop); err != nil {
		t.Fatal(err)
	}
	if _, got := getRun(t, mux, nil, old.ID); !got.Live {
		t.Error("the registered run itself did not read as live")
	}
}

// The GET shares the path of the configured-run PATCH / DELETE, which are run
// writes; the read must sit at runs:read and leave their gate where it is.
func TestRequiredScopeFor_GetRunIsARunsRead(t *testing.T) {
	for _, tc := range []struct{ method, want string }{
		{http.MethodGet, auth.ScopeRunsRead},
		{http.MethodPatch, auth.ScopeRunsCreate},
		{http.MethodDelete, auth.ScopeRunsCreate},
	} {
		if got := requiredScopeFor(tc.method, "/v1/runs/r_1"); got != tc.want {
			t.Errorf("%s /v1/runs/r_1 scope = %q, want %q", tc.method, got, tc.want)
		}
	}
}
