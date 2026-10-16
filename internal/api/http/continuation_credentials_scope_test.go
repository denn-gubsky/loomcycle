package http

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// runsUnreadableStore fails every read of a session's runs.
type runsUnreadableStore struct{ store.Store }

func (runsUnreadableStore) RunsForSession(context.Context, string) ([]store.Run, error) {
	return nil, errors.New("injected: runs unreadable")
}

// continuationServer is a server whose "resumer" agent records the identity of
// each model call, over the given store (nil: a fresh sqlite one).
func continuationServer(t *testing.T, wrap func(store.Store) store.Store) (*Server, *identityRecorder) {
	t.Helper()
	st, err := storesqlite.Open(t.TempDir() + "/continue.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var s store.Store = st
	if wrap != nil {
		s = wrap(st)
	}
	prov := &identityRecorder{Provider: answeringProvider()}
	srv := New(resumeTreeConfig("scripted", "", ""), &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(8, 8, time.Second), s)
	return srv, prov
}

// seedSession files a finished run of the resumer agent for alice, with or
// without the no-user-scope bit a trigger's run carries.
func seedSession(t *testing.T, srv *Server, noUserScope bool) store.Session {
	t.Helper()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "resumer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_first", UserID: "alice", Model: "stub-model", CredentialsNoUserScope: noUserScope})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	return sess
}

// continueBoth continues sess once through RunOnce (gRPC Continue, MCP, the
// connector) and once through POST /v1/sessions/{id}/messages, and returns
// the agent ids of the two continuation runs.
func continueBoth(t *testing.T, srv *Server, sess store.Session) []string {
	t.Helper()
	seg := []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go on"}}}}
	if err := srv.RunOnce(context.Background(), runner.RunInput{SessionID: sess.ID, AgentID: "a_cont_runonce", Segments: seg}, runner.RunCallbacks{}); err != nil {
		t.Fatalf("RunOnce continuation: %v", err)
	}
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/sessions/"+sess.ID+"/messages", "application/json", strings.NewReader(
		`{"agent_id":"a_cont_http","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go on"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST messages = %d %s", resp.StatusCode, body)
	}
	return []string{"a_cont_runonce", "a_cont_http"}
}

// A continuation runs as the session's user. A trigger's session got that user
// from its definition or a delivery's payload, so its continuation — whoever
// sends it — resolves $cred: no more widely than the trigger's run did: the bit
// is inherited from the session's runs, on every continuation path.
func TestContinuation_ATriggerSessionKeepsItsNoUserScope(t *testing.T) {
	for _, noUserScope := range []bool{true, false} {
		srv, prov := continuationServer(t, nil)
		sess := seedSession(t, srv, noUserScope)
		for _, agentID := range continueBoth(t, srv, sess) {
			run, err := srv.store.GetRunByAgentID(context.Background(), agentID)
			if err != nil {
				t.Fatal(err)
			}
			if run.CredentialsNoUserScope != noUserScope {
				t.Errorf("session bit %t: continuation %s row credentials_no_user_scope = %t", noUserScope, agentID, run.CredentialsNoUserScope)
			}
		}
		seen := prov.identities()
		if len(seen) != 2 {
			t.Fatalf("session bit %t: %d model calls, want one per continuation", noUserScope, len(seen))
		}
		for _, ri := range seen {
			if ri.UserID != "alice" || ri.CredentialsNoUserScope != noUserScope {
				t.Errorf("session bit %t: a continuation called its model as %q with CredentialsNoUserScope=%t", noUserScope, ri.UserID, ri.CredentialsNoUserScope)
			}
		}
	}
}

// A session whose runs cannot be read withholds the user's scope: the bit
// cannot be shown absent, so it is assumed. (Both continuation paths read the
// runs once before this, for the session's team, and refuse on that fault;
// this pins the answer on its own, so it does not rest on that order.)
func TestSessionNoUserScope_AnUnreadableSessionWithholdsTheUsersScope(t *testing.T) {
	srv, _ := continuationServer(t, nil)
	sess := seedSession(t, srv, false)
	if srv.sessionNoUserScope(context.Background(), sess.ID) {
		t.Fatal("a readable session with no trigger run withholds the user's scope")
	}
	unreadable, _ := continuationServer(t, func(st store.Store) store.Store { return runsUnreadableStore{st} })
	if !unreadable.sessionNoUserScope(context.Background(), sess.ID) {
		t.Error("a session whose runs cannot be read keeps the user's scope")
	}
}
