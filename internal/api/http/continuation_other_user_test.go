package http

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// scopeSeen is what one model call ran under, read AT the call: the
// restriction can arrive while a run is live, so a later read would not say
// what that call had.
type scopeSeen struct {
	user        string
	noUserScope bool
	creds       map[string]string
}

type scopeRecorder struct {
	providers.Provider
	mu   sync.Mutex
	seen []scopeSeen
}

func (p *scopeRecorder) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	ri := tools.RunIdentity(ctx)
	p.mu.Lock()
	p.seen = append(p.seen, scopeSeen{ri.UserID, ri.NoUserScope(), ri.UserCredentials})
	p.mu.Unlock()
	return p.Provider.Call(ctx, req)
}

func (p *scopeRecorder) calls() []scopeSeen {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]scopeSeen(nil), p.seen...)
}

const (
	ouLegacySecret = "legacy-secret-for-tests"
	ouAliceSecret  = "alice-secret-for-tests"
	ouBobSecret    = "bob-secret-for-tests"
	ouRootSecret   = "root-secret-for-tests"
)

var (
	ouAlice  = auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant}}
	ouBob    = auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeTenant}}
	ouRoot   = auth.Principal{TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin}}
	ouLegacy = auth.Principal{TenantID: "default", Subject: "default", Scopes: []string{auth.ScopeAdmin}, Legacy: true}
)

// otherUserServer is a server whose "resumer" agent records the scope of each
// model call. authed declares alice, bob and an admin as principals behind
// bearers (and a legacy token); unauthed is open mode.
func otherUserServer(t *testing.T, authed bool) (*Server, *scopeRecorder) {
	t.Helper()
	st, err := storesqlite.Open(t.TempDir() + "/other-user.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := resumeTreeConfig("scripted", "", "")
	if authed {
		cfg.Env.AuthToken = ouLegacySecret
		cfg.ResolvedPrincipals = []auth.DeclaredPrincipal{
			{Secret: ouAliceSecret, Principal: ouAlice},
			{Secret: ouBobSecret, Principal: ouBob},
			{Secret: ouRootSecret, Principal: ouRoot},
		}
	}
	prov := &scopeRecorder{Provider: answeringProvider()}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(8, 8, time.Second), st)
	return srv, prov
}

// ouAliceSession is an ordinary chat of alice's in tenant acme: no trigger.
func ouAliceSession(t *testing.T, srv *Server) store.Session {
	t.Helper()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "acme", "resumer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_alice_first", UserID: "alice", TenantID: "acme", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	return sess
}

func ouPostMessage(t *testing.T, ts *httptest.Server, sessID, bearer, body string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/sessions/"+sessID+"/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST messages = %d %s", resp.StatusCode, b)
	}
}

// Anyone the tenant lets continue a session may, but the run is the session's
// user's: a continuation sent by someone else — another member, an admin —
// never resolves that user's own stored credentials. The user themselves, a
// legacy token (an integration acting for its users) and open mode keep them.
// Both continuation paths, judged by what the model call ran under and by the
// new run's row.
func TestContinuation_ByAnotherUserWithholdsTheSessionUsersScope(t *testing.T) {
	seg := []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go on"}}}}
	for _, tc := range []struct {
		name      string
		authed    bool
		principal *auth.Principal
		bearer    string
		withhold  bool
	}{
		{"alice herself", true, &ouAlice, ouAliceSecret, false},
		{"bob", true, &ouBob, ouBobSecret, true},
		{"an admin", true, &ouRoot, ouRootSecret, true},
		{"the legacy token", true, &ouLegacy, ouLegacySecret, false},
		{"open mode", false, nil, "", false},
	} {
		srv, prov := otherUserServer(t, tc.authed)
		sess := ouAliceSession(t, srv)
		ctx := context.Background()
		if tc.principal != nil {
			ctx = auth.WithPrincipal(ctx, *tc.principal)
		}
		if err := srv.RunOnce(ctx, runner.RunInput{SessionID: sess.ID, AgentID: "a_cont_runonce", Segments: seg}, runner.RunCallbacks{}); err != nil {
			t.Fatalf("%s: RunOnce continuation: %v", tc.name, err)
		}
		ts := httptest.NewServer(srv.Mux())
		ouPostMessage(t, ts, sess.ID, tc.bearer, `{"agent_id":"a_cont_http","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go on"}]}]}`)
		ts.Close()

		calls := prov.calls()
		if len(calls) != 2 {
			t.Fatalf("%s: %d model calls, want one per continuation", tc.name, len(calls))
		}
		for i, c := range calls {
			if c.user != "alice" || c.noUserScope != tc.withhold {
				t.Errorf("%s: continuation %d ran as %q with no-user-scope=%t, want alice and %t", tc.name, i, c.user, c.noUserScope, tc.withhold)
			}
		}
		for _, agentID := range []string{"a_cont_runonce", "a_cont_http"} {
			run, err := srv.store.GetRunByAgentID(context.Background(), agentID)
			if err != nil {
				t.Fatal(err)
			}
			if run.CredentialsNoUserScope != tc.withhold {
				t.Errorf("%s: %s row credentials_no_user_scope = %t, want %t", tc.name, agentID, run.CredentialsNoUserScope, tc.withhold)
			}
		}
		// Each run's latch goes with it.
		left := 0
		srv.userScopeLatches.Range(func(any, any) bool { left++; return true })
		if left != 0 {
			t.Errorf("%s: %d user-scope latch(es) outlived their runs", tc.name, left)
		}
	}
}

// What bob brings with him is his: per-run credentials on his continuation
// reach the run, while alice's stored ones stay out of reach.
func TestContinuation_AnotherUsersOwnPerRunCredentialsStillReachTheRun(t *testing.T) {
	srv, prov := otherUserServer(t, true)
	sess := ouAliceSession(t, srv)
	ctx := auth.WithPrincipal(context.Background(), ouBob)
	seg := []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go on"}}}}
	if err := srv.RunOnce(ctx, runner.RunInput{SessionID: sess.ID, Segments: seg, UserCredentials: map[string]string{"tok": "bobs-own-token"}}, runner.RunCallbacks{}); err != nil {
		t.Fatal(err)
	}
	calls := prov.calls()
	if len(calls) != 1 || calls[0].creds["tok"] != "bobs-own-token" || !calls[0].noUserScope {
		t.Errorf("bob's continuation ran with %+v, want his per-run credential and alice's scope withheld", calls)
	}
}

// parkedChatOf restores an interactive chat of alice's that ends on the
// assistant's turn, so it parks waiting for its operator.
func parkedChatOf(t *testing.T, srv *Server) store.Run {
	t.Helper()
	srv.SetSteerRegistry(steer.NewRegistry(0))
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "acme", "resumer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_chat", UserID: "alice", TenantID: "acme", Model: "stub-model", Interactive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "hi"}}},
	})
	appendResumeEvent(t, srv, run.ID, "text", providers.Event{Type: providers.EventText, Text: "hello"})
	appendResumeEvent(t, srv, run.ID, "done", providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}})
	if err := srv.store.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	if n, warns := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("resumed %d, want 1 (warnings: %v)", n, warns)
	}
	waitFor(t, "the chat to park", func() bool {
		return strings.Contains(runTranscriptText(t, srv.store, run.SessionID, run.ID), "awaiting_input")
	})
	return run
}

// A message sent into alice's live chat by someone else is a turn that person
// authored: from that turn on — and on resume — the run resolves no stored
// credential in alice's scope. Her own messages keep it.
func TestSteer_ByAnotherUserWithholdsTheRunUsersScope(t *testing.T) {
	for _, tc := range []struct {
		name      string
		principal auth.Principal
		withhold  bool
	}{
		{"alice herself", ouAlice, false},
		{"bob", ouBob, true},
	} {
		srv, prov := otherUserServer(t, true)
		run := parkedChatOf(t, srv)
		if _, err := srv.SteerRun(auth.WithPrincipal(context.Background(), tc.principal), run.ID, "carry on", "api"); err != nil {
			t.Fatalf("%s: SteerRun: %v", tc.name, err)
		}
		waitFor(t, "the steered turn's model call", func() bool { return len(prov.calls()) > 0 })
		if c := prov.calls()[0]; c.user != "alice" || c.noUserScope != tc.withhold {
			t.Errorf("%s: the steered turn ran as %q with no-user-scope=%t, want alice and %t", tc.name, c.user, c.noUserScope, tc.withhold)
		}
		row, err := srv.store.GetRun(context.Background(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.CredentialsNoUserScope != tc.withhold {
			t.Errorf("%s: the run's row credentials_no_user_scope = %t, want %t (a resume reads it)", tc.name, row.CredentialsNoUserScope, tc.withhold)
		}
	}
}
