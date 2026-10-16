package http

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// identityRecorder wraps a provider and records the run identity each call
// was made under — the identity the run's tools resolve credentials for.
type identityRecorder struct {
	providers.Provider
	mu   sync.Mutex
	seen []tools.RunIdentityValue
}

func (p *identityRecorder) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.seen = append(p.seen, tools.RunIdentity(ctx))
	p.mu.Unlock()
	return p.Provider.Call(ctx, req)
}

func (p *identityRecorder) identities() []tools.RunIdentityValue {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]tools.RunIdentityValue(nil), p.seen...)
}

// A trigger's run that may not use its user's own scope records that on its
// row, the only place a resume can read it back from.
func TestRunOnce_ATriggersNoUserScopeIsRecordedOnTheRunsRow(t *testing.T) {
	srv, _ := makeServer(t, answeringProvider(), resumeTreeConfig("scripted", "", ""))
	for agentID, noUserScope := range map[string]bool{"a_hooked": true, "a_own": false} {
		err := srv.RunOnce(context.Background(), runner.RunInput{
			Agent: "resumer", AgentID: agentID, UserID: "alice", CredentialsNoUserScope: noUserScope,
			Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
		}, runner.RunCallbacks{})
		if err != nil {
			t.Fatalf("%s: RunOnce: %v", agentID, err)
		}
		run, err := srv.store.GetRunByAgentID(context.Background(), agentID)
		if err != nil {
			t.Fatal(err)
		}
		if run.CredentialsNoUserScope != noUserScope {
			t.Errorf("%s: row credentials_no_user_scope = %t, want %t", agentID, run.CredentialsNoUserScope, noUserScope)
		}
	}
}

// A resumed run comes back as narrow as it was started: resume has no trigger
// to re-derive the bit from, so it reads it off the row.
func TestResumePausedRuns_AResumedRunKeepsItsNoUserScope(t *testing.T) {
	prov := &identityRecorder{Provider: answeringProvider()}
	srv, _ := makeServer(t, prov, resumeTreeConfig("scripted", "", ""))
	run := createTreeRun(t, srv, store.RunIdentity{AgentID: "a_hooked", CredentialsNoUserScope: true})
	pauseMidTurn(t, srv, run)

	resumeOne(t, srv)
	waitRunStatus(t, srv.store, run.ID, store.RunCompleted)

	seen := prov.identities()
	if len(seen) == 0 {
		t.Fatal("the resumed run never called its model")
	}
	for _, ri := range seen {
		if ri.UserID != "alice" || !ri.CredentialsNoUserScope {
			t.Errorf("the resumed run called its model as user %q with CredentialsNoUserScope=%t, want alice and true", ri.UserID, ri.CredentialsNoUserScope)
		}
	}
}

// A queued run restarted after a restart keeps the bit whichever record of it
// survived: the row (as its other confinement bits) or the stored request.
func TestRestartInput_AQueuedRunKeepsItsNoUserScope(t *testing.T) {
	srv, _ := makeServer(t, answeringProvider(), resumeTreeConfig("scripted", "", ""))
	for name, tc := range map[string]struct{ row, request bool }{
		"on the row":         {row: true},
		"in the request":     {request: true},
		"on neither, absent": {},
	} {
		raw, err := json.Marshal(queuedRecord{Input: &runner.RunInput{Agent: "resumer", CredentialsNoUserScope: tc.request}})
		if err != nil {
			t.Fatal(err)
		}
		in, why := srv.restartInput(store.QueuedRun{Run: store.Run{ID: "r_q", UserID: "alice", CredentialsNoUserScope: tc.row}, Input: raw})
		if why != "" {
			t.Fatalf("%s: restartInput: %s", name, why)
		}
		if want := tc.row || tc.request; in.CredentialsNoUserScope != want {
			t.Errorf("%s: restarted CredentialsNoUserScope = %t, want %t", name, in.CredentialsNoUserScope, want)
		}
	}
}

// A triggered walk's row, and every member's, record the bit too: each is a
// run a resume rebuilds from its own row.
func TestStartTeamWalk_TheWalkAndItsMembersRecordTheNoUserScope(t *testing.T) {
	h := newTriggerWalkHarness(t)
	seedTenantTeam(t, h.st, "acme", "pr-review", varsTeam)
	runID, err := h.srv.StartTeamWalk(context.Background(), runner.TeamWalkInput{
		Team: "pr-review", Input: "the payload", TenantID: "acme", UserID: "u-42", CredentialsNoUserScope: true,
	})
	if err != nil {
		t.Fatalf("StartTeamWalk: %v", err)
	}
	if run := h.finished(runID); !run.CredentialsNoUserScope {
		t.Error("the walk's row does not record the no-user-scope bit")
	}
	members, _, err := h.st.ListRunsByWalk(context.Background(), "acme", runID, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	sawMember := false
	for _, m := range members {
		if m.ID == runID {
			continue
		}
		sawMember = true
		if !m.CredentialsNoUserScope {
			t.Errorf("member %s does not record the no-user-scope bit", m.ID)
		}
	}
	if !sawMember {
		t.Error("the walk spawned no member")
	}
}
