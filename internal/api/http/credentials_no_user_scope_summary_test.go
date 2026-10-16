package http

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// seedHookedConversation files a finished run that may not use its user's own
// scope (a webhook's), with a transcript worth summarising.
func seedHookedConversation(t *testing.T, srv *Server) (sessID, runID string) {
	t.Helper()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "compactor", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_hooked", UserID: "alice", Model: "stub-model", CredentialsNoUserScope: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	uinput := func(text string) []loop.PromptSegment {
		return []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: text}}}}
	}
	bulk := strings.Repeat("with enough substance that summarising it is a saving. ", 6)
	for i := 1; i <= 8; i++ {
		appendResumeEvent(t, srv, run.ID, "user_input", uinput(fmt.Sprintf("question %d %s", i, bulk)))
		appendResumeEvent(t, srv, run.ID, "text", providers.Event{Type: providers.EventText, Text: fmt.Sprintf("answer %d %s", i, bulk)})
		appendResumeEvent(t, srv, run.ID, "done", providers.Event{Type: providers.EventDone, StopReason: "end_turn"})
	}
	if err := srv.store.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	return sess.ID, run.ID
}

// A hook fired outside the run's loop (its run_end, a manual compaction's)
// resolves the $cred: in its webhook headers, and a code body calls tools, as
// the run. It gets the run's no-user-scope bit from the row, as the loop's
// own hooks get it from the loop's identity.
func TestWithHookRun_AHookOutsideTheLoopKeepsTheRunsNoUserScope(t *testing.T) {
	srv, _ := compactFixture(t)
	_, runID := seedHookedConversation(t, srv)
	ctx := srv.withHookRun(context.Background(), hooks.Identity{Agent: "compactor", UserID: "alice", AgentID: "a_hooked", RunID: runID})
	if ri := tools.RunIdentity(ctx); ri.UserID != "alice" || !ri.CredentialsNoUserScope {
		t.Errorf("the hook runs as user %q with CredentialsNoUserScope=%t, want alice and the run's bit", ri.UserID, ri.CredentialsNoUserScope)
	}
	// A run with no row to read — fail closed.
	ctx = srv.withHookRun(context.Background(), hooks.Identity{Agent: "compactor", UserID: "alice", AgentID: "a_gone", RunID: "r_gone"})
	if !tools.RunIdentity(ctx).CredentialsNoUserScope {
		t.Error("a hook whose run cannot be read takes its user's own scope")
	}
}

// A summary is a model call made outside the run loop, under an identity
// rebuilt from the run's row. A provider key the call resolves (a tenant's or
// user's own ANTHROPIC_API_KEY) must come from no wider a scope than the run's
// own calls did: a webhook's run whose summary took its payload user's
// personal key would spend it on the sender's behalf.
func TestCompactAndRecap_ASummaryOfAHookedRunKeepsItsNoUserScope(t *testing.T) {
	for name, call := range map[string]func(*Server, string, string) error{
		"compact": func(srv *Server, _, runID string) error {
			_, err := srv.CompactRun(context.Background(), runID)
			return err
		},
		"recap": func(srv *Server, sessID, _ string) error {
			_, err := srv.RecapSession(context.Background(), sessID)
			return err
		},
	} {
		srv, scripted := compactFixture(t)
		prov := &identityRecorder{Provider: scripted}
		srv.providers = &stubResolver{p: prov}
		sessID, runID := seedHookedConversation(t, srv)
		if err := call(srv, sessID, runID); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		seen := prov.identities()
		if len(seen) == 0 {
			t.Fatalf("%s: the summary never called its model", name)
		}
		for _, ri := range seen {
			if !ri.CredentialsNoUserScope {
				t.Errorf("%s: the summary called its model as user %q with CredentialsNoUserScope=false, want the run's bit", name, ri.UserID)
			}
		}
	}
}
