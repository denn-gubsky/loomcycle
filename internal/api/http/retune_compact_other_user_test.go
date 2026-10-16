package http

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// A retune by someone other than the run's user lands at the run's next turn,
// and so does the loss of the user's scope: another user cannot point the
// user's own keys at a provider or model they choose. The run's own user's
// next message is then acted on without them too — the retune is still there.
func TestRetune_ByAnotherUserWithholdsTheRunUsersScope(t *testing.T) {
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
		if err := srv.RetuneRun(auth.WithPrincipal(context.Background(), tc.principal), run.ID, connector.RunOverrides{MaxTokens: 512}); err != nil {
			t.Fatalf("%s: RetuneRun: %v", tc.name, err)
		}
		if row, _ := srv.store.GetRun(context.Background(), run.ID); row.CredentialsNoUserScope != tc.withhold {
			t.Errorf("%s: after the retune the row's credentials_no_user_scope = %t, want %t", tc.name, row.CredentialsNoUserScope, tc.withhold)
		}
		if _, err := srv.SteerRun(auth.WithPrincipal(context.Background(), ouAlice), run.ID, "carry on", "api"); err != nil {
			t.Fatalf("%s: SteerRun: %v", tc.name, err)
		}
		waitFor(t, "the next turn's model call", func() bool { return len(prov.calls()) > 0 })
		if c := prov.calls()[0]; c.noUserScope != tc.withhold {
			t.Errorf("%s: alice's next turn ran with no-user-scope=%t, want %t", tc.name, c.noUserScope, tc.withhold)
		}
	}
}

// A compaction or a recap someone else asks for is a model call made on the
// run's — or chat's — behalf: it resolves its provider key without the user's
// own scope, as that person's own turn would.
func TestCompactAndRecap_ByAnotherUserWithholdTheUsersScope(t *testing.T) {
	for _, tc := range []struct {
		name     string
		subject  string
		withhold bool
	}{
		{"alice herself", "alice", false},
		{"bob", "bob", true},
	} {
		for what, call := range map[string]func(context.Context, *Server, string, string) error{
			"compact": func(ctx context.Context, srv *Server, _, runID string) error {
				_, err := srv.CompactRun(ctx, runID)
				return err
			},
			"recap": func(ctx context.Context, srv *Server, sessID, _ string) error {
				_, err := srv.RecapSession(ctx, sessID)
				return err
			},
		} {
			srv, scripted := compactFixture(t)
			prov := &scopeRecorder{Provider: scripted}
			srv.providers = &stubResolver{p: prov}
			sessID, runID := seedConversation(t, srv, true)
			ctx := auth.WithPrincipal(context.Background(), auth.Principal{Subject: tc.subject, Scopes: []string{auth.ScopeTenant}})
			if err := call(ctx, srv, sessID, runID); err != nil {
				t.Fatalf("%s %s: %v", tc.name, what, err)
			}
			calls := prov.calls()
			if len(calls) == 0 {
				t.Fatalf("%s %s: no model call", tc.name, what)
			}
			for _, c := range calls {
				if c.noUserScope != tc.withhold {
					t.Errorf("%s %s: the summary call ran with no-user-scope=%t, want %t", tc.name, what, c.noUserScope, tc.withhold)
				}
			}
		}
	}
}
