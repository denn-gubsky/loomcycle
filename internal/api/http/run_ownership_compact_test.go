package http

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Compaction is a run mutation: it makes a billed model call over the session's
// transcript and persists a marker (or pushes a summary into the live loop) that
// every later turn rebuilds from. The tenant fold alone let an isolated member
// do that to another user's conversation in its tenant.

// seedTenantConversation is seedConversation for a named tenant and user: a
// finished run whose transcript is long enough that compaction goes as far as
// the summary call and the marker.
func seedTenantConversation(t *testing.T, srv *Server, tenant, user, agentID string) (sessID, runID string) {
	t.Helper()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, tenant, "compactor", user)
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: agentID, UserID: user, TenantID: tenant, Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	bulk := strings.Repeat("with enough substance that summarising it is a saving. ", 6)
	for i := 1; i <= 8; i++ {
		appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{{Role: "user",
			Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: fmt.Sprintf("question %d %s", i, bulk)}}}})
		appendResumeEvent(t, srv, run.ID, "text", providers.Event{Type: providers.EventText, Text: fmt.Sprintf("answer %d %s", i, bulk)})
		appendResumeEvent(t, srv, run.ID, "done", providers.Event{Type: providers.EventDone, StopReason: "end_turn"})
	}
	if err := srv.store.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	return sess.ID, run.ID
}

// compactionMarkers counts the context_compaction events on a session.
func compactionMarkers(t *testing.T, srv *Server, sessID string) int {
	t.Helper()
	events, err := srv.store.GetTranscript(context.Background(), sessID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Type == string(providers.EventContextCompaction) {
			n++
		}
	}
	return n
}

func TestHandleCompactRun_IsolatedMemberCannotCompactAnotherUsersRun(t *testing.T) {
	srv, prov := compactFixture(t)
	aliceSess, aliceRun := seedTenantConversation(t, srv, "acme", "alice", "a_alice")
	bobSess, bobRun := seedTenantConversation(t, srv, "acme", "bob", "a_bob")

	compactAs := func(ctx context.Context, runID string) (int, string) {
		r := httptest.NewRequest(http.MethodPost, "/v1/runs/"+runID+"/compact", strings.NewReader(`{}`))
		r.SetPathValue("run_id", runID)
		rr := httptest.NewRecorder()
		srv.handleCompactRun(rr, r.WithContext(ctx))
		return rr.Code, rr.Body.String()
	}
	bob := tenantPrincipalCtx("acme", "bob", auth.ScopeUser)

	ghostCode, ghostBody := compactAs(bob, "run_ghost")
	if ghostCode != http.StatusNotFound {
		t.Fatalf("unknown run id: status %d, want 404", ghostCode)
	}
	if code, body := compactAs(bob, aliceRun); code != ghostCode || body != ghostBody {
		t.Errorf("isolated member compacting another user's run = %d %q, want the unknown-id reply %d %q", code, body, ghostCode, ghostBody)
	}
	if calls := prov.calls.Load(); calls != 0 {
		t.Errorf("the summary model was called %d time(s) for another user's run", calls)
	}
	if n := compactionMarkers(t, srv, aliceSess); n != 0 {
		t.Fatalf("another user's session got %d compaction marker(s)", n)
	}

	if code, body := compactAs(bob, bobRun); code != http.StatusOK || compactionMarkers(t, srv, bobSess) != 1 {
		t.Errorf("isolated member compacting its own run = %d %s, want 200 and a marker", code, body)
	}
	op := tenantPrincipalCtx("acme", "op", auth.ScopeTenant)
	if code, body := compactAs(op, aliceRun); code != http.StatusOK || compactionMarkers(t, srv, aliceSess) != 1 {
		t.Errorf("tenant operator compacting a member's run = %d %s, want 200 and a marker", code, body)
	}
}

// connector.CompactRun backs gRPC CompactRun and MCP compact_run.
func TestCompactRun_IsolatedMemberCannotCompactAnotherUsersRun(t *testing.T) {
	srv, prov := compactFixture(t)
	aliceSess, aliceRun := seedTenantConversation(t, srv, "acme", "alice", "a_alice")
	bob := tenantPrincipalCtx("acme", "bob", auth.ScopeUser)

	_, ghostErr := srv.CompactRun(bob, "run_ghost")
	res, err := srv.CompactRun(bob, aliceRun)
	var ghost, got *compactErr
	if !errors.As(ghostErr, &ghost) || !errors.As(err, &got) || *got != *ghost {
		t.Errorf("isolated member compacting another user's run = %+v, %v; want the unknown-id error %v", res, err, ghostErr)
	}
	if prov.calls.Load() != 0 || compactionMarkers(t, srv, aliceSess) != 0 {
		t.Fatal("another user's run was compacted through the connector")
	}
}
